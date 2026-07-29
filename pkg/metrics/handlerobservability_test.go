package metrics_test

// Tests for the two HTTP-handler observability gaps found by diffing LIVE endpoints
// against pne, neither of which any existing test could have caught.
//
//  1. promhttp_metric_handler_requests_total / _requests_in_flight were absent, because
//     promhttp.InstrumentMetricHandler was never called (Q9). This was the last remaining
//     metric-NAME difference against pne.
//
//  2. ErrorLog was nil, so a gather error incremented
//     promhttp_metric_handler_errors_total{cause="gathering"} and logged NOTHING. On the
//     live cluster one agent pod had 3182 gathering errors and not one log line
//     explaining any of them.
//
// WHY (2) NEEDS A TEST AND NOT JUST A FIX. Setting ErrorLog is one line, and a test that
// only asserts "the field is non-nil" would pass while proving nothing about whether an
// error actually reaches an operator. So the test below FORCES a gather error with a
// deliberately broken collector and asserts on the log OUTPUT. That is the difference
// between testing the wiring and testing the behaviour.
//
// Both are asserted in the negative direction too -- errors_total must stay 0 on a
// healthy scrape -- because a test that only checks the failure case cannot tell a
// working error path from one that fires constantly.

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/eks-node-monitoring-agent/pkg/metrics"
)

// scrape returns the body of a /metrics request against srv.
func scrape(t *testing.T, srv *metrics.Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

// sampleValue extracts a single sample's value by exact metric-line prefix. It fails the
// test when the sample is absent rather than returning a zero, because "0 errors" and "no
// such metric" are different claims and conflating them is how a silent regression passes.
func sampleValue(t *testing.T, body, prefix string) float64 {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(prefix) + `\s+(\S+)$`)
	m := re.FindStringSubmatch(body)
	require.NotNil(t, m, "metric %q not present in scrape output", prefix)
	v, err := strconv.ParseFloat(m[1], 64)
	require.NoError(t, err)
	return v
}

// --- Q9: the handler request metrics -----------------------------------------

func TestHandlerRequestMetricsPresentWithExporterMetrics(t *testing.T) {
	// These are the two series pne exports and the agent did not. They are what a
	// dashboard graphs to answer "are scrapes succeeding", so their absence is
	// invisible until someone migrates off pne and their panel goes blank.
	srv := newTestServer(t, metrics.Options{IncludeExporterMetrics: true})

	body := scrape(t, srv)

	assert.Contains(t, body, "promhttp_metric_handler_requests_total",
		"pne exports this via InstrumentMetricHandler; the agent must too")
	assert.Contains(t, body, "promhttp_metric_handler_requests_in_flight")

	// In-flight must be exactly 1 during its own scrape -- the request doing the
	// gathering. A 0 would mean the instrumentation is not actually wrapping the
	// handler, which is precisely the bug being fixed, and which a bare Contains
	// check on the name would not distinguish.
	assert.Equal(t, 1.0, sampleValue(t, body, "promhttp_metric_handler_requests_in_flight"),
		"the scrape must observe itself in flight")

	// The 200 counter is registered but not yet incremented for the in-progress
	// request, since the code is only known once the response is written. Asserting
	// it exists rather than its value keeps the test from depending on that ordering.
	assert.Contains(t, body, `promhttp_metric_handler_requests_total{code="200"}`)
}

func TestHandlerRequestMetricsFollowUpstreamConditional(t *testing.T) {
	// Upstream registers these on the EXPORTER registry, so they appear only when
	// exporter metrics are enabled. Reproduced deliberately: promoting them to the
	// main registry would make the agent export series in a configuration where pne
	// exports none, which is a parity difference in the other direction.
	srv := newTestServer(t, metrics.Options{})

	body := scrape(t, srv)

	assert.NotContains(t, body, "promhttp_metric_handler_requests_total",
		"without exporter metrics pne has no request counter, so neither may the agent")
	assert.NotContains(t, body, "promhttp_metric_handler_requests_in_flight")
}

func TestHandlerRequestCounterIncrementsAcrossScrapes(t *testing.T) {
	// A counter registered but never wired to the handler would sit at 0 forever and
	// still satisfy a name check. Two scrapes prove it actually counts.
	srv := newTestServer(t, metrics.Options{IncludeExporterMetrics: true})

	scrape(t, srv)
	first := sampleValue(t, scrape(t, srv), `promhttp_metric_handler_requests_total{code="200"}`)
	second := sampleValue(t, scrape(t, srv), `promhttp_metric_handler_requests_total{code="200"}`)

	assert.Greater(t, first, 0.0, "earlier scrapes must have been counted")
	assert.Equal(t, first+1, second, "each scrape must increment the 200 counter exactly once")
}

// --- the duplicate error counter (the live bug) -------------------------------

func TestExporterMetricsScrapeProducesNoGatherError(t *testing.T) {
	// THE REGRESSION TEST FOR THE LIVE BUG. newHandler used to build a handler with
	// `Registry: registry`, then reassign it with `Registry: exporterRegistry`. Both
	// registrations happened, so promhttp_metric_handler_errors_total existed in the
	// main AND exporter registries, and gathering Gatherers{exporter, main} collected
	// it twice -> "was collected before with the same name and label values" on EVERY
	// scrape. Measured live: +1 per scrape, 3182 on a 13h-old pod, with no log line
	// anywhere because ErrorLog was also unset.
	//
	// Asserted as a VALUE across two scrapes rather than as "the metric exists",
	// because the broken version exported the metric perfectly well -- it just
	// counted itself failing. Only the number showed it.
	srv := newTestServer(t, metrics.Options{IncludeExporterMetrics: true})

	first := sampleValue(t, scrape(t, srv), `promhttp_metric_handler_errors_total{cause="gathering"}`)
	second := sampleValue(t, scrape(t, srv), `promhttp_metric_handler_errors_total{cause="gathering"}`)

	assert.Equal(t, 0.0, first, "a healthy scrape with exporter metrics must not error")
	assert.Equal(t, 0.0, second,
		"and must not accumulate -- the live bug incremented this by exactly 1 per scrape")
}

func TestErrorCounterRegisteredExactlyOnce(t *testing.T) {
	// The cause, checked directly rather than only through its symptom: the error
	// counter must appear in exactly ONE gathered family. A duplicate registration
	// across the two registries yields two families with the same name, which is what
	// Gather rejects. Checking the count makes the failure legible instead of leaving
	// a reader to infer the cause from an error string.
	srv := newTestServer(t, metrics.Options{IncludeExporterMetrics: true})

	mfs, err := srv.Registry().Gather()
	require.NoError(t, err, "the main registry alone must gather cleanly")

	n := 0
	for _, mf := range mfs {
		if mf.GetName() == "promhttp_metric_handler_errors_total" {
			n++
		}
	}
	assert.Equal(t, 0, n,
		"the handler's self-metrics belong to the exporter registry only; finding them in "+
			"the main registry is the duplicate-registration bug")
}

// --- the silent gather error ------------------------------------------------

// brokenCollector emits the SAME metric twice, which Gather rejects with
// "was collected before with the same name and label values".
//
// WHY THIS FAILURE MODE AND NOT AN INCONSISTENT LABEL COUNT. My first version emitted a
// metric with the wrong number of label values via MustNewConstMetric. That panics inside
// Collect, and client_golang's safeCollect recovers it and reports the COLLECTOR TYPE
// ("type=*metrics_test.brokenCollector") with a stack trace -- never the metric name. So
// the assertion that an operator can see WHICH metric broke was failing against a real
// log line that genuinely did not contain it.
//
// Emitting a duplicate produces a plain (non-panic) gather error that names the metric,
// which is both the behaviour worth asserting and -- not coincidentally -- the exact error
// shape the live duplicate-registration bug produced.
type brokenCollector struct{ desc *prometheus.Desc }

func newBrokenCollector() *brokenCollector {
	return &brokenCollector{desc: prometheus.NewDesc(
		"nma_test_broken_metric", "Deliberately duplicated, to force a gather error.",
		nil, nil,
	)}
}

func (c *brokenCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *brokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1)
}

func TestGatherErrorsAreLoggedNotJustCounted(t *testing.T) {
	// THE POINT OF THIS TEST. ErrorHandling: ContinueOnError serves a partial scrape
	// and bumps errors_total. With ErrorLog nil that is ALL it does -- which is what
	// produced 3182 counted-but-unexplained errors on the live cluster. So this
	// asserts the log output, not the presence of a config field.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	srv, err := metrics.NewServer(logger, metrics.Options{
		Address:                "127.0.0.1:0",
		Collectors:             []string{"loadavg"},
		IncludeExporterMetrics: true,
	})
	require.NoError(t, err)
	require.NoError(t, srv.Registry().Register(newBrokenCollector()))

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	// ContinueOnError means the scrape still succeeds and still returns the healthy
	// metrics; only the broken family is dropped. Asserted so a future change to
	// ErrorHandling (which would start failing whole scrapes) is caught here.
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "node_load1",
		"a partial gather must still serve the collectors that worked")

	logged := buf.String()
	require.NotEmpty(t, logged,
		"a gather error MUST reach the log; counting it silently is the bug this fixes")
	assert.Contains(t, logged, "error gathering metrics",
		"the log line must identify the failure as a gather error")
	assert.Contains(t, logged, "nma_test_broken_metric",
		"the log must name the offending metric, or an operator cannot act on it")
}

func TestHealthyScrapeLogsNothingAndCountsNoErrors(t *testing.T) {
	// The other half: an error path that fires on healthy scrapes is as useless as one
	// that never fires. Without this, a change that logged on every scrape would pass
	// the test above.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	srv, err := metrics.NewServer(logger, metrics.Options{
		Address:                "127.0.0.1:0",
		Collectors:             []string{"loadavg"},
		IncludeExporterMetrics: true,
	})
	require.NoError(t, err)

	body := scrape(t, srv)

	assert.Empty(t, buf.String(), "a healthy scrape must log nothing at ERROR")
	assert.Equal(t, 0.0,
		sampleValue(t, body, `promhttp_metric_handler_errors_total{cause="gathering"}`),
		"a healthy scrape must not count a gather error")
}

// --- the same guarantees on the native implementation -----------------------

func TestNativeServerHasSameHandlerObservability(t *testing.T) {
	// The native and dependency servers share newHandler, so this asserts the shared
	// wiring is genuinely shared rather than reimplemented -- if the two ever diverge
	// here, the three-way comparison stops isolating the collector difference.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	srv, err := metrics.NewNativeServer(logger, metrics.Options{
		Address:                "127.0.0.1:0",
		Collectors:             []string{"loadavg"},
		IncludeExporterMetrics: true,
		HostRoot:               "/",
	})
	require.NoError(t, err)

	body := scrape(t, srv)

	assert.Contains(t, body, "promhttp_metric_handler_requests_total")
	assert.Contains(t, body, "promhttp_metric_handler_requests_in_flight")
	assert.Equal(t, 0.0,
		sampleValue(t, body, `promhttp_metric_handler_errors_total{cause="gathering"}`),
		"the native collectors must not produce gather errors on a healthy host")

	require.NoError(t, srv.Registry().Register(newBrokenCollector()))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Contains(t, buf.String(), "nma_test_broken_metric",
		"the native server must log gather errors too, via the shared handler")
}
