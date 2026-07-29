package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/aws/eks-node-monitoring-agent/pkg/hostmetrics"
	"github.com/prometheus/client_golang/prometheus"
	promcollectors "github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	"github.com/prometheus/node_exporter/collector"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultAddress is the conventional node_exporter listen address. Matching it
// means existing Prometheus scrape configs and ServiceMonitors keep working
// without change when the agent replaces a node_exporter deployment.
const DefaultAddress = ":9100"

// DefaultMetricsPath is the conventional node_exporter metrics path.
const DefaultMetricsPath = "/metrics"

// defaultMaxRequests mirrors the upstream --web.max-requests default and bounds
// concurrent scrapes so a slow collector cannot pile up unbounded work.
const defaultMaxRequests = 40

// Options configures the metrics server.
type Options struct {
	// Address is the listen address, for example ":9100".
	Address string
	// MetricsPath is the path served, for example "/metrics".
	MetricsPath string
	// Collectors optionally restricts which upstream collectors run. Empty means
	// every collector enabled by the resolved flag state, matching an
	// unconfigured upstream node_exporter.
	Collectors []string
	// IncludeExporterMetrics adds go_* and process_* metrics for the agent
	// itself, mirroring the upstream --web.disable-exporter-metrics inverse.
	IncludeExporterMetrics bool
	// MaxRequests bounds concurrent scrapes. Zero uses defaultMaxRequests.
	MaxRequests int
	// UpstreamArgs are node_exporter-style flags such as "--no-collector.zfs".
	UpstreamArgs []string
	// HostRoot is the mount point of the host filesystem, typically "/host".
	HostRoot string
	// CollectorTimeout bounds a single collector's Update call. Zero selects the
	// default; negative disables the bound and restores upstream's unbounded
	// behaviour.
	CollectorTimeout time.Duration
}

// validateAddress rejects an address that can never be bound, so an operator
// config error fails at construction rather than silently disabling the endpoint.
//
// See the note on Start: a MALFORMED address is a config bug (fail loudly), while
// a port already IN USE is an environmental condition (degrade). Splitting them by
// when they are detected avoids matching on error strings at runtime, which would
// be brittle across Go versions and platforms.
//
// net.SplitHostPort catches missing/extra colons; the port must then be a decimal
// in 0-65535. Port 0 is allowed and means "any free port", which tests rely on.
// The HOST is deliberately NOT resolved -- that would make construction depend on
// DNS, and an unresolvable host is exactly the runtime condition Start handles.
func validateAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid metrics address %q: %w", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("invalid metrics address %q: port %q is not a number", addr, port)
	}
	if n < 0 || n > 65535 {
		return fmt.Errorf("invalid metrics address %q: port %d is out of range 0-65535", addr, n)
	}
	_ = host
	return nil
}

// withDefaults returns a copy of o with unset fields populated.
func (o Options) withDefaults() Options {
	if o.Address == "" {
		o.Address = DefaultAddress
	}
	if o.MetricsPath == "" {
		o.MetricsPath = DefaultMetricsPath
	}
	if o.MaxRequests == 0 {
		o.MaxRequests = defaultMaxRequests
	}
	return o
}

// Server serves node_exporter-compatible metrics over HTTP.
//
// It deliberately owns its own listener rather than reusing the controller
// runtime metrics server: that endpoint reports the agent's own condition
// metrics and must keep its existing contract, whereas this endpoint replaces a
// node_exporter deployment on the conventional port.
type Server struct {
	opts     Options
	registry *prometheus.Registry
	handler  http.Handler
	server   *http.Server
	// mu guards listener, which is written by Start and read concurrently by
	// Address. Without it the two race, which the race detector flags for any
	// caller polling Address() while the server is coming up.
	mu sync.RWMutex
	// listener is retained so callers can discover the bound port when Address
	// uses port 0.
	listener net.Listener
	// listen is the socket-opening function, overridable in tests to exercise
	// bind failures. Defaults to net.Listen.
	listen func(network, address string) (net.Listener, error)
	// serve runs the HTTP server, overridable in tests to exercise the error
	// path that a healthy net/http.Serve does not otherwise reach.
	serve func(*http.Server, net.Listener) error
	// shutdown gracefully stops the HTTP server, overridable in tests to
	// exercise the shutdown failure path.
	shutdown func(*http.Server, context.Context) error
}

// NewServer constructs a metrics server. It resolves the upstream collector
// flags, builds the collector set, and prepares the HTTP handler. The listener
// is not opened until Start.
func NewServer(logger *slog.Logger, opts Options) (*Server, error) {
	return newServer(logger, opts, ResolveUpstreamFlags, NewCollector, registerCollectors)
}

// resolveFunc and collectorFunc mirror ResolveUpstreamFlags and NewCollector.
// They are injected into newServer so the construction failure paths are
// reachable in tests: ResolveUpstreamFlags latches via sync.Once for the process
// lifetime, so its error branch cannot otherwise be exercised.
type resolveFunc func(args []string) error

type collectorFunc func(logger *slog.Logger, filters ...string) (*collector.NodeCollector, error)

type registerFunc func(prometheus.Registerer, *collector.NodeCollector, time.Duration, *slog.Logger) error

func newServer(logger *slog.Logger, opts Options, resolve resolveFunc, newCollector collectorFunc, register registerFunc) (*Server, error) {
	opts = opts.withDefaults()

	if err := validateAddress(opts.Address); err != nil {
		return nil, err
	}

	// Order matters: EKS defaults first, then host paths, then the operator's own
	// flags last so they take precedence (kingpin is last-wins).
	args := applyEKSDefaults(append(HostPathArgs(opts.HostRoot), opts.UpstreamArgs...))
	if err := resolve(args); err != nil {
		return nil, err
	}

	nc, err := newCollector(logger, opts.Collectors...)
	if err != nil {
		return nil, err
	}

	registry := prometheus.NewRegistry()
	if err := register(registry, nc, opts.CollectorTimeout, logger); err != nil {
		return nil, err
	}

	handler := newHandler(registry, opts, logger)

	return &Server{
		opts:     opts,
		registry: registry,
		handler:  handler,
		listen:   net.Listen,
		serve:    func(s *http.Server, l net.Listener) error { return s.Serve(l) },
		shutdown: func(s *http.Server, ctx context.Context) error { return s.Shutdown(ctx) },
	}, nil
}

// NewNativeServer builds a metrics server backed by pkg/hostmetrics, which has no
// dependency on prometheus/node_exporter.
//
// EVERYTHING BELOW THE REGISTRY IS SHARED with NewServer: the same Options, the same
// Server type, the same newHandler, the same listen/serve/shutdown seams. Only the
// registry's contents differ. That is deliberate and is what makes N5/N6 a controlled
// comparison -- if the two servers differed in their HTTP layer, concurrency limit or
// landing page, any measured difference could not be attributed to the collectors.
//
// The upstream flag resolution (ResolveUpstreamFlags, HostPathArgs, applyEKSDefaults)
// is NOT called here: it is kingpin plumbing that exists only to configure upstream's
// package-level flag state. The native implementation takes its paths as a struct
// field instead, which is the whole point of Paths in that package.
func NewNativeServer(logger *slog.Logger, opts Options) (*Server, error) {
	opts = opts.withDefaults()

	// Same construction-time address check as newServer. This was MISSED when F-K4-1's
	// fix was cherry-picked from the dependency branch -- the hunk did not apply here
	// because this function's body differs -- and a native-specific test caught it.
	// Without it a malformed address would silently disable the endpoint on the native
	// implementation while failing loudly on the other, so the same values.yaml typo
	// would behave differently depending on which implementation was selected.
	if err := validateAddress(opts.Address); err != nil {
		return nil, err
	}

	set, err := hostmetrics.New(logger, hostmetrics.Config{
		Paths:   hostmetrics.ForHostRoot(opts.HostRoot),
		Include: opts.Collectors,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build native collector set: %w", err)
	}

	registry := prometheus.NewRegistry()

	// node_exporter_build_info is part of the endpoint contract that dashboards and
	// alerts depend on, so the native variant registers it too. Omitting it would be a
	// parity difference that has nothing to do with collectors.
	if err := registry.Register(versioncollector.NewCollector("node_exporter")); err != nil {
		return nil, fmt.Errorf("failed to register version collector: %w", err)
	}

	if err := hostmetrics.NewPrometheusCollector(set, opts.CollectorTimeout, logger).
		Register(registry); err != nil {
		return nil, fmt.Errorf("failed to register native node collector: %w", err)
	}

	return &Server{
		opts:     opts,
		registry: registry,
		handler:  newHandler(registry, opts, logger),
		listen:   net.Listen,
		serve:    func(s *http.Server, l net.Listener) error { return s.Serve(l) },
		shutdown: func(s *http.Server, ctx context.Context) error { return s.Shutdown(ctx) },
	}, nil
}

// registerCollectors registers the endpoint's collectors onto reg.
//
// It is separate from NewServer so the failure paths are reachable in tests by
// passing a registry that already holds a conflicting collector.
func registerCollectors(reg prometheus.Registerer, nc *collector.NodeCollector, timeout time.Duration, logger *slog.Logger) error {
	// Panic and timeout counters are part of the resilience boundary; surfacing
	// them makes a contained collector fault visible rather than silent.
	if err := reg.Register(collectorPanicsTotal); err != nil {
		return fmt.Errorf("failed to register collector panic counter: %w", err)
	}
	if err := reg.Register(collectorTimeoutsTotal); err != nil {
		return fmt.Errorf("failed to register collector timeout counter: %w", err)
	}
	// node_exporter_build_info is part of the endpoint contract that dashboards
	// and alerts depend on, so it is registered even though the agent has its
	// own version metric elsewhere.
	if err := reg.Register(versioncollector.NewCollector("node_exporter")); err != nil {
		return fmt.Errorf("failed to register version collector: %w", err)
	}
	// The resilient wrapper replaces upstream's NodeCollector so a panicking or
	// hanging collector cannot take the agent down. See resilience.go.
	if err := reg.Register(newResilientCollector(nc, timeout, logger)); err != nil {
		return fmt.Errorf("failed to register node collector: %w", err)
	}
	return nil
}

// newHandler builds the HTTP handler tree: the metrics path, a landing page on
// unknown paths (matching upstream behaviour), and concurrency limiting.
//
// It takes only the registry and opts -- NOT a collector. That is what lets the native
// (no-upstream-dependency) implementation reuse this entire HTTP layer unchanged, so
// the three-way comparison isolates the collector difference rather than confounding it
// with two different servers. The collector parameter this used to take was never read.
//
// TWO THINGS HERE WERE FOUND BY COMPARING LIVE ENDPOINTS, not by any test.
//
//  1. ErrorLog WAS UNSET. `ErrorHandling: ContinueOnError` means a gather error
//     increments promhttp_metric_handler_errors_total{cause="gathering"} and serves a
//     partial scrape. With ErrorLog nil, client_golang counts it and says NOTHING.
//     Measured on the live cluster: 3182 gathering errors on one agent pod with zero
//     matching log lines, because there was nowhere for them to go. Upstream sets
//     ErrorLog on both paths (node_exporter.go:157,172); the port dropped it. A counter
//     that records a fault and a log that never mentions it is the worst of both --
//     the operator sees a number rise with no way to learn what broke.
//
//  2. InstrumentMetricHandler WAS NOT CALLED, so promhttp_metric_handler_requests_total
//     and _requests_in_flight were absent -- the last metric-name difference against
//     pne (Q9). It costs 2 series per node and is what a dashboard graphs to see
//     whether scrapes are succeeding at all. Note upstream registers these on the
//     EXPORTER registry, so they only appear when exporter metrics are enabled; that
//     conditional is reproduced rather than "improved", because moving them to the main
//     registry would make them appear in a configuration where pne has none.
//
// AND THE CAUSE OF (1), which the fix for (2) exposed: this function used to BUILD a
// handler with `Registry: registry` and then REASSIGN promHandler with
// `Registry: exporterRegistry` when exporter metrics were on. `HandlerOpts.Registry`
// registers promhttp_metric_handler_errors_total onto whatever it is given, so the
// discarded first handler had already put that counter in the MAIN registry -- and the
// replacement put it in the exporter registry too. Gathering
// `Gatherers{exporterRegistry, registry}` then collected the same family from both and
// failed with "was collected before with the same name and label values" on EVERY
// scrape: measured +1 per scrape on the live cluster, 3182 on a 13h-old pod.
//
// The structure below is upstream's if/else, where only ONE handler is ever built, so
// the counter is registered exactly once. Reproducing upstream's shape rather than
// restructuring it is the lesson: the assign-then-reassign version reads as equivalent
// and is not.
//
// SCOPE OF THAT BUG: it fired only with includeExporterMetrics: true, which is NOT the
// chart default (charts/.../values.yaml sets false). Default deployments were unaffected;
// our three-way test cluster enables it, which is why both agents showed it and pne --
// a separate program using the if/else -- did not.
func newHandler(registry *prometheus.Registry, opts Options, logger *slog.Logger) http.Handler {
	// errorLog routes client_golang's own errors into the agent's logger at ERROR.
	// slog.NewLogLogger is the same adapter upstream uses.
	errorLog := slog.NewLogLogger(logger.Handler(), slog.LevelError)

	var promHandler http.Handler
	if opts.IncludeExporterMetrics {
		exporterRegistry := prometheus.NewRegistry()
		exporterRegistry.MustRegister(
			promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{}),
			promcollectors.NewGoCollector(),
		)
		promHandler = promhttp.HandlerFor(
			prometheus.Gatherers{exporterRegistry, registry},
			promhttp.HandlerOpts{
				ErrorLog:            errorLog,
				ErrorHandling:       promhttp.ContinueOnError,
				MaxRequestsInFlight: opts.MaxRequests,
				// exporterRegistry, NOT registry: the error counter is a self-metric, and
				// putting it in the main registry as well is what caused the duplicate
				// collection above.
				Registry: exporterRegistry,
			},
		)
		// Registered against exporterRegistry, matching upstream: these describe the
		// scrape endpoint, and upstream keeps them with the other self-metrics.
		promHandler = promhttp.InstrumentMetricHandler(exporterRegistry, promHandler)
	} else {
		promHandler = promhttp.HandlerFor(
			registry,
			promhttp.HandlerOpts{
				ErrorLog:            errorLog,
				ErrorHandling:       promhttp.ContinueOnError,
				MaxRequestsInFlight: opts.MaxRequests,
				Registry:            registry,
			},
		)
	}

	mux := http.NewServeMux()
	mux.Handle(opts.MetricsPath, promHandler)

	// Upstream serves an HTML landing page on "/" rather than a bare 404, and
	// integration tests in the wild assert on it.
	//
	// Registering it is conditional because http.ServeMux panics on a duplicate
	// pattern: if the operator sets the metrics path to "/" the two registrations
	// collide and the agent dies at startup. Serving metrics takes precedence,
	// since that is what the operator explicitly asked for.
	if opts.MetricsPath != "/" {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, landingPage, opts.MetricsPath, version.Version)
		})
	}
	return mux
}

const landingPage = `<html>
<head><title>Node Exporter</title></head>
<body>
<h1>Node Exporter</h1>
<p><a href="%s">Metrics</a></p>
<p>Served by the EKS Node Monitoring Agent (node_exporter %s compatible).</p>
</body>
</html>
`

// Registry exposes the Prometheus registry, primarily for tests.
func (s *Server) Registry() *prometheus.Registry { return s.registry }

// Handler exposes the HTTP handler, primarily for tests.
func (s *Server) Handler() http.Handler { return s.handler }

// Address returns the address the server is bound to. Once Start has run with a
// port of 0 this reports the actual chosen port.
func (s *Server) Address() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.opts.Address
}

// Start runs the server until ctx is cancelled, then shuts it down gracefully.
//
// It satisfies controller-runtime's Runnable interface so the agent's manager
// owns its lifecycle alongside the monitors.
//
// A BIND FAILURE MUST NOT KILL THE AGENT. See FINDING F-K4-1 in
// JOURNAL-KARPENTER.md: a port conflict here used to propagate out of Start, and
// because controller-runtime treats a Runnable error as fatal — and
// `utilruntime.Must(run())` in main.go turns it into a panic — the WHOLE PROCESS
// died. That process also publishes the NodeConditions that EKS node auto repair
// and Karpenter act on, so a metrics-port conflict took down node health
// reporting. Measured on a Karpenter node: 5/5 pods in CrashLoopBackOff with
// `panic: failed to listen on :9102: bind: address already in use`.
//
// That is the exact inverse of what the resilience boundary exists for. That
// boundary stops a *collector* defect from killing condition reporting, and it
// works — but a *startup* bind failure bypassed it entirely.
//
// So the endpoint now degrades instead: the failure is logged at ERROR, the
// listener is not retried, and Start returns nil so the manager keeps the
// monitors running. Losing node metrics is bad; losing NodeConditions is worse,
// because it makes a node invisible to repair.
//
// TWO KINDS OF FAILURE, DELIBERATELY HANDLED DIFFERENTLY — this distinction is the
// reason validateAddress exists in NewServer/NewNativeServer:
//
//	MALFORMED ADDRESS ("not-an-address", port 99999) is an OPERATOR CONFIG ERROR.
//	It can never succeed, no environment will fix it, and silently disabling the
//	endpoint would mean a typo in values.yaml produces an agent that looks healthy
//	and serves nothing. That still fails loudly, at CONSTRUCTION.
//
//	PORT ALREADY IN USE is an ENVIRONMENTAL CONDITION. The config is valid, another
//	process got there first, and the right response is to keep reporting node
//	health. That degrades, here.
//
// Collapsing the two would trade one bad outcome for another, so they are split by
// *when* they are detected rather than by inspecting error strings at runtime.
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx)

	listener, err := s.listen("tcp", s.opts.Address)
	if err != nil {
		// Deliberately nil, not an error. The agent's primary job is node health
		// reporting, and it must survive an unusable metrics port.
		logger.Error(err, "failed to listen for node_exporter compatible metrics; "+
			"the metrics endpoint is DISABLED for the lifetime of this process, but node "+
			"condition reporting continues",
			"address", s.opts.Address,
			"hint", "another process already holds this port -- a node_exporter DaemonSet is "+
				"the usual cause; set nodeAgent.metrics.port to a free port or remove the "+
				"conflicting deployment",
		)
		return nil
	}
	srv := &http.Server{
		Handler: s.handler,
		// ReadHeaderTimeout bounds slow-header clients; the metrics endpoint is
		// reachable on the host network so it must not be trivially tied up.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// listener and server are read by the serve goroutine and by Address, so both
	// are published under the lock. srv is also kept in a local so this function
	// and its goroutine never read the field concurrently with a second Start.
	s.mu.Lock()
	s.listener = listener
	s.server = srv
	s.mu.Unlock()

	logger.Info("serving node_exporter compatible metrics",
		"address", listener.Addr().String(),
		"path", s.opts.MetricsPath,
	)

	errCh := make(chan error, 1)
	go func() {
		if err := s.serve(srv, listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server failed: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.shutdown(srv, shutdownCtx); err != nil {
			return fmt.Errorf("failed to shut down metrics server: %w", err)
		}
		return nil
	}
}

// NeedLeaderElection reports that this runnable must run on every node, not just
// a single elected leader: node metrics are inherently per-node.
func (s *Server) NeedLeaderElection() bool { return false }
