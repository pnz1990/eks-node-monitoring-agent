package hostmetrics

// Tests for the pressure (PSI) collector.
//
// Two things dominate:
//
//   - THE UNIT. The kernel reports totals in MICROseconds; schedstat, in this same
//     package, uses NANOseconds. Copying the wrong constant is a silent 1000x error
//     that still produces a plausible near-zero reading on a healthy node. Verified
//     empirically rather than from memory -- see TestPSITotalIsMicroseconds.
//   - THE some/full ASYMMETRY, which is NOT uniform: cpu has some but no full, irq
//     has full but no some, io and memory have both. So 4 resources yield 6 series,
//     not 8.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- the unit -------------------------------------------------------------

func TestPSITotalIsMicroseconds(t *testing.T) {
	// The divisor is asserted against the named constant AND against upstream's
	// literal, so neither a typo here nor a divergence from upstream passes.
	assert.Equal(t, 1000.0*1000.0, psiMicrosecondsPerSecond,
		"PSI totals are microseconds; 1e9 would make every value 1000x too small")

	// And it must NOT be schedstat's constant. These live in the same package and
	// copying one to the other is the likely mistake.
	assert.NotEqual(t, float64(schedstatNanosecondsPerSecond), psiMicrosecondsPerSecond,
		"pressure uses microseconds and schedstat nanoseconds; they must not be the same constant")
}

func TestPSIUnitMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/pressure_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	// Upstream divides inline: float64(vals.Some.Total)/1000.0/1000.0
	assert.Regexp(t, regexp.MustCompile(`Total\)\s*/\s*1000\.0\s*/\s*1000\.0`), string(data),
		"upstream divides by 1000*1000; our constant must match that")
}

func TestPSIUnitIsEmpiricallyMicroseconds(t *testing.T) {
	// Not taken on faith from the docs. /proc/pressure/cpu's "some" total interpreted
	// as milliseconds EXCEEDS system uptime, which is arithmetically impossible, and
	// as nanoseconds gives an implausible 0.002% of uptime. Measured on this host:
	//
	//   uptime 1429441s, cpu some total=32151574389
	//     microseconds -> 32151.6s =  2.249% of uptime   plausible
	//     nanoseconds  ->    32.2s =  0.002% of uptime
	//     milliseconds -> 32151574s = 2249% of uptime     IMPOSSIBLE
	//
	// This test reruns that bound on whatever host it executes on.
	raw, err := os.ReadFile("/proc/pressure/cpu")
	if err != nil {
		t.Skipf("PSI unavailable on this host (%v)", err)
	}
	m := regexp.MustCompile(`some .*total=(\d+)`).FindSubmatch(raw)
	require.NotNil(t, m, "could not parse /proc/pressure/cpu")

	uptimeRaw, err := os.ReadFile("/proc/uptime")
	require.NoError(t, err)
	var uptime float64
	_, err = fmt.Sscan(string(uptimeRaw), &uptime)
	require.NoError(t, err)

	var total float64
	_, err = fmt.Sscan(string(m[1]), &total)
	require.NoError(t, err)

	asSeconds := total / psiMicrosecondsPerSecond
	assert.LessOrEqual(t, asSeconds, uptime,
		"pressure time cannot exceed uptime; the divisor is too small")
	// A divisor 1000x smaller (i.e. treating microseconds as milliseconds) must
	// break that bound, which is what makes the assertion above meaningful rather
	// than trivially true.
	if total > 0 {
		assert.Greater(t, total/1000.0, uptime,
			"if a 1000x-smaller divisor did NOT exceed uptime, the bound above proves nothing")
	}
}

func TestPSIValuesAreConvertedFromMicroseconds(t *testing.T) {
	c := newPressureFixtureCollector(t)
	// 2_500_000 microseconds = 2.5 seconds. Chosen so the correct answer is not a
	// round number that a wrong divisor might coincidentally produce.
	c.psiStats = psiStub(map[string]procfs.PSIStats{
		psiResourceCPU:    {Some: &procfs.PSILine{Total: 2_500_000}},
		psiResourceIO:     {Some: &procfs.PSILine{Total: 1_000_000}, Full: &procfs.PSILine{Total: 500_000}},
		psiResourceMemory: {Some: &procfs.PSILine{Total: 250_000}, Full: &procfs.PSILine{Total: 125_000}},
		psiResourceIRQ:    {Full: &procfs.PSILine{Total: 62_500}},
	})

	got := gatherPressure(t, c)
	assert.Equal(t, 2.5, got["node_pressure_cpu_waiting_seconds_total"])
	assert.Equal(t, 1.0, got["node_pressure_io_waiting_seconds_total"])
	assert.Equal(t, 0.5, got["node_pressure_io_stalled_seconds_total"])
	assert.Equal(t, 0.25, got["node_pressure_memory_waiting_seconds_total"])
	assert.Equal(t, 0.125, got["node_pressure_memory_stalled_seconds_total"])
	assert.Equal(t, 0.0625, got["node_pressure_irq_stalled_seconds_total"])
}

// --- the some/full asymmetry ---------------------------------------------

func TestPSIEmitsSixSeriesFromFourResources(t *testing.T) {
	// cpu: some only. io: some+full. memory: some+full. irq: full only. So 6, not 8.
	// A collector that emitted cpu_stalled or irq_waiting would be inventing a
	// measurement the kernel does not make.
	c := newPressureFixtureCollector(t)
	c.psiStats = psiStub(map[string]procfs.PSIStats{
		psiResourceCPU:    {Some: &procfs.PSILine{Total: 1}},
		psiResourceIO:     {Some: &procfs.PSILine{Total: 1}, Full: &procfs.PSILine{Total: 1}},
		psiResourceMemory: {Some: &procfs.PSILine{Total: 1}, Full: &procfs.PSILine{Total: 1}},
		psiResourceIRQ:    {Full: &procfs.PSILine{Total: 1}},
	})

	got := gatherPressure(t, c)
	assert.Len(t, got, 6)
	assert.NotContains(t, got, "node_pressure_cpu_stalled_seconds_total",
		"a fully-stalled CPU is not a state the kernel reports")
	assert.NotContains(t, got, "node_pressure_irq_waiting_seconds_total",
		"IRQ pressure has no 'some' data by design")
}

func TestPSICPUWithNoFullIsNotAnError(t *testing.T) {
	// cpu legitimately has no Full. Treating that as missing data would suppress
	// every pressure metric on every kernel.
	c := newPressureFixtureCollector(t)
	c.psiStats = psiStub(map[string]procfs.PSIStats{
		psiResourceCPU: {Some: &procfs.PSILine{Total: 3_000_000}},
	})

	got := gatherPressure(t, c)
	assert.Equal(t, 3.0, got["node_pressure_cpu_waiting_seconds_total"])
	assert.Len(t, got, 1)
}

func TestPSIIRQWithNoSomeIsNotAnError(t *testing.T) {
	c := newPressureFixtureCollector(t)
	c.psiStats = psiStub(map[string]procfs.PSIStats{
		psiResourceIRQ: {Full: &procfs.PSILine{Total: 4_000_000}},
	})

	got := gatherPressure(t, c)
	assert.Equal(t, 4.0, got["node_pressure_irq_stalled_seconds_total"])
	assert.Len(t, got, 1)
}

func TestPSIMissingSomeOnANonIRQResourceIsNoData(t *testing.T) {
	// io without Some means the file parsed but the expected line was absent, which
	// is not something a working kernel does -- so it is reported rather than
	// papered over with a zero.
	c := newPressureFixtureCollector(t)
	c.psiStats = psiStub(map[string]procfs.PSIStats{
		psiResourceIO: {Full: &procfs.PSILine{Total: 1}},
	})

	err := c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err), "expected ErrNoData, got %v", err)
}

func TestPSIMissingFullOnANonCPUResourceIsNoData(t *testing.T) {
	c := newPressureFixtureCollector(t)
	c.psiStats = psiStub(map[string]procfs.PSIStats{
		psiResourceMemory: {Some: &procfs.PSILine{Total: 1}},
	})

	err := c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
}

// --- ENOENT vs ENOTSUP ----------------------------------------------------

func TestPSIMissingIRQFileDoesNotSuppressOtherResources(t *testing.T) {
	// THE CASE THAT ACTUALLY HAPPENS. IRQ pressure needs kernel 6.1; the others need
	// 4.20. On this very host /proc/pressure contains cpu, io, memory and NOT irq
	// (kernel 6.12 without the IRQ PSI config). A missing irq file must cost only
	// the irq metric.
	c := newPressureFixtureCollector(t)
	c.psiStats = func(res string) (procfs.PSIStats, error) {
		if res == psiResourceIRQ {
			return procfs.PSIStats{}, os.ErrNotExist
		}
		return procfs.PSIStats{
			Some: &procfs.PSILine{Total: 1_000_000},
			Full: &procfs.PSILine{Total: 1_000_000},
		}, nil
	}

	got := gatherPressure(t, c)
	assert.Len(t, got, 5, "cpu + io x2 + memory x2, with irq absent")
	assert.NotContains(t, got, "node_pressure_irq_stalled_seconds_total")
	assert.Contains(t, got, "node_pressure_cpu_waiting_seconds_total")
}

func TestPSIAllResourcesMissingIsNoData(t *testing.T) {
	// A kernel without CONFIG_PSI. Every file absent, so nothing to report -- but
	// not a failure, because this is a supported configuration and alerting on it
	// would page someone about a working node.
	c := newPressureFixtureCollector(t)
	c.psiStats = func(string) (procfs.PSIStats, error) {
		return procfs.PSIStats{}, os.ErrNotExist
	}

	err := c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err), "expected ErrNoData, got %v", err)
}

func TestPSIENOTSUPIsNoDataAndStopsImmediately(t *testing.T) {
	// PSI compiled in but disabled at boot (no psi=1). Unlike ENOENT this is
	// system-wide, so there is no point trying the remaining resources -- asserted by
	// counting calls, since continuing would be three wasted syscalls per scrape.
	c := newPressureFixtureCollector(t)
	calls := 0
	c.psiStats = func(string) (procfs.PSIStats, error) {
		calls++
		return procfs.PSIStats{}, syscall.ENOTSUP
	}

	err := c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
	assert.Equal(t, 1, calls, "ENOTSUP is system-wide; the remaining resources must not be probed")
}

func TestPSIRealErrorIsAFailureNotNoData(t *testing.T) {
	// Anything other than ENOENT/ENOTSUP is a genuine problem and must not be
	// laundered into the benign signal.
	c := newPressureFixtureCollector(t)
	c.psiStats = func(string) (procfs.PSIStats, error) {
		return procfs.PSIStats{}, assert.AnError
	}

	err := c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err), "a real error must not be reported as no-data")
	assert.Contains(t, err.Error(), "failed to retrieve pressure stats")
}

// --- metric shape ---------------------------------------------------------

func TestPSIMetricsAreCounters(t *testing.T) {
	// These are cumulative totals since boot, so counters. As gauges, rate() would
	// be unavailable and the metric would be nearly useless -- pressure is only
	// meaningful as a rate.
	c := newPressureFixtureCollector(t)
	c.psiStats = psiStub(map[string]procfs.PSIStats{
		psiResourceCPU: {Some: &procfs.PSILine{Total: 1}},
		psiResourceIO:  {Some: &procfs.PSILine{Total: 1}, Full: &procfs.PSILine{Total: 1}},
	})

	ch := make(chan prometheus.Metric, 32)
	require.NoError(t, c.Update(ch))
	close(ch)

	n := 0
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.NotNil(t, pb.Counter, "%s must be a counter; pressure is only useful as a rate",
			metricName(t, m))
		assert.Empty(t, pb.GetLabel(), "pressure metrics are node-wide and unlabelled")
		n++
	}
	assert.Equal(t, 3, n)
}

func TestPSIResourceListMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/pressure_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	m := regexp.MustCompile(`psiResources\s*=\s*\[\]string\{([^}]*)\}`).FindSubmatch(data)
	require.NotNil(t, m, "failed to extract upstream's resource list; the regexp may be stale")

	// Upstream lists them by constant name; compare the count and that each of ours
	// appears, since a missing resource silently drops its metrics.
	assert.Len(t, psiResources, 4)
	for _, res := range []string{"psiResourceCPU", "psiResourceIO", "psiResourceMemory", "psiResourceIRQ"} {
		assert.Contains(t, string(m[1]), res)
	}
}

func TestPSILiveOnThisHost(t *testing.T) {
	// The real procfs path, not a stub. This host has cpu/io/memory but not irq, so
	// it also exercises the partial-availability path end to end.
	c, err := newPressureCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 64)
	err = c.Update(ch)
	close(ch)
	if err != nil && IsNoDataError(err) {
		t.Skip("PSI unavailable on this host")
	}
	require.NoError(t, err)

	require.NotEmpty(t, ch, "PSI is available here, so metrics must be emitted")
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.GreaterOrEqual(t, pb.GetCounter().GetValue(), 0.0,
			"%s must not be negative", metricName(t, m))
	}
}

func TestPSIConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newPressureCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

func TestPSIRegisteredConstructorWiresTheRealReader(t *testing.T) {
	c, err := newPressureCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	require.NotNil(t, c.(*pressureCollector).psiStats)
}

// --- helpers --------------------------------------------------------------

func newPressureFixtureCollector(t *testing.T) *pressureCollector {
	t.Helper()
	c, err := newPressureCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	return c.(*pressureCollector)
}

// psiStub returns a reader serving the given resources; anything absent from the
// map reports ErrNotExist, mirroring a kernel that lacks that file.
func psiStub(byResource map[string]procfs.PSIStats) func(string) (procfs.PSIStats, error) {
	return func(res string) (procfs.PSIStats, error) {
		stats, ok := byResource[res]
		if !ok {
			return procfs.PSIStats{}, os.ErrNotExist
		}
		return stats, nil
	}
}

// gatherPressure returns metric name -> value. Every PSI metric is unlabelled.
func gatherPressure(t *testing.T, c *pressureCollector) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		out[metricName(t, m)] = pb.GetCounter().GetValue()
	}
	return out
}
