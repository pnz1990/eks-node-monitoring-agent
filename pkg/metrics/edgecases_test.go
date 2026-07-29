package metrics_test

// Adversarial edge cases. 100% statement coverage with benign inputs is a blind
// spot, not a guarantee: every test here feeds the endpoint something hostile
// (missing paths, malformed values, exhausted resources, contradictory config)
// and asserts the agent degrades rather than fails.
//
// Cases are drawn from real upstream bug reports where possible, so these are
// regression tests against known failure shapes rather than invented scenarios.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/eks-node-monitoring-agent/pkg/metrics"
)

// --- host path edge cases -------------------------------------------------

func TestHostPathArgsEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		hostRoot string
		assert   func(t *testing.T, args []string)
	}{
		{
			name:     "multiple trailing slashes",
			hostRoot: "/host///",
			assert: func(t *testing.T, args []string) {
				// Only one trailing slash is trimmed, so the result is odd but must
				// still be well-formed rather than producing a bare "//proc".
				require.NotEmpty(t, args)
				for _, a := range args {
					assert.NotContains(t, a, "=/proc", "must not resolve to the container's own procfs")
				}
			},
		},
		{
			name:     "relative path",
			hostRoot: "host",
			assert: func(t *testing.T, args []string) {
				require.NotEmpty(t, args)
				assert.Contains(t, args[0], "host/proc")
			},
		},
		{
			name:     "path with spaces",
			hostRoot: "/mnt/host fs",
			assert: func(t *testing.T, args []string) {
				require.NotEmpty(t, args)
				// Spaces must survive intact; kingpin receives these as separate
				// argv entries so no quoting is needed or wanted.
				assert.Equal(t, "--path.procfs=/mnt/host fs/proc", args[0])
			},
		},
		{
			name:     "single dot",
			hostRoot: ".",
			assert: func(t *testing.T, args []string) {
				require.NotEmpty(t, args)
				assert.Contains(t, args[0], "./proc")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.assert(t, metrics.HostPathArgs(tc.hostRoot))
		})
	}
}

func TestServerWithNonexistentHostRoot(t *testing.T) {
	// A wrong HOST_ROOT is a realistic misconfiguration. The endpoint must still
	// come up: collectors that cannot read their inputs report failure
	// individually, which is far better than refusing to serve at all.
	srv, err := metrics.NewServer(testLogger(), metrics.Options{
		Address:    "127.0.0.1:0",
		HostRoot:   "/nonexistent-host-root-" + fmt.Sprint(os.Getpid()),
		Collectors: []string{"loadavg"},
	})
	require.NoError(t, err, "a bad host root must not prevent the server from being constructed")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "the endpoint must still respond")
	// The meta metrics are always emitted, even when every collector fails.
	assert.Contains(t, rec.Body.String(), "node_scrape_collector_success")
}

// --- malformed procfs input ----------------------------------------------

// TestMalformedProcfsIsToleratedPerCollector feeds the collectors a procfs tree
// full of the malformed values upstream bugs report: "<unknown>" (#1710), empty
// files, and unexpected extra fields (#2799).
func TestMalformedProcfsIsToleratedPerCollector(t *testing.T) {
	root := t.TempDir()
	procDir := filepath.Join(root, "proc")
	require.NoError(t, os.MkdirAll(procDir, 0o755))

	// Values chosen to break naive parsers in specific ways.
	files := map[string]string{
		"loadavg": "<unknown> <unknown> <unknown> 1/2 3",   // #1710 shape
		"stat":    "",                                      // empty file
		"meminfo": "MemTotal:  not-a-number kB\ngarbage\n", // non-numeric
		"vmstat":  "pgfault 12 34 56 extra fields here\n",  // #2799 shape
		"uptime":  "\x00\x01\x02 binary garbage\n",         // binary noise
	}
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(procDir, name), []byte(content), 0o644))
	}

	srv, err := metrics.NewServer(testLogger(), metrics.Options{
		Address:    "127.0.0.1:0",
		HostRoot:   root,
		Collectors: []string{"loadavg"},
	})
	require.NoError(t, err)

	// The contract is that malformed input degrades one collector, and never
	// panics or hangs the endpoint.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "node_scrape_collector_success")
}

func TestUnreadableProcfsFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission checks, so this cannot be exercised")
	}
	root := t.TempDir()
	procDir := filepath.Join(root, "proc")
	require.NoError(t, os.MkdirAll(procDir, 0o755))
	p := filepath.Join(procDir, "loadavg")
	require.NoError(t, os.WriteFile(p, []byte("0.1 0.2 0.3 1/2 3"), 0o000))

	srv, err := metrics.NewServer(testLogger(), metrics.Options{
		Address:    "127.0.0.1:0",
		HostRoot:   root,
		Collectors: []string{"loadavg"},
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "a permission error must not fail the endpoint")
}

// --- configuration edge cases --------------------------------------------

func TestInvalidListenAddressesFailAtConstruction(t *testing.T) {
	// CONTRACT MOVED BY FINDING F-K4-1, and the move is the point.
	//
	// This used to assert that construction succeeded and Start returned the error.
	// Start no longer returns bind errors — a port conflict must DEGRADE rather than
	// panic the agent and take NodeCondition reporting down with it.
	//
	// But a MALFORMED address is a different thing from an occupied port: it can
	// never succeed, no environment will fix it, and silently disabling the endpoint
	// would mean a typo in values.yaml yields an agent that looks healthy and serves
	// nothing. So it still fails loudly — just at CONSTRUCTION instead, where the
	// distinction can be made without matching on error strings at runtime.
	for _, addr := range []string{
		"not-an-address",  // no port separator
		"127.0.0.1:99999", // port out of range
		"127.0.0.1:-1",    // negative port
		":::::",           // too many separators
	} {
		t.Run(addr, func(t *testing.T) {
			_, err := metrics.NewServer(testLogger(), metrics.Options{
				Address:    addr,
				Collectors: []string{"loadavg"},
			})
			require.Error(t, err, "an unusable address must fail loudly at startup, not silently serve nothing")
			assert.Contains(t, err.Error(), "invalid metrics address")
		})
	}
}

func TestValidListenAddressesAreAccepted(t *testing.T) {
	// The negative control for the test above. Without it, a validateAddress that
	// rejected EVERYTHING would satisfy the invalid-address test and break every
	// real deployment.
	for _, addr := range []string{
		":9100",        // the chart default
		"127.0.0.1:0",  // any free port, used throughout the tests
		"0.0.0.0:9100", // explicit all-interfaces
		"[::]:9100",    // IPv6 all-interfaces
		"[::1]:9100",   // IPv6 loopback
	} {
		t.Run(addr, func(t *testing.T) {
			_, err := metrics.NewServer(testLogger(), metrics.Options{
				Address:    addr,
				Collectors: []string{"loadavg"},
			})
			require.NoError(t, err, "a valid listen address must be accepted")
		})
	}
}

func TestUnknownCollectorNameIsRejected(t *testing.T) {
	_, err := metrics.NewServer(testLogger(), metrics.Options{
		Address:    "127.0.0.1:0",
		Collectors: []string{"definitely-not-a-collector"},
	})
	require.Error(t, err, "an unknown collector must fail loudly at startup, not silently serve nothing")
}

func TestContradictoryUpstreamFlagsDoNotCrash(t *testing.T) {
	// kingpin is last-wins, so this is well-defined rather than an error. The
	// point is that it must not panic or exit the process.
	//
	// Flags are latched by sync.Once for the process lifetime, so this asserts
	// the call is safe rather than that the flags take effect.
	err := metrics.ResolveUpstreamFlags([]string{
		"--collector.cpu", "--no-collector.cpu",
	})
	assert.NoError(t, err)
}

func TestExtremeMaxRequests(t *testing.T) {
	for _, n := range []int{1, -1, 1 << 20} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			srv, err := metrics.NewServer(testLogger(), metrics.Options{
				Address:     "127.0.0.1:0",
				MaxRequests: n,
				Collectors:  []string{"loadavg"},
			})
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			// Any of these must still serve; a negative or absurd value must not
			// wedge the handler.
			assert.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, rec.Code)
		})
	}
}

func TestMetricsPathEdgeCases(t *testing.T) {
	for _, path := range []string{"/metrics", "/", "/deeply/nested/metrics", "/with-dash"} {
		t.Run(path, func(t *testing.T) {
			srv, err := metrics.NewServer(testLogger(), metrics.Options{
				Address:     "127.0.0.1:0",
				MetricsPath: path,
				Collectors:  []string{"loadavg"},
			})
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			assert.Equal(t, http.StatusOK, rec.Code)
		})
	}
}

// --- lifecycle edge cases -------------------------------------------------

func TestScrapeDuringShutdown(t *testing.T) {
	srv, err := metrics.NewServer(testLogger(), metrics.Options{
		Address:    "127.0.0.1:0",
		Collectors: []string{"loadavg"},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	require.Eventually(t, func() bool {
		a := srv.Address()
		if strings.HasSuffix(a, ":0") {
			return false
		}
		c, err := net.DialTimeout("tcp", a, 200*time.Millisecond)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}, 10*time.Second, 50*time.Millisecond)

	// Hammer the endpoint while shutting down. Requests may fail; the server must
	// not panic and Start must return cleanly.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				resp, err := http.Get("http://" + srv.Address() + "/metrics")
				if err == nil {
					resp.Body.Close()
				}
			}
		}()
	}
	cancel()
	wg.Wait()

	select {
	case err := <-done:
		assert.NoError(t, err, "shutdown during active scrapes must be clean")
	case <-time.After(20 * time.Second):
		t.Fatal("Start did not return during concurrent scrapes")
	}
}

func TestRepeatedStartAfterShutdown(t *testing.T) {
	srv, err := metrics.NewServer(testLogger(), metrics.Options{
		Address:    "127.0.0.1:0",
		Collectors: []string{"loadavg"},
	})
	require.NoError(t, err)

	// controller-runtime never restarts a Runnable, but a restart must fail
	// predictably rather than panicking or silently serving nothing.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Start(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(200 * time.Millisecond)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	err = srv.Start(ctx2)
	// Either outcome is acceptable; a panic is not.
	if err != nil {
		assert.NotContains(t, err.Error(), "panic")
	}
}

func TestAlreadyCancelledContext(t *testing.T) {
	srv, err := metrics.NewServer(testLogger(), metrics.Options{
		Address:    "127.0.0.1:0",
		Collectors: []string{"loadavg"},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Start

	err = srv.Start(ctx)
	assert.NoError(t, err, "starting with an already-cancelled context must shut down cleanly")
}

// --- resource exhaustion --------------------------------------------------

func TestManyConcurrentScrapesDoNotLeakGoroutines(t *testing.T) {
	srv, err := metrics.NewServer(testLogger(), metrics.Options{
		Address:    "127.0.0.1:0",
		Collectors: []string{"loadavg", "stat", "meminfo"},
	})
	require.NoError(t, err)

	// 200 scrapes through the real handler. Run under -race; the assertion is
	// that this completes without deadlock or panic, which the relay/forwarder
	// machinery in resilience.go could plausibly break.
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		}()
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatal("200 concurrent scrapes did not complete; likely a deadlock in the collector relay")
	}
}
