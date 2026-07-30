package metrics

// Tests for FINDING F-K4-5: binding a port successfully is not the same as owning it.
//
// THE FAILURE BEING GUARDED. On a Karpenter node the agent bound `[::]:9100` while the
// prometheus-node-exporter addon held `[HOST_IP]:9100`. Both binds SUCCEEDED -- Linux permits a
// wildcard bind alongside an existing specific-address bind -- but the kernel routes inbound
// traffic to the more specific socket. So pne answered every scrape, the agent's metrics were
// unreachable, and the agent logged "serving node_exporter compatible metrics" with its DaemonSet
// reporting Ready.
//
// That is worse than the F-K4-1 crash, because nothing reports it: the scrape returns 200 with
// plausible node metrics from the wrong process. A customer migrating off the pne addon -- the
// exact scenario the endpoint exists to enable -- would see success while reading pne.
//
// WHY THESE TESTS ARE STRUCTURED AS THEY ARE. The check cannot be tested by asserting "the log
// contains a warning", because the interesting property is that it distinguishes THREE outcomes:
//
//	ours          -> quiet
//	not ours      -> ERROR naming the cause and the remediation
//	unreachable   -> INDETERMINATE, and explicitly NOT reported as shadowing
//
// The third is the one that matters most for trustworthiness. An inconclusive check that reports a
// confident answer is the failure family that has recurred throughout this project, so it gets its
// own test rather than being assumed.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// captureCtx returns a context whose logger writes into buf, so the self-check's output can be
// asserted on rather than inferred.
func captureCtx(t *testing.T, buf *bytes.Buffer) context.Context {
	t.Helper()
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return log.IntoContext(context.Background(), logr.FromSlogHandler(h))
}

// serveOn starts a real HTTP server on 127.0.0.1 returning body, and returns its port. This is a
// genuine socket rather than an httptest transport, because the whole point of the check is that it
// dials the port it believes it owns.
func serveOn(t *testing.T, body string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	return port
}

// newBareServer builds a Server without collectors, since these tests exercise only the
// self-check. Using NewServer would drag in the whole upstream flag latch for no benefit.
func newBareServer() *Server {
	return &Server{opts: Options{MetricsPath: DefaultMetricsPath}}
}

func TestVerifyOwnEndpointQuietWhenTheResponseIsOurs(t *testing.T) {
	// A response containing our marker family means we are the process answering. The check must
	// say nothing at ERROR -- a self-check that warns on a healthy agent would be muted within a
	// week and then never catch the real thing.
	var buf bytes.Buffer
	port := serveOn(t, `node_exporter_build_info{revision="unknown",version=""} 1`+"\n")

	old := verifyOwnEndpointDelay
	verifyOwnEndpointDelay = time.Millisecond
	defer func() { verifyOwnEndpointDelay = old }()

	newBareServer().verifyOwnEndpoint(captureCtx(t, &buf), "127.0.0.1:"+port)

	out := buf.String()
	assert.NotContains(t, out, "ANOTHER PROCESS IS ANSWERING",
		"the check must be silent when the endpoint is genuinely ours")
	assert.Contains(t, out, "self-check passed", "and it must record that it actually ran")
}

func TestVerifyOwnEndpointDetectsAShadowedPort(t *testing.T) {
	// THE F-K4-5 CASE. The port answers 200 with a plausible node_exporter metric set that does
	// NOT contain our marker -- exactly what a co-resident node_exporter serves.
	var buf bytes.Buffer
	// A REAL pne response shape, including its build_info -- so this proves the check
	// DISCRIMINATES between two similar payloads rather than merely detecting absence.
	pneLike := "# HELP node_cpu_seconds_total x\n" +
		"node_cpu_seconds_total{cpu=\"0\",mode=\"idle\"} 1234\n" +
		"node_scrape_collector_success{collector=\"cpu\"} 1\n" +
		`node_exporter_build_info{branch="HEAD",revision="6044da783597cc3b57aef7580ddcdcff58a4ee99",version="1.12.1"} 1` + "\n"
	port := serveOn(t, pneLike)

	old := verifyOwnEndpointDelay
	verifyOwnEndpointDelay = time.Millisecond
	defer func() { verifyOwnEndpointDelay = old }()

	newBareServer().verifyOwnEndpoint(captureCtx(t, &buf), "127.0.0.1:"+port)

	out := buf.String()
	require.Contains(t, out, "ANOTHER PROCESS IS ANSWERING",
		"a 200 response without our marker means something else owns the port")
	// The message must be actionable, not just alarming: an operator seeing this needs to know
	// what to do, or it becomes noise.
	assert.Contains(t, out, "prometheus-node-exporter", "the likely cause must be named")
	assert.Contains(t, out, "remediation", "the log must carry a remediation")
	assert.Contains(t, out, "Node condition reporting is unaffected",
		"and must say what still works, so this is not read as an outage")
}

func TestVerifyOwnEndpointIsIndeterminateWhenUnreachable(t *testing.T) {
	// THE MOST IMPORTANT OF THE THREE. If the probe cannot be performed at all, the check must NOT
	// claim shadowing. Reporting "another process owns your port" because a dial failed would be a
	// confident answer from an inconclusive check -- the exact failure family this project keeps
	// producing.
	//
	// A closed port is used rather than a fake transport, so the real net.Dial error path runs.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, ln.Close()) // nothing is listening now

	var buf bytes.Buffer
	old := verifyOwnEndpointDelay
	verifyOwnEndpointDelay = time.Millisecond
	defer func() { verifyOwnEndpointDelay = old }()

	newBareServer().verifyOwnEndpoint(captureCtx(t, &buf), "127.0.0.1:"+port)

	out := buf.String()
	assert.NotContains(t, out, "ANOTHER PROCESS IS ANSWERING",
		"an unreachable endpoint is INDETERMINATE, not evidence of shadowing")
	assert.Contains(t, out, "inconclusive", "and it must say so explicitly")
}

func TestVerifyOwnEndpointHonoursContextCancellation(t *testing.T) {
	// The check runs on its own goroutine for the process lifetime, so it must not outlive
	// shutdown. Asserted by cancelling before the delay elapses and requiring it to return
	// without probing.
	var buf bytes.Buffer
	port := serveOn(t, `node_exporter_build_info{revision="unknown"} 1`+"\n")

	old := verifyOwnEndpointDelay
	verifyOwnEndpointDelay = 30 * time.Second // long enough that only cancellation can end it
	defer func() { verifyOwnEndpointDelay = old }()

	ctx, cancel := context.WithCancel(captureCtx(t, &buf))
	cancel()

	done := make(chan struct{})
	go func() { defer close(done); newBareServer().verifyOwnEndpoint(ctx, "127.0.0.1:"+port) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("verifyOwnEndpoint ignored context cancellation")
	}
	assert.NotContains(t, buf.String(), "self-check passed", "a cancelled check must not probe")
}

func TestVerifyOwnEndpointSkipsAnUnparsableAddress(t *testing.T) {
	// Defensive: Start passes listener.Addr().String(), which is always well-formed, but a future
	// caller might not. Skipping quietly is right -- it is a programming error, not an operational
	// condition, and warning about it would train operators to ignore this log line.
	var buf bytes.Buffer
	old := verifyOwnEndpointDelay
	verifyOwnEndpointDelay = time.Millisecond
	defer func() { verifyOwnEndpointDelay = old }()

	newBareServer().verifyOwnEndpoint(captureCtx(t, &buf), "not-an-address")

	assert.NotContains(t, buf.String(), "ANOTHER PROCESS IS ANSWERING")
	assert.Contains(t, buf.String(), "unparsable bound address")
}

func TestOwnEndpointMarkerIsActuallyServedByThisAgent(t *testing.T) {
	// THE LOAD-BEARING CONTROL FOR THE WHOLE MECHANISM. Every test above assumes the marker family
	// appears in a real scrape of this agent. If it did not -- a rename, or a build where the
	// resilience counters are not registered -- the check would report shadowing on every healthy
	// node, which is far worse than the bug it detects.
	//
	// So this scrapes the REAL handler and asserts the marker is present.
	srv, err := NewServer(quietLogger(), Options{Address: "127.0.0.1:0", Collectors: []string{"loadavg"}})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, DefaultMetricsPath, nil))
	require.Equal(t, http.StatusOK, rec.Code)

	assert.True(t, strings.Contains(rec.Body.String(), ownEndpointMarker),
		"%s must appear in a real scrape, or the endpoint self-check would report shadowing on "+
			"every healthy node", ownEndpointMarker)
}
