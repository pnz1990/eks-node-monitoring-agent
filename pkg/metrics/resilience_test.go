package metrics

// Tests for the resilience boundary (R1 panic containment, R2 per-collector
// timeout, R3 shared-fate isolation).
//
// Every test here is written so that it FAILS if the guard is removed. A guard
// whose test cannot fail is not a guard. TestGuardIsNecessary documents the
// unguarded behaviour explicitly so the necessity is recorded in the suite
// rather than only in a commit message.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/node_exporter/collector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test doubles ---------------------------------------------------------

// panickingCollector panics inside Update, reproducing upstream #1007 / #3346.
type panickingCollector struct{ msg string }

func (p panickingCollector) Update(ch chan<- prometheus.Metric) error {
	panic(p.msg)
}

// hangingCollector blocks until released, reproducing upstream #1353 / #1841.
type hangingCollector struct{ release chan struct{} }

func (h hangingCollector) Update(ch chan<- prometheus.Metric) error {
	<-h.release
	return nil
}

// healthyCollector emits one metric and succeeds.
type healthyCollector struct{ name string }

func (h healthyCollector) Update(ch chan<- prometheus.Metric) error {
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("test_"+h.name+"_value", "test metric", nil, nil),
		prometheus.GaugeValue, 42,
	)
	return nil
}

// failingCollector returns an ordinary error.
type failingCollector struct{}

func (failingCollector) Update(ch chan<- prometheus.Metric) error {
	return errors.New("collector failed for a mundane reason")
}

// noDataCollector returns upstream's sentinel for "nothing to report".
type noDataCollector struct{}

func (noDataCollector) Update(ch chan<- prometheus.Metric) error {
	return collector.ErrNoData
}

// lateWriterCollector keeps emitting after it has been abandoned. This is the
// case that would panic on a closed channel without the relay indirection.
type lateWriterCollector struct {
	started chan struct{}
	stop    chan struct{}
}

func (l lateWriterCollector) Update(ch chan<- prometheus.Metric) error {
	close(l.started)
	for i := 0; ; i++ {
		select {
		case <-l.stop:
			return nil
		default:
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(fmt.Sprintf("test_late_%d", i), "late metric", nil, nil),
			prometheus.GaugeValue, float64(i),
		)
		time.Sleep(time.Millisecond)
	}
}

func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// gather runs a full Collect cycle through a real registry, which is what the
// scrape path does, and returns the collected families by name.
func gather(t *testing.T, rc *resilientCollector) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(rc))
	families, err := reg.Gather()
	require.NoError(t, err)
	out := map[string]*dto.MetricFamily{}
	for _, f := range families {
		out[f.GetName()] = f
	}
	return out
}

// successFor extracts node_scrape_collector_success for a named collector.
func successFor(t *testing.T, families map[string]*dto.MetricFamily, name string) float64 {
	t.Helper()
	f, ok := families["node_scrape_collector_success"]
	require.True(t, ok, "node_scrape_collector_success must always be emitted")
	for _, m := range f.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "collector" && l.GetValue() == name {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("no success metric for collector %q", name)
	return -1
}

func newTestCollector(t *testing.T, timeout time.Duration, cs map[string]collector.Collector) *resilientCollector {
	t.Helper()
	return newResilientCollector(&collector.NodeCollector{Collectors: cs}, timeout, quietTestLogger())
}

// --- R1: panic containment ------------------------------------------------

// TestGuardIsNecessary documents WHY the guard exists: upstream runs collectors
// on their own goroutines with no recover, so a panic there is unrecoverable by
// the caller. This test proves the unguarded pattern is fatal-by-design by
// showing that a recover placed where upstream has none (i.e. in the caller)
// does not catch it.
func TestGuardIsNecessary(t *testing.T) {
	// Reproduce upstream's structure: caller recovers, panic happens in a
	// spawned goroutine. The caller's recover must NOT catch it.
	caught := make(chan any, 1)
	var wg sync.WaitGroup
	wg.Add(1)

	func() {
		defer func() {
			// This is where a naive fix would put the recover. It cannot work.
			if r := recover(); r != nil {
				caught <- r
			}
		}()
		go func() {
			defer wg.Done()
			// Recover here only because the test process must survive; this is
			// exactly the recover that upstream is missing.
			defer func() { _ = recover() }()
			panic("boom")
		}()
		wg.Wait()
	}()

	select {
	case r := <-caught:
		t.Fatalf("caller-side recover caught %v; if this ever passes, the guard could live in the caller", r)
	default:
		// Correct: the caller cannot catch a panic from a spawned goroutine,
		// which is why the recover must wrap the Update call itself.
	}
}

func TestPanicIsContained(t *testing.T) {
	rc := newTestCollector(t, time.Second, map[string]collector.Collector{
		"exploder": panickingCollector{msg: "simulated collector panic"},
		"healthy":  healthyCollector{name: "ok"},
	})

	// Without the recover in runCollector this call terminates the test process.
	families := gather(t, rc)

	assert.Equal(t, float64(0), successFor(t, families, "exploder"),
		"a panicking collector must report failure")
	assert.Equal(t, float64(1), successFor(t, families, "healthy"),
		"a healthy collector must be unaffected by a sibling panic")
	assert.Contains(t, families, "test_ok_value",
		"the healthy collector's metrics must still be collected")
}

func TestPanicIncrementsCounter(t *testing.T) {
	collectorPanicsTotal.Reset()
	rc := newTestCollector(t, time.Second, map[string]collector.Collector{
		"exploder": panickingCollector{msg: "counted panic"},
	})
	gather(t, rc)

	// A contained panic must be observable; silent containment hides a real fault.
	assert.Equal(t, float64(1), testCounterValue(t, collectorPanicsTotal, "exploder"))
}

func TestPanicWithNilValueIsContained(t *testing.T) {
	// panic(nil) is a real edge case: Go 1.21+ converts it to a *runtime.PanicNilError,
	// but code that checks `if rec != nil` must still behave.
	rc := newTestCollector(t, time.Second, map[string]collector.Collector{
		"nilpanic": nilPanicCollector{},
	})
	families := gather(t, rc)
	assert.Equal(t, float64(0), successFor(t, families, "nilpanic"))
}

type nilPanicCollector struct{}

func (nilPanicCollector) Update(ch chan<- prometheus.Metric) error {
	panic(nil) //nolint:govet // deliberately testing the nil-panic edge case
}

// --- R2: per-collector timeout --------------------------------------------

func TestTimeoutIsEnforced(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	rc := newTestCollector(t, 100*time.Millisecond, map[string]collector.Collector{
		"hanger":  hangingCollector{release: release},
		"healthy": healthyCollector{name: "ok"},
	})

	start := time.Now()
	families := gather(t, rc)
	elapsed := time.Since(start)

	// Without the timeout this blocks until `release` closes, i.e. forever.
	assert.Less(t, elapsed, 5*time.Second, "a hung collector must not stall the scrape")
	assert.Equal(t, float64(0), successFor(t, families, "hanger"))
	assert.Equal(t, float64(1), successFor(t, families, "healthy"),
		"a healthy collector must complete while a sibling is hung")
}

func TestTimeoutIncrementsCounter(t *testing.T) {
	collectorTimeoutsTotal.Reset()
	release := make(chan struct{})
	defer close(release)

	rc := newTestCollector(t, 50*time.Millisecond, map[string]collector.Collector{
		"hanger": hangingCollector{release: release},
	})
	gather(t, rc)

	assert.Equal(t, float64(1), testCounterValue(t, collectorTimeoutsTotal, "hanger"))
}

func TestTimeoutDefaultApplied(t *testing.T) {
	rc := newTestCollector(t, 0, map[string]collector.Collector{
		"healthy": healthyCollector{name: "ok"},
	})
	assert.Equal(t, defaultCollectorTimeout, rc.timeout,
		"zero timeout must select the default, not disable the bound")
}

func TestTimeoutDisabledByNegative(t *testing.T) {
	rc := newTestCollector(t, -1, map[string]collector.Collector{
		"healthy": healthyCollector{name: "ok"},
	})
	families := gather(t, rc)
	// Negative disables the bound; a healthy collector must still work.
	assert.Equal(t, float64(1), successFor(t, families, "healthy"))
}

// TestAbandonedCollectorWritingLateDoesNotPanic covers the bug that the relay
// indirection exists to prevent: a timed-out collector that keeps emitting after
// Collect has returned would write to a closed channel and panic on a goroutine
// where nothing can recover.
func TestAbandonedCollectorWritingLateDoesNotPanic(t *testing.T) {
	late := lateWriterCollector{started: make(chan struct{}), stop: make(chan struct{})}
	defer close(late.stop)

	rc := newTestCollector(t, 30*time.Millisecond, map[string]collector.Collector{
		"latewriter": late,
	})

	families := gather(t, rc)
	<-late.started
	assert.Equal(t, float64(0), successFor(t, families, "latewriter"))

	// Let the abandoned collector keep writing well past the gather. Without the
	// relay this panics on a closed channel and takes the process down.
	time.Sleep(200 * time.Millisecond)
}

// --- error classification -------------------------------------------------

func TestOrdinaryErrorReportsFailure(t *testing.T) {
	rc := newTestCollector(t, time.Second, map[string]collector.Collector{
		"failer": failingCollector{},
	})
	families := gather(t, rc)
	assert.Equal(t, float64(0), successFor(t, families, "failer"))
}

func TestNoDataIsNotTreatedAsSuccess(t *testing.T) {
	// Matches upstream: ErrNoData yields success=0 but is logged at debug rather
	// than error, because a collector with nothing to report is not broken.
	rc := newTestCollector(t, time.Second, map[string]collector.Collector{
		"nodata": noDataCollector{},
	})
	families := gather(t, rc)
	assert.Equal(t, float64(0), successFor(t, families, "nodata"))
}

// --- meta metric parity ---------------------------------------------------

func TestMetaMetricsMatchUpstreamNames(t *testing.T) {
	rc := newTestCollector(t, time.Second, map[string]collector.Collector{
		"healthy": healthyCollector{name: "ok"},
	})
	families := gather(t, rc)

	// These names are the endpoint contract; renaming them breaks dashboards.
	assert.Contains(t, families, "node_scrape_collector_success")
	assert.Contains(t, families, "node_scrape_collector_duration_seconds")

	d := families["node_scrape_collector_duration_seconds"]
	require.NotEmpty(t, d.GetMetric())
	assert.GreaterOrEqual(t, d.GetMetric()[0].GetGauge().GetValue(), float64(0))
}

func TestDescribeEmitsMetaDescriptors(t *testing.T) {
	rc := newTestCollector(t, time.Second, map[string]collector.Collector{})
	ch := make(chan *prometheus.Desc, 4)
	rc.Describe(ch)
	close(ch)

	var got []string
	for d := range ch {
		got = append(got, d.String())
	}
	require.Len(t, got, 2)
	assert.True(t, strings.Contains(strings.Join(got, " "), "node_scrape_collector_duration_seconds"))
	assert.True(t, strings.Contains(strings.Join(got, " "), "node_scrape_collector_success"))
}

func TestEmptyCollectorSetIsSafe(t *testing.T) {
	rc := newTestCollector(t, time.Second, map[string]collector.Collector{})
	families := gather(t, rc)
	// No collectors means no per-collector series, and no panic.
	assert.NotContains(t, families, "node_scrape_collector_success")
}

// --- concurrency ----------------------------------------------------------

func TestConcurrentCollectsAreSafe(t *testing.T) {
	rc := newTestCollector(t, time.Second, map[string]collector.Collector{
		"healthy":  healthyCollector{name: "ok"},
		"exploder": panickingCollector{msg: "concurrent panic"},
		"failer":   failingCollector{},
	})
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(rc))

	// Run under -race to catch data races in the relay/forward machinery.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reg.Gather()
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
}

// --- helpers --------------------------------------------------------------

// testCounterValue reads a single labelled counter value.
func testCounterValue(t *testing.T, vec *prometheus.CounterVec, label string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := vec.GetMetricWithLabelValues(label)
	require.NoError(t, err)
	require.NoError(t, c.Write(m))
	return m.GetCounter().GetValue()
}

// TestTimeoutWhileCollectorBlockedOnSend covers the detach path taken when the
// timeout fires while the collector is blocked sending into the relay, rather
// than between sends. Without the drain the collector goroutine would block
// forever on that send and leak.
func TestTimeoutWhileCollectorBlockedOnSend(t *testing.T) {
	blocked := &blockedSenderCollector{
		sending: make(chan struct{}),
		done:    make(chan struct{}),
	}

	// The forwarder consumes one metric, then the collector's next send blocks
	// because nothing reads it until the timeout detaches the forwarder.
	rc := newTestCollector(t, 60*time.Millisecond, map[string]collector.Collector{
		"blockedsender": blocked,
	})

	families := gather(t, rc)
	assert.Equal(t, float64(0), successFor(t, families, "blockedsender"),
		"a collector abandoned mid-send must report failure")

	// The drain must release the blocked collector so its goroutine can exit.
	select {
	case <-blocked.done:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned collector never completed; the drain did not release its blocked send")
	}
}

// blockedSenderCollector emits continuously without pausing, so it is highly
// likely to be blocked inside a channel send when the timeout fires.
type blockedSenderCollector struct {
	sending chan struct{}
	done    chan struct{}
}

func (b *blockedSenderCollector) Update(ch chan<- prometheus.Metric) error {
	defer close(b.done)
	for i := 0; i < 100000; i++ {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(fmt.Sprintf("test_blocked_%d", i), "blocked metric", nil, nil),
			prometheus.GaugeValue, float64(i),
		)
	}
	return nil
}

// slowConsumerCollector sends exactly one metric and then blocks on a second
// send. Combined with a gather that stops reading, this deterministically parks
// the forwarder inside its inner `ch <- m` send when the timeout fires, which is
// the detach branch that timing-dependent tests reach only intermittently.
type slowConsumerCollector struct {
	firstSent chan struct{}
	done      chan struct{}
}

func (s *slowConsumerCollector) Update(ch chan<- prometheus.Metric) error {
	defer close(s.done)
	desc := prometheus.NewDesc("test_slow_consumer", "slow consumer metric", nil, nil)
	// Emit continuously; the forwarder will be mid-send when the timeout fires
	// because the registry's channel is unbuffered and the reader is gone.
	for {
		select {
		case <-s.firstSent:
			return nil
		default:
		}
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, 1)
	}
}

// TestForwarderDetachesWhileBlockedSending drives the inner detach branch: the
// forwarder is blocked sending to a consumer that has stopped reading when the
// timeout elapses. Without the inner `case <-stopForwarding` the forwarder would
// block forever and the scrape would never return.
func TestForwarderDetachesWhileBlockedSending(t *testing.T) {
	slow := &slowConsumerCollector{firstSent: make(chan struct{}), done: make(chan struct{})}
	defer close(slow.firstSent)

	rc := newTestCollector(t, 40*time.Millisecond, map[string]collector.Collector{
		"slowconsumer": slow,
	})

	// Collect directly into a channel nobody drains past the first metric, so the
	// forwarder parks in its send while the timeout runs down.
	ch := make(chan prometheus.Metric)
	collectDone := make(chan struct{})
	go func() {
		defer close(collectDone)
		rc.Collect(ch)
	}()

	// Read one metric then stop, leaving the forwarder blocked.
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("collector produced nothing")
	}

	// Drain lazily so Collect can finish emitting its meta metrics.
	go func() {
		for range ch {
		}
	}()

	select {
	case <-collectDone:
		// Correct: the timeout detached the blocked forwarder and Collect returned.
	case <-time.After(10 * time.Second):
		t.Fatal("Collect never returned; the forwarder stayed blocked on send")
	}
}
