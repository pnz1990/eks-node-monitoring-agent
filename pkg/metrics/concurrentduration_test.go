package metrics

// WHY THIS FILE EXISTS
//
// N7 reported FINDING F-N7-1: "the dependency branch is ~15x slower than native over the
// 39 shared collectors, median ~250x". It was tracked as an open performance question
// (Q8) with a partial explanation (an unbuffered relay channel, ~4x) and an unexplained
// remainder.
//
// The finding was an ARTEFACT OF THE MEASUREMENT, not a property of the code.
//
// node_scrape_collector_duration_seconds measures WALL time. Every collector runs
// CONCURRENTLY. When the process has fewer usable cores than collectors, a collector's
// timer keeps running while its goroutine is descheduled, so each one reports
// approximately the whole batch's wall-clock window rather than its own work. Summing N
// such measurements multiplies the real cost by up to N -- and the harness summed them.
//
// Evidence gathered while closing Q8 (GOMAXPROCS swept, same host, same collectors):
//
//	GOMAXPROCS=1  dep sum=0.5552s  min=0.010862s med=0.011322s max=0.011806s
//	GOMAXPROCS=8  dep sum=0.0620s  min=0.000028s med=0.001314s max=0.005501s
//
// At GOMAXPROCS=1 the spread across 49 collectors is 1.09x. Those collectors do wildly
// different amounts of work (netclass walks every interface; loadavg reads one short
// file), so near-identical durations cannot be 49 honest measurements.
//
// Confirmed three ways:
//   - the same 49 collectors run SERIALLY cost 0.0166s total, against 0.9114s reported
//   - WALL time is nearly identical between implementations (0.0121s vs 0.0110s)
//   - buffering the relay channel changed nothing (0.5552s -> 0.5243s), so the ~4x
//     previously attributed to it does not survive a controlled test either
//
// THE TEST BELOW guards the conclusion rather than the numbers. Absolute timings are
// machine-dependent and would make this flaky, so it asserts the RELATIONSHIP that has to
// hold if the metric is being interpreted correctly: the sum of concurrent per-collector
// durations must be able to exceed the wall time of the collection that produced them.
// That is not a defect -- it is what "concurrent" means -- and asserting it here is what
// stops the sum being reported as cost again.
//
// A real per-collector-overhead regression would show up as WALL time growing, which the
// second test covers.

import (
	"io"
	"log/slog"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func concurrencyTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// gatherDurations returns the per-collector durations from one Gather, plus that Gather's
// wall time.
func gatherDurations(t *testing.T, srv *Server) (durs []float64, wall time.Duration) {
	t.Helper()
	start := time.Now()
	mfs, _ := srv.Registry().Gather()
	wall = time.Since(start)
	for _, mf := range mfs {
		if mf.GetName() != "node_scrape_collector_duration_seconds" {
			continue
		}
		for _, m := range mf.Metric {
			durs = append(durs, m.GetGauge().GetValue())
		}
	}
	sort.Float64s(durs)
	return durs, wall
}

func TestSummedConcurrentDurationsAreNotACostMeasure(t *testing.T) {
	// The claim: summing node_scrape_collector_duration_seconds does not measure how long
	// collection took, because the collectors overlap. Demonstrated by pinning GOMAXPROCS
	// to 1, which maximises the overlap and makes the effect unambiguous.
	//
	// This is the regression test for a REPORTING bug, so what it protects is the
	// interpretation: if someone "fixes" the sum to look like a cost, this fails.
	original := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(original)

	srv, err := NewNativeServer(concurrencyTestLogger(), Options{HostRoot: "/"})
	require.NoError(t, err)

	// Warm up: first-scrape costs (opening files, walking sysfs) are real but one-off, and
	// attributing them to the concurrency effect would overstate it.
	for i := 0; i < 2; i++ {
		_, _ = srv.Registry().Gather()
	}

	durs, wall := gatherDurations(t, srv)
	require.NotEmpty(t, durs, "no per-collector durations were produced")

	var sum float64
	for _, d := range durs {
		sum += d
	}

	t.Logf("GOMAXPROCS=1: %d collectors, wall=%.6fs, summed=%.6fs, min=%.6fs, max=%.6fs",
		len(durs), wall.Seconds(), sum, durs[0], durs[len(durs)-1])

	// The load-bearing assertion. Concurrent collectors each measure a window that overlaps
	// the others, so the sum is not bounded by the wall time. Anything that made this
	// assertion false -- serialising collection, or timing CPU instead of wall -- would be
	// a real behavioural change and should fail here so it gets read.
	require.LessOrEqual(t, wall.Seconds(), sum+1e-9,
		"wall time should not exceed the summed durations; if it does, collection is no "+
			"longer concurrent and the comparison in hack/three-way/stress.sh needs revisiting")
}

func TestCollectionWallTimeStaysWithinScrapeInterval(t *testing.T) {
	// The check that actually matters operationally, and the one F-N7-1 should have been.
	// A 15s Prometheus scrape interval has to accommodate the WALL time of a scrape; the
	// sum of overlapping per-collector timers is not a budget anything has to fit in.
	//
	// The bound is deliberately loose (2s against a 15s interval): this runs on shared CI
	// hardware, and the purpose is to catch an order-of-magnitude regression, not to police
	// milliseconds. A tight bound here would be flaky and would get muted, which is worse
	// than no bound.
	srv, err := NewNativeServer(concurrencyTestLogger(), Options{HostRoot: "/"})
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		_, _ = srv.Registry().Gather()
	}

	_, wall := gatherDurations(t, srv)
	t.Logf("native collection wall time: %.6fs", wall.Seconds())
	require.Less(t, wall, 2*time.Second,
		"a full native collection must finish well inside a 15s scrape interval")
}

func TestUpstreamAndNativeWallTimesAreComparable(t *testing.T) {
	// The measurement that refuted F-N7-1. Both implementations read the same files through
	// the same procfs library, so their WALL times should be within an order of magnitude
	// even though upstream runs 10 more collectors.
	//
	// Asserted as an order of magnitude rather than a tight ratio because the two genuinely
	// differ in collector count and in per-collector work, and because CI timing noise on a
	// few-millisecond measurement is large in relative terms. A 10x bound still catches the
	// ~250x that was originally claimed.
	logger := concurrencyTestLogger()

	depSrv, err := NewServer(logger, Options{HostRoot: "/"})
	require.NoError(t, err)
	natSrv, err := NewNativeServer(logger, Options{HostRoot: "/"})
	require.NoError(t, err)

	// Median of several passes: a single pass on shared hardware can be dominated by one
	// scheduling hiccup, and a ratio built from two noisy single samples is not evidence.
	median := func(srv *Server) float64 {
		var walls []float64
		for i := 0; i < 5; i++ {
			_, w := gatherDurations(t, srv)
			walls = append(walls, w.Seconds())
		}
		sort.Float64s(walls)
		return walls[len(walls)/2]
	}
	// Warm both before timing either, so neither pays first-scrape costs the other avoided.
	for i := 0; i < 2; i++ {
		_, _ = depSrv.Registry().Gather()
		_, _ = natSrv.Registry().Gather()
	}

	depWall, natWall := median(depSrv), median(natSrv)
	t.Logf("median wall time: upstream=%.6fs native=%.6fs ratio=%.2fx",
		depWall, natWall, depWall/natWall)

	require.Less(t, depWall, natWall*10,
		"upstream collection must not take ~10x the native wall time; F-N7-1 claimed ~250x, "+
			"which was an artefact of summing concurrent per-collector durations")
}

// TestPerCollectorDurationsSpreadWithWork is the negative control for the whole story
// above: it shows the durations DO reflect real work when the collectors are not competing
// for a core, which is what makes the GOMAXPROCS=1 flattening evidence of an artefact
// rather than simply how the metric behaves.
func TestPerCollectorDurationsSpreadWithWork(t *testing.T) {
	if runtime.NumCPU() < 4 {
		t.Skip("needs >=4 CPUs to give collectors room to run without overlapping")
	}
	original := runtime.GOMAXPROCS(runtime.NumCPU())
	defer runtime.GOMAXPROCS(original)

	srv, err := NewNativeServer(concurrencyTestLogger(), Options{HostRoot: "/"})
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, _ = srv.Registry().Gather()
	}

	durs, _ := gatherDurations(t, srv)
	require.NotEmpty(t, durs)
	require.Greater(t, durs[0], 0.0, "a zero minimum would make the spread meaningless")

	spread := durs[len(durs)-1] / durs[0]
	t.Logf("spread with cores available: %.1fx (min=%.6fs max=%.6fs)",
		spread, durs[0], durs[len(durs)-1])

	// Collectors doing genuinely different amounts of work must produce genuinely different
	// durations. If this ever collapses toward 1x on a many-core machine, the durations have
	// stopped measuring per-collector work and the diagnostic in stress.sh is reading a
	// constant.
	require.Greater(t, spread, 3.0,
		"per-collector durations must vary with the work each collector does")
}
