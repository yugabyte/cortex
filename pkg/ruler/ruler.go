package ruler

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	ot "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/promql"
	promRules "github.com/prometheus/prometheus/rules"
	promStorage "github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/strutil"
	"github.com/weaveworks/common/user"
	"golang.org/x/net/context/ctxhttp"
	"google.golang.org/grpc"

	"github.com/cortexproject/cortex/pkg/ingester/client"
	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/ruler/rules"
	store "github.com/cortexproject/cortex/pkg/ruler/rules"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/flagext"
	"github.com/cortexproject/cortex/pkg/util/services"
)

var (
	ringCheckErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "cortex",
		Name:      "ruler_ring_check_errors_total",
		Help:      "Number of errors that have occurred when checking the ring for ownership",
	})
	configUpdatesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "cortex",
		Name:      "ruler_config_updates_total",
		Help:      "Total number of config updates triggered by a user",
	}, []string{"user"})
)

// Config is the configuration for the recording rules server.
type Config struct {
	ExternalURL        flagext.URLValue // This is used for template expansion in alerts; must be a valid URL
	EvaluationInterval time.Duration    // How frequently to evaluate rules by default.
	PollInterval       time.Duration    // How frequently to poll for updated rules
	StoreConfig        RuleStoreConfig  // Rule Storage and Polling configuration
	RulePath           string           // Path to store rule files for prom manager

	AlertmanagerURL             flagext.URLValue // URL of the Alertmanager to send notifications to.
	AlertmanagerDiscovery       bool             // Whether to use DNS SRV records to discover alertmanagers.
	AlertmanagerRefreshInterval time.Duration    // How long to wait between refreshing the list of alertmanagers based on DNS service discovery.
	AlertmanangerEnableV2API    bool             // Enables the ruler notifier to use the alertmananger V2 API
	NotificationQueueCapacity   int              // Capacity of the queue for notifications to be sent to the Alertmanager.
	NotificationTimeout         time.Duration    // HTTP timeout duration when sending notifications to the Alertmanager.

	EnableSharding   bool // Enable sharding rule groups
	SearchPendingFor time.Duration
	Ring             RingConfig
	FlushCheckPeriod time.Duration

	EnableAPI bool `yaml:"enable_api"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.StoreConfig.RegisterFlags(f)
	cfg.Ring.RegisterFlags(f)

	// Deprecated Flags that will be maintained to avoid user disruption
	flagext.DeprecatedFlag(f, "ruler.client-timeout", "This flag has been renamed to ruler.configs.client-timeout")
	flagext.DeprecatedFlag(f, "ruler.group-timeout", "This flag is no longer functional.")
	flagext.DeprecatedFlag(f, "ruler.num-workers", "This flag is no longer functional. For increased concurrency horizontal sharding is recommended")

	cfg.ExternalURL.URL, _ = url.Parse("") // Must be non-nil
	f.Var(&cfg.ExternalURL, "ruler.external.url", "URL of alerts return path.")
	f.DurationVar(&cfg.EvaluationInterval, "ruler.evaluation-interval", 1*time.Minute, "How frequently to evaluate rules")
	f.DurationVar(&cfg.PollInterval, "ruler.poll-interval", 1*time.Minute, "How frequently to poll for rule changes")
	f.Var(&cfg.AlertmanagerURL, "ruler.alertmanager-url", "URL of the Alertmanager to send notifications to.")
	f.BoolVar(&cfg.AlertmanagerDiscovery, "ruler.alertmanager-discovery", false, "Use DNS SRV records to discover alertmanager hosts.")
	f.DurationVar(&cfg.AlertmanagerRefreshInterval, "ruler.alertmanager-refresh-interval", 1*time.Minute, "How long to wait between refreshing alertmanager hosts.")
	f.BoolVar(&cfg.AlertmanangerEnableV2API, "ruler.alertmanager-use-v2", false, "If enabled requests to alertmanager will utilize the V2 API.")
	f.IntVar(&cfg.NotificationQueueCapacity, "ruler.notification-queue-capacity", 10000, "Capacity of the queue for notifications to be sent to the Alertmanager.")
	f.DurationVar(&cfg.NotificationTimeout, "ruler.notification-timeout", 10*time.Second, "HTTP timeout duration when sending notifications to the Alertmanager.")
	if flag.Lookup("promql.lookback-delta") == nil {
		flag.DurationVar(&promql.LookbackDelta, "promql.lookback-delta", promql.LookbackDelta, "Time since the last sample after which a time series is considered stale and ignored by expression evaluations.")
	}
	f.DurationVar(&cfg.SearchPendingFor, "ruler.search-pending-for", 5*time.Minute, "Time to spend searching for a pending ruler when shutting down.")
	f.BoolVar(&cfg.EnableSharding, "ruler.enable-sharding", false, "Distribute rule evaluation using ring backend")
	f.DurationVar(&cfg.FlushCheckPeriod, "ruler.flush-period", 1*time.Minute, "Period with which to attempt to flush rule groups.")
	f.StringVar(&cfg.RulePath, "ruler.rule-path", "/rules", "file path to store temporary rule files for the prometheus rule managers")
	f.BoolVar(&cfg.EnableAPI, "experimental.ruler.enable-api", false, "Enable the ruler api")
}

// Ruler evaluates rules.
type Ruler struct {
	services.Service

	cfg         Config
	engine      *promql.Engine
	queryable   promStorage.Queryable
	pusher      Pusher
	alertURL    *url.URL
	notifierCfg *config.Config

	lifecycler  *ring.Lifecycler
	ring        *ring.Ring
	subservices *services.Manager

	store          rules.RuleStore
	mapper         *mapper
	userManagerMtx sync.Mutex
	userManagers   map[string]*promRules.Manager

	// Per-user notifiers with separate queues.
	notifiersMtx sync.Mutex
	notifiers    map[string]*rulerNotifier

	registry prometheus.Registerer
	logger   log.Logger
}

// NewRuler creates a new ruler from a distributor and chunk store.
func NewRuler(cfg Config, engine *promql.Engine, queryable promStorage.Queryable, pusher Pusher, reg prometheus.Registerer, logger log.Logger) (*Ruler, error) {
	ncfg, err := buildNotifierConfig(&cfg)
	if err != nil {
		return nil, err
	}

	ruleStore, err := NewRuleStorage(cfg.StoreConfig)
	if err != nil {
		return nil, err
	}

	ruler := &Ruler{
		cfg:          cfg,
		engine:       engine,
		queryable:    queryable,
		alertURL:     cfg.ExternalURL.URL,
		notifierCfg:  ncfg,
		notifiers:    map[string]*rulerNotifier{},
		store:        ruleStore,
		pusher:       pusher,
		mapper:       newMapper(cfg.RulePath, logger),
		userManagers: map[string]*promRules.Manager{},
		registry:     reg,
		logger:       logger,
	}

	ruler.Service = services.NewBasicService(ruler.starting, ruler.run, ruler.stopping)
	return ruler, nil
}

func (r *Ruler) starting(ctx context.Context) error {
	// If sharding is enabled, create/join a ring to distribute tokens to
	// the ruler
	if r.cfg.EnableSharding {
		lifecyclerCfg := r.cfg.Ring.ToLifecyclerConfig()
		var err error
		r.lifecycler, err = ring.NewLifecycler(lifecyclerCfg, r, "ruler", ring.RulerRingKey, true)
		if err != nil {
			return errors.Wrap(err, "failed to initialize ruler's lifecycler")
		}

		r.ring, err = ring.New(lifecyclerCfg.RingConfig, "ruler", ring.RulerRingKey)
		if err != nil {
			return errors.Wrap(err, "failed to initialize ruler's ring")
		}

		r.subservices, err = services.NewManager(r.lifecycler, r.ring)
		if err == nil {
			err = services.StartManagerAndAwaitHealthy(ctx, r.subservices)
		}
		return errors.Wrap(err, "failed to start ruler's services")
	}

	// TODO: ideally, ruler would wait until its queryable is finished starting.
	return nil
}

// Stop stops the Ruler.
// Each function of the ruler is terminated before leaving the ring
func (r *Ruler) stopping() error {
	r.notifiersMtx.Lock()
	for _, n := range r.notifiers {
		n.stop()
	}
	r.notifiersMtx.Unlock()

	if r.subservices != nil {
		// subservices manages ring and lifecycler, if sharding was enabled.
		_ = services.StopManagerAndAwaitStopped(context.Background(), r.subservices)
	}

	level.Info(r.logger).Log("msg", "stopping user managers")
	wg := sync.WaitGroup{}
	r.userManagerMtx.Lock()
	for user, manager := range r.userManagers {
		level.Debug(r.logger).Log("msg", "shutting down user  manager", "user", user)
		wg.Add(1)
		go func(manager *promRules.Manager, user string) {
			manager.Stop()
			wg.Done()
			level.Debug(r.logger).Log("msg", "user manager shut down", "user", user)
		}(manager, user)
	}
	wg.Wait()
	r.userManagerMtx.Unlock()
	level.Info(r.logger).Log("msg", "all user managers stopped")
	return nil
}

// sendAlerts implements a rules.NotifyFunc for a Notifier.
// It filters any non-firing alerts from the input.
//
// Copied from Prometheus's main.go.
func sendAlerts(n *notifier.Manager, externalURL string) promRules.NotifyFunc {
	return func(ctx context.Context, expr string, alerts ...*promRules.Alert) {
		var res []*notifier.Alert

		for _, alert := range alerts {
			// Only send actually firing alerts.
			if alert.State == promRules.StatePending {
				continue
			}
			a := &notifier.Alert{
				StartsAt:     alert.FiredAt,
				Labels:       alert.Labels,
				Annotations:  alert.Annotations,
				GeneratorURL: externalURL + strutil.TableLinkForExpression(expr),
			}
			if !alert.ResolvedAt.IsZero() {
				a.EndsAt = alert.ResolvedAt
			}
			res = append(res, a)
		}

		if len(alerts) > 0 {
			n.Send(res...)
		}
	}
}

func (r *Ruler) getOrCreateNotifier(userID string) (*notifier.Manager, error) {
	r.notifiersMtx.Lock()
	defer r.notifiersMtx.Unlock()

	n, ok := r.notifiers[userID]
	if ok {
		return n.notifier, nil
	}

	n = newRulerNotifier(&notifier.Options{
		QueueCapacity: r.cfg.NotificationQueueCapacity,
		Do: func(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
			// Note: The passed-in context comes from the Prometheus notifier
			// and does *not* contain the userID. So it needs to be added to the context
			// here before using the context to inject the userID into the HTTP request.
			ctx = user.InjectOrgID(ctx, userID)
			if err := user.InjectOrgIDIntoHTTPRequest(ctx, req); err != nil {
				return nil, err
			}
			// Jaeger complains the passed-in context has an invalid span ID, so start a new root span
			sp := ot.GlobalTracer().StartSpan("notify", ot.Tag{Key: "organization", Value: userID})
			defer sp.Finish()
			ctx = ot.ContextWithSpan(ctx, sp)
			_ = ot.GlobalTracer().Inject(sp.Context(), ot.HTTPHeaders, ot.HTTPHeadersCarrier(req.Header))
			return ctxhttp.Do(ctx, client, req)
		},
	}, util.Logger)

	go n.run()

	// This should never fail, unless there's a programming mistake.
	if err := n.applyConfig(r.notifierCfg); err != nil {
		return nil, err
	}

	r.notifiers[userID] = n
	return n.notifier, nil
}

func (r *Ruler) ownsRule(hash uint32) (bool, error) {
	rlrs, err := r.ring.Get(hash, ring.Read, []ring.IngesterDesc{})
	if err != nil {
		level.Warn(r.logger).Log("msg", "error reading ring to verify rule group ownership", "err", err)
		ringCheckErrors.Inc()
		return false, err
	}
	if rlrs.Ingesters[0].Addr == r.lifecycler.Addr {
		level.Debug(r.logger).Log("msg", "rule group owned", "owner_addr", rlrs.Ingesters[0].Addr, "addr", r.lifecycler.Addr)
		return true, nil
	}
	level.Debug(r.logger).Log("msg", "rule group not owned, address does not match", "owner_addr", rlrs.Ingesters[0].Addr, "addr", r.lifecycler.Addr)
	return false, nil
}

func (r *Ruler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.cfg.EnableSharding {
		r.ring.ServeHTTP(w, req)
	} else {
		var unshardedPage = `
			<!DOCTYPE html>
			<html>
				<head>
					<meta charset="UTF-8">
					<title>Cortex Ruler Status</title>
				</head>
				<body>
					<h1>Cortex Ruler Status</h1>
					<p>Ruler running with shards disabled</p>
				</body>
			</html>`
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte(unshardedPage))
		if err != nil {
			level.Error(r.logger).Log("msg", "unable to serve status page", "err", err)
		}
	}
}

func (r *Ruler) run(ctx context.Context) error {
	level.Info(r.logger).Log("msg", "ruler up and running")

	tick := time.NewTicker(r.cfg.PollInterval)
	defer tick.Stop()

	r.loadRules(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			r.loadRules(ctx)
		}
	}
}

func (r *Ruler) loadRules(ctx context.Context) {
	ringHasher := fnv.New32a()

	configs, err := r.store.ListAllRuleGroups(ctx)
	if err != nil {
		level.Error(r.logger).Log("msg", "unable to poll for rules", "err", err)
		return
	}

	// Iterate through each users configuration and determine if the on-disk
	// configurations need to be updated
	for user, cfg := range configs {
		filteredGroups := store.RuleGroupList{}

		// If sharding is enabled, prune the rule group to only contain rules
		// this ruler is responsible for.
		if r.cfg.EnableSharding {
			for _, g := range cfg {
				id := g.User + "/" + g.Namespace + "/" + g.Name
				ringHasher.Reset()
				_, err = ringHasher.Write([]byte(id))
				if err != nil {
					level.Error(r.logger).Log("msg", "failed to create group for user", "user", user, "namespace", g.Namespace, "group", g.Name, "err", err)
					continue
				}
				hash := ringHasher.Sum32()
				owned, err := r.ownsRule(hash)
				if err != nil {
					level.Error(r.logger).Log("msg", "unable to verify rule group ownership ownership, will retry on the next poll", "err", err)
					return
				}
				if owned {
					filteredGroups = append(filteredGroups, g)
				}
			}
		} else {
			filteredGroups = cfg
		}

		r.syncManager(ctx, user, filteredGroups)
	}

	// Check for deleted users and remove them
	r.userManagerMtx.Lock()
	defer r.userManagerMtx.Unlock()
	for user, mngr := range r.userManagers {
		if _, exists := configs[user]; !exists {
			go mngr.Stop()
			delete(r.userManagers, user)
			level.Info(r.logger).Log("msg", "deleting rule manager", "user", user)
		}
	}

}

// syncManager maps the rule files to disk, detects any changes and will create/update the
// the users Prometheus Rules Manager.
func (r *Ruler) syncManager(ctx context.Context, user string, groups store.RuleGroupList) {
	// A lock is taken to ensure if syncManager is called concurrently, that each call
	// returns after the call map files and check for updates
	r.userManagerMtx.Lock()
	defer r.userManagerMtx.Unlock()

	// Map the files to disk and return the file names to be passed to the users manager if they
	// have been updated
	update, files, err := r.mapper.MapRules(user, groups.Formatted())
	if err != nil {
		level.Error(r.logger).Log("msg", "unable to map rule files", "user", user, "err", err)
		return
	}

	if update {
		level.Debug(r.logger).Log("msg", "updating rules", "user", "user")
		configUpdatesTotal.WithLabelValues(user).Inc()
		manager, exists := r.userManagers[user]
		if !exists {
			manager, err = r.newManager(ctx, user)
			if err != nil {
				level.Error(r.logger).Log("msg", "unable to create rule manager", "user", user, "err", err)
				return
			}
			manager.Run()
			r.userManagers[user] = manager
		}
		err = manager.Update(r.cfg.EvaluationInterval, files, nil)
		if err != nil {
			level.Error(r.logger).Log("msg", "unable to update rule manager", "user", user, "err", err)
			return
		}
	}
}

// newManager creates a prometheus rule manager wrapped with a user id
// configured storage, appendable, notifier, and instrumentation
func (r *Ruler) newManager(ctx context.Context, userID string) (*promRules.Manager, error) {
	tsdb := &tsdb{
		pusher:    r.pusher,
		userID:    userID,
		queryable: r.queryable,
	}

	notifier, err := r.getOrCreateNotifier(userID)
	if err != nil {
		return nil, err
	}

	// Wrap registerer with userID and cortex_ prefix
	reg := prometheus.WrapRegistererWith(prometheus.Labels{"user": userID}, r.registry)
	reg = prometheus.WrapRegistererWithPrefix("cortex_", reg)
	logger := log.With(r.logger, "user", userID)
	opts := &promRules.ManagerOptions{
		Appendable:  tsdb,
		TSDB:        tsdb,
		QueryFunc:   promRules.EngineQueryFunc(r.engine, r.queryable),
		Context:     user.InjectOrgID(ctx, userID),
		ExternalURL: r.alertURL,
		NotifyFunc:  sendAlerts(notifier, r.alertURL.String()),
		Logger:      logger,
		Registerer:  reg,
	}
	return promRules.NewManager(opts), nil
}

// GetRules retrieves the running rules from this ruler and all running rulers in the ring if
// sharding is enabled
func (r *Ruler) GetRules(ctx context.Context, userID string) ([]*rules.RuleGroupDesc, error) {
	if r.cfg.EnableSharding {
		return r.getShardedRules(ctx, userID)
	}

	return r.getLocalRules(userID)
}

func (r *Ruler) getLocalRules(userID string) ([]*rules.RuleGroupDesc, error) {
	var groups []*promRules.Group
	r.userManagerMtx.Lock()
	if mngr, exists := r.userManagers[userID]; exists {
		groups = mngr.RuleGroups()
	}
	r.userManagerMtx.Unlock()

	groupDescs := make([]*rules.RuleGroupDesc, 0, len(groups))
	prefix := filepath.Join(r.cfg.RulePath, userID) + "/"

	for _, group := range groups {
		interval := group.Interval()
		groupDesc := &rules.RuleGroupDesc{
			Name:                group.Name(),
			Namespace:           strings.TrimPrefix(group.File(), prefix),
			Interval:            interval,
			User:                userID,
			EvaluationTimestamp: group.GetEvaluationTimestamp(),
			EvaluationDuration:  group.GetEvaluationDuration(),
		}
		for _, r := range group.Rules() {
			lastError := ""
			if r.LastError() != nil {
				lastError = r.LastError().Error()
			}

			var ruleDesc *rules.RuleDesc
			switch rule := r.(type) {
			case *promRules.AlertingRule:
				rule.ActiveAlerts()
				alerts := []*rules.AlertDesc{}
				for _, a := range rule.ActiveAlerts() {
					alerts = append(alerts, &rules.AlertDesc{
						State:       a.State.String(),
						Labels:      client.FromLabelsToLabelAdapters(a.Labels),
						Annotations: client.FromLabelsToLabelAdapters(a.Annotations),
						Value:       a.Value,
						ActiveAt:    a.ActiveAt,
						FiredAt:     a.FiredAt,
						ResolvedAt:  a.ResolvedAt,
						LastSentAt:  a.LastSentAt,
						ValidUntil:  a.ValidUntil,
					})
				}
				ruleDesc = &rules.RuleDesc{
					Expr:                rule.Query().String(),
					Alert:               rule.Name(),
					For:                 rule.Duration(),
					Labels:              client.FromLabelsToLabelAdapters(rule.Labels()),
					Annotations:         client.FromLabelsToLabelAdapters(rule.Annotations()),
					State:               rule.State().String(),
					Health:              string(rule.Health()),
					LastError:           lastError,
					Alerts:              alerts,
					EvaluationTimestamp: rule.GetEvaluationTimestamp(),
					EvaluationDuration:  rule.GetEvaluationDuration(),
				}
			case *promRules.RecordingRule:
				ruleDesc = &rules.RuleDesc{
					Record:              rule.Name(),
					Expr:                rule.Query().String(),
					Labels:              client.FromLabelsToLabelAdapters(rule.Labels()),
					Health:              string(rule.Health()),
					LastError:           lastError,
					EvaluationTimestamp: rule.GetEvaluationTimestamp(),
					EvaluationDuration:  rule.GetEvaluationDuration(),
				}
			default:
				return nil, errors.Errorf("failed to assert type of rule '%v'", rule.Name())
			}
			groupDesc.Rules = append(groupDesc.Rules, ruleDesc)
		}
		groupDescs = append(groupDescs, groupDesc)
	}
	return groupDescs, nil
}

func (r *Ruler) getShardedRules(ctx context.Context, userID string) ([]*rules.RuleGroupDesc, error) {
	rulers, err := r.ring.GetAll()
	if err != nil {
		return nil, err
	}

	ctx, err = user.InjectIntoGRPCRequest(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to inject user ID into grpc request, %v", err)
	}

	// len(rgs) can't be larger than len(rulers.Ingesters)
	// alloc it in advance to avoid realloc
	rgs := make([]*rules.RuleGroupDesc, 0, len(rulers.Ingesters))

	for _, rlr := range rulers.Ingesters {
		conn, err := grpc.Dial(rlr.Addr, grpc.WithInsecure())
		if err != nil {
			return nil, err
		}
		cc := NewRulerClient(conn)
		newGrps, err := cc.Rules(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("unable to retrieve rules from other rulers, %v", err)
		}
		rgs = append(rgs, newGrps.Groups...)
	}

	return rgs, nil
}

// Rules implements the rules service
func (r *Ruler) Rules(ctx context.Context, in *RulesRequest) (*RulesResponse, error) {
	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, fmt.Errorf("no user id found in context")
	}

	groupDescs, err := r.getLocalRules(userID)
	if err != nil {
		return nil, err
	}

	return &RulesResponse{Groups: groupDescs}, nil
}
