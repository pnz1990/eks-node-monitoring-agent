package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

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

	handler := newHandler(registry, nc, opts, logger)

	return &Server{
		opts:     opts,
		registry: registry,
		handler:  handler,
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
func newHandler(registry *prometheus.Registry, nc *collector.NodeCollector, opts Options, logger *slog.Logger) http.Handler {
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

	// FINDING F-K4-5: binding successfully is NOT the same as owning the port.
	//
	// Measured on a Karpenter node: the agent bound `[::]:9100` while the
	// prometheus-node-exporter addon held `[HOST_IP]:9100`. BOTH BINDS SUCCEEDED --
	// Linux permits a wildcard bind alongside an existing specific-address bind --
	// but the kernel routes inbound traffic to the MORE SPECIFIC socket. So pne
	// answered every scrape and the agent's endpoint was unreachable, while the agent
	// logged "serving node_exporter compatible metrics" and its DaemonSet reported
	// Ready.
	//
	// That is worse than the crash F-K4-1 fixed, because nothing reports it: the
	// scrape returns 200 with entirely plausible node metrics, just from the wrong
	// process. A customer migrating off the pne addon -- the exact scenario this
	// endpoint exists to enable -- would see success and be reading pne the whole
	// time.
	//
	// It cannot be prevented from inside the process: the bind is legal and returns no
	// error. So it is DETECTED instead, by scraping our own endpoint and looking for a
	// metric family only this agent emits. Reported at ERROR rather than fatal --
	// serving nothing is bad, but killing NodeCondition reporting over it would repeat
	// F-K4-1's mistake.
	go s.verifyOwnEndpoint(ctx, listener.Addr().String())

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

// ownEndpointMarker identifies a scrape as coming from THIS agent rather than from a
// co-resident node_exporter that shadowed the port.
//
// CHOOSING THIS WAS NOT OBVIOUS, and the first two candidates were both wrong:
//
//	node_collector_panics_total -- the resilience counters ARE unique to this agent,
//	  but they are CounterVecs with no series until a panic occurs, so Prometheus
//	  omits the family entirely. On a healthy agent the marker would be ABSENT, and
//	  the check would report shadowing on every healthy node -- far worse than the bug
//	  it detects. Caught by the control test below, which is the only reason this is
//	  not shipping broken.
//
//	node_exporter_build_info -- always present, but node_exporter emits it too, so its
//	  presence proves nothing.
//
// What actually distinguishes us is that family's LABEL VALUES. Measured on the live
// cluster, same node, same scrape pass:
//
//	pne  : node_exporter_build_info{...,revision="6044da78...",version="1.12.1"}
//	agent: node_exporter_build_info{...,revision="unknown",version=""}
//
// The agent registers versioncollector.NewCollector("node_exporter") without the
// linker-injected version stamps upstream's release build sets, so `revision="unknown"`
// is a reliable and always-present signature. A release build that DID stamp them would
// make this ambiguous -- so the control test asserts the marker really appears in a real
// scrape, and will fail loudly if that ever changes.
const ownEndpointMarker = `revision="unknown"`

// verifyOwnEndpointDelay is how long to wait before self-checking. The listener is
// accepting by the time Start publishes it, but the serve goroutine and the
// registry's first gather are not instantaneous, and a check that races startup
// would report a false positive on a healthy agent.
var verifyOwnEndpointDelay = 5 * time.Second

// verifyOwnEndpoint scrapes our own metrics endpoint and warns if the response did
// not come from us. See FINDING F-K4-5 at the call site.
//
// FAILURE-MODE DISCIPLINE: this check must never itself be mistaken for a defect.
// A scrape that cannot be performed at all (dial refused, timeout, context
// cancelled) is reported as INDETERMINATE, not as shadowing -- an inconclusive check
// reporting a confident answer is the failure family that has bitten this project
// repeatedly.
func (s *Server) verifyOwnEndpoint(ctx context.Context, addr string) {
	logger := log.FromContext(ctx)

	select {
	case <-time.After(verifyOwnEndpointDelay):
	case <-ctx.Done():
		return
	}

	// A wildcard listen address is not dialable, so target loopback on the bound
	// port. Loopback is also the right probe: if a specific-address socket shadowed
	// us, it is bound to the host IP and will NOT answer on 127.0.0.1 -- meaning a
	// loopback probe that reaches US while external scrapes reach THEM would be
	// invisible. So the host-routable address is probed too, when one is known.
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		logger.V(1).Info("skipping endpoint self-check: unparsable bound address", "address", addr)
		return
	}

	probe := "127.0.0.1:" + port
	url := "http://" + probe + s.opts.MetricsPath

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		logger.V(1).Info("skipping endpoint self-check: could not build request", "url", url)
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// INDETERMINATE, deliberately not an error: the endpoint may simply not be
		// reachable from inside the pod's network namespace in every topology.
		logger.V(1).Info("endpoint self-check inconclusive; could not scrape own endpoint",
			"url", url, "err", err.Error())
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		logger.V(1).Info("endpoint self-check inconclusive; could not read response", "url", url)
		return
	}

	if strings.Contains(string(body), ownEndpointMarker) {
		logger.V(1).Info("endpoint self-check passed: this agent is serving its own metrics port",
			"address", addr)
		return
	}

	// A 200 response that is NOT ours means another process owns the port.
	logger.Error(nil, "ANOTHER PROCESS IS ANSWERING THIS AGENT'S METRICS PORT; the agent bound "+
		"the port successfully but scrapes are being served by something else, so this agent's "+
		"node metrics are NOT reachable. Node condition reporting is unaffected.",
		"address", addr,
		"status", resp.StatusCode,
		"cause", "a co-resident exporter bound a MORE SPECIFIC address on the same port (for "+
			"example prometheus-node-exporter listening on [HOST_IP]:9100 while this agent "+
			"listens on [::]:9100). Linux allows both binds and routes traffic to the specific one.",
		"remediation", "remove the conflicting exporter, or set nodeAgent.metrics.port to a "+
			"port nothing else uses",
	)
}

// NeedLeaderElection reports that this runnable must run on every node, not just
// a single elected leader: node metrics are inherently per-node.
func (s *Server) NeedLeaderElection() bool { return false }
