package hostmetrics

// Tests for the time and timex collectors.
//
// timex is entirely about DIVISORS, and there are three different ones applying to
// different fields of the same struct:
//
//	offset, jitter            -> divisor DEPENDS ON THE STA_NANO STATUS BIT
//	maxerror, esterror, tick  -> ALWAYS microseconds, even when STA_NANO is set
//	freq, ppsfreq, stabil     -> 16-bit-fraction PPM (1e6 * 65536), and freq has +1
//	shift, tai, constant      -> no conversion at all
//
// Every one of these is asserted against a computed expectation, because getting any
// wrong produces a number that reads as a perfectly healthy clock.

import (
	"os"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs/sysfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// --- timex: the conditional divisor ---------------------------------------

func TestTimexOffsetUsesMicrosecondsWhenStaNanoClear(t *testing.T) {
	// STA_NANO clear: the kernel reports microseconds.
	c := newTimexFixtureCollector(t, &unix.Timex{Status: 0, Offset: 1500, Jitter: 2500}, 0)

	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 0.0015, got["node_timex_offset_seconds"],
		"1500 microseconds = 0.0015s")
	assert.Equal(t, 0.0025, got["node_timex_pps_jitter_seconds"])
}

func TestTimexOffsetUsesNanosecondsWhenStaNanoSet(t *testing.T) {
	// STA_NANO set: the SAME field is now nanoseconds. Hardcoding either divisor is a
	// 1000x error on half the machines in existence, and the metric still exists with
	// a plausible small value.
	c := newTimexFixtureCollector(t, &unix.Timex{Status: staNano, Offset: 1500, Jitter: 2500}, 0)

	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 0.0000015, got["node_timex_offset_seconds"],
		"1500 nanoseconds = 0.0000015s")
	assert.Equal(t, 0.0000025, got["node_timex_pps_jitter_seconds"])
}

func TestTimexStaNanoDoesNotAffectMaxerrorEsterrorOrTick(t *testing.T) {
	// THE ASYMMETRY. maxerror, esterror and tick are ALWAYS microseconds regardless
	// of STA_NANO -- applying the conditional divisor to them would make them 1000x
	// too small on a nanosecond kernel, which is most modern kernels.
	withNano := newTimexFixtureCollector(t, &unix.Timex{
		Status: staNano, Maxerror: 16000000, Esterror: 4000, Tick: 10000,
	}, 0)
	withoutNano := newTimexFixtureCollector(t, &unix.Timex{
		Status: 0, Maxerror: 16000000, Esterror: 4000, Tick: 10000,
	}, 0)

	a := gatherUnlabelledMixed(t, withNano)
	b := gatherUnlabelledMixed(t, withoutNano)

	for _, name := range []string{
		"node_timex_maxerror_seconds",
		"node_timex_estimated_error_seconds",
		"node_timex_tick_seconds",
	} {
		assert.Equal(t, b[name], a[name],
			"%s must not depend on STA_NANO", name)
	}
	assert.Equal(t, 16.0, a["node_timex_maxerror_seconds"], "16000000 us = 16s")
	assert.Equal(t, 0.004, a["node_timex_estimated_error_seconds"])
	assert.Equal(t, 0.01, a["node_timex_tick_seconds"], "10000 us = 10ms, a 100Hz tick")
}

// --- timex: the PPM fields ------------------------------------------------

func TestTimexFrequencyIsARatioAroundOne(t *testing.T) {
	// freq is a frequency RATIO, so 1 is ADDED. Without the +1 the metric reads as
	// ~0, i.e. a stopped clock, which is a dramatic-looking but entirely wrong
	// number.
	c := newTimexFixtureCollector(t, &unix.Timex{Freq: 0}, 0)
	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 1.0, got["node_timex_frequency_adjustment_ratio"],
		"a zero adjustment is a ratio of 1.0, not 0.0")

	// ppm16frac = 1e6 * 65536. A Freq of 65536 is therefore exactly 1 PPM.
	c = newTimexFixtureCollector(t, &unix.Timex{Freq: 65536}, 0)
	got = gatherUnlabelledMixed(t, c)
	assert.InDelta(t, 1.000001, got["node_timex_frequency_adjustment_ratio"], 1e-12,
		"Freq=65536 is 1 PPM, so the ratio is 1.000001")
}

func TestTimexPPMFieldsUseThe16BitFractionScale(t *testing.T) {
	// ppsfreq and stabil use the same scale as freq but WITHOUT the +1: they are
	// frequency values in hertz, not ratios.
	c := newTimexFixtureCollector(t, &unix.Timex{Ppsfreq: 65536, Stabil: 131072}, 0)

	got := gatherUnlabelledMixed(t, c)
	assert.InDelta(t, 1e-6, got["node_timex_pps_frequency_hertz"], 1e-15,
		"Ppsfreq=65536 / (1e6*65536) = 1e-6; note NO +1 here, unlike freq")
	assert.InDelta(t, 2e-6, got["node_timex_pps_stability_hertz"], 1e-15)
}

func TestTimexPPMScaleMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/timex.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	m := regexp.MustCompile(`ppm16frac\s*=\s*([^\n]+)`).FindSubmatch(data)
	require.NotNil(t, m, "failed to extract upstream's ppm16frac; the regexp may be stale")
	assert.Equal(t, "1000000.0 * 65536.0", string(m[1]))
	assert.Equal(t, 1000000.0*65536.0, float64(ppm16frac))
}

func TestTimexUnconvertedFieldsPassThrough(t *testing.T) {
	// shift, tai, constant and status are NOT durations despite shift's _seconds
	// suffix -- shift is an exponent. Dividing them would produce near-zero values.
	c := newTimexFixtureCollector(t, &unix.Timex{
		Shift: 4, Tai: 37, Constant: 2, Status: 8193,
	}, 0)

	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 4.0, got["node_timex_pps_shift_seconds"], "shift is an exponent, not a duration")
	assert.Equal(t, 37.0, got["node_timex_tai_offset_seconds"], "TAI offset is whole seconds")
	assert.Equal(t, 2.0, got["node_timex_loop_time_constant"])
	assert.Equal(t, 8193.0, got["node_timex_status"], "status is a raw bitmask")
}

func TestTimexEventCountsPassThroughAsCounters(t *testing.T) {
	c := newTimexFixtureCollector(t, &unix.Timex{
		Jitcnt: 1, Calcnt: 2, Errcnt: 3, Stbcnt: 4,
	}, 0)

	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 1.0, got["node_timex_pps_jitter_total"])
	assert.Equal(t, 2.0, got["node_timex_pps_calibration_total"])
	assert.Equal(t, 3.0, got["node_timex_pps_error_total"])
	assert.Equal(t, 4.0, got["node_timex_pps_stability_exceeded_total"])
}

// --- timex: sync status ---------------------------------------------------

func TestTimexSyncStatusComesFromTheReturnValueNotTheStatusField(t *testing.T) {
	// These are DIFFERENT things: the return value is the clock state enum (TIME_OK,
	// TIME_INS, ..., TIME_ERROR=5) while Status is a bitmask. Reading sync_status
	// from Status would produce a nonsense 0/1 from an unrelated number.
	//
	// Status is deliberately set to a large value here: if sync_status were derived
	// from it, the assertion below would fail.
	c := newTimexFixtureCollector(t, &unix.Timex{Status: 8193}, timeError)
	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 0.0, got["node_timex_sync_status"],
		"a TIME_ERROR return means unsynchronised")

	c = newTimexFixtureCollector(t, &unix.Timex{Status: 0}, 0)
	got = gatherUnlabelledMixed(t, c)
	assert.Equal(t, 1.0, got["node_timex_sync_status"], "TIME_OK means synchronised")
}

func TestTimexSyncStatusIsOneForEveryNonErrorState(t *testing.T) {
	// TIME_OK=0, TIME_INS=1, TIME_DEL=2, TIME_OOP=3, TIME_WAIT=4 all mean the clock
	// IS synchronised -- a leap-second insertion is not a sync failure. Only
	// TIME_ERROR=5 is unsynchronised.
	for _, status := range []int{0, 1, 2, 3, 4} {
		c := newTimexFixtureCollector(t, &unix.Timex{}, status)
		got := gatherUnlabelledMixed(t, c)
		assert.Equal(t, 1.0, got["node_timex_sync_status"],
			"clock state %d is synchronised", status)
	}

	c := newTimexFixtureCollector(t, &unix.Timex{}, timeError)
	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 0.0, got["node_timex_sync_status"])
}

func TestTimexErrorConstantMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/timex.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	for name, want := range map[string]string{
		"timeError": "5",
		"staNano":   "0x2000",
	} {
		m := regexp.MustCompile(name + `\s*=\s*(\S+)`).FindSubmatch(data)
		require.NotNil(t, m, "failed to extract upstream's %s", name)
		assert.Equal(t, want, string(m[1]), "%s must match upstream", name)
	}
	assert.Equal(t, 5, timeError)
	assert.Equal(t, 0x2000, staNano)
}

// --- timex: error handling ------------------------------------------------

func TestTimexPermissionDeniedIsNoData(t *testing.T) {
	// adjtimex(2) with a zeroed modes field is a read, but some seccomp profiles deny
	// it outright. Not a failure: the agent simply cannot see the clock state, and
	// alerting on a hardened sandbox would be alerting on a deliberate configuration.
	c := newTimexFixtureCollector(t, &unix.Timex{}, 0)
	c.adjtimex = func(*unix.Timex) (int, error) { return 0, os.ErrPermission }

	err := c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err), "expected ErrNoData, got %v", err)
}

func TestTimexEPERMSyscallErrorIsAlsoNoData(t *testing.T) {
	// The real syscall returns syscall.EPERM, not os.ErrPermission. errors.Is bridges
	// them, but only because syscall.Errno implements Is -- worth pinning, since a
	// refactor to a plain comparison would break it and turn a hardened sandbox into
	// a reported failure.
	c := newTimexFixtureCollector(t, &unix.Timex{}, 0)
	c.adjtimex = func(*unix.Timex) (int, error) { return 0, syscall.EPERM }

	err := c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err),
		"syscall.EPERM must map to ErrNoData via errors.Is, got %v", err)
}

func TestTimexOtherErrorIsAFailure(t *testing.T) {
	c := newTimexFixtureCollector(t, &unix.Timex{}, 0)
	c.adjtimex = func(*unix.Timex) (int, error) { return 0, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err))
	assert.Contains(t, err.Error(), "failed to retrieve adjtimex stats")
}

// --- timex: shape ---------------------------------------------------------

func TestTimexEmitsSeventeenMetrics(t *testing.T) {
	c := newTimexFixtureCollector(t, &unix.Timex{}, 0)
	got := gatherUnlabelledMixed(t, c)
	assert.Len(t, got, 17, "13 gauges + 4 counters")
}

func TestTimexCounterFieldsAreCountersAndTheRestGauges(t *testing.T) {
	// The four *cnt fields are cumulative event counts. Everything else is an
	// instantaneous reading -- offset in particular goes up and down, so as a counter
	// rate() would treat every correction as a reset.
	c := newTimexFixtureCollector(t, &unix.Timex{}, 0)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	counters := map[string]bool{
		"node_timex_pps_jitter_total":             true,
		"node_timex_pps_calibration_total":        true,
		"node_timex_pps_error_total":              true,
		"node_timex_pps_stability_exceeded_total": true,
	}
	seen := 0
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		name := metricName(t, m)
		if counters[name] {
			assert.NotNil(t, pb.Counter, "%s must be a counter", name)
			seen++
			continue
		}
		assert.NotNil(t, pb.Gauge, "%s must be a gauge", name)
	}
	assert.Equal(t, 4, seen, "all four counter fields must be present")
}

func TestTimexLiveOnThisHost(t *testing.T) {
	// The real syscall. A synchronised host reports sync_status=1 and a
	// frequency_adjustment_ratio near 1.
	c, err := newTimexCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 64)
	err = c.Update(ch)
	close(ch)
	if err != nil && IsNoDataError(err) {
		t.Skip("adjtimex not permitted in this environment")
	}
	require.NoError(t, err)

	got := drainMixed(t, ch)
	require.Len(t, got, 17)
	assert.InDelta(t, 1.0, got["node_timex_frequency_adjustment_ratio"], 0.001,
		"a real clock's frequency ratio must be within 1000 PPM of 1.0")
	assert.Contains(t, []float64{0, 1}, got["node_timex_sync_status"])
}

func TestTimexRegisteredConstructorWiresTheSyscall(t *testing.T) {
	c, err := newTimexCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	require.NotNil(t, c.(*timexCollector).adjtimex)
}

// --- time -----------------------------------------------------------------

func TestTimeSecondsHasSubSecondResolution(t *testing.T) {
	// UnixNano/1e9 rather than Unix(): the point of this metric is measuring skew
	// between nodes, and truncating to whole seconds would put the floor on
	// detectable skew at 1s.
	c := newTimeFixtureCollector(t)
	instant := time.Unix(1700000000, 500000000) // .5s
	c.nowFunc = func() time.Time { return instant }

	got := gatherTimeMetrics(t, c)
	assert.Equal(t, 1700000000.5, got["node_time_seconds"],
		"the fractional part must survive")
}

func TestTimeZoneOffsetIsLabelledWithTheZoneName(t *testing.T) {
	c := newTimeFixtureCollector(t)
	zone := time.FixedZone("TESTZONE", 3600)
	c.nowFunc = func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, zone) }

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	found := false
	for m := range ch {
		if metricName(t, m) != "node_time_zone_offset_seconds" {
			continue
		}
		found = true
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.Equal(t, 3600.0, pb.GetGauge().GetValue())
		require.Len(t, pb.GetLabel(), 1)
		assert.Equal(t, "time_zone", pb.GetLabel()[0].GetName())
		assert.Equal(t, "TESTZONE", pb.GetLabel()[0].GetValue())
	}
	assert.True(t, found, "zone_offset_seconds must be emitted")
}

func TestTimeClocksourceLabelIsTheIndex(t *testing.T) {
	// The device label is the POSITION in the clocksource list, not a name:
	// /sys/devices/system/clocksource holds clocksource0, clocksource1, ... and
	// upstream labels by index. Changing it changes the label value.
	c := newTimeFixtureCollector(t)
	c.clockSources = func() ([]sysfs.ClockSource, error) {
		return []sysfs.ClockSource{
			{Available: []string{"tsc", "hpet", "acpi_pm"}, Current: "tsc"},
			{Available: []string{"kvm-clock"}, Current: "kvm-clock"},
		}, nil
	}

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	type key struct{ device, clocksource string }
	available := map[key]bool{}
	current := map[key]bool{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		labels := map[string]string{}
		for _, l := range pb.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		k := key{labels["device"], labels["clocksource"]}
		switch metricName(t, m) {
		case "node_time_clocksource_available_info":
			available[k] = true
		case "node_time_clocksource_current_info":
			current[k] = true
		}
	}

	assert.True(t, available[key{"0", "tsc"}])
	assert.True(t, available[key{"0", "hpet"}])
	assert.True(t, available[key{"0", "acpi_pm"}])
	assert.True(t, available[key{"1", "kvm-clock"}])
	assert.Len(t, available, 4, "three available on device 0 plus one on device 1")

	assert.True(t, current[key{"0", "tsc"}])
	assert.True(t, current[key{"1", "kvm-clock"}])
	assert.Len(t, current, 2, "one current per device")
}

func TestTimeClocksourceFailureIsReported(t *testing.T) {
	c := newTimeFixtureCollector(t)
	c.clockSources = func() ([]sysfs.ClockSource, error) { return nil, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 64))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get clocksources")
}

func TestTimeStillEmitsClockMetricsBeforeClocksourceFailure(t *testing.T) {
	// now and zone_offset are emitted before the sysfs read, so a clocksource failure
	// does not cost them. That ordering is deliberate: the time metrics are the ones
	// used for skew detection and they cannot fail.
	c := newTimeFixtureCollector(t)
	c.clockSources = func() ([]sysfs.ClockSource, error) { return nil, assert.AnError }

	ch := make(chan prometheus.Metric, 64)
	err := c.Update(ch)
	close(ch)
	require.Error(t, err)

	got := drainMixed(t, ch)
	assert.Contains(t, got, "node_time_seconds",
		"the time metrics precede the sysfs read and must survive its failure")
	assert.Contains(t, got, "node_time_zone_offset_seconds")
}

func TestTimeConstructionFailsOnMissingSysfs(t *testing.T) {
	// Upstream opens sysfs inside update() on every scrape; here it is opened once at
	// construction, so a bad path fails at startup rather than every 15 seconds.
	_, err := newTimeCollector(quietLogger(),
		Paths{SysFS: "/definitely/absent/sysfs"}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open sysfs")
}

func TestTimeLiveOnThisHost(t *testing.T) {
	c, err := newTimeCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	before := float64(time.Now().UnixNano()) / 1e9
	got := gatherTimeMetrics(t, c)
	after := float64(time.Now().UnixNano()) / 1e9

	require.Contains(t, got, "node_time_seconds")
	assert.GreaterOrEqual(t, got["node_time_seconds"], before)
	assert.LessOrEqual(t, got["node_time_seconds"], after,
		"the reported time must fall within the scrape window")
}

func TestTimeRegisteredConstructorWiresBothReaders(t *testing.T) {
	c, err := newTimeCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	tc := c.(*timeCollector)
	require.NotNil(t, tc.nowFunc)
	require.NotNil(t, tc.clockSources)
}

// --- helpers --------------------------------------------------------------

func newTimexFixtureCollector(t *testing.T, timex *unix.Timex, status int) *timexCollector {
	t.Helper()
	c, err := newTimexCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	tc := c.(*timexCollector)
	tc.adjtimex = func(out *unix.Timex) (int, error) {
		*out = *timex
		return status, nil
	}
	return tc
}

func newTimeFixtureCollector(t *testing.T) *timeCollector {
	t.Helper()
	c, err := newTimeCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	return c.(*timeCollector)
}

// gatherUnlabelledMixed collects unlabelled metrics of either type.
func gatherUnlabelledMixed(t *testing.T, c Collector) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 128)
	require.NoError(t, c.Update(ch))
	close(ch)
	return drainMixed(t, ch)
}

func gatherTimeMetrics(t *testing.T, c Collector) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)
	return drainMixed(t, ch)
}

// drainMixed reads a closed channel of gauges, counters or UNTYPED metrics.
//
// The untyped case is not hypothetical: netstat emits UntypedValue throughout, and a
// textfile *.prom line with no "# TYPE" is parsed as untyped too. An earlier version
// handled only gauges and counters, so every untyped metric silently read as 0 -- which
// presented as "the collector dropped the value" and sent me looking at the collector.
//
// THIRD instance of this helper-bug family (gatherAllLabels read counters only;
// gatherLabelled likewise). A test helper that returns a plausible zero for an entire
// metric type is worse than one that panics.
func drainMixed(t *testing.T, ch chan prometheus.Metric) map[string]float64 {
	t.Helper()

	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		out[metricName(t, m)] = metricValueOf(t, &pb)
	}
	return out
}

// metricValueOf extracts the value regardless of metric type.
//
// Fails the test on a type it does not understand, rather than returning 0: a silent
// zero is the bug this function exists to prevent.
func metricValueOf(t *testing.T, pb *dto.Metric) float64 {
	t.Helper()

	switch {
	case pb.Gauge != nil:
		return pb.GetGauge().GetValue()
	case pb.Counter != nil:
		return pb.GetCounter().GetValue()
	case pb.Untyped != nil:
		return pb.GetUntyped().GetValue()
	}
	t.Fatalf("metric has no gauge, counter or untyped value: %v", pb)
	return 0
}
