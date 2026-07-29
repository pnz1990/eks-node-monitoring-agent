package metrics_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/eks-node-monitoring-agent/pkg/metrics"
)

// newTestServer builds a server bound to an ephemeral port with a small, fast
// collector set so tests stay quick and hermetic.
func newTestServer(t *testing.T, opts metrics.Options) *metrics.Server {
	t.Helper()
	if opts.Address == "" {
		opts.Address = "127.0.0.1:0"
	}
	if opts.Collectors == nil {
		opts.Collectors = []string{"loadavg"}
	}
	srv, err := metrics.NewServer(testLogger(), opts)
	require.NoError(t, err)
	return srv
}

func TestNewServerDefaults(t *testing.T) {
	srv, err := metrics.NewServer(testLogger(), metrics.Options{Collectors: []string{"loadavg"}})
	require.NoError(t, err)

	// Defaults must match upstream node_exporter conventions so existing scrape
	// configuration keeps working.
	assert.Equal(t, metrics.DefaultAddress, srv.Address())
	assert.Equal(t, ":9100", metrics.DefaultAddress)
	assert.Equal(t, "/metrics", metrics.DefaultMetricsPath)
	assert.NotNil(t, srv.Registry())
	assert.NotNil(t, srv.Handler())
}

func TestNewServerInvalidCollector(t *testing.T) {
	_, err := metrics.NewServer(testLogger(), metrics.Options{Collectors: []string{"nope-not-real"}})
	require.Error(t, err)
}

func TestServeMetrics(t *testing.T) {
	srv := newTestServer(t, metrics.Options{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// node_load1 comes from the loadavg collector; node_scrape_collector_success
	// and node_exporter_build_info are part of the endpoint contract (S4) that
	// alerts depend on.
	assert.Contains(t, body, "node_load1")
	assert.Contains(t, body, "node_scrape_collector_success")
	assert.Contains(t, body, "node_scrape_collector_duration_seconds")
	assert.Contains(t, body, "node_exporter_build_info")
}

func TestServeMetricsCustomPath(t *testing.T) {
	srv := newTestServer(t, metrics.Options{MetricsPath: "/custom"})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/custom", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "node_load1")
}

func TestLandingPage(t *testing.T) {
	srv := newTestServer(t, metrics.Options{})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	// Upstream serves an HTML landing page on / rather than a 404.
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, rec.Body.String(), "Node Exporter")
	assert.Contains(t, rec.Body.String(), "/metrics")
}

func TestUnknownPathReturns404(t *testing.T) {
	srv := newTestServer(t, metrics.Options{})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestIncludeExporterMetrics(t *testing.T) {
	srv := newTestServer(t, metrics.Options{IncludeExporterMetrics: true})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "go_goroutines")
	assert.Contains(t, body, "node_load1", "host metrics must still be present")
}

func TestExporterMetricsExcludedByDefault(t *testing.T) {
	srv := newTestServer(t, metrics.Options{})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "go_goroutines")
}

func TestStartAndShutdown(t *testing.T) {
	srv := newTestServer(t, metrics.Options{})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// Poll until the listener is up rather than sleeping a fixed duration.
	var addr string
	require.Eventually(t, func() bool {
		addr = srv.Address()
		if addr == "" || strings.HasSuffix(addr, ":0") {
			return false
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}, 10*time.Second, 50*time.Millisecond, "server should begin listening")

	resp, err := http.Get(fmt.Sprintf("http://%s/metrics", addr))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "node_load1")

	cancel()
	select {
	case err := <-errCh:
		assert.NoError(t, err, "graceful shutdown must not error")
	case <-time.After(15 * time.Second):
		t.Fatal("server did not shut down after context cancellation")
	}
}

func TestStartOnAnOccupiedPortDegradesRatherThanFailing(t *testing.T) {
	// The real-world case behind FINDING F-K4-1: another process already holds the
	// configured port. On a customer node that is a node_exporter DaemonSet, and it
	// used to panic the agent -- killing NodeCondition reporting, which is the one
	// thing that must keep working.
	//
	// This occupies a port for real rather than injecting an error, so it exercises
	// net.Listen's actual EADDRINUSE path.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	srv := newTestServer(t, metrics.Options{Address: ln.Addr().String()})

	require.NoError(t, srv.Start(context.Background()),
		"a port conflict must degrade the metrics endpoint, not end the process that "+
			"reports node health to Karpenter")
}

func TestStartSucceedsOnAFreePortAfterTheOccupiedCase(t *testing.T) {
	// The negative control for the test above. If Start returned nil unconditionally
	// -- say a future refactor swallowed every error -- the occupied-port test would
	// still pass and prove nothing. This asserts the healthy path still genuinely
	// binds and serves, so "nil" means "degraded deliberately" rather than "nil
	// always".
	srv := newTestServer(t, metrics.Options{Address: "127.0.0.1:0"})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// Address() reports the bound port only once the listener exists, so polling it
	// proves a real bind happened.
	var addr string
	for i := 0; i < 100; i++ {
		addr = srv.Address()
		if addr != "127.0.0.1:0" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.NotEqual(t, "127.0.0.1:0", addr, "the server must actually bind a real port on the happy path")

	cancel()
	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("server did not shut down after context cancellation")
	}
}

func TestNeedLeaderElection(t *testing.T) {
	srv := newTestServer(t, metrics.Options{})
	// Node metrics are per-node, so this must never be gated on leader election.
	assert.False(t, srv.NeedLeaderElection())
}

func TestAddressBeforeStart(t *testing.T) {
	srv := newTestServer(t, metrics.Options{Address: "127.0.0.1:19999"})
	assert.Equal(t, "127.0.0.1:19999", srv.Address())
}

func TestHostRootWiring(t *testing.T) {
	// A non-default host root must be accepted and produce a working server; the
	// upstream flags were already latched by an earlier test, so this asserts the
	// option is plumbed without error rather than re-parsing flags.
	srv := newTestServer(t, metrics.Options{HostRoot: "/host"})
	assert.NotNil(t, srv.Handler())
}

func TestMaxRequestsDefault(t *testing.T) {
	srv := newTestServer(t, metrics.Options{MaxRequests: 0})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestConcurrentScrapes(t *testing.T) {
	srv := newTestServer(t, metrics.Options{MaxRequests: 2})

	const n = 8
	done := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			done <- rec.Code
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case code := <-done:
			// Upstream returns 503 when the in-flight limit is exceeded; both
			// outcomes are acceptable, a panic or hang is not.
			assert.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, code)
		case <-time.After(30 * time.Second):
			t.Fatal("concurrent scrape did not complete")
		}
	}
}
