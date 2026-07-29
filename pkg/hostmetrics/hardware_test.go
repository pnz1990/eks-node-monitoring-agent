package hostmetrics

// Tests for the hardware group: thermal_zone, cpufreq, edac, kernel_hung.
//
// THE CENTRAL ASSERTION OF THIS FILE is that absent hardware yields
// success=1 with ZERO SERIES, not ErrNoData. Measured against the live cluster's
// prometheus-node-exporter: thermal_zone, cpufreq, edac and watchdog all report
// node_scrape_collector_success=1 with no metrics on EC2. ErrNoData would report
// success=0 and differ from upstream on every EKS node.
//
// I got exactly this wrong once before on the dependency branch -- asserted zero
// collector failures when upstream fails the same set on EKS -- so it is pinned
// here rather than assumed.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/prometheus/procfs/sysfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- the contract: absent hardware is success with no series ---------------

func TestHardwareCollectorsSucceedWithZeroSeriesWhenHardwareAbsent(t *testing.T) {
	// An empty sysfs/procfs stands in for an EC2 instance. Each of these must return
	// nil -- NOT ErrNoData -- because upstream does, and ErrNoData sets
	// node_scrape_collector_success=0.
	//
	// The mechanism is that procfs's sysfs helpers use filepath.Glob, which returns
	// an EMPTY SLICE rather than an error when nothing matches. Reproducing upstream
	// means NOT adding a helpful "nothing found" check.
	// BUILDING THIS FIXTURE CORRECTLY TOOK TWO CORRECTIONS, both of them my fixture
	// being wrong rather than the collector:
	//
	//  1. procfs's SystemCpufreq reads devices/system/cpu/offline unconditionally,
	//     and that file exists on every real host (verified on this machine: mode
	//     0444, empty, meaning no offline CPUs). Omitting it made cpufreq "fail" for
	//     a reason no real machine would ever hit.
	//  2. It also needs at least one cpu[0-9]* directory. With none, procfs returns
	//     "could not find any cpufreq files" -- but with CPUs present and no cpufreq
	//     subdirectory it returns a pre-sized slice of ZERO-VALUED entries and a nil
	//     error, because it does make([]SystemCPUCpufreqStats, len(cpus)) up front
	//     and only fills the ones it can read. THAT is the mechanism behind
	//     success=1-with-no-series on EC2, and it is why every field is a pointer:
	//     the zero-valued entries have nil everywhere and emit nothing.
	//
	// Confirmed by probing the live collector on this host, which has 32 CPUs and no
	// cpufreq directories: series=0, err=nil -- matching the cluster golden's
	// collector_success=1.
	root := t.TempDir()
	cpuDir := filepath.Join(root, "devices", "system", "cpu")
	require.NoError(t, os.MkdirAll(filepath.Join(cpuDir, "cpu0"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(cpuDir, "cpu1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cpuDir, "offline"), []byte("\n"), 0o644))
	paths := Paths{SysFS: root, ProcFS: t.TempDir(), RootFS: root}.withDefaults()

	for name, build := range map[string]factory{
		"thermal_zone": newThermalZoneCollector,
		"cpufreq":      newCPUFreqCollector,
		"edac":         newEDACCollector,
	} {
		t.Run(name, func(t *testing.T) {
			c, err := build(quietLogger(), paths)
			require.NoError(t, err)

			ch := make(chan prometheus.Metric, 64)
			err = c.Update(ch)
			close(ch)

			require.NoError(t, err,
				"%s must succeed with no hardware; ErrNoData would set collector_success=0 and differ from upstream on every EKS node", name)
			assert.Empty(t, ch, "%s must emit no series when the hardware is absent", name)
		})
	}
}

// --- thermal_zone ---------------------------------------------------------

func TestThermalZoneConvertsMillidegreesToCelsius(t *testing.T) {
	// The kernel reports millidegrees. Omitting the /1000 gives 45000 degrees, which
	// is obvious -- but a /100 or /10000 would give 450 or 4.5, both plausible enough
	// to survive review.
	c := newThermalZoneStub(t,
		[]sysfs.ClassThermalZoneStats{{Name: "0", Type: "x86_pkg_temp", Temp: 45000}}, nil)

	got := gatherLabelled(t, c, "zone")
	assert.Equal(t, 45.0, got["node_thermal_zone_temp"]["0"])
}

func TestThermalZoneNegativeTemperatureIsPreserved(t *testing.T) {
	// Temp is signed and some sensors legitimately report below zero. A uint cast
	// would wrap it to a huge positive value.
	c := newThermalZoneStub(t,
		[]sysfs.ClassThermalZoneStats{{Name: "0", Type: "acpitz", Temp: -5000}}, nil)

	got := gatherLabelled(t, c, "zone")
	assert.Equal(t, -5.0, got["node_thermal_zone_temp"]["0"])
}

func TestThermalZoneEmitsBothCoolingDeviceStates(t *testing.T) {
	c := newThermalZoneStub(t, nil, []sysfs.ClassCoolingDeviceStats{
		{Name: "0", Type: "Processor", CurState: 3, MaxState: 10},
	})

	got := gatherLabelled(t, c, "name")
	assert.Equal(t, 3.0, got["node_cooling_device_cur_state"]["0"])
	assert.Equal(t, 10.0, got["node_cooling_device_max_state"]["0"])
}

func TestThermalZoneToleratedErrorsYieldNoData(t *testing.T) {
	// Upstream tolerates four distinct error kinds. EINVAL in particular is what a
	// sysfs "temp" file returns when the sensor exists but is not readable -- a real
	// hardware condition, and not a scrape failure.
	for name, stubErr := range map[string]error{
		"not exist":  os.ErrNotExist,
		"permission": os.ErrPermission,
		"invalid":    os.ErrInvalid,
		"EINVAL":     syscall.EINVAL,
	} {
		t.Run(name, func(t *testing.T) {
			c := newThermalZoneStub(t, nil, nil)
			c.thermalZoneStats = func() ([]sysfs.ClassThermalZoneStats, error) {
				return nil, stubErr
			}

			err := c.Update(make(chan prometheus.Metric, 8))
			require.Error(t, err)
			assert.True(t, IsNoDataError(err), "expected ErrNoData for %s, got %v", name, err)
		})
	}
}

func TestThermalZoneOtherErrorIsAFailure(t *testing.T) {
	c := newThermalZoneStub(t, nil, nil)
	c.thermalZoneStats = func() ([]sysfs.ClassThermalZoneStats, error) {
		return nil, assert.AnError
	}

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err), "an unexpected error must not be laundered into no-data")
}

func TestThermalZoneCoolingDeviceErrorIsAFailure(t *testing.T) {
	// The cooling-device read has NO tolerated-error list upstream, unlike the zone
	// read. That asymmetry is preserved.
	c := newThermalZoneStub(t, nil, nil)
	c.coolingDeviceStats = func() ([]sysfs.ClassCoolingDeviceStats, error) {
		return nil, os.ErrNotExist
	}

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err),
		"upstream does not tolerate errors on the cooling-device read; that asymmetry is preserved")
}

func TestThermalZoneConstructionFailsOnMissingSysfs(t *testing.T) {
	_, err := newThermalZoneCollector(quietLogger(),
		Paths{SysFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open sysfs")
}

// --- cpufreq --------------------------------------------------------------

func TestCPUFreqConvertsKilohertzToHertz(t *testing.T) {
	// The kernel reports kHz and the metric is hertz. Missing the *1000 gives
	// 2400 Hz instead of 2.4 GHz -- a number that only looks wrong if you think
	// about it.
	freq := uint64(2400000)
	c := newCPUFreqStub(t, []sysfs.SystemCPUCpufreqStats{{
		Name: "0", CpuinfoCurrentFrequency: &freq,
	}})

	got := gatherLabelled(t, c, "cpu")
	assert.Equal(t, 2.4e9, got["node_cpu_frequency_hertz"]["0"],
		"2400000 kHz = 2.4 GHz")
}

func TestCPUFreqEmitsEveryUpstreamField(t *testing.T) {
	// frequency_avg_hertz was MISSING from my first pass: upstream's descriptors live
	// in cpufreq_common.go and I had read only cpufreq_linux.go. A dropped metric is
	// invisible to any test that only checks the metrics present, so this asserts the
	// full set.
	v := func(x uint64) *uint64 { return &x }
	c := newCPUFreqStub(t, []sysfs.SystemCPUCpufreqStats{{
		Name:                    "0",
		CpuinfoCurrentFrequency: v(1000),
		CpuinfoAverageFrequency: v(2000),
		CpuinfoMinimumFrequency: v(3000),
		CpuinfoMaximumFrequency: v(4000),
		ScalingCurrentFrequency: v(5000),
		ScalingMinimumFrequency: v(6000),
		ScalingMaximumFrequency: v(7000),
	}})

	got := gatherLabelled(t, c, "cpu")
	// Distinct values so a swapped pair of descriptors fails.
	for name, want := range map[string]float64{
		"node_cpu_frequency_hertz":             1e6,
		"node_cpu_frequency_avg_hertz":         2e6,
		"node_cpu_frequency_min_hertz":         3e6,
		"node_cpu_frequency_max_hertz":         4e6,
		"node_cpu_scaling_frequency_hertz":     5e6,
		"node_cpu_scaling_frequency_min_hertz": 6e6,
		"node_cpu_scaling_frequency_max_hertz": 7e6,
	} {
		assert.Equal(t, want, got[name]["0"], "%s", name)
	}
	assert.Len(t, got, 7, "all seven frequency metrics; got %v", keysOfAny(got))
}

func TestCPUFreqDescriptorsMatchUpstreamIncludingHelpText(t *testing.T) {
	// Compared against cpufreq_common.go, where the descriptors actually live.
	// Reading only cpufreq_linux.go is what caused me to drop frequency_avg_hertz and
	// to write "cpu thread" where upstream writes "CPU thread" -- help text is part of
	// the exposition output, so that is a real diff against the reference endpoint.
	data, err := os.ReadFile("../../../node_exporter/collector/cpufreq_common.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}

	upstream := map[string]string{}
	for _, m := range regexp.MustCompile(
		`prometheus\.BuildFQName\(namespace,\s*cpuCollectorSubsystem,\s*"([^"]+)"\),\s*\n\s*"([^"]*)",`,
	).FindAllStringSubmatch(string(data), -1) {
		upstream["node_cpu_"+m[1]] = m[2]
	}
	require.Len(t, upstream, 8, "expected 8 upstream cpufreq descriptors, got %d", len(upstream))

	ours := map[string]*prometheus.Desc{
		"node_cpu_frequency_hertz":             cpuFreqHertzDesc,
		"node_cpu_frequency_avg_hertz":         cpuFreqAvgDesc,
		"node_cpu_frequency_min_hertz":         cpuFreqMinDesc,
		"node_cpu_frequency_max_hertz":         cpuFreqMaxDesc,
		"node_cpu_scaling_frequency_hertz":     cpuFreqScalingFreqDesc,
		"node_cpu_scaling_frequency_min_hertz": cpuFreqScalingFreqMinDesc,
		"node_cpu_scaling_frequency_max_hertz": cpuFreqScalingFreqMaxDesc,
		"node_cpu_scaling_governor":            cpuFreqScalingGovernorDesc,
	}
	assert.Len(t, ours, len(upstream))

	for name, help := range upstream {
		desc, ok := ours[name]
		require.True(t, ok, "upstream descriptor %q is missing", name)
		assert.Contains(t, desc.String(), help,
			"help text for %s must be upstream's verbatim", name)
	}
}

func TestCPUFreqNilFieldsAreOmitted(t *testing.T) {
	// Every field is a pointer: nil means the kernel does not expose that attribute
	// for this CPU. Emitting zero would report a stopped CPU.
	freq := uint64(1000)
	c := newCPUFreqStub(t, []sysfs.SystemCPUCpufreqStats{{
		Name: "0", CpuinfoCurrentFrequency: &freq,
	}})

	got := gatherLabelled(t, c, "cpu")
	assert.Len(t, got, 1, "only the one non-nil field may be emitted")
	assert.Contains(t, got, "node_cpu_frequency_hertz")
}

func TestCPUFreqGovernorEmitsOneSeriesPerAvailableGovernor(t *testing.T) {
	// The shape is one series per AVAILABLE governor with 1 for the active one and 0
	// for the rest, so a query can see which governors EXIST rather than only which
	// is active. Emitting only the active one would lose that.
	c := newCPUFreqStub(t, []sysfs.SystemCPUCpufreqStats{{
		Name:               "0",
		Governor:           "performance",
		AvailableGovernors: "conservative ondemand performance powersave",
	}})

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	byGovernor := map[string]float64{}
	for m := range ch {
		require.Equal(t, "node_cpu_scaling_governor", metricName(t, m))
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		for _, l := range pb.GetLabel() {
			if l.GetName() == "governor" {
				byGovernor[l.GetValue()] = pb.GetGauge().GetValue()
			}
		}
	}

	assert.Equal(t, map[string]float64{
		"conservative": 0, "ondemand": 0, "performance": 1, "powersave": 0,
	}, byGovernor)
}

func TestCPUFreqNoGovernorEmitsNothing(t *testing.T) {
	c := newCPUFreqStub(t, []sysfs.SystemCPUCpufreqStats{{Name: "0", Governor: ""}})

	ch := make(chan prometheus.Metric, 16)
	require.NoError(t, c.Update(ch))
	close(ch)
	assert.Empty(t, ch, "no governor reported means no governor series")
}

func TestCPUFreqReadFailureIsAFailure(t *testing.T) {
	c := newCPUFreqStub(t, nil)
	c.systemCPUFreqStats = func() ([]sysfs.SystemCPUCpufreqStats, error) {
		return nil, assert.AnError
	}

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err))
}

func TestCPUFreqConstructionFailsOnMissingSysfs(t *testing.T) {
	_, err := newCPUFreqCollector(quietLogger(),
		Paths{SysFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open sysfs")
}

// --- edac -----------------------------------------------------------------

func TestEDACEmitsControllerAndCsRowAndChannelMetrics(t *testing.T) {
	// A synthetic EDAC tree, since no EC2 instance has one. Values are distinct so a
	// misrouted descriptor fails rather than coincidentally matching.
	root := writeEDACTree(t, map[string]string{
		"mc0/ce_count":              "10",
		"mc0/ce_noinfo_count":       "20",
		"mc0/ue_count":              "30",
		"mc0/ue_noinfo_count":       "40",
		"mc0/csrow0/ce_count":       "50",
		"mc0/csrow0/ue_count":       "60",
		"mc0/csrow0/ch0_ce_count":   "70",
		"mc0/csrow0/ch0_ue_count":   "80",
		"mc0/csrow0/ch0_dimm_label": "CPU#1_csrow0",
	})

	c, err := newEDACCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherAllLabels(t, c)

	assert.Equal(t, 10.0, got["node_edac_correctable_errors_total"]["controller=0"])
	assert.Equal(t, 30.0, got["node_edac_uncorrectable_errors_total"]["controller=0"])
	// The *_noinfo_count files become csrow metrics with csrow="unknown": errors the
	// controller could not attribute to a row. Dropping them loses real counts.
	assert.Equal(t, 20.0, got["node_edac_csrow_correctable_errors_total"]["controller=0,csrow=unknown"])
	assert.Equal(t, 40.0, got["node_edac_csrow_uncorrectable_errors_total"]["controller=0,csrow=unknown"])
	assert.Equal(t, 50.0, got["node_edac_csrow_correctable_errors_total"]["controller=0,csrow=0"])
	assert.Equal(t, 60.0, got["node_edac_csrow_uncorrectable_errors_total"]["controller=0,csrow=0"])
	// Channel metrics carry FOUR labels, not two. A two-label version is a different
	// metric that no existing query matches.
	assert.Equal(t, 70.0,
		got["node_edac_channel_correctable_errors_total"]["channel=0,controller=0,csrow=0,dimm_label=CPU1__csrow0"])
	assert.Equal(t, 80.0,
		got["node_edac_channel_uncorrectable_errors_total"]["channel=0,controller=0,csrow=0,dimm_label=CPU1__csrow0"])
}

func TestEDACDimmLabelSubstitutionsAreUpstreamVerbatim(t *testing.T) {
	// Not cosmetic: "#" is stripped and "csrow"/"channel" get an underscore prefix, so
	// a BIOS label "CPU#1_csrow0" becomes "CPU1__csrow0". Changing them changes the
	// label value on real hardware.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ch0_dimm_label"),
		[]byte("  CPU#1_csrow0_channel2  \n"), 0o644))

	assert.Equal(t, "CPU1__csrow0__channel2", edacDimmLabel(dir, "0"))
}

func TestEDACDimmLabelDefaultsToUnknown(t *testing.T) {
	// Most hardware sets no label. "unknown" rather than empty, so the series stays
	// selectable.
	assert.Equal(t, "unknown", edacDimmLabel(t.TempDir(), "0"))
}

func TestEDACChannelMissingUECountIsSkippedNotFatal(t *testing.T) {
	// UPSTREAM'S ASYMMETRY, preserved: a missing ch*_ue_count is logged and skipped
	// while every other read failure aborts the collector. Some hardware exposes
	// ch*_ce_count without ch*_ue_count, so failing would lose the whole collector
	// there.
	root := writeEDACTree(t, map[string]string{
		"mc0/ce_count":            "1",
		"mc0/ce_noinfo_count":     "0",
		"mc0/ue_count":            "0",
		"mc0/ue_noinfo_count":     "0",
		"mc0/csrow0/ce_count":     "2",
		"mc0/csrow0/ue_count":     "3",
		"mc0/csrow0/ch0_ce_count": "4",
		// no ch0_ue_count
	})

	c, err := newEDACCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherAllLabels(t, c)
	require.Contains(t, got, "node_edac_channel_correctable_errors_total",
		"the CE count must still be reported")
	assert.NotContains(t, got, "node_edac_channel_uncorrectable_errors_total")
}

func TestEDACMissingControllerFileIsFatal(t *testing.T) {
	// Unlike the channel UE count, a missing controller counter IS fatal upstream --
	// it means the tree is malformed rather than the hardware being limited.
	root := writeEDACTree(t, map[string]string{"mc0/ce_count": "1"})

	c, err := newEDACCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ce_noinfo_count")
}

func TestEDACUnparseableCounterIsFatal(t *testing.T) {
	root := writeEDACTree(t, map[string]string{
		"mc0/ce_count":        "not-a-number",
		"mc0/ce_noinfo_count": "0",
		"mc0/ue_count":        "0",
		"mc0/ue_noinfo_count": "0",
	})

	c, err := newEDACCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ce_count")
}

func TestEDACChannelFileNotMatchingRegexpIsSkipped(t *testing.T) {
	// A file matching the glob but not the regexp is skipped rather than failing,
	// matching upstream. "chX_ce_count" globs but has no numeric channel.
	root := writeEDACTree(t, map[string]string{
		"mc0/ce_count":            "0",
		"mc0/ce_noinfo_count":     "0",
		"mc0/ue_count":            "0",
		"mc0/ue_noinfo_count":     "0",
		"mc0/csrow0/ce_count":     "0",
		"mc0/csrow0/ue_count":     "0",
		"mc0/csrow0/chX_ce_count": "5",
	})

	c, err := newEDACCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherAllLabels(t, c)
	assert.NotContains(t, got, "node_edac_channel_correctable_errors_total",
		"a non-numeric channel must be skipped, not emitted")
}

func TestEDACBadSysfsPathMakesTheGlobPatternInvalid(t *testing.T) {
	// filepath.Glob errors ONLY on ErrBadPattern. The pattern is a compile-time
	// constant, but it is joined onto a caller-supplied sysfs root -- so a root
	// containing "[" produces an invalid pattern. Verified: filepath.Glob("/tmp/[")
	// returns "syntax error in pattern".
	c, err := newEDACCollector(quietLogger(), Paths{SysFS: "/tmp/["}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "syntax error in pattern")
}

func TestEDACNestedGlobErrorsArePropagated(t *testing.T) {
	// The csrow and channel globs are built from the PREVIOUS glob's results, so a
	// controller path containing "[" makes those patterns invalid too. Driven through
	// the seam, returning the error directly rather than redirecting the pattern --
	// my first attempt rewrote the pattern instead of failing, which left the csrow
	// branch uncovered and only showed up in the coverage report.
	root := writeEDACTree(t, map[string]string{
		"mc0/ce_count": "0", "mc0/ce_noinfo_count": "0",
		"mc0/ue_count": "0", "mc0/ue_noinfo_count": "0",
		"mc0/csrow0/ce_count": "0", "mc0/csrow0/ue_count": "0",
	})

	for name, failOn := range map[string]string{
		"csrow glob":   "csrow[0-9]*",
		"channel glob": "ch*_ce_count",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := newEDACCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
			require.NoError(t, err)

			ec := c.(*edacCollector)
			real := ec.glob
			ec.glob = func(pattern string) ([]string, error) {
				if strings.HasSuffix(pattern, failOn) {
					return nil, filepath.ErrBadPattern
				}
				// Everything else resolves against the real tree, so the file reads
				// preceding the failing glob all succeed.
				return real(pattern)
			}

			err = ec.Update(make(chan prometheus.Metric, 32))
			require.Error(t, err)
			assert.ErrorIs(t, err, filepath.ErrBadPattern)
		})
	}
}

func TestEDACPathsNotMatchingTheRegexpAreRejected(t *testing.T) {
	// These branches CANNOT be reached through the real glob: every path it returns
	// necessarily contains "devices/system/edac/mc/mc", which is exactly what the
	// regexp requires. Upstream has the same dead branch. The guards are kept rather
	// than deleted -- they would matter if either pattern changed -- and covered
	// through the seam.
	root := writeEDACTree(t, map[string]string{
		"mc0/ce_count": "0", "mc0/ce_noinfo_count": "0",
		"mc0/ue_count": "0", "mc0/ue_noinfo_count": "0",
	})

	t.Run("controller", func(t *testing.T) {
		c, err := newEDACCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
		require.NoError(t, err)
		ec := c.(*edacCollector)
		ec.glob = func(string) ([]string, error) { return []string{"/not/an/edac/path"}, nil }

		err = ec.Update(make(chan prometheus.Metric, 8))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "controller string didn't match regexp")
	})

	t.Run("csrow", func(t *testing.T) {
		c, err := newEDACCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
		require.NoError(t, err)
		ec := c.(*edacCollector)
		real := ec.glob
		ec.glob = func(pattern string) ([]string, error) {
			if strings.Contains(pattern, "csrow") {
				return []string{"/not/a/csrow/path"}, nil
			}
			return real(pattern)
		}

		err = ec.Update(make(chan prometheus.Metric, 32))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "csrow string didn't match regexp")
	})
}

func TestEDACCsRowCounterReadFailureIsFatal(t *testing.T) {
	// A csrow directory with no ce_count. Unlike the channel ue_count, this aborts:
	// a csrow that exists must have its counters.
	root := writeEDACTree(t, map[string]string{
		"mc0/ce_count": "0", "mc0/ce_noinfo_count": "0",
		"mc0/ue_count": "0", "mc0/ue_noinfo_count": "0",
	})
	require.NoError(t, os.MkdirAll(
		filepath.Join(root, "devices/system/edac/mc/mc0/csrow0"), 0o755))

	c, err := newEDACCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "controller/csrow")
}

func TestEDACChannelCECountUnreadableIsFatal(t *testing.T) {
	// The channel CE count aborts on a read failure, while the UE count does not.
	// Both halves of that asymmetry are now pinned.
	if os.Geteuid() == 0 {
		t.Skip("running as root, mode 000 is still readable")
	}
	root := writeEDACTree(t, map[string]string{
		"mc0/ce_count": "0", "mc0/ce_noinfo_count": "0",
		"mc0/ue_count": "0", "mc0/ue_noinfo_count": "0",
		"mc0/csrow0/ce_count": "0", "mc0/csrow0/ue_count": "0",
		"mc0/csrow0/ch0_ce_count": "1",
	})
	chFile := filepath.Join(root, "devices/system/edac/mc/mc0/csrow0/ch0_ce_count")
	require.NoError(t, os.Chmod(chFile, 0o000))
	t.Cleanup(func() { _ = os.Chmod(chFile, 0o644) })

	c, err := newEDACCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "controller/csrow/channel")
}

func TestEDACDescriptorsMatchUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/edac_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}

	upstream := map[string]string{}
	for _, m := range regexp.MustCompile(
		`prometheus\.BuildFQName\(namespace,\s*edacSubsystem,\s*"([^"]+)"\),\s*\n\s*"[^"]*",\s*\n\s*\[\]string\{([^}]*)\}`,
	).FindAllStringSubmatch(string(data), -1) {
		upstream["node_edac_"+m[1]] = m[2]
	}
	require.Len(t, upstream, 6, "expected 6 upstream edac descriptors, got %d", len(upstream))

	ours := map[string]*prometheus.Desc{
		"node_edac_correctable_errors_total":           edacCECountDesc,
		"node_edac_uncorrectable_errors_total":         edacUECountDesc,
		"node_edac_csrow_correctable_errors_total":     edacCsRowCECountDesc,
		"node_edac_csrow_uncorrectable_errors_total":   edacCsRowUECountDesc,
		"node_edac_channel_correctable_errors_total":   edacChannelCECountDesc,
		"node_edac_channel_uncorrectable_errors_total": edacChannelUECountDesc,
	}
	assert.Len(t, ours, len(upstream))

	for name, labels := range upstream {
		desc, ok := ours[name]
		require.True(t, ok, "upstream descriptor %q is missing", name)
		for _, label := range regexp.MustCompile(`"([a-z_]+)"`).FindAllStringSubmatch(labels, -1) {
			assert.Contains(t, desc.String(), label[1],
				"%s must carry the %q label", name, label[1])
		}
	}
}

// --- kernel_hung ----------------------------------------------------------

func TestKernelHungEmitsTasksTotal(t *testing.T) {
	// A task blocked in uninterruptible sleep for 120s is usually a stuck I/O path --
	// what a hung NFS or EBS volume looks like from the kernel's side.
	c := newKernelHungStub(t, 7)

	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 7.0, got["node_kernel_hung_tasks_total"])
	assert.Len(t, got, 1)
}

func TestKernelHungIsACounter(t *testing.T) {
	// Cumulative since boot, so a counter: rate() over it is the useful query.
	c := newKernelHungStub(t, 3)

	ch := make(chan prometheus.Metric, 8)
	require.NoError(t, c.Update(ch))
	close(ch)

	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.NotNil(t, pb.Counter, "hung tasks is cumulative since boot")
	}
}

func TestKernelHungMissingFileIsNoData(t *testing.T) {
	// hung_task_detect_count needs kernel 6.7. Absent is a supported configuration.
	c := newKernelHungStub(t, 0)
	c.kernelHung = func() (procfs.KernelHung, error) {
		return procfs.KernelHung{}, os.ErrNotExist
	}

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
}

func TestKernelHungNilCountIsNoDataNotAPanic(t *testing.T) {
	// Upstream dereferences HungTaskDetectCount unconditionally, so a nil pointer with
	// a nil error would panic. Guarded here: a nil-deref in a collector is far worse
	// than a missing metric, and the resilience layer should not be the only thing
	// standing between this and a crash.
	c := newKernelHungStub(t, 0)
	c.kernelHung = func() (procfs.KernelHung, error) {
		return procfs.KernelHung{HungTaskDetectCount: nil}, nil
	}

	// The assertion is that this does not panic.
	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
}

func TestKernelHungRealErrorIsAFailure(t *testing.T) {
	c := newKernelHungStub(t, 0)
	c.kernelHung = func() (procfs.KernelHung, error) {
		return procfs.KernelHung{}, assert.AnError
	}

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err))
}

func TestKernelHungConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newKernelHungCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

func TestKernelHungRegisteredConstructorWiresTheRealReader(t *testing.T) {
	c, err := newKernelHungCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	require.NotNil(t, c.(*kernelHungCollector).kernelHung)
}

// --- live ------------------------------------------------------------------

func TestHardwareGroupLiveOnThisHost(t *testing.T) {
	// The real sysfs. This host has cooling devices but no thermal zones, cpufreq or
	// EDAC, which is close to an EC2 instance and exercises the "some present, most
	// absent" mix.
	for name, build := range map[string]factory{
		"thermal_zone": newThermalZoneCollector,
		"cpufreq":      newCPUFreqCollector,
		"edac":         newEDACCollector,
	} {
		t.Run(name, func(t *testing.T) {
			c, err := build(quietLogger(), Paths{}.withDefaults())
			require.NoError(t, err)

			ch := make(chan prometheus.Metric, 4096)
			err = c.Update(ch)
			close(ch)
			if err != nil && IsNoDataError(err) {
				t.Skipf("%s reports no data on this host", name)
			}
			require.NoError(t, err, "%s must not fail on a real host", name)
		})
	}
}

// --- helpers --------------------------------------------------------------

func newThermalZoneStub(t *testing.T, zones []sysfs.ClassThermalZoneStats,
	devices []sysfs.ClassCoolingDeviceStats) *thermalZoneCollector {
	t.Helper()
	c, err := newThermalZoneCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	tc := c.(*thermalZoneCollector)
	tc.thermalZoneStats = func() ([]sysfs.ClassThermalZoneStats, error) { return zones, nil }
	tc.coolingDeviceStats = func() ([]sysfs.ClassCoolingDeviceStats, error) { return devices, nil }
	return tc
}

func newCPUFreqStub(t *testing.T, stats []sysfs.SystemCPUCpufreqStats) *cpuFreqCollector {
	t.Helper()
	c, err := newCPUFreqCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	fc := c.(*cpuFreqCollector)
	fc.systemCPUFreqStats = func() ([]sysfs.SystemCPUCpufreqStats, error) { return stats, nil }
	return fc
}

func newKernelHungStub(t *testing.T, count uint64) *kernelHungCollector {
	t.Helper()
	c, err := newKernelHungCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	kc := c.(*kernelHungCollector)
	kc.kernelHung = func() (procfs.KernelHung, error) {
		v := count
		return procfs.KernelHung{HungTaskDetectCount: &v}, nil
	}
	return kc
}

// writeEDACTree builds a synthetic /sys/devices/system/edac/mc tree.
func writeEDACTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "devices", "system", "edac", "mc")

	for rel, content := range files {
		path := filepath.Join(base, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content+"\n"), 0o644))
	}
	return root
}

// gatherLabelled returns metric name -> single label value -> value.
func gatherLabelled(t *testing.T, c Collector, labelName string) map[string]map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 4096)
	require.NoError(t, c.Update(ch))
	close(ch)

	out := map[string]map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))

		name := metricName(t, m)
		key := ""
		for _, l := range pb.GetLabel() {
			if l.GetName() == labelName {
				key = l.GetValue()
			}
		}
		if out[name] == nil {
			out[name] = map[string]float64{}
		}
		if pb.Counter != nil {
			out[name][key] = pb.GetCounter().GetValue()
			continue
		}
		out[name][key] = pb.GetGauge().GetValue()
	}
	return out
}

// gatherAllLabels keys by the full sorted label set, so a metric with the wrong
// label CARDINALITY does not silently collide with the right one.
func gatherAllLabels(t *testing.T, c Collector) map[string]map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 4096)
	require.NoError(t, c.Update(ch))
	close(ch)

	out := map[string]map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))

		parts := make([]string, 0, len(pb.GetLabel()))
		for _, l := range pb.GetLabel() {
			parts = append(parts, l.GetName()+"="+l.GetValue())
		}
		sort.Strings(parts)

		name := metricName(t, m)
		if out[name] == nil {
			out[name] = map[string]float64{}
		}
		out[name][strings.Join(parts, ",")] = pb.GetCounter().GetValue()
	}
	return out
}

func keysOfAny[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
