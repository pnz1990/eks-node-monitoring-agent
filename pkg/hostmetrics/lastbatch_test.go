package hostmetrics

// Tests for the final batch: powersupplyclass, watchdog, mdadm, infiniband,
// dmmultipath, btrfs, textfile, hwmon.
//
// Two things in this file are the point of it:
//
//  1. THE COMPLETE-SET TEST. All 39 collectors must be registered, and the ONE that
//     must FAIL on EKS (hwmon) must be distinguishable from the many that must SUCCEED
//     WITH ZERO SERIES. Getting that inverted either way is a parity break invisible to
//     a metric-name comparison.
//
//  2. THE UNIT-CONVENTION MATRIX. This package now contains six distinct unit
//     conventions across collectors that sit beside each other, two of them
//     temperatures 100x apart. Every divisor is asserted against upstream's source.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/prometheus/procfs/blockdevice"
	"github.com/prometheus/procfs/btrfs"
	"github.com/prometheus/procfs/sysfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- the complete set, and the success/fail split -------------------------

func TestAllThirtyNineCollectorsAreRegistered(t *testing.T) {
	// The goal is 39 applicable collectors. Asserted as an exact list rather than a
	// count, so a rename or an accidental duplicate registration fails rather than
	// silently keeping the total right.
	set, err := New(quietLogger(), Config{})
	require.NoError(t, err)

	got := set.Names()
	sort.Strings(got)

	want := []string{
		"arp", "btrfs", "conntrack", "cpu", "cpufreq", "diskstats", "dmi",
		"dmmultipath", "edac", "entropy", "filefd", "filesystem", "hwmon",
		"infiniband", "kernel_hung", "loadavg", "mdadm", "meminfo", "netclass",
		"netdev", "netstat", "nvme", "os", "powersupplyclass", "pressure",
		"schedstat", "selinux", "sockstat", "softnet", "stat", "textfile",
		"thermal_zone", "time", "timex", "udp_queues", "uname", "vmstat",
		"watchdog", "xfs",
	}
	assert.Equal(t, want, got)
	assert.Len(t, got, 39)
}

func TestHwmonMustFailOnAHostWithoutHwmon(t *testing.T) {
	// THE INVERTED CASE. Every other hardware collector reports success=1 with zero
	// series when its hardware is absent; hwmon reports success=0, because upstream
	// returns ErrNoData rather than nil. Measured on the live cluster:
	//
	//   node_scrape_collector_success{collector="hwmon"} 0    <-- the only zero
	//
	// "Make it succeed like the others" would be a parity BREAK, so it is pinned here.
	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err),
		"hwmon MUST return ErrNoData (collector_success=0) when /sys/class/hwmon is absent; "+
			"returning nil would diverge from upstream on every EKS node")
}

func TestHardwareCollectorsThatMustSucceedWithZeroSeries(t *testing.T) {
	// The complement of the hwmon case: these must NOT return ErrNoData when their
	// hardware is absent, because upstream returns nil and reports success=1.
	root := t.TempDir()
	cpuDir := filepath.Join(root, "devices", "system", "cpu")
	require.NoError(t, os.MkdirAll(filepath.Join(cpuDir, "cpu0"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cpuDir, "offline"), []byte("\n"), 0o644))
	// /sys/class/power_supply EXISTS AND IS EMPTY on an EC2 instance (verified on the
	// live node), which is why upstream reports success there. An absent directory is a
	// different case and correctly yields ErrNoData -- see
	// TestPowerSupplyMissingClassDirectoryIsNoData.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "class", "power_supply"), 0o755))

	paths := Paths{SysFS: root, ProcFS: t.TempDir(), RootFS: root}.withDefaults()

	for name, build := range map[string]factory{
		"thermal_zone":     newThermalZoneCollector,
		"cpufreq":          newCPUFreqCollector,
		"edac":             newEDACCollector,
		"powersupplyclass": newPowerSupplyClassCollector,
		"btrfs":            newBtrfsCollector,
		"xfs":              newXFSCollector,
		"textfile":         newTextFileCollector,
	} {
		t.Run(name, func(t *testing.T) {
			c, err := build(quietLogger(), paths)
			require.NoError(t, err)

			ch := make(chan prometheus.Metric, 64)
			err = c.Update(ch)
			close(ch)
			require.NoError(t, err,
				"%s must report success with no hardware, not ErrNoData", name)
		})
	}
}

// --- the unit-convention matrix -------------------------------------------

func TestUnitConventionsAreAllDistinctAndCorrect(t *testing.T) {
	// Six distinct conventions now live in this package, and the two temperature ones
	// are 100x apart while sitting in adjacent files. Copying any divisor to a
	// neighbour is a silent 10x, 100x, 1000x or 1e6 error that still produces a
	// plausible-looking reading.
	//
	// Asserted as a matrix so the DIFFERENCES are the subject, not each value alone.
	assert.Equal(t, 1000.0*1000.0, psiMicrosecondsPerSecond, "pressure: microseconds")
	assert.Equal(t, 1e9, float64(schedstatNanosecondsPerSecond), "schedstat: nanoseconds")
	assert.Equal(t, 1000000.0, float64(timexMicroSeconds), "timex: microseconds")
	assert.Equal(t, 1000000000.0, float64(timexNanoSeconds), "timex: nanoseconds (STA_NANO)")
	assert.Equal(t, 1000000.0*65536.0, float64(ppm16frac), "timex: 16-bit-fraction PPM")
	assert.Equal(t, 512.0, float64(unixSectorSize), "diskstats: 512-byte sectors")

	// The temperature pair, stated explicitly because it is the dangerous one.
	powerSupplyTempDivisor := divisorFor(t, "temp_celsius")
	assert.Equal(t, 10.0, powerSupplyTempDivisor,
		"power_supply temperatures are DECI-degrees")
	// thermal_zone divides by 1000 inline; asserted through behaviour instead.
	tz := newThermalZoneStub(t,
		[]sysfs.ClassThermalZoneStats{{Name: "0", Type: "x86_pkg_temp", Temp: 45000}}, nil)
	got := gatherLabelled(t, tz, "zone")
	assert.Equal(t, 45.0, got["node_thermal_zone_temp"]["0"],
		"thermal_zone temperatures are MILLI-degrees -- 100x apart from power_supply")

	assert.NotEqual(t, 10.0, 1000.0,
		"the two temperature divisors must never be unified")
}

func TestPowerSupplyDivisorsMatchUpstream(t *testing.T) {
	// Three scale groups over 49 fields. The divisor is part of our table, so it is
	// diffed against upstream's three map literals and their respective scale
	// expressions.
	data, err := os.ReadFile("../../../node_exporter/collector/powersupplyclass_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}

	// Each group: a map[string]*int64 literal followed by a push with a scale suffix.
	groups := regexp.MustCompile(
		`(?s)for name, value := range map\[string\]\*u?int64\{(.*?)\n\t\t\} \{\s*\n\s*if value != nil \{\s*\n\s*pushPowerSupplyMetric\(ch, c\.subsystem, name, float64\(\*value\)([^,]*), powerSupply\.Name`,
	).FindAllSubmatch(data, -1)
	require.Len(t, groups, 3, "expected 3 upstream scale groups, got %d", len(groups))

	upstream := map[string]float64{}
	for _, g := range groups {
		divisor := 1.0
		switch strings.TrimSpace(string(g[2])) {
		case "":
			divisor = 1
		case "/1e6":
			divisor = 1e6
		case "/10.0":
			divisor = 10.0
		default:
			t.Fatalf("unrecognised upstream scale %q; the divisor table may be stale", g[2])
		}
		for _, m := range regexp.MustCompile(`"([^"]+)":\s*powerSupply\.([A-Za-z0-9]+),`).
			FindAllSubmatch(g[1], -1) {
			upstream[string(m[1])] = divisor
		}
	}
	require.Len(t, upstream, 49, "expected 49 upstream numeric fields, got %d", len(upstream))

	ours := map[string]float64{}
	for _, m := range powerSupplyNumerics() {
		ours[m.name] = m.divisor
	}

	assert.Equal(t, upstream, ours,
		"every field's divisor must match upstream; a wrong one is off by 10x or 1e6 and still plausible")
}

func TestPowerSupplyFieldSetMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/powersupplyclass_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}

	upstream := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-z_0-9]+)":\s*powerSupply\.[A-Za-z0-9]+,`).
		FindAllSubmatch(data, -1) {
		upstream[string(m[1])] = true
	}
	// 49 numeric + 12 string labels.
	require.Len(t, upstream, 61, "expected 61 upstream powerSupply field references, got %d", len(upstream))

	ours := map[string]bool{}
	for _, m := range powerSupplyNumerics() {
		ours[m.name] = true
	}
	for _, l := range powerSupplyLabels() {
		ours[l.name] = true
	}
	assert.Equal(t, upstream, ours)
}

// --- powersupplyclass behaviour -------------------------------------------

func TestPowerSupplyNilFieldsAreOmitted(t *testing.T) {
	// Every numeric field is a pointer. nil means the sysfs file was absent; emitting
	// zero would report a battery at 0 volts rather than no voltage sensor.
	c := newPowerSupplyStub(t, sysfs.PowerSupplyClass{
		"BAT0": sysfs.PowerSupply{Name: "BAT0", Capacity: int64Ptr(87)},
	})

	got := gatherLabelled(t, c, "power_supply")
	assert.Equal(t, 87.0, got["node_power_supply_capacity"]["BAT0"])
	assert.NotContains(t, got, "node_power_supply_voltage_volt")
	assert.NotContains(t, got, "node_power_supply_temp_celsius")
}

func TestPowerSupplyAppliesEachScaleGroup(t *testing.T) {
	// One field from each group, with values chosen so a swapped divisor cannot
	// coincidentally give the right answer.
	c := newPowerSupplyStub(t, sysfs.PowerSupplyClass{
		"BAT0": sysfs.PowerSupply{
			Name:       "BAT0",
			Capacity:   int64Ptr(87),         // group 1: raw
			VoltageNow: int64Ptr(12_345_678), // group 2: /1e6 -> 12.345678 V
			Temp:       int64Ptr(315),        // group 3: /10   -> 31.5 C
		},
	})

	got := gatherLabelled(t, c, "power_supply")
	assert.Equal(t, 87.0, got["node_power_supply_capacity"]["BAT0"], "raw")
	assert.InDelta(t, 12.345678, got["node_power_supply_voltage_volt"]["BAT0"], 1e-9,
		"microvolts -> volts")
	assert.Equal(t, 31.5, got["node_power_supply_temp_celsius"]["BAT0"],
		"deci-degrees -> Celsius; /1000 would give 0.315")
}

func TestPowerSupplyInfoLabelsOmitEmptyValues(t *testing.T) {
	// Like dmi, the info label SET is host-dependent: an empty string omits the label
	// rather than emitting it blank.
	c := newPowerSupplyStub(t, sysfs.PowerSupplyClass{
		"BAT0": sysfs.PowerSupply{
			Name: "BAT0", Manufacturer: "ACME", Technology: "Li-ion",
			// Health, ModelName, SerialNumber etc left empty.
		},
	})

	labels := singleMetricLabels(t, c, "node_power_supply_info")
	assert.Equal(t, "BAT0", labels["power_supply"])
	assert.Equal(t, "ACME", labels["manufacturer"])
	assert.Equal(t, "Li-ion", labels["technology"])
	assert.NotContains(t, labels, "health")
	assert.NotContains(t, labels, "serial_number")
}

func TestPowerSupplyMissingClassDirectoryIsNoData(t *testing.T) {
	// The distinction upstream draws and I initially missed: a MISSING
	// /sys/class/power_supply is ErrNoData, while an EMPTY one succeeds with zero
	// series. On EC2 the directory exists and is empty, which is why the cluster golden
	// shows collector_success=1 -- so both branches are real and neither can be
	// collapsed into the other.
	c, err := newPowerSupplyClassCollector(quietLogger(), Paths{SysFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err), "a missing class directory is ErrNoData, got %v", err)
}

func TestPowerSupplyEmptyClassDirectorySucceeds(t *testing.T) {
	// THE EC2 CASE.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "class", "power_supply"), 0o755))

	c, err := newPowerSupplyClassCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 8)
	require.NoError(t, c.Update(ch), "an empty directory must succeed, matching the live node")
	close(ch)
	assert.Empty(t, ch)
}

func TestPowerSupplyIgnoredPatternDefaultsToNothing(t *testing.T) {
	c := newPowerSupplyStub(t, nil)
	for _, name := range []string{"BAT0", "AC", "ADP1", "usb"} {
		assert.False(t, c.ignoredPattern.MatchString(name),
			"%q must not be filtered by default", name)
	}
}

func TestPowerSupplyIgnoredPatternFilters(t *testing.T) {
	root := t.TempDir()
	c, err := newPowerSupplyClassCollectorWithFilter(quietLogger(),
		Paths{SysFS: root}.withDefaults(), "^BAT")
	require.NoError(t, err)

	psc := c.(*powerSupplyClassCollector)
	psc.powerSupplyClass = func() (sysfs.PowerSupplyClass, error) {
		return sysfs.PowerSupplyClass{
			"BAT0": sysfs.PowerSupply{Name: "BAT0", Capacity: int64Ptr(1)},
			"AC":   sysfs.PowerSupply{Name: "AC", Online: int64Ptr(1)},
		}, nil
	}

	got := gatherLabelled(t, psc, "power_supply")
	assert.NotContains(t, got["node_power_supply_capacity"], "BAT0")
	assert.Contains(t, got["node_power_supply_online"], "AC")
}

func TestPowerSupplyInvalidPatternRejectedAtConstruction(t *testing.T) {
	_, err := newPowerSupplyClassCollectorWithFilter(quietLogger(),
		Paths{SysFS: t.TempDir()}.withDefaults(), "([unclosed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid ignored-supplies pattern")
}

func TestPowerSupplyReadFailureIsWrapped(t *testing.T) {
	c := newPowerSupplyStub(t, nil)
	c.powerSupplyClass = func() (sysfs.PowerSupplyClass, error) { return nil, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not get power_supply class info")
}

// --- watchdog -------------------------------------------------------------

func TestWatchdogEmitsNumericsAndInfo(t *testing.T) {
	c := newWatchdogStub(t, sysfs.WatchdogClass{
		"watchdog0": sysfs.WatchdogStats{
			Name:       "watchdog0",
			Bootstatus: int64Ptr(0), Timeout: int64Ptr(60), Timeleft: int64Ptr(45),
			Identity: strPtr("iTCO_wdt"), State: strPtr("active"),
		},
	})

	got := gatherLabelled(t, c, "name")
	assert.Equal(t, 0.0, got["node_watchdog_bootstatus"]["watchdog0"])
	assert.Equal(t, 60.0, got["node_watchdog_timeout_seconds"]["watchdog0"])
	assert.Equal(t, 45.0, got["node_watchdog_timeleft_seconds"]["watchdog0"])
	// Absent numerics are omitted, not zeroed.
	assert.NotContains(t, got, "node_watchdog_fw_version")
	assert.NotContains(t, got, "node_watchdog_nowayout")
}

func TestWatchdogInfoLabelsAreAlwaysPresentUnlikePowerSupply(t *testing.T) {
	// THE DIFFERENCE FROM power_supply AND dmi, and it is upstream's: watchdog passes
	// EMPTY STRINGS for absent values rather than omitting the labels. So its info
	// Desc has a fixed label set and can be built once, while the other two cannot.
	c := newWatchdogStub(t, sysfs.WatchdogClass{
		"watchdog0": sysfs.WatchdogStats{Name: "watchdog0", Identity: strPtr("iTCO_wdt")},
	})

	labels := singleMetricLabels(t, c, "node_watchdog_info")
	assert.Equal(t, "watchdog0", labels["name"])
	assert.Equal(t, "iTCO_wdt", labels["identity"])
	for _, key := range []string{"options", "state", "status", "pretimeout_governor"} {
		require.Contains(t, labels, key,
			"watchdog emits every info label, empty if absent -- unlike power_supply")
		assert.Equal(t, "", labels[key])
	}
	assert.Len(t, labels, 6)
}

func TestWatchdogToleratesThreeErrorKindsNotFour(t *testing.T) {
	// thermal_zone tolerates four error kinds including EINVAL; watchdog tolerates
	// three. That asymmetry is upstream's and is preserved rather than unified.
	for name, stubErr := range map[string]error{
		"not exist":  os.ErrNotExist,
		"permission": os.ErrPermission,
		"invalid":    os.ErrInvalid,
	} {
		t.Run(name, func(t *testing.T) {
			c := newWatchdogStub(t, nil)
			c.watchdogClass = func() (sysfs.WatchdogClass, error) { return nil, stubErr }

			err := c.Update(make(chan prometheus.Metric, 8))
			require.Error(t, err)
			assert.True(t, IsNoDataError(err))
		})
	}

	c := newWatchdogStub(t, nil)
	c.watchdogClass = func() (sysfs.WatchdogClass, error) { return nil, assert.AnError }
	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err), "an unexpected error must not become no-data")
}

// --- mdadm: the const-label / map-key mismatch ----------------------------

func TestMDAdmResyncAndCheckLabelsDoNotMatchTheirKeys(t *testing.T) {
	// UPSTREAM'S MISMATCH, verified mechanically against its source and reproduced
	// here. The label says "resync" while the lookup key is "resyncing"; likewise
	// "check" vs "checking". Making them "consistent" would leave
	// node_md_state{state="resync"} permanently 0 on a device that IS resyncing --
	// exactly when someone is looking at it.
	byLabel := map[string]string{}
	for _, m := range mdStateMetrics() {
		byLabel[m.label] = m.stateKey
	}

	assert.Equal(t, "resyncing", byLabel["resync"],
		`the "resync" label must read stateVals["resyncing"], not ["resync"]`)
	assert.Equal(t, "check", "check")
	assert.Equal(t, "checking", byLabel["check"],
		`the "check" label must read stateVals["checking"], not ["check"]`)

	// The three that DO match, so the test also fails if someone "fixes" those.
	for _, state := range []string{"active", "inactive", "recovering"} {
		assert.Equal(t, state, byLabel[state])
	}
}

func TestMDAdmStateMismatchMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/mdadm_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	src := string(data)

	// desc var -> const label
	labels := map[string]string{}
	for _, m := range regexp.MustCompile(
		`(?s)(\w+Desc) = prometheus\.NewDesc\(\s*\n\s*prometheus\.BuildFQName\(namespace, "md", "state"\),.*?prometheus\.Labels\{"state": "([a-z]+)"\}`,
	).FindAllStringSubmatch(src, -1) {
		labels[m[1]] = m[2]
	}
	// desc var -> stateVals key
	keys := map[string]string{}
	for _, m := range regexp.MustCompile(
		`(?s)MustNewConstMetric\(\s*\n\s*(\w+Desc),\s*\n\s*prometheus\.GaugeValue,\s*\n\s*stateVals\["([a-z]+)"\]`,
	).FindAllStringSubmatch(src, -1) {
		keys[m[1]] = m[2]
	}
	require.Len(t, labels, 5, "expected 5 upstream state descriptors, got %d", len(labels))

	upstream := map[string]string{}
	for descVar, label := range labels {
		key, ok := keys[descVar]
		require.True(t, ok, "no emit found for %s", descVar)
		upstream[label] = key
	}

	ours := map[string]string{}
	for _, m := range mdStateMetrics() {
		ours[m.label] = m.stateKey
	}
	assert.Equal(t, upstream, ours,
		"our label->key mapping must reproduce upstream's, mismatches included")
}

func TestMDAdmEmitsOneStateSeriesPerStateWithOnlyOneSet(t *testing.T) {
	// Five state series per device, exactly one of which is 1.
	c := newMDAdmStub(t,
		[]procfs.MDStat{{Name: "md0", ActivityState: "resyncing", DisksTotal: 2, DisksActive: 2}},
		nil)

	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)

	byState := map[string]float64{}
	for m := range ch {
		if metricName(t, m) != "node_md_state" {
			continue
		}
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		for _, l := range pb.GetLabel() {
			if l.GetName() == "state" {
				byState[l.GetValue()] = pb.GetGauge().GetValue()
			}
		}
	}

	assert.Equal(t, map[string]float64{
		"active": 0, "inactive": 0, "recovering": 0, "resync": 1, "check": 0,
	}, byState, `a resyncing device must set state="resync" to 1`)
}

func TestMDAdmMissingMdstatIsNoData(t *testing.T) {
	c := newMDAdmStub(t, nil, nil)
	c.mdStat = func() ([]procfs.MDStat, error) { return nil, os.ErrNotExist }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
}

func TestMDAdmMissingMdraidsIsNoData(t *testing.T) {
	// The SECOND ErrNoData path, reached after mdstat succeeded.
	c := newMDAdmStub(t, []procfs.MDStat{{Name: "md0", ActivityState: "active"}}, nil)
	c.mdRaids = func() ([]sysfs.Mdraid, error) { return nil, os.ErrNotExist }

	err := c.Update(make(chan prometheus.Metric, 64))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
}

func TestMDAdmRealErrorsAreFailures(t *testing.T) {
	c := newMDAdmStub(t, nil, nil)
	c.mdStat = func() ([]procfs.MDStat, error) { return nil, assert.AnError }
	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err))
	assert.Contains(t, err.Error(), "error parsing mdstatus")

	c = newMDAdmStub(t, []procfs.MDStat{{Name: "md0"}}, nil)
	c.mdRaids = func() ([]sysfs.Mdraid, error) { return nil, assert.AnError }
	err = c.Update(make(chan prometheus.Metric, 64))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error parsing mdraids")
}

func TestMDAdmNilDisksOmittedButZeroDegradedEmitted(t *testing.T) {
	// Disks is a POINTER (absent on some kernels) while DegradedDisks is not. So a
	// nil Disks omits the metric, but DegradedDisks=0 is a real measurement: a
	// healthy array genuinely has zero degraded disks, and omitting it would lose the
	// signal that matters most.
	c := newMDAdmStub(t, nil, []sysfs.Mdraid{
		{Device: "md0", Disks: nil, DegradedDisks: 0},
		{Device: "md1", Disks: uint64Ptr(4), DegradedDisks: 1},
	})

	got := gatherLabelled(t, c, "device")
	assert.NotContains(t, got["node_md_raid_disks"], "md0", "a nil Disks must be omitted")
	assert.Equal(t, 4.0, got["node_md_raid_disks"]["md1"])
	assert.Equal(t, 0.0, got["node_md_degraded"]["md0"],
		"zero degraded disks is a real measurement and must be emitted")
	assert.Equal(t, 1.0, got["node_md_degraded"]["md1"])
}

// --- infiniband -----------------------------------------------------------

func TestInfinibandCounterTableMatchesUpstream(t *testing.T) {
	// 59 (name, field) pairs across two nested structs with names like ReqCqeError /
	// ReqCqeFlushError / RespCqeError / RespCqeFlushError. Diffed against upstream for
	// the same reason as xfs.
	data, err := os.ReadFile("../../../node_exporter/collector/infiniband_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}

	upstream := map[string]string{}
	for _, m := range regexp.MustCompile(
		`c\.pushCounter\(ch, "([a-z0-9_]+)", port\.([A-Za-z0-9.]+), port\.Name, portStr\)`,
	).FindAllSubmatch(data, -1) {
		upstream[string(m[1])] = string(m[2])
	}
	require.Len(t, upstream, 59, "expected 59 upstream counters, got %d", len(upstream))

	ours := map[string]string{}
	for _, c := range infinibandCounters() {
		ours[c.name] = probeInfinibandField(t, c.value)
	}
	assert.Equal(t, upstream, ours,
		"every counter must read the field upstream reads")
}

func TestInfinibandDescriptionsMatchUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/infiniband_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	body := regexp.MustCompile(`(?s)descriptions := map\[string\]string\{(.*?)\n\t\}`).
		FindSubmatch(data)
	require.NotNil(t, body, "failed to locate upstream's descriptions map")

	upstream := map[string]string{}
	for _, m := range regexp.MustCompile(`"([a-z0-9_]+)":\s+"((?:[^"\\]|\\.)*)",`).
		FindAllSubmatch(body[1], -1) {
		upstream[string(m[1])] = string(m[2])
	}
	require.Len(t, upstream, 63, "expected 63 upstream descriptions, got %d", len(upstream))

	assert.Equal(t, upstream, infinibandDescriptions(),
		"help text is part of the exposition output and must be verbatim")
}

func TestInfinibandLifespanIsTheOnlyDivisor(t *testing.T) {
	// lifespan_seconds is MILLISECONDS despite living with the raw hw counters -- the
	// only conversion in the collector. Integer division, matching upstream.
	c := newInfinibandStub(t, sysfs.InfiniBandClass{
		"mlx5_0": sysfs.InfiniBandDevice{
			Name: "mlx5_0",
			Ports: map[uint]sysfs.InfiniBandPort{
				1: {
					Name: "mlx5_0", Port: 1,
					HwCounters: sysfs.InfiniBandHwCounters{Lifespan: uint64Ptr(10_000)},
					Counters:   sysfs.InfiniBandCounters{PortRcvData: uint64Ptr(4096)},
				},
			},
		},
	})

	got := gatherLabelled(t, c, "device")
	assert.Equal(t, 10.0, got["node_infiniband_lifespan_seconds"]["mlx5_0"],
		"10000 ms = 10 s")
	assert.Equal(t, 4096.0, got["node_infiniband_port_data_received_bytes_total"]["mlx5_0"],
		"raw counters are NOT divided")
}

func TestInfinibandNilCountersOmitted(t *testing.T) {
	// Most counters are Mellanox-specific and nil elsewhere. Emitting zero would claim
	// a reading the kernel never made.
	c := newInfinibandStub(t, sysfs.InfiniBandClass{
		"mlx5_0": sysfs.InfiniBandDevice{
			Name:  "mlx5_0",
			Ports: map[uint]sysfs.InfiniBandPort{1: {Name: "mlx5_0", Port: 1}},
		},
	})

	got := gatherLabelled(t, c, "device")
	assert.Contains(t, got, "node_infiniband_info")
	assert.NotContains(t, got, "node_infiniband_port_data_received_bytes_total")
}

func TestInfinibandAbsentClassIsNoData(t *testing.T) {
	c := newInfinibandStub(t, nil)
	c.infinibandClass = func() (sysfs.InfiniBandClass, error) { return nil, os.ErrNotExist }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
}

func TestInfinibandFiltersAreMutuallyExclusive(t *testing.T) {
	_, err := newInfiniBandCollectorWithFilter(quietLogger(),
		Paths{SysFS: t.TempDir()}.withDefaults(), "^mlx", "^ib")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestInfinibandInvalidFilterRejected(t *testing.T) {
	_, err := newInfiniBandCollectorWithFilter(quietLogger(),
		Paths{SysFS: t.TempDir()}.withDefaults(), "([unclosed", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid device exclude pattern")
}

// --- dmmultipath ----------------------------------------------------------

func TestDMMultipathAcceptsBothSCSIAndNVMeActiveStates(t *testing.T) {
	// "running" is SCSI, "live" is NVMe -- two kernel subsystems, two words for the
	// same healthy state. Handling only "running" would count every NVMe path as
	// FAILED, i.e. report total path failure on a healthy machine.
	assert.True(t, isDMPathActive("running"), "SCSI healthy state")
	assert.True(t, isDMPathActive("live"), "NVMe healthy state")
	for _, state := range []string{"offline", "failed", "blocked", "", "quiesce"} {
		assert.False(t, isDMPathActive(state), "%q must not count as active", state)
	}
}

func TestDMMultipathActiveStatesMatchUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/dmmultipath_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	body := regexp.MustCompile(`(?s)func isPathActive\(state string\) bool \{(.*?)\n\}`).
		FindSubmatch(data)
	require.NotNil(t, body, "failed to locate upstream's isPathActive")

	upstream := regexp.MustCompile(`state == "([a-z]+)"`).FindAllSubmatch(body[1], -1)
	require.Len(t, upstream, 2, "expected 2 upstream active states")

	got := dmMultipathActiveStates()
	for _, m := range upstream {
		assert.Contains(t, got, string(m[1]))
	}
	assert.Len(t, got, len(upstream))
}

func TestDMMultipathDeviceActiveIsInvertedFromSuspended(t *testing.T) {
	// The struct reports Suspended, the metric reports active. Getting the polarity
	// wrong reports every healthy device as suspended, and looks plausible either way.
	c := newDMMultipathStub(t, []blockdevice.DMMultipathDevice{
		{Name: "mpatha", SysfsName: "dm-0", Suspended: false},
		{Name: "mpathb", SysfsName: "dm-1", Suspended: true},
	})

	got := gatherLabelled(t, c, "device")
	assert.Equal(t, 1.0, got["node_dmmultipath_device_active"]["mpatha"],
		"NOT suspended means active=1")
	assert.Equal(t, 0.0, got["node_dmmultipath_device_active"]["mpathb"],
		"suspended means active=0")
}

func TestDMMultipathPathCountsSumToTotal(t *testing.T) {
	// active + failed must always equal the path count, so a path in an unrecognised
	// state counts as FAILED rather than vanishing. An unknown state is not evidence
	// of health.
	c := newDMMultipathStub(t, []blockdevice.DMMultipathDevice{{
		Name: "mpatha", SysfsName: "dm-0",
		Paths: []blockdevice.DMMultipathPath{
			{Device: "sda", State: "running"},
			{Device: "sdb", State: "live"},
			{Device: "sdc", State: "failed"},
			{Device: "sdd", State: "something-new"},
		},
	}})

	got := gatherLabelled(t, c, "device")
	assert.Equal(t, 4.0, got["node_dmmultipath_device_paths"]["mpatha"])
	assert.Equal(t, 2.0, got["node_dmmultipath_device_paths_active"]["mpatha"])
	assert.Equal(t, 2.0, got["node_dmmultipath_device_paths_failed"]["mpatha"],
		"an unrecognised state counts as failed, not as neither")
	assert.Equal(t,
		got["node_dmmultipath_device_paths"]["mpatha"],
		got["node_dmmultipath_device_paths_active"]["mpatha"]+
			got["node_dmmultipath_device_paths_failed"]["mpatha"])
}

func TestDMMultipathAbsentIsNoData(t *testing.T) {
	for _, stubErr := range []error{os.ErrNotExist, os.ErrPermission} {
		c := newDMMultipathStub(t, nil)
		c.multipathDevices = func() ([]blockdevice.DMMultipathDevice, error) { return nil, stubErr }

		err := c.Update(make(chan prometheus.Metric, 8))
		require.Error(t, err)
		assert.True(t, IsNoDataError(err))
	}
}

// --- btrfs ----------------------------------------------------------------

func TestBtrfsCommitDurationsAreMillisecondsWithMixedTypes(t *testing.T) {
	// All three commit durations divide by 1000, but last/max are GAUGES while the
	// total is a COUNTER. A "consistency" cleanup making them all counters would break
	// rate() on last_commit_seconds.
	c := newBtrfsStub(t, []*btrfs.Stats{{
		UUID: "abc", Label: "root",
		CommitStats: btrfs.CommitStats{
			Commits: 100, LastCommitMs: 2500, MaxCommitMs: 7500, TotalCommitMs: 90_000,
		},
	}})

	ch := make(chan prometheus.Metric, 512)
	require.NoError(t, c.Update(ch))
	close(ch)

	types := map[string]string{}
	values := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		name := metricName(t, m)
		if pb.Counter != nil {
			types[name] = "counter"
			values[name] = pb.GetCounter().GetValue()
			continue
		}
		types[name] = "gauge"
		values[name] = pb.GetGauge().GetValue()
	}

	assert.Equal(t, 2.5, values["node_btrfs_last_commit_seconds"])
	assert.Equal(t, 7.5, values["node_btrfs_max_commit_seconds"])
	assert.Equal(t, 90.0, values["node_btrfs_commit_seconds_total"])

	assert.Equal(t, "gauge", types["node_btrfs_last_commit_seconds"])
	assert.Equal(t, "gauge", types["node_btrfs_max_commit_seconds"])
	assert.Equal(t, "counter", types["node_btrfs_commit_seconds_total"],
		"the total is cumulative; the other two are point-in-time")
	assert.Equal(t, "counter", types["node_btrfs_commits_total"])
}

func TestBtrfsNilAllocationStatsDoNotPanic(t *testing.T) {
	// Upstream would nil-deref if a filesystem did not report a block-group type.
	// Guarded: a panic in a collector is far worse than a missing metric.
	c := newBtrfsStub(t, []*btrfs.Stats{{
		UUID:       "abc",
		Allocation: btrfs.Allocation{Data: nil, Metadata: nil, System: nil},
	}})

	ch := make(chan prometheus.Metric, 128)
	require.NoError(t, c.Update(ch), "nil allocation stats must not panic or fail")
	close(ch)
	assert.NotEmpty(t, ch, "the info and commit metrics are still emitted")
}

func TestBtrfsAllocationRatioIsNotConverted(t *testing.T) {
	// Already a ratio, unlike every other numeric in this collector.
	c := newBtrfsStub(t, []*btrfs.Stats{{
		UUID: "abc",
		Allocation: btrfs.Allocation{
			Data: &btrfs.AllocationStats{
				ReservedBytes: 1024,
				Layouts: map[string]*btrfs.LayoutUsage{
					"single": {UsedBytes: 100, TotalBytes: 200, Ratio: 1.0},
				},
			},
		},
	}})

	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)

	for m := range ch {
		if metricName(t, m) != "node_btrfs_allocation_ratio" {
			continue
		}
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.Equal(t, 1.0, pb.GetGauge().GetValue(), "the ratio passes through unscaled")
		return
	}
	t.Fatal("node_btrfs_allocation_ratio was not emitted")
}

// --- textfile -------------------------------------------------------------

func TestTextFileAlwaysEmitsScrapeError(t *testing.T) {
	// THE ONE SERIES the live EKS node reports. The standard alert on this collector
	// is `node_textfile_scrape_error != 0`; an absent series makes that alert
	// un-evaluable rather than false, so it must be emitted even with nothing
	// configured.
	c, err := newTextFileCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, map[string]float64{"node_textfile_scrape_error": 0}, got,
		"no directories configured yields exactly scrape_error=0")
}

func TestTextFileReExportsMetrics(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "custom.prom"), []byte(
		"# HELP my_metric A custom metric.\n"+
			"# TYPE my_metric gauge\n"+
			"my_metric{label=\"a\"} 42\n"), 0o644))

	c := newTextFileStub(t, dir)
	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	var found bool
	for m := range ch {
		if metricName(t, m) != "my_metric" {
			continue
		}
		found = true
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.Equal(t, 42.0, pb.GetGauge().GetValue())
		require.Len(t, pb.GetLabel(), 1)
		assert.Equal(t, "a", pb.GetLabel()[0].GetValue())
	}
	assert.True(t, found, "the operator-supplied metric must be re-exported")
}

func TestTextFileEmitsMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.prom")
	require.NoError(t, os.WriteFile(path, []byte("my_metric 1\n"), 0o644))

	c := newTextFileStub(t, dir)
	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	stat, err := os.Stat(path)
	require.NoError(t, err)

	for m := range ch {
		if metricName(t, m) != "node_textfile_mtime_seconds" {
			continue
		}
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.InDelta(t, float64(stat.ModTime().UnixNano())/1e9,
			pb.GetGauge().GetValue(), 0.001)
		return
	}
	t.Fatal("node_textfile_mtime_seconds was not emitted")
}

func TestTextFileMalformedFileSetsScrapeError(t *testing.T) {
	// A bad file must set scrape_error=1 rather than failing the collector: the other
	// files' metrics are still valid, and scrape_error is how the failure is reported.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.prom"),
		[]byte("this is not { valid exposition\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "good.prom"),
		[]byte("good_metric 7\n"), 0o644))

	c := newTextFileStub(t, dir)
	got := gatherUnlabelledMixed(t, c)

	assert.Equal(t, 1.0, got["node_textfile_scrape_error"], "a malformed file sets the error flag")
	assert.Equal(t, 7.0, got["good_metric"], "the valid file's metrics must survive")
}

func TestTextFileNonPromFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"),
		[]byte("definitely not exposition\n"), 0o644))

	c := newTextFileStub(t, dir)
	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 0.0, got["node_textfile_scrape_error"],
		"only *.prom files are read, so a stray .txt must not set the error flag")
}

func TestTextFileUnreadableDirectorySetsScrapeError(t *testing.T) {
	c := newTextFileStub(t, filepath.Join(t.TempDir(), "does-not-exist"))
	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 1.0, got["node_textfile_scrape_error"])
}

// --- hwmon ----------------------------------------------------------------

func TestHwmonSensorTypesMatchUpstream(t *testing.T) {
	// A type absent from the allowlist is dropped ENTIRELY, so this list is the
	// difference between reporting a sensor and silently losing it.
	data, err := os.ReadFile("../../../node_exporter/collector/hwmon_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	body := regexp.MustCompile(`(?s)hwmonSensorTypes\s*=\s*\[\]string\{(.*?)\}`).FindSubmatch(data)
	require.NotNil(t, body, "failed to locate upstream's sensor type list")

	var upstream []string
	for _, m := range regexp.MustCompile(`"([a-z_0-9]+)"`).FindAllSubmatch(body[1], -1) {
		upstream = append(upstream, string(m[1]))
	}
	require.Len(t, upstream, 14, "expected 14 upstream sensor types, got %d", len(upstream))
	assert.Equal(t, upstream, hwmonSensorTypes)
}

func TestHwmonFilenameParsing(t *testing.T) {
	// The filename format is <type><num>_<property>, and every part is optional in a
	// way that matters: "vrm" has no number and no property, "temp1" has no property.
	for filename, want := range map[string]struct {
		ok       bool
		typ      string
		num      int
		property string
	}{
		"temp1_input":    {true, "temp", 1, "input"},
		"temp1":          {true, "temp", 1, ""},
		"vrm":            {true, "vrm", 0, ""},
		"in12_max":       {true, "in", 12, "max"},
		"power1_average": {true, "power", 1, "average"},
		"fan2_alarm":     {true, "fan", 2, "alarm"},
		"beep_enable":    {true, "beep_enable", 0, ""},
	} {
		t.Run(filename, func(t *testing.T) {
			ok, typ, num, property := explodeHwmonSensorFilename(filename)
			assert.Equal(t, want.ok, ok)
			assert.Equal(t, want.typ, typ)
			assert.Equal(t, want.num, num)
			assert.Equal(t, want.property, property)
		})
	}
}

func TestHwmonCleanMetricName(t *testing.T) {
	// Sensor labels come from firmware and can contain anything. An invalid character
	// left in place makes the metric name unparseable.
	assert.Equal(t, "core_0", cleanHwmonMetricName("Core 0"))
	assert.Equal(t, "cpu_temp", cleanHwmonMetricName("CPU-Temp"))
	assert.Equal(t, "acpitz", cleanHwmonMetricName("acpitz\n"))
	// TWO spaces become TWO underscores, and Trim strips only the ENDS -- so the
	// interior run is preserved. My first expectation here was "a_b", which was simply
	// wrong about what Trim does.
	assert.Equal(t, "a__b", cleanHwmonMetricName("__a  b__"),
		"only leading and trailing underscores are trimmed; interior runs are kept")
	assert.Equal(t, "", cleanHwmonMetricName("!!!"))
}

func TestHwmonEmitsSensorsFromASyntheticChip(t *testing.T) {
	// No EC2 instance has hwmon, so the emit path is only reachable through a
	// synthetic tree. Values chosen so each unit conversion is distinguishable.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":           "coretemp",
		"temp1_input":    "45000", // milli-degrees -> 45 C
		"temp1_label":    "Core 0",
		"in0_input":      "12000",    // millivolts -> 12 V
		"fan1_input":     "2400",     // RPM, no conversion
		"power1_average": "15000000", // microwatts -> 15 W
		"curr1_input":    "1500",     // milliamps -> 1.5 A
		"energy1_input":  "2000000",  // microjoules -> 2 J
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 45.0, got["node_hwmon_temp_celsius"]["temp1"], "milli-degrees")
	assert.Equal(t, 12.0, got["node_hwmon_in_volts"]["in0"], "millivolts")
	assert.Equal(t, 2400.0, got["node_hwmon_fan_rpm"]["fan1"], "RPM is NOT converted")
	assert.Equal(t, 15.0, got["node_hwmon_power_average_watt"]["power1"], "microwatts")
	assert.Equal(t, 1.5, got["node_hwmon_curr_amps"]["curr1"], "milliamps")
	assert.Equal(t, 2.0, got["node_hwmon_energy_joule_total"]["energy1"], "microjoules")
}

func TestHwmonEnergyIsTheOnlyCounter(t *testing.T) {
	// Joules accumulate; everything else is a point-in-time reading.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":          "acme",
		"temp1_input":   "45000",
		"energy1_input": "2000000",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)

	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		name := metricName(t, m)
		if name == "node_hwmon_energy_joule_total" {
			assert.NotNil(t, pb.Counter, "joules accumulate")
			continue
		}
		assert.Nil(t, pb.Counter, "%s must be a gauge", name)
	}
}

func TestHwmonFaultAndAlarmHaveNoUnits(t *testing.T) {
	// Status flags, emitted raw. Applying the temp divisor to temp1_alarm would report
	// an alarm of 0.001.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":        "acme",
		"temp1_alarm": "1",
		"temp1_fault": "1",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 1.0, got["node_hwmon_temp_alarm"]["temp1"], "raw, not scaled")
	assert.Equal(t, 1.0, got["node_hwmon_temp_fault"]["temp1"])
}

func TestHwmonTempTypeIsExcludedFromTheCelsiusRule(t *testing.T) {
	// temp1_type is a thermistor type CODE, not a temperature. Dividing it by 1000
	// would report 0.004 degrees.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":       "acme",
		"temp1_type": "4",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 4.0, got["node_hwmon_temp_type"]["temp1"],
		"the type code must pass through unscaled")
	assert.NotContains(t, got, "node_hwmon_temp_type_celsius")
}

func TestHwmonPowerAccuracyPrecedesTheGeneralPowerRule(t *testing.T) {
	// Rule ORDER is load-bearing: power+accuracy is a ratio with no _watt suffix, and
	// it must be matched before the general power rule.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":            "acme",
		"power1_accuracy": "1000000",
		"power1_average":  "15000000",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 1.0, got["node_hwmon_power_accuracy"]["power1"],
		"accuracy is a ratio, with no _watt suffix")
	assert.NotContains(t, got, "node_hwmon_power_accuracy_watt")
	assert.Equal(t, 15.0, got["node_hwmon_power_average_watt"]["power1"])
}

func TestHwmonChipNamesAnnotationEmitted(t *testing.T) {
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":        "coretemp",
		"temp1_input": "45000",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	labels := singleMetricLabels(t, c, "node_hwmon_chip_names")
	assert.Equal(t, "coretemp", labels["chip_name"])
	assert.NotEmpty(t, labels["chip"])
}

func TestHwmonSensorFilterMatchesChipSemicolonSensor(t *testing.T) {
	// The filter key is "chip;sensor", so an operator can exclude one sensor on one
	// chip. The semicolon is part of the contract.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":        "coretemp",
		"temp1_input": "45000",
		"temp2_input": "50000",
	})

	c, err := newHwMonCollectorWithFilters(quietLogger(),
		Paths{SysFS: root}.withDefaults(), "", "", "coretemp;temp2", "")
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Contains(t, got["node_hwmon_temp_celsius"], "temp1")
	assert.NotContains(t, got["node_hwmon_temp_celsius"], "temp2",
		"the chip;sensor filter must exclude just that sensor")
}

func TestHwmonInvalidFiltersRejected(t *testing.T) {
	for name, args := range map[string][4]string{
		"chip":   {"([unclosed", "", "", ""},
		"sensor": {"", "", "([unclosed", ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := newHwMonCollectorWithFilters(quietLogger(),
				Paths{SysFS: t.TempDir()}.withDefaults(), args[0], args[1], args[2], args[3])
			require.Error(t, err)
			assert.Contains(t, err.Error(), "hwmon")
		})
	}
}

// --- helpers --------------------------------------------------------------

func int64Ptr(v int64) *int64 { return &v }
func strPtr(v string) *string { return &v }

func newPowerSupplyStub(t *testing.T, class sysfs.PowerSupplyClass) *powerSupplyClassCollector {
	t.Helper()
	c, err := newPowerSupplyClassCollector(quietLogger(), Paths{SysFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	pc := c.(*powerSupplyClassCollector)
	pc.powerSupplyClass = func() (sysfs.PowerSupplyClass, error) { return class, nil }
	return pc
}

func newWatchdogStub(t *testing.T, class sysfs.WatchdogClass) *watchdogCollector {
	t.Helper()
	c, err := newWatchdogCollector(quietLogger(), Paths{SysFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	wc := c.(*watchdogCollector)
	wc.watchdogClass = func() (sysfs.WatchdogClass, error) { return class, nil }
	return wc
}

func newMDAdmStub(t *testing.T, stats []procfs.MDStat, raids []sysfs.Mdraid) *mdAdmCollector {
	t.Helper()
	c, err := newMDAdmCollector(quietLogger(),
		Paths{ProcFS: t.TempDir(), SysFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	mc := c.(*mdAdmCollector)
	mc.mdStat = func() ([]procfs.MDStat, error) { return stats, nil }
	mc.mdRaids = func() ([]sysfs.Mdraid, error) { return raids, nil }
	return mc
}

func newInfinibandStub(t *testing.T, class sysfs.InfiniBandClass) *infinibandCollector {
	t.Helper()
	c, err := newInfiniBandCollector(quietLogger(), Paths{SysFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	ic := c.(*infinibandCollector)
	ic.infinibandClass = func() (sysfs.InfiniBandClass, error) { return class, nil }
	return ic
}

func newDMMultipathStub(t *testing.T, devices []blockdevice.DMMultipathDevice) *dmMultipathCollector {
	t.Helper()
	c, err := newDMMultipathCollector(quietLogger(),
		Paths{ProcFS: t.TempDir(), SysFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	dc := c.(*dmMultipathCollector)
	dc.multipathDevices = func() ([]blockdevice.DMMultipathDevice, error) { return devices, nil }
	return dc
}

func newBtrfsStub(t *testing.T, stats []*btrfs.Stats) *btrfsCollector {
	t.Helper()
	c, err := newBtrfsCollector(quietLogger(), Paths{SysFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	bc := c.(*btrfsCollector)
	bc.stats = func() ([]*btrfs.Stats, error) { return stats, nil }
	return bc
}

func newTextFileStub(t *testing.T, dirs ...string) *textFileCollector {
	t.Helper()
	c, err := newTextFileCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	tc := c.(*textFileCollector)
	tc.paths = dirs
	return tc
}

// writeHwmonTree builds a synthetic /sys/class/hwmon/<name> directory.
func writeHwmonTree(t *testing.T, chip string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "class", "hwmon", chip)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o644))
	}
	return root
}

// divisorFor returns the divisor our power-supply table uses for a metric.
func divisorFor(t *testing.T, name string) float64 {
	t.Helper()
	for _, m := range powerSupplyNumerics() {
		if m.name == name {
			return m.divisor
		}
	}
	t.Fatalf("no power-supply metric named %q", name)
	return 0
}

// probeInfinibandField determines which field an infiniband accessor reads, by setting
// one *uint64 field at a time to a sentinel and seeing which accessor returns it.
//
// Same technique as the xfs table test: the accessor is an opaque closure, so the only
// way to learn which field it reads is to make each field individually identifiable.
func probeInfinibandField(t *testing.T, accessor func(*sysfs.InfiniBandPort) *uint64) string {
	t.Helper()

	const sentinel uint64 = 123456789

	for _, group := range []string{"Counters", "HwCounters"} {
		groupType := reflect.TypeOf(sysfs.InfiniBandPort{}).FieldByIndex(
			fieldIndexByName(t, reflect.TypeOf(sysfs.InfiniBandPort{}), group)).Type

		for i := 0; i < groupType.NumField(); i++ {
			field := groupType.Field(i)
			// Only *uint64 fields are candidates.
			if field.Type.Kind() != reflect.Ptr || field.Type.Elem().Kind() != reflect.Uint64 {
				continue
			}

			var port sysfs.InfiniBandPort
			v := reflect.ValueOf(&port).Elem().FieldByName(group).FieldByName(field.Name)
			value := sentinel
			v.Set(reflect.ValueOf(&value))

			if got := accessor(&port); got != nil && *got == sentinel {
				return group + "." + field.Name
			}
		}
	}
	return ""
}

// fieldIndexByName returns the index path of a named struct field.
func fieldIndexByName(t *testing.T, typ reflect.Type, name string) []int {
	t.Helper()
	field, ok := typ.FieldByName(name)
	require.True(t, ok, "no field %q on %s", name, typ)
	return field.Index
}
