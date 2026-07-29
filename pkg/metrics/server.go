package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
func newHandler(registry *prometheus.Registry, opts Options, logger *slog.Logger) http.Handler {
	promHandler := promhttp.HandlerFor(
		registry,
		promhttp.HandlerOpts{
			ErrorHandling:       promhttp.ContinueOnError,
			MaxRequestsInFlight: opts.MaxRequests,
			Registry:            registry,
		},
	)

	if opts.IncludeExporterMetrics {
		exporterRegistry := prometheus.NewRegistry()
		exporterRegistry.MustRegister(
			promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{}),
			promcollectors.NewGoCollector(),
		)
		promHandler = promhttp.HandlerFor(
			prometheus.Gatherers{exporterRegistry, registry},
			promhttp.HandlerOpts{
				ErrorHandling:       promhttp.ContinueOnError,
				MaxRequestsInFlight: opts.MaxRequests,
				Registry:            exporterRegistry,
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
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx)

	listener, err := s.listen("tcp", s.opts.Address)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.opts.Address, err)
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
