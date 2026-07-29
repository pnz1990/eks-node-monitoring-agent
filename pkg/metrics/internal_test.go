package metrics

// These tests live in the metrics package (rather than metrics_test) so they can
// exercise unexported seams: the kingpin parse helper, collector registration
// failures, and the server's listen/serve error paths. Those paths are real
// failure modes for a privileged, host-networked listener, so they are tested
// rather than excluded from coverage.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/node_exporter/collector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestParseKingpinSuccess(t *testing.T) {
	app := kingpin.New("test", "test app")
	app.Flag("collector.example", "example").Default("true").Bool()

	require.NoError(t, parseKingpin(app, []string{"--no-collector.example"}))
}

func TestParseKingpinError(t *testing.T) {
	app := kingpin.New("test", "test app")

	err := parseKingpin(app, []string{"--not-a-real-flag"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse node_exporter collector flags")
}

func TestParseKingpinDoesNotExit(t *testing.T) {
	// A parse failure must return an error rather than terminating the process:
	// the metrics endpoint is optional and must never take the agent down.
	app := kingpin.New("test", "test app")
	err := parseKingpin(app, []string{"--bogus"})
	assert.Error(t, err, "parse failure must be returned, not os.Exit")
}

func TestRegisterCollectorsVersionConflict(t *testing.T) {
	require.NoError(t, ResolveUpstreamFlags(nil))
	nc, err := NewCollector(quietLogger(), "loadavg")
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	// Pre-register the same version collector to force the first failure branch.
	require.NoError(t, registerCollectors(reg, nc, time.Second, quietLogger()))

	err = registerCollectors(reg, nc, time.Second, quietLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate metrics collector registration")
}

func TestRegisterCollectorsNodeCollectorConflict(t *testing.T) {
	require.NoError(t, ResolveUpstreamFlags(nil))
	nc, err := NewCollector(quietLogger(), "loadavg")
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	// Register the node collector alone, so the version collector succeeds on the
	// next call but the node collector conflicts.
	require.NoError(t, reg.Register(nc))

	err = registerCollectors(reg, nc, time.Second, quietLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate metrics collector registration")
}

func TestStartServeError(t *testing.T) {
	srv, err := NewServer(quietLogger(), Options{Address: "127.0.0.1:0", Collectors: []string{"loadavg"}})
	require.NoError(t, err)

	sentinel := errors.New("boom")
	srv.serve = func(*http.Server, net.Listener) error { return sentinel }

	err = srv.Start(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "metrics server failed")
}

func TestStartServeClosedIsNotAnError(t *testing.T) {
	srv, err := NewServer(quietLogger(), Options{Address: "127.0.0.1:0", Collectors: []string{"loadavg"}})
	require.NoError(t, err)

	// http.ErrServerClosed is the normal shutdown signal and must not surface.
	srv.serve = func(*http.Server, net.Listener) error { return http.ErrServerClosed }

	assert.NoError(t, srv.Start(context.Background()))
}

func TestStartListenInjectedErrorDoesNotKillTheAgent(t *testing.T) {
	// CONTRACT CHANGED BY FINDING F-K4-1, and this test changed with it.
	//
	// Start used to RETURN the bind error. controller-runtime treats a Runnable
	// error as fatal and main.go wraps run() in utilruntime.Must, so that error
	// became a panic and killed the process -- the same process that publishes the
	// NodeConditions EKS node auto repair and Karpenter act on. Measured on a
	// Karpenter node: 5/5 CrashLoopBackOff with
	// "panic: failed to listen on :9102: bind: address already in use".
	//
	// So a bind failure must now DEGRADE the endpoint, not end the agent. Asserting
	// nil here is asserting that node health reporting survives an unusable metrics
	// port.
	srv, err := NewServer(quietLogger(), Options{Address: "127.0.0.1:0", Collectors: []string{"loadavg"}})
	require.NoError(t, err)

	sentinel := errors.New("cannot bind")
	srv.listen = func(string, string) (net.Listener, error) { return nil, sentinel }

	require.NoError(t, srv.Start(context.Background()),
		"a bind failure must not propagate: it would panic the agent and take node "+
			"condition reporting down with it")

	// And the endpoint must genuinely be absent rather than half-initialised, so a
	// caller cannot mistake a failed bind for a working listener.
	srv.mu.RLock()
	ln := srv.listener
	srv.mu.RUnlock()
	assert.Nil(t, ln, "no listener should be published when the bind failed")
}

func TestStartShutdownError(t *testing.T) {
	srv, err := NewServer(quietLogger(), Options{Address: "127.0.0.1:0", Collectors: []string{"loadavg"}})
	require.NoError(t, err)

	// Block in serve so Start reaches the ctx.Done() branch and shuts down.
	release := make(chan struct{})
	srv.serve = func(_ *http.Server, l net.Listener) error {
		<-release
		return http.ErrServerClosed
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// Give Start time to reach its select, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
	close(release)
}

func TestNewServerResolveError(t *testing.T) {
	sentinel := errors.New("flag resolution failed")
	_, err := newServer(quietLogger(), Options{},
		func([]string) error { return sentinel },
		NewCollector,
		registerCollectors,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

func TestNewServerCollectorError(t *testing.T) {
	sentinel := errors.New("collector construction failed")
	_, err := newServer(quietLogger(), Options{},
		func([]string) error { return nil },
		func(*slog.Logger, ...string) (*collector.NodeCollector, error) { return nil, sentinel },
		registerCollectors,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

// failOnNthRegisterer fails the Nth Register call, which makes each sequential
// registration branch in registerCollectors independently reachable. The real
// counters are package-level singletons, so a shared registry cannot exercise
// the later branches: the first one always conflicts.
type failOnNthRegisterer struct {
	n    int
	seen int
	err  error
}

func (f *failOnNthRegisterer) Register(prometheus.Collector) error {
	f.seen++
	if f.seen == f.n {
		return f.err
	}
	return nil
}
func (f *failOnNthRegisterer) MustRegister(...prometheus.Collector) {}
func (f *failOnNthRegisterer) Unregister(prometheus.Collector) bool { return true }

func TestRegisterCollectorsFailureBranches(t *testing.T) {
	require.NoError(t, ResolveUpstreamFlags(nil))
	nc, err := NewCollector(quietLogger(), "loadavg")
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		nth  int
		want string
	}{
		{"panic counter", 1, "failed to register collector panic counter"},
		{"timeout counter", 2, "failed to register collector timeout counter"},
		{"version collector", 3, "failed to register version collector"},
		{"node collector", 4, "failed to register node collector"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sentinel := errors.New("registration refused")
			reg := &failOnNthRegisterer{n: tc.nth, err: sentinel}

			err := registerCollectors(reg, nc, time.Second, quietLogger())
			require.Error(t, err)
			assert.ErrorIs(t, err, sentinel)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestNewServerRegisterError(t *testing.T) {
	sentinel := errors.New("registration failed")
	_, err := newServer(quietLogger(), Options{},
		func([]string) error { return nil },
		func(*slog.Logger, ...string) (*collector.NodeCollector, error) {
			return &collector.NodeCollector{Collectors: nil}, nil
		},
		func(prometheus.Registerer, *collector.NodeCollector, time.Duration, *slog.Logger) error {
			return sentinel
		},
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

func TestStartShutdownFailure(t *testing.T) {
	srv, err := NewServer(quietLogger(), Options{Address: "127.0.0.1:0", Collectors: []string{"loadavg"}})
	require.NoError(t, err)

	// Hold serve open so Start reaches the ctx.Done() branch, then make the
	// graceful shutdown fail so the error is surfaced to the manager.
	blocked := make(chan struct{})
	srv.serve = func(*http.Server, net.Listener) error {
		<-blocked
		return http.ErrServerClosed
	}
	sentinel := errors.New("shutdown refused")
	srv.shutdown = func(*http.Server, context.Context) error { return sentinel }

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.ErrorIs(t, err, sentinel)
		assert.Contains(t, err.Error(), "failed to shut down metrics server")
	case <-time.After(15 * time.Second):
		t.Fatal("Start did not return")
	}
	close(blocked)
}

func TestWithDefaultsPreservesExplicitValues(t *testing.T) {
	opts := Options{Address: "1.2.3.4:1234", MetricsPath: "/m", MaxRequests: 7}.withDefaults()
	assert.Equal(t, "1.2.3.4:1234", opts.Address)
	assert.Equal(t, "/m", opts.MetricsPath)
	assert.Equal(t, 7, opts.MaxRequests)
}

func TestWithDefaultsFillsUnset(t *testing.T) {
	opts := Options{}.withDefaults()
	assert.Equal(t, DefaultAddress, opts.Address)
	assert.Equal(t, DefaultMetricsPath, opts.MetricsPath)
	assert.Equal(t, defaultMaxRequests, opts.MaxRequests)
}
