package hostmetrics

// Tests for the resilience boundary.
//
// This is the file that matters most for whether the agent survives a bad collector,
// and the equivalent code on the dependency branch cost THREE bugs -- all in the
// timeout path, all found only by writing these tests:
//
//   1. send on a closed channel, which turned the timeout guard into a NEW crash source
//   2. dropped healthy metrics by closing the forwarder early
//   3. defer ordering that skipped close(relay)
//
// So the tests here are adversarial by design: a collector that panics, one that hangs
// past the timeout, one that emits and THEN panics, one that keeps emitting after being
// abandoned. Each asserts both that the scrape survives AND that the signal is right.

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- stub collectors ------------------------------------------------------

// fnCollector adapts a function to Collector.
type fnCollector func(chan<- prometheus.Metric) error

func (f fnCollector) Update(ch chan<- prometheus.Metric) error { return f(ch) }

func testMetric(name string, value float64) prometheus.Metric {
	return prometheus.MustNewConstMetric(
		prometheus.NewDesc(name, "test metric", nil, nil),
		prometheus.GaugeValue, value)
}

func newTestSet(collectors map[string]Collector) *Set {
	return &Set{collectors: collectors, logger: quietLogger()}
}

// gatherAdapter registers the collector and gathers once, returning name -> value for
// unlabelled metrics and name{label} -> value for the meta metrics.
func gatherAdapter(t *testing.T, pc *PrometheusCollector) map[string]float64 {
	t.Helper()

	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(pc))

	mfs, err := reg.Gather()
	require.NoError(t, err, "Gather must succeed even when a collector misbehaves")

	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.Metric {
			key := mf.GetName()
			for _, l := range m.GetLabel() {
				key += "{" + l.GetName() + "=" + l.GetValue() + "}"
			}
			// Passed by POINTER, not copied. `pb = *m` copies a dto.Metric, which
			// embeds protoimpl.MessageState and therefore a sync.Mutex -- vet flags it
			// as "assignment copies lock value". Copying a locked mutex is undefined
			// behaviour, and the copy served no purpose here.
			out[key] = metricValueOf(t, m)
		}
	}
	return out
}

// --- the happy path -------------------------------------------------------

func TestAdapterEmitsMetricsAndMetaMetrics(t *testing.T) {
	set := newTestSet(map[string]Collector{
		"good": fnCollector(func(ch chan<- prometheus.Metric) error {
			ch <- testMetric("test_metric", 42)
			return nil
		}),
	})

	got := gatherAdapter(t, NewPrometheusCollector(set, 0, quietLogger()))

	assert.Equal(t, 42.0, got["test_metric"])
	assert.Equal(t, 1.0, got["node_scrape_collector_success{collector=good}"])
	assert.Contains(t, got, "node_scrape_collector_duration_seconds{collector=good}")
}

func TestAdapterRunsEveryCollector(t *testing.T) {
	// All collectors run concurrently, so a slow one must not prevent the others from
	// reporting.
	set := newTestSet(map[string]Collector{
		"a": fnCollector(func(ch chan<- prometheus.Metric) error {
			ch <- testMetric("metric_a", 1)
			return nil
		}),
		"b": fnCollector(func(ch chan<- prometheus.Metric) error {
			time.Sleep(20 * time.Millisecond)
			ch <- testMetric("metric_b", 2)
			return nil
		}),
	})

	got := gatherAdapter(t, NewPrometheusCollector(set, 0, quietLogger()))
	assert.Equal(t, 1.0, got["metric_a"])
	assert.Equal(t, 2.0, got["metric_b"])
	assert.Equal(t, 1.0, got["node_scrape_collector_success{collector=a}"])
	assert.Equal(t, 1.0, got["node_scrape_collector_success{collector=b}"])
}

func TestAdapterErrNoDataReportsSuccessZero(t *testing.T) {
	// ErrNoData is success=0, matching upstream. This is what makes hwmon report 0 on
	// EKS, so getting it wrong flips 3 of 39 collectors on every node.
	set := newTestSet(map[string]Collector{
		"nodata": fnCollector(func(chan<- prometheus.Metric) error { return ErrNoData }),
	})

	got := gatherAdapter(t, NewPrometheusCollector(set, 0, quietLogger()))
	assert.Equal(t, 0.0, got["node_scrape_collector_success{collector=nodata}"],
		"ErrNoData must report success=0, as upstream does")
}

func TestAdapterErrorReportsSuccessZero(t *testing.T) {
	set := newTestSet(map[string]Collector{
		"broken": fnCollector(func(chan<- prometheus.Metric) error { return errors.New("boom") }),
	})

	got := gatherAdapter(t, NewPrometheusCollector(set, 0, quietLogger()))
	assert.Equal(t, 0.0, got["node_scrape_collector_success{collector=broken}"])
}

// --- panic containment ----------------------------------------------------

func TestAdapterContainsAPanic(t *testing.T) {
	// The whole point of the boundary: a panicking collector must not take the scrape
	// down, and must report success=0 in the SAME scrape.
	set := newTestSet(map[string]Collector{
		"panicky": fnCollector(func(chan<- prometheus.Metric) error { panic("boom") }),
		"healthy": fnCollector(func(ch chan<- prometheus.Metric) error {
			ch <- testMetric("healthy_metric", 7)
			return nil
		}),
	})

	got := gatherAdapter(t, NewPrometheusCollector(set, 0, quietLogger()))

	assert.Equal(t, 0.0, got["node_scrape_collector_success{collector=panicky}"])
	assert.Equal(t, 1.0, got["node_scrape_collector_success{collector=healthy}"],
		"a panic in one collector must not affect another")
	assert.Equal(t, 7.0, got["healthy_metric"],
		"the healthy collector's metrics must survive")
}

func TestAdapterKeepsMetricsEmittedBeforeAPanic(t *testing.T) {
	// BUG 2 FROM THE DEPENDENCY BRANCH. Metrics already emitted are valid and must be
	// kept: partial data beats no data when the alternative is a failed scrape.
	set := newTestSet(map[string]Collector{
		"halfway": fnCollector(func(ch chan<- prometheus.Metric) error {
			ch <- testMetric("before_panic", 1)
			panic("boom")
		}),
	})

	got := gatherAdapter(t, NewPrometheusCollector(set, 0, quietLogger()))

	assert.Equal(t, 1.0, got["before_panic"],
		"a metric emitted before the panic must not be discarded")
	assert.Equal(t, 0.0, got["node_scrape_collector_success{collector=halfway}"])
}

func TestAdapterPanicCounterIncrementsButLagsOneScrape(t *testing.T) {
	// MEASURED BEHAVIOUR, not what I first assumed. The counter is incremented during
	// Collect, but the registry has already snapshotted its collector list -- so the
	// increment appears on the NEXT gather. The same-scrape signal is
	// node_scrape_collector_success, which is what an alert should use.
	//
	// ASSERTED ON THIS COLLECTOR'S OWN LABEL, not on family presence. The counters are
	// PACKAGE-LEVEL, so any earlier test in this binary that triggered a panic leaves
	// the family present -- my first version asserted absence of the family and failed
	// for that reason, not because the lag claim was wrong. Verified in isolation: with
	// a unique collector name, gather 1 has no series for it and gather 2 does.
	const name = "panic_lag_probe"
	set := newTestSet(map[string]Collector{
		name: fnCollector(func(chan<- prometheus.Metric) error { panic("boom") }),
	})

	reg := prometheus.NewRegistry()
	pc := NewPrometheusCollector(set, 0, quietLogger())
	require.NoError(t, pc.Register(reg))

	first, err := reg.Gather()
	require.NoError(t, err)
	assert.False(t, hasSeriesWithLabel(first, "node_collector_panics_total", name),
		"no series for this collector on the scrape during which the panic happened")

	second, err := reg.Gather()
	require.NoError(t, err)
	assert.True(t, hasSeriesWithLabel(second, "node_collector_panics_total", name),
		"and it appears on the next one")
}

// --- the timeout path -----------------------------------------------------

func TestAdapterTimesOutAHangingCollector(t *testing.T) {
	// A collector that blocks forever. The scrape must complete anyway.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	set := newTestSet(map[string]Collector{
		"hanging": fnCollector(func(chan<- prometheus.Metric) error {
			<-release
			return nil
		}),
		"healthy": fnCollector(func(ch chan<- prometheus.Metric) error {
			ch <- testMetric("healthy_metric", 3)
			return nil
		}),
	})

	start := time.Now()
	got := gatherAdapter(t, NewPrometheusCollector(set, 50*time.Millisecond, quietLogger()))
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 5*time.Second, "the scrape must not wait for the hung collector")
	assert.Equal(t, 0.0, got["node_scrape_collector_success{collector=hanging}"])
	assert.Equal(t, 3.0, got["healthy_metric"], "the healthy collector still reports")
}

func TestAdapterKeepsMetricsEmittedBeforeATimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	set := newTestSet(map[string]Collector{
		"slow": fnCollector(func(ch chan<- prometheus.Metric) error {
			ch <- testMetric("before_timeout", 5)
			<-release
			return nil
		}),
	})

	got := gatherAdapter(t, NewPrometheusCollector(set, 50*time.Millisecond, quietLogger()))

	assert.Equal(t, 5.0, got["before_timeout"],
		"a metric emitted before the timeout must be kept")
	assert.Equal(t, 0.0, got["node_scrape_collector_success{collector=slow}"])
}

func TestAdapterAbandonedCollectorWritingLateDoesNotPanic(t *testing.T) {
	// BUG 1 FROM THE DEPENDENCY BRANCH, and the reason the relay channel exists.
	//
	// An abandoned collector keeps emitting AFTER Collect returns, by which time the
	// registry has closed its channel. Writing to a closed channel panics on a goroutine
	// where nothing can recover it -- so a naive timeout guard becomes a NEW crash
	// source, worse than the hang it replaced.
	//
	// Here the collector emits 500 metrics well after being abandoned. The assertion is
	// that the process survives.
	proceed := make(chan struct{})
	done := make(chan struct{})

	set := newTestSet(map[string]Collector{
		"late": fnCollector(func(ch chan<- prometheus.Metric) error {
			<-proceed
			// Long after the scrape gave up.
			for i := 0; i < 500; i++ {
				ch <- testMetric("late_metric", float64(i))
			}
			close(done)
			return nil
		}),
	})

	got := gatherAdapter(t, NewPrometheusCollector(set, 20*time.Millisecond, quietLogger()))
	assert.Equal(t, 0.0, got["node_scrape_collector_success{collector=late}"])

	// Now let the abandoned collector flood its channel. Without the relay and its
	// drain, this panics or deadlocks.
	close(proceed)
	select {
	case <-done:
		// The abandoned collector finished writing into the drained relay.
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned collector blocked; the relay is not being drained")
	}
}

func TestAdapterTimeoutCounterIncrements(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	set := newTestSet(map[string]Collector{
		"hanging": fnCollector(func(chan<- prometheus.Metric) error {
			<-release
			return nil
		}),
	})

	reg := prometheus.NewRegistry()
	pc := NewPrometheusCollector(set, 20*time.Millisecond, quietLogger())
	require.NoError(t, pc.Register(reg))

	_, err := reg.Gather()
	require.NoError(t, err)
	second, err := reg.Gather()
	require.NoError(t, err)

	// Lags one scrape, like the panic counter. Keyed on the collector's own label for
	// the same reason: the counter is package-level.
	assert.True(t, hasSeriesWithLabel(second, "node_collector_timeouts_total", "hanging"))
}

func TestAdapterNegativeTimeoutDisablesTheBound(t *testing.T) {
	// The escape hatch: a negative timeout restores unbounded behaviour, for an operator
	// who would rather hang than lose a metric. A slow-but-finite collector must
	// complete rather than be abandoned.
	set := newTestSet(map[string]Collector{
		"slow": fnCollector(func(ch chan<- prometheus.Metric) error {
			time.Sleep(60 * time.Millisecond)
			ch <- testMetric("slow_metric", 9)
			return nil
		}),
	})

	got := gatherAdapter(t, NewPrometheusCollector(set, -1, quietLogger()))

	assert.Equal(t, 9.0, got["slow_metric"],
		"with the bound disabled, a slow collector must complete")
	assert.Equal(t, 1.0, got["node_scrape_collector_success{collector=slow}"])
}

func TestAdapterZeroTimeoutSelectsTheDefault(t *testing.T) {
	set := newTestSet(map[string]Collector{})
	pc := NewPrometheusCollector(set, 0, quietLogger())
	assert.Equal(t, defaultCollectorTimeout, pc.timeout)

	// And a negative one is preserved rather than defaulted.
	pc = NewPrometheusCollector(set, -1, quietLogger())
	assert.Equal(t, time.Duration(-1), pc.timeout)
}

// --- shape and registration ----------------------------------------------

func TestAdapterDescribeEmitsOnlyMetaDescriptors(t *testing.T) {
	// The collectors' own descriptors are built at collection time from kernel-supplied
	// names, so they cannot be enumerated up front. Describing only the meta descriptors
	// is what makes this an unchecked collector, as upstream's is.
	pc := NewPrometheusCollector(newTestSet(nil), 0, quietLogger())

	ch := make(chan *prometheus.Desc, 8)
	pc.Describe(ch)
	close(ch)

	var descs []string
	for d := range ch {
		descs = append(descs, d.String())
	}
	require.Len(t, descs, 2)
	assert.Contains(t, descs[0]+descs[1], "node_scrape_collector_duration_seconds")
	assert.Contains(t, descs[0]+descs[1], "node_scrape_collector_success")
}

func TestAdapterRegisterToleratesRepeatedCounterRegistration(t *testing.T) {
	// The panic and timeout counters are package-level, so a second server in the same
	// process -- which the tests create -- would otherwise fail to start on
	// AlreadyRegisteredError.
	set := newTestSet(map[string]Collector{})

	reg1 := prometheus.NewRegistry()
	require.NoError(t, NewPrometheusCollector(set, 0, quietLogger()).Register(reg1))

	reg2 := prometheus.NewRegistry()
	require.NoError(t, NewPrometheusCollector(set, 0, quietLogger()).Register(reg2),
		"a second registry must not fail on the package-level counters")
}

func TestAdapterRegisterFailsOnANonDuplicateCounterError(t *testing.T) {
	// The counter-registration error path that is NOT AlreadyRegisteredError. Reached
	// with a registry that already holds a DIFFERENT collector under the same name, which
	// is a genuine conflict rather than the benign repeat-registration case.
	reg := prometheus.NewRegistry()
	require.NoError(t, reg.Register(prometheus.NewCounter(prometheus.CounterOpts{
		Name: "node_collector_panics_total",
		Help: "a conflicting collector with the same name but no labels",
	})))

	err := NewPrometheusCollector(newTestSet(nil), 0, quietLogger()).Register(reg)
	require.Error(t, err,
		"a real conflict on the counter must be reported, not tolerated like a repeat registration")
}

func TestAdapterRegisterFailsOnAConflictingCollector(t *testing.T) {
	// A genuine registration conflict must still be reported.
	set := newTestSet(map[string]Collector{})
	reg := prometheus.NewRegistry()

	pc := NewPrometheusCollector(set, 0, quietLogger())
	require.NoError(t, pc.Register(reg))

	err := pc.Register(reg)
	require.Error(t, err, "registering the same collector twice must fail")
}

func TestAdapterEmptySetStillReportsNothingRatherThanFailing(t *testing.T) {
	got := gatherAdapter(t, NewPrometheusCollector(newTestSet(map[string]Collector{}), 0, quietLogger()))
	assert.Empty(t, got, "an empty set emits no metrics and does not fail")
}

func TestAdapterConcurrentGathersAreRaceFree(t *testing.T) {
	// promhttp permits concurrent scrapes (--web.max-requests defaults to 40 upstream),
	// and the os_release race on that branch was reachable precisely because of it. Run
	// under -race.
	set := newTestSet(map[string]Collector{
		"a": fnCollector(func(ch chan<- prometheus.Metric) error {
			ch <- testMetric("metric_a", 1)
			return nil
		}),
		"b": fnCollector(func(chan<- prometheus.Metric) error { return ErrNoData }),
		"c": fnCollector(func(chan<- prometheus.Metric) error { panic("boom") }),
	})

	reg := prometheus.NewRegistry()
	require.NoError(t, NewPrometheusCollector(set, 0, quietLogger()).Register(reg))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, err := reg.Gather()
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()
}

func TestAdapterLiveSetServesTheRealCollectors(t *testing.T) {
	// The real 39-collector set through the adapter, end to end.
	set, err := New(quietLogger(), Config{})
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	require.NoError(t, NewPrometheusCollector(set, 0, quietLogger()).Register(reg))

	mfs, err := reg.Gather()
	require.NoError(t, err)

	names := map[string]bool{}
	series := 0
	for _, mf := range mfs {
		names[mf.GetName()] = true
		series += len(mf.Metric)
	}

	assert.Greater(t, series, 500, "the real set must produce a substantial scrape")
	for _, want := range []string{
		"node_cpu_seconds_total", "node_memory_MemAvailable_bytes",
		"node_scrape_collector_success", "node_scrape_collector_duration_seconds",
	} {
		assert.True(t, names[want], "%s must be present", want)
	}
}

func TestIsCollectorFailure(t *testing.T) {
	assert.False(t, isCollectorFailure(nil))
	assert.True(t, isCollectorFailure(ErrNoData),
		"ErrNoData counts as a failure for success=0 purposes, matching upstream")
	assert.True(t, isCollectorFailure(errors.New("boom")))
	assert.True(t, isCollectorFailure(errPanicked))
}

func TestDrainRelayDiscardsAndReturns(t *testing.T) {
	// The drain must terminate when the relay closes, or it leaks a goroutine per
	// abandoned collector.
	relay := make(chan prometheus.Metric, 4)
	relay <- testMetric("x", 1)
	close(relay)

	done := make(chan struct{})
	go func() { drainRelay(relay); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drainRelay did not return after the relay closed")
	}
}

// --- helpers --------------------------------------------------------------

// hasSeriesWithLabel reports whether a family contains a series carrying labelValue.
//
// Keyed on the label rather than on family presence because the panic and timeout
// counters are PACKAGE-LEVEL: an earlier test in the same binary leaves the family
// present, so family presence says nothing about this collector.
func hasSeriesWithLabel(mfs []*dto.MetricFamily, family, labelValue string) bool {
	for _, mf := range mfs {
		if mf.GetName() != family {
			continue
		}
		for _, m := range mf.Metric {
			for _, l := range m.GetLabel() {
				if l.GetValue() == labelValue {
					return true
				}
			}
		}
	}
	return false
}
