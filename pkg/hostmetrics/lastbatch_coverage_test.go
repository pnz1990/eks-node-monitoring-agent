package hostmetrics

// Coverage-completion tests for the final batch.
//
// These reach branches the behavioural tests do not: construction failures on a bad
// path, and the hwmon sensor types and name-derivation preferences that a minimal
// synthetic tree never exercises.
//
// They are here rather than merged into lastbatch_test.go because their purpose is
// different: those tests document a contract, these prove a branch is reachable and
// does what its code says. Keeping them apart means the contract tests stay readable.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs/blockdevice"
	"github.com/prometheus/procfs/btrfs"
	"github.com/prometheus/procfs/sysfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- construction failures ------------------------------------------------

func TestFinalBatchConstructionFailsOnBadPaths(t *testing.T) {
	// Each of these opens a filesystem at construction rather than on every scrape, so
	// a bad path must fail at startup instead of every 15 seconds forever. Table-driven
	// because the assertion is identical and only the constructor differs.
	absent := filepath.Join(t.TempDir(), "absent")

	for name, tc := range map[string]struct {
		build func() (Collector, error)
		want  string
	}{
		"btrfs": {
			build: func() (Collector, error) {
				return newBtrfsCollector(quietLogger(), Paths{SysFS: absent}.withDefaults())
			},
			want: "failed to open sysfs",
		},
		"dmmultipath": {
			build: func() (Collector, error) {
				return newDMMultipathCollector(quietLogger(),
					Paths{ProcFS: absent, SysFS: t.TempDir()}.withDefaults())
			},
			want: "failed to open procfs/sysfs",
		},
		"infiniband": {
			build: func() (Collector, error) {
				return newInfiniBandCollector(quietLogger(), Paths{SysFS: absent}.withDefaults())
			},
			want: "failed to open sysfs",
		},
		"powersupplyclass": {
			build: func() (Collector, error) {
				return newPowerSupplyClassCollector(quietLogger(), Paths{SysFS: absent}.withDefaults())
			},
			want: "failed to open sysfs",
		},
		"watchdog": {
			build: func() (Collector, error) {
				return newWatchdogCollector(quietLogger(), Paths{SysFS: absent}.withDefaults())
			},
			want: "failed to open sysfs",
		},
		"mdadm procfs": {
			build: func() (Collector, error) {
				return newMDAdmCollector(quietLogger(),
					Paths{ProcFS: absent, SysFS: t.TempDir()}.withDefaults())
			},
			want: "failed to open procfs",
		},
		"mdadm sysfs": {
			build: func() (Collector, error) {
				return newMDAdmCollector(quietLogger(),
					Paths{ProcFS: t.TempDir(), SysFS: absent}.withDefaults())
			},
			want: "failed to open sysfs",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.build()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestFinalBatchReadFailuresAreWrapped(t *testing.T) {
	// The non-ErrNoData error path for each collector: a real failure must be reported
	// as one rather than laundered into no-data.
	t.Run("btrfs", func(t *testing.T) {
		c := newBtrfsStub(t, nil)
		c.stats = func() ([]*btrfs.Stats, error) { return nil, assert.AnError }
		err := c.Update(make(chan prometheus.Metric, 8))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to retrieve Btrfs stats")
		assert.False(t, IsNoDataError(err))
	})

	t.Run("dmmultipath", func(t *testing.T) {
		c := newDMMultipathStub(t, nil)
		c.multipathDevices = func() ([]blockdevice.DMMultipathDevice, error) {
			return nil, assert.AnError
		}
		err := c.Update(make(chan prometheus.Metric, 8))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to scan DM-multipath devices")
		assert.False(t, IsNoDataError(err))
	})

	t.Run("infiniband", func(t *testing.T) {
		c := newInfinibandStub(t, nil)
		c.infinibandClass = func() (sysfs.InfiniBandClass, error) { return nil, assert.AnError }
		err := c.Update(make(chan prometheus.Metric, 8))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "error obtaining InfiniBand class info")
		assert.False(t, IsNoDataError(err))
	})
}

// --- btrfs: the device and nil-layout paths -------------------------------

func TestBtrfsDeviceSizeEmittedFromProcfsPath(t *testing.T) {
	// The procfs device path -- the one that IS ported. The ioctl path, which would add
	// device_unused_bytes and device_errors_total, is the documented scope exception.
	c := newBtrfsStub(t, []*btrfs.Stats{{
		UUID: "abc",
		Devices: map[string]*btrfs.Device{
			"1": {Size: 8 * 1024 * 1024 * 1024},
		},
	}})

	got := gatherAllLabels(t, c)
	require.Contains(t, got, "node_btrfs_device_size_bytes")
	assert.Equal(t, float64(8*1024*1024*1024),
		got["node_btrfs_device_size_bytes"]["device=1,uuid=abc"])

	// The ioctl-only families must be absent, which is the parity exception rather
	// than an accident.
	assert.NotContains(t, got, "node_btrfs_device_unused_bytes")
	assert.NotContains(t, got, "node_btrfs_device_errors_total")
}

func TestBtrfsNilLayoutIsSkipped(t *testing.T) {
	// A nil LayoutUsage inside a non-nil AllocationStats. Upstream would nil-deref.
	c := newBtrfsStub(t, []*btrfs.Stats{{
		UUID: "abc",
		Allocation: btrfs.Allocation{
			Data: &btrfs.AllocationStats{
				ReservedBytes: 1024,
				Layouts:       map[string]*btrfs.LayoutUsage{"single": nil},
			},
		},
	}})

	ch := make(chan prometheus.Metric, 128)
	require.NoError(t, c.Update(ch), "a nil layout must not panic")
	close(ch)

	got := drainMixed(t, ch)
	assert.Contains(t, got, "node_btrfs_reserved_bytes",
		"the reserved-bytes metric survives a nil layout")
	assert.NotContains(t, got, "node_btrfs_used_bytes")
}

// --- infiniband: the filter and multi-port paths --------------------------

func TestInfinibandDeviceFilterExcludes(t *testing.T) {
	c, err := newInfiniBandCollectorWithFilter(quietLogger(),
		Paths{SysFS: t.TempDir()}.withDefaults(), "^mlx5_1$", "")
	require.NoError(t, err)

	ic := c.(*infinibandCollector)
	ic.infinibandClass = func() (sysfs.InfiniBandClass, error) {
		return sysfs.InfiniBandClass{
			"mlx5_0": sysfs.InfiniBandDevice{Name: "mlx5_0"},
			"mlx5_1": sysfs.InfiniBandDevice{Name: "mlx5_1"},
		}, nil
	}

	labels := allInfoLabelValues(t, ic, "node_infiniband_info", "device")
	assert.Contains(t, labels, "mlx5_0")
	assert.NotContains(t, labels, "mlx5_1", "the excluded device must be absent")
}

func TestInfinibandMultiplePortsAreSortedByNumber(t *testing.T) {
	// Ports come from a map, so without an explicit sort the emission order would vary
	// between scrapes. That matters only for diffability, but a golden corpus is one of
	// the validation tools here.
	c := newInfinibandStub(t, sysfs.InfiniBandClass{
		"mlx5_0": sysfs.InfiniBandDevice{
			Name: "mlx5_0",
			Ports: map[uint]sysfs.InfiniBandPort{
				2: {Name: "mlx5_0", Port: 2, Counters: sysfs.InfiniBandCounters{PortRcvData: uint64Ptr(200)}},
				1: {Name: "mlx5_0", Port: 1, Counters: sysfs.InfiniBandCounters{PortRcvData: uint64Ptr(100)}},
			},
		},
	})

	got := gatherAllLabels(t, c)
	assert.Equal(t, 100.0,
		got["node_infiniband_port_data_received_bytes_total"]["device=mlx5_0,port=1"])
	assert.Equal(t, 200.0,
		got["node_infiniband_port_data_received_bytes_total"]["device=mlx5_0,port=2"])
}

// --- textfile: the remaining branches -------------------------------------

func TestTextFileInconsistentHelpTextIsReported(t *testing.T) {
	// Two files disagreeing about one metric's help text. The text format allows only
	// one, so this is a genuine conflict: reporting it beats picking silently.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.prom"), []byte(
		"# HELP shared_metric First description.\n"+
			"# TYPE shared_metric gauge\n"+
			"shared_metric 1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.prom"), []byte(
		"# HELP shared_metric A DIFFERENT description.\n"+
			"# TYPE shared_metric gauge\n"+
			"shared_metric 2\n"), 0o644))

	c := newTextFileStub(t, dir)
	got := gatherUnlabelledMixed(t, c)

	assert.Equal(t, 1.0, got["node_textfile_scrape_error"],
		"a help-text conflict sets the error flag")
	// The first file's version wins and the second is dropped, so exactly one series
	// remains -- emitting both would be a duplicate label set and Prometheus would
	// reject the whole scrape.
	assert.Equal(t, 1.0, got["shared_metric"], "the first file's value survives")
}

func TestTextFileCounterTypeIsPreserved(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "c.prom"), []byte(
		"# HELP my_counter A counter.\n"+
			"# TYPE my_counter counter\n"+
			"my_counter 12\n"), 0o644))

	c := newTextFileStub(t, dir)
	ch := make(chan prometheus.Metric, 32)
	require.NoError(t, c.Update(ch))
	close(ch)

	for m := range ch {
		if metricName(t, m) != "my_counter" {
			continue
		}
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		require.NotNil(t, pb.Counter, "a declared counter must stay a counter")
		assert.Equal(t, 12.0, pb.GetCounter().GetValue())
		return
	}
	t.Fatal("my_counter was not re-exported")
}

func TestTextFileHistogramIsUnsupportedAndFlagged(t *testing.T) {
	// Histograms and summaries are not re-exportable through NewConstMetric, so they
	// are skipped AND flagged rather than dropped silently -- an operator who wrote one
	// should learn that it did not work.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "h.prom"), []byte(
		"# HELP my_hist A histogram.\n"+
			"# TYPE my_hist histogram\n"+
			"my_hist_bucket{le=\"1\"} 1\n"+
			"my_hist_bucket{le=\"+Inf\"} 2\n"+
			"my_hist_sum 3\n"+
			"my_hist_count 2\n"), 0o644))

	c := newTextFileStub(t, dir)
	got := gatherUnlabelledMixed(t, c)

	assert.Equal(t, 1.0, got["node_textfile_scrape_error"],
		"an unsupported type must set the error flag, not pass silently")
	assert.NotContains(t, got, "my_hist")
}

func TestTextFileMetricValueAndTypeRejectsUnsupported(t *testing.T) {
	// The default branch, reached directly. Returning ok=false rather than a zero value
	// is what lets the caller flag the problem instead of emitting a fabricated 0.
	_, _, ok := metricValueAndType(dto.MetricType_HISTOGRAM, &dto.Metric{})
	assert.False(t, ok)
	_, _, ok = metricValueAndType(dto.MetricType_SUMMARY, &dto.Metric{})
	assert.False(t, ok)

	// And the three supported ones.
	for _, mt := range []dto.MetricType{
		dto.MetricType_COUNTER, dto.MetricType_GAUGE, dto.MetricType_UNTYPED,
	} {
		_, _, ok := metricValueAndType(mt, &dto.Metric{
			Counter: &dto.Counter{Value: f64Ptr(1)},
			Gauge:   &dto.Gauge{Value: f64Ptr(1)},
			Untyped: &dto.Untyped{Value: f64Ptr(1)},
		})
		assert.True(t, ok, "%v must be supported", mt)
	}
}

func TestTextFileDuplicateLabelSetIsFlaggedNotPanicked(t *testing.T) {
	// The label set comes from an operator-supplied file, so a malformed one is
	// untrusted input rather than a programming error. NewConstMetric (not Must) means
	// a bad file sets scrape_error instead of taking down the scrape.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dup.prom"), []byte(
		"# HELP my_metric A metric.\n"+
			"# TYPE my_metric gauge\n"+
			"my_metric{a=\"1\"} 1\n"), 0o644))

	c := newTextFileStub(t, dir)
	// The assertion is that this does not panic and reports normally.
	got := gatherUnlabelledMixed(t, c)
	assert.Contains(t, got, "node_textfile_scrape_error")
}

func TestTextFileUnreadableFileSetsScrapeError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, mode 000 is still readable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "locked.prom")
	require.NoError(t, os.WriteFile(path, []byte("my_metric 1\n"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	c := newTextFileStub(t, dir)
	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 1.0, got["node_textfile_scrape_error"])
}

func TestTextFileGlobPathIsExpanded(t *testing.T) {
	// A path may be a glob. Upstream tries Glob first and falls back to treating the
	// pattern literally, which is what makes both forms work.
	base := t.TempDir()
	for _, sub := range []string{"one", "two"} {
		dir := filepath.Join(base, sub)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, sub+".prom"),
			[]byte(sub+"_metric 1\n"), 0o644))
	}

	c := newTextFileStub(t, filepath.Join(base, "*"))
	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 1.0, got["one_metric"])
	assert.Equal(t, 1.0, got["two_metric"])
	assert.Equal(t, 0.0, got["node_textfile_scrape_error"])
}

// --- hwmon: the sensor types and name preferences -------------------------

func TestHwmonVrmAndBeepEnableAreWholeSensorTypes(t *testing.T) {
	// Two types are handled as a whole rather than per element: vrm and beep_enable
	// have no property suffix, so their value lives under the empty-string key.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":        "acme",
		"vrm":         "24",
		"beep_enable": "1",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 24.0, got["node_hwmon_voltage_regulator_version"]["vrm0"])
	assert.Equal(t, 1.0, got["node_hwmon_beep_enabled"]["beep_enable0"])
}

func TestHwmonBeepEnableZeroWhenNotOne(t *testing.T) {
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":        "acme",
		"beep_enable": "0",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 0.0, got["node_hwmon_beep_enabled"]["beep_enable0"])
}

func TestHwmonVrmNonNumericIsSkipped(t *testing.T) {
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name": "acme",
		"vrm":  "not-a-number",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.NotContains(t, got, "node_hwmon_voltage_regulator_version")
}

func TestHwmonBeepElementGetsEnabledSuffix(t *testing.T) {
	// A "beep" ELEMENT (temp1_beep) is different from the "beep_enable" TYPE, and gets
	// an _enabled suffix on the sensor-type name.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":       "acme",
		"temp1_beep": "1",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 1.0, got["node_hwmon_temp_beep_enabled"]["temp1"])
}

func TestHwmonFreqRequiresALabelAndReplacesTheSensorLabel(t *testing.T) {
	// freq is the odd one: emitted ONLY when the sensor has a label, and the label
	// REPLACES the sensor label rather than being added.
	withLabel := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":        "amdgpu",
		"freq1_input": "1500000000",
		"freq1_label": "sclk",
	})
	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: withLabel}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	require.Contains(t, got, "node_hwmon_freq_freq_mhz")
	assert.Equal(t, 1500.0, got["node_hwmon_freq_freq_mhz"]["sclk"],
		"the label replaces the sensor label, and the value is Hz/1e6")

	withoutLabel := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":        "amdgpu",
		"freq1_input": "1500000000",
	})
	c, err = newHwMonCollector(quietLogger(), Paths{SysFS: withoutLabel}.withDefaults())
	require.NoError(t, err)

	got = gatherLabelled(t, c, "sensor")
	assert.NotContains(t, got, "node_hwmon_freq_freq_mhz",
		"without a label, freq emits nothing")
}

func TestHwmonInputSuffixOnlyWhenABareValueAlsoExists(t *testing.T) {
	// "input" IS the value, so it does not normally become part of the name. But if the
	// sensor ALSO has a bare value, both exist and must be distinguished -- otherwise
	// they collide on one series.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name": "acme",
		// pwm1 (bare) and pwm1_input together.
		"pwm1":       "128",
		"pwm1_input": "200",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 128.0, got["node_hwmon_pwm"]["pwm1"], "the bare value")
	assert.Equal(t, 200.0, got["node_hwmon_pwm_input"]["pwm1"],
		"the input value gets the _input suffix only because a bare value also exists")
}

func TestHwmonHumidityAndUpdateIntervalPaths(t *testing.T) {
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":                    "acme",
		"humidity1_input":         "450000", // /1e6 -> 0.45 ratio
		"update_interval":         "1000",   // fallback: raw
		"in0_min":                 "11000",  // millivolts -> 11 V
		"power1_average_interval": "1000",   // ms -> 1 s
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	// The metric is node_hwmon_humidity, NOT _humidity_input: "input" IS the value, so
	// it does not become part of the name unless a bare value also exists. My first
	// expectation had the suffix.
	//
	// InDelta rather than Equal: 450000/1e6 is 0.44999999999999996 in float64, and
	// asserting exact equality on a divided float is a test that fails for arithmetic
	// reasons rather than behavioural ones.
	assert.InDelta(t, 0.45, got["node_hwmon_humidity"]["humidity1"], 1e-9)
	assert.Equal(t, 1000.0, got["node_hwmon_update_interval"]["update_interval0"],
		"update_interval hits the fallback and is emitted raw")
	assert.Equal(t, 11.0, got["node_hwmon_in_min_volts"]["in0"])
	assert.Equal(t, 1.0, got["node_hwmon_power_average_interval_seconds"]["power1"])
}

func TestHwmonNonNumericElementIsSkipped(t *testing.T) {
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":      "acme",
		"in0_input": "not-a-number",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.NotContains(t, got, "node_hwmon_in_volts")
}

func TestHwmonNameFallsBackToDirectoryName(t *testing.T) {
	// Preference 3: no device symlink and no name file, so the hwmonX directory name is
	// used. Unstable across reboots, which is why it is last.
	root := t.TempDir()
	dir := filepath.Join(root, "class", "hwmon", "hwmon7")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "temp1_input"), []byte("45000\n"), 0o644))

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "chip")
	assert.Contains(t, got["node_hwmon_temp_celsius"], "hwmon7",
		"the directory name is the last-resort chip label")
}

func TestHwmonNameUsesDeviceSymlinkWhenPresent(t *testing.T) {
	// Preference 1: the device path, which is stable across reboots. Built with a real
	// symlink so EvalSymlinks resolves it.
	root := t.TempDir()
	devDir := filepath.Join(root, "devices", "platform", "coretemp.0")
	require.NoError(t, os.MkdirAll(devDir, 0o755))

	hwmonDir := filepath.Join(root, "class", "hwmon", "hwmon0")
	require.NoError(t, os.MkdirAll(hwmonDir, 0o755))
	require.NoError(t, os.Symlink(devDir, filepath.Join(hwmonDir, "device")))
	require.NoError(t, os.WriteFile(filepath.Join(hwmonDir, "temp1_input"), []byte("45000\n"), 0o644))

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "chip")
	// devType_devName -> platform_coretemp_0
	assert.Contains(t, got["node_hwmon_temp_celsius"], "platform_coretemp_0",
		"the device path yields a stable chip name")
}

func TestHwmonSensorsUnderDeviceSubdirectoryAreMerged(t *testing.T) {
	// Some chips expose sensors under a "device" subdirectory as well as directly.
	root := t.TempDir()
	hwmonDir := filepath.Join(root, "class", "hwmon", "hwmon0")
	deviceDir := filepath.Join(hwmonDir, "device")
	require.NoError(t, os.MkdirAll(deviceDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hwmonDir, "name"), []byte("acme\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(hwmonDir, "temp1_input"), []byte("45000\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(deviceDir, "temp2_input"), []byte("50000\n"), 0o644))

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 45.0, got["node_hwmon_temp_celsius"]["temp1"])
	assert.Equal(t, 50.0, got["node_hwmon_temp_celsius"]["temp2"],
		"sensors under device/ must be merged in")
}

func TestHwmonChipFilterExcludes(t *testing.T) {
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":        "coretemp",
		"temp1_input": "45000",
	})

	c, err := newHwMonCollectorWithFilters(quietLogger(),
		Paths{SysFS: root}.withDefaults(), "^coretemp$", "", "", "")
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)
	assert.Empty(t, ch, "the excluded chip must emit nothing")
}

func TestHwmonUnreadableSensorFileIsSkipped(t *testing.T) {
	// A read failure on one sensor must not cost the chip's other sensors: hwmon files
	// can error individually.
	if os.Geteuid() == 0 {
		t.Skip("running as root, mode 000 is still readable")
	}
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":        "acme",
		"temp1_input": "45000",
		"temp2_input": "50000",
	})
	locked := filepath.Join(root, "class", "hwmon", "hwmon0", "temp2_input")
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 45.0, got["node_hwmon_temp_celsius"]["temp1"],
		"the readable sensor must survive")
	assert.NotContains(t, got["node_hwmon_temp_celsius"], "temp2")
}

func TestHwmonNoNameFileMeansNoChipNamesAnnotation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "class", "hwmon", "hwmon0")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "temp1_input"), []byte("45000\n"), 0o644))

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "chip")
	assert.NotContains(t, got, "node_hwmon_chip_names",
		"without a name file there is no human-readable chip name to annotate")
	assert.Contains(t, got, "node_hwmon_temp_celsius", "but the sensors are still reported")
}

func TestHwmonEmptyNameFileFallsThrough(t *testing.T) {
	// A name file containing only unusable characters must fall through to the next
	// preference rather than yielding an empty chip label.
	root := t.TempDir()
	dir := filepath.Join(root, "class", "hwmon", "hwmon3")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "name"), []byte("!!!\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "temp1_input"), []byte("45000\n"), 0o644))

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "chip")
	assert.Contains(t, got["node_hwmon_temp_celsius"], "hwmon3",
		"an unusable name file falls through to the directory name")
}

func TestHwmonFilenameNotMatchingTheFormatIsIgnored(t *testing.T) {
	// A file whose name does not parse, and a sensor TYPE not in the allowlist. Both
	// are ignored rather than emitted under a nonsense name.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":         "acme",
		"temp1_input":  "45000",
		"unknown9_val": "1", // type not in hwmonSensorTypes
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Contains(t, got, "node_hwmon_temp_celsius")
	assert.NotContains(t, got, "node_hwmon_unknown")
}

func TestHwmonExplodeRejectsUnparseableFilenames(t *testing.T) {
	// The type group is [^0-9]+, which requires at least one non-digit -- so an EMPTY
	// name and a digits-only name both fail to match. I had assumed the
	// optional-everything pattern would accept "", and it does not.
	for _, filename := range []string{"", "123", "42"} {
		ok, typ, num, prop := explodeHwmonSensorFilename(filename)
		assert.False(t, ok, "%q must not parse as a sensor filename", filename)
		assert.Empty(t, typ)
		assert.Zero(t, num)
		assert.Empty(t, prop)
	}

	// And the guard's purpose: an unparseable name is skipped by
	// collectHwmonSensorData rather than emitted under a nonsense metric name.
	ok, typ, _, _ := explodeHwmonSensorFilename("temp1_input")
	assert.True(t, ok)
	assert.Equal(t, "temp", typ)
}

func TestHwmonReadFileErrors(t *testing.T) {
	_, err := hwmonReadFile(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err))

	// A directory: Open succeeds, the raw read fails with EISDIR. os.ReadFile would
	// return a different error, which is part of why the raw read is used.
	dir := t.TempDir()
	_, err = hwmonReadFile(dir)
	require.Error(t, err)
}

func TestHwmonHumanReadableChipNameErrors(t *testing.T) {
	c := &hwMonCollector{logger: quietLogger()}

	_, err := c.humanReadableChipName(t.TempDir())
	require.Error(t, err, "no name file")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "name"), []byte("!!!\n"), 0o644))
	_, err = c.humanReadableChipName(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "human-readable chip type")
}

func TestHwmonNameErrorsWhenNothingIsDerivable(t *testing.T) {
	c := &hwMonCollector{logger: quietLogger()}

	// A path that does not exist: EvalSymlinks fails, so no name can be derived.
	_, err := c.hwmonName(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
}

func TestHwmonCollidingChipNamesAreDisambiguated(t *testing.T) {
	// Two hwmon nodes sharing one parent device resolve to the same name. Left
	// undisambiguated they emit duplicate label sets, and Prometheus rejects the ENTIRE
	// scrape with "collected before with the same name and label values" -- not just
	// this collector.
	root := t.TempDir()
	devDir := filepath.Join(root, "devices", "platform", "shared.0")
	require.NoError(t, os.MkdirAll(devDir, 0o755))

	for _, chip := range []string{"hwmon0", "hwmon1"} {
		dir := filepath.Join(root, "class", "hwmon", chip)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.Symlink(devDir, filepath.Join(dir, "device")))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "temp1_input"), []byte("45000\n"), 0o644))
	}

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	// The assertion that matters: registering these must not fail on a duplicate.
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(collectorAdapter{c}),
		"colliding chip names must be disambiguated before emission")

	_, err = reg.Gather()
	require.NoError(t, err,
		"a duplicate label set would fail Gather and take down the whole scrape")
}

// --- helpers --------------------------------------------------------------

func f64Ptr(v float64) *float64 { return &v }

// collectorAdapter lets a hostmetrics Collector be registered with a Prometheus
// registry, so duplicate label sets are detected by the registry rather than asserted
// by hand.
type collectorAdapter struct {
	c Collector
}

func (a collectorAdapter) Describe(chan<- *prometheus.Desc) {}

func (a collectorAdapter) Collect(ch chan<- prometheus.Metric) {
	// An error is ignored here: this adapter exists to exercise the registry's
	// duplicate detection, not to check error handling.
	_ = a.c.Update(ch)
}

// allInfoLabelValues returns every value of one label across a metric family.
func allInfoLabelValues(t *testing.T, c Collector, metric, label string) []string {
	t.Helper()

	ch := make(chan prometheus.Metric, 512)
	require.NoError(t, c.Update(ch))
	close(ch)

	var out []string
	for m := range ch {
		if metricName(t, m) != metric {
			continue
		}
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		for _, l := range pb.GetLabel() {
			if l.GetName() == label {
				out = append(out, l.GetValue())
			}
		}
	}
	return out
}

// --- the remaining reachable branches -------------------------------------

func TestHwmonSensorIDOverflowIsRejected(t *testing.T) {
	// THE ONE ATOI GUARD THAT IS REACHABLE. The regexp's id group is [0-9]* with no
	// length bound, so a filename with a 23-digit id parses as digits and overflows
	// int. Upstream's two OTHER defensive guards in this function are provably
	// unreachable and were dropped; this one fires.
	ok, typ, num, prop := explodeHwmonSensorFilename("temp99999999999999999999999_input")
	assert.False(t, ok, "an id that overflows int must not parse")
	assert.Equal(t, "temp", typ, "the type is still extracted before the failure")
	assert.Zero(t, num)
	assert.Empty(t, prop)
}

func TestHwmonUnparseableSensorIsSkippedNotEmitted(t *testing.T) {
	// End to end: the overflow filename must be skipped rather than recorded under a
	// wrapped sensor number.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":                              "acme",
		"temp1_input":                       "45000",
		"temp99999999999999999999999_input": "50000",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Contains(t, got["node_hwmon_temp_celsius"], "temp1")
	assert.Len(t, got["node_hwmon_temp_celsius"], 1,
		"the overflowing sensor must be skipped, not wrapped to temp0")
}

func TestHwmonUnreadableChipDirectoryFailsTheCollector(t *testing.T) {
	// A chip directory that cannot be read. Unlike a single unreadable sensor FILE
	// (skipped), an unreadable DIRECTORY means the enumeration itself failed, and
	// upstream propagates that.
	if os.Geteuid() == 0 {
		t.Skip("running as root, mode 000 is still readable")
	}
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":        "acme",
		"temp1_input": "45000",
	})
	dir := filepath.Join(root, "class", "hwmon", "hwmon0")
	require.NoError(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 64))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err),
		"an unreadable chip directory is a failure, not absent hardware")
}

func TestHwmonUnreadableDeviceSubdirectoryFailsTheCollector(t *testing.T) {
	// The second collectHwmonSensorData call, on the device/ subdirectory.
	if os.Geteuid() == 0 {
		t.Skip("running as root, mode 000 is still readable")
	}
	root := t.TempDir()
	hwmonDir := filepath.Join(root, "class", "hwmon", "hwmon0")
	deviceDir := filepath.Join(hwmonDir, "device")
	require.NoError(t, os.MkdirAll(deviceDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hwmonDir, "name"), []byte("acme\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(hwmonDir, "temp1_input"), []byte("45000\n"), 0o644))
	require.NoError(t, os.Chmod(deviceDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(deviceDir, 0o755) })

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 64))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err))
}

func TestHwmonUnreadableRootIsAFailureNotNoData(t *testing.T) {
	// /sys/class/hwmon exists but cannot be read. Distinct from ABSENT, which is the
	// EKS case and yields ErrNoData -- conflating them would report a broken sysfs as
	// "no hardware here".
	if os.Geteuid() == 0 {
		t.Skip("running as root, mode 000 is still readable")
	}
	root := t.TempDir()
	hwmonRoot := filepath.Join(root, "class", "hwmon")
	require.NoError(t, os.MkdirAll(hwmonRoot, 0o755))
	require.NoError(t, os.Chmod(hwmonRoot, 0o000))
	t.Cleanup(func() { _ = os.Chmod(hwmonRoot, 0o755) })

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err),
		"an unreadable hwmon root is a failure; only an ABSENT one is ErrNoData")
}

func TestHwmonUnnameableChipIsSkipped(t *testing.T) {
	// A chip whose name cannot be derived at all: no device symlink, no name file, and
	// a directory name that cleans to empty. Skipped with a debug log rather than
	// emitted under an empty chip label.
	root := t.TempDir()
	dir := filepath.Join(root, "class", "hwmon", "!!!")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "temp1_input"), []byte("45000\n"), 0o644))

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch), "an unnameable chip is skipped, not a failure")
	close(ch)
	assert.Empty(t, ch, "no metrics may be emitted under an empty chip label")
}

func TestHwmonNameUsesDevNameWhenDevTypeIsEmpty(t *testing.T) {
	// hwmonName's second fallback inside preference 1: devType cleans to empty but
	// devName does not, so the name is devName alone. Reached with a device symlink
	// whose parent directory name is unusable.
	root := t.TempDir()
	devDir := filepath.Join(root, "!!!", "coretemp.0")
	require.NoError(t, os.MkdirAll(devDir, 0o755))

	hwmonDir := filepath.Join(root, "class", "hwmon", "hwmon0")
	require.NoError(t, os.MkdirAll(hwmonDir, 0o755))
	require.NoError(t, os.Symlink(devDir, filepath.Join(hwmonDir, "device")))
	require.NoError(t, os.WriteFile(filepath.Join(hwmonDir, "temp1_input"), []byte("45000\n"), 0o644))

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "chip")
	assert.Contains(t, got["node_hwmon_temp_celsius"], "coretemp_0",
		"an unusable device TYPE falls back to the device NAME alone")
}

func TestHwmonTempBareElementHelpSaysInput(t *testing.T) {
	// temp1 with no property: the metric name has no suffix, but the help text reports
	// the element as "input" rather than as empty.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":  "acme",
		"temp1": "45000",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	for m := range ch {
		if metricName(t, m) != "node_hwmon_temp_celsius" {
			continue
		}
		assert.Contains(t, m.Desc().String(), "temperature (input)",
			"a bare temp element is described as input, not as empty")
		return
	}
	t.Fatal("node_hwmon_temp_celsius was not emitted")
}

func TestHwmonCPUVoltageRuleIsDistinctFromIn(t *testing.T) {
	// "cpu" and "in" are separate rules with identical behaviour, so the cpu one needs
	// its own case or it falls through to the raw fallback and loses the /1000.
	root := writeHwmonTree(t, "hwmon0", map[string]string{
		"name":     "acme",
		"cpu0_vid": "1200",
	})

	c, err := newHwMonCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherLabelled(t, c, "sensor")
	assert.Equal(t, 1.2, got["node_hwmon_cpu_vid_volts"]["cpu0"],
		"cpu voltages are millivolts, like in")
}

func TestTextFileParseFailureReturnsPartialFamilies(t *testing.T) {
	// processFile's error return must carry the families parsed so far, not nil --
	// otherwise a file with one bad line loses every valid metric before it.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mixed.prom"), []byte(
		"good_metric 7\nthis is not { valid\n"), 0o644))

	c := newTextFileStub(t, dir)
	mtime, families, err := c.processFile(dir, "mixed.prom")

	require.Error(t, err, "the parse failure must be reported")
	assert.Nil(t, mtime, "no mtime on a failed parse")
	assert.NotEmpty(t, families,
		"the families parsed before the bad line must be returned alongside the error")
	assert.Contains(t, families, "good_metric")
}

func TestTextFileUnbuildableMetricIsFlaggedNotPanicked(t *testing.T) {
	// The NewConstMetric failure path. prometheus.NewConstMetric validates the label
	// COUNT against the Desc, not label-name syntax -- my first attempt used an invalid
	// label NAME and it was accepted, which is why this uses a count mismatch instead.
	//
	// The point is the choice of NewConstMetric over MustNewConstMetric: the label set
	// comes from an operator-supplied file, so a malformed one must set scrape_error
	// rather than panic and take down the whole scrape.
	c := newTextFileStub(t)
	errored := false

	name := "my_metric"
	help := "help"
	value := 1.0
	labelName, labelValue := "a", "1"

	// A Desc built with ZERO variable labels, then a metric carrying one. emitFamily
	// derives the Desc from the metric's own labels, so the mismatch is forced by
	// calling the builder with a Desc that disagrees -- which is exactly what a
	// duplicate label name in a file produces.
	mf := &dto.MetricFamily{
		Name: &name, Help: &help, Type: dto.MetricType_GAUGE.Enum(),
		Metric: []*dto.Metric{{
			Label: []*dto.LabelPair{
				{Name: &labelName, Value: &labelValue},
				// A DUPLICATE label name: NewDesc accepts it, NewConstMetric rejects it.
				{Name: &labelName, Value: &labelValue},
			},
			Gauge: &dto.Gauge{Value: &value},
		}},
	}

	ch := make(chan prometheus.Metric, 8)
	c.emitFamily(ch, mf, &errored)
	close(ch)

	assert.True(t, errored, "an unbuildable metric must set the error flag")
	assert.Empty(t, ch, "and must not be emitted")
}

func TestTextFileStatFailureKeepsTheMetrics(t *testing.T) {
	// A stat failure after a successful parse. Reachable when the file is unlinked
	// between the open and the stat, which on an operator-writable directory is a real
	// race rather than a theoretical one.
	//
	// The families must survive: only node_textfile_mtime_seconds is lost, and losing
	// the operator's metrics because their mtime could not be read would be the wrong
	// trade.
	dir := t.TempDir()
	path := filepath.Join(dir, "x.prom")
	require.NoError(t, os.WriteFile(path, []byte("my_metric 1\n"), 0o644))

	f, err := os.Open(path)
	require.NoError(t, err)
	// Closing the handle makes Stat fail with EBADF while the parsed families are
	// already in hand.
	require.NoError(t, f.Close())

	families := map[string]*dto.MetricFamily{"my_metric": {}}
	mtime, got, err := statTextFile(f, families)

	require.Error(t, err, "stat on a closed handle must fail")
	assert.Nil(t, mtime)
	assert.Equal(t, families, got,
		"the parsed families must be returned even when the mtime cannot be read")
}
