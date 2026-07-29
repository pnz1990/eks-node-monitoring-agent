package hostmetrics

// Tests for the diskstats collector.
//
// Everything that can go wrong here is a WRONG VALUE, not a wrong name, so the
// tests assert numbers against a fixture with known inputs rather than checking
// that families exist. Three things get the most attention:
//
//   - the unit conversions, asserted per field with values chosen so a missed or
//     doubled conversion cannot coincidentally produce the right answer
//   - the discard-sectors asymmetry (read/write sectors are converted to bytes,
//     discard sectors are not), which is the easiest field here to "fix" into a bug
//   - the statCount truncation, asserted at all three kernel field counts, because
//     a short line must produce NO discard metrics rather than zero-valued ones

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs/blockdevice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixture's nvme1n1 line, field by field. Values are deliberately distinct
// and non-round so that a swapped pair of positional descriptors cannot pass.
const (
	fxReadIOs         = 100.0
	fxReadMerges      = 200.0
	fxReadSectors     = 300.0
	fxReadTicks       = 4000.0
	fxWriteIOs        = 500.0
	fxWriteMerges     = 600.0
	fxWriteSectors    = 700.0
	fxWriteTicks      = 8000.0
	fxIOsInProgress   = 9.0
	fxIOsTotalTicks   = 10000.0
	fxWeightedIOTicks = 11000.0
	fxDiscardIOs      = 1200.0
	fxDiscardMerges   = 1300.0
	fxDiscardSectors  = 1400.0
	fxDiscardTicks    = 15000.0
	fxFlushRequests   = 1600.0
	fxFlushTicks      = 17000.0
)

// --- unit conversions ------------------------------------------------------

func TestDiskstatsSectorsConvertedToBytesAt512(t *testing.T) {
	// The kernel reports "standard UNIX 512-byte sectors, not any device- or
	// filesystem-specific block size". Using an NVMe device's real 4096-byte block
	// size instead would make these 8x too large, and the metric would still have
	// the right name, type and labels.
	got := collectDiskstats(t)

	assert.Equal(t, fxReadSectors*512, got["node_disk_read_bytes_total"]["nvme1n1"],
		"read_bytes must be sectors * 512")
	assert.Equal(t, fxWriteSectors*512, got["node_disk_written_bytes_total"]["nvme1n1"],
		"written_bytes must be sectors * 512")
}

func TestDiskstatsDiscardSectorsAreNotConvertedToBytes(t *testing.T) {
	// THE ASYMMETRY. read/write sectors become *_bytes_total and are multiplied by
	// 512; discard sectors are reported RAW because the metric is named
	// discarded_SECTORS_total. All three are "sectors" in /proc/diskstats and only
	// two are converted, so "making it consistent" is a 512x error.
	got := collectDiskstats(t)

	assert.Equal(t, fxDiscardSectors, got["node_disk_discarded_sectors_total"]["nvme1n1"],
		"discarded_sectors_total is denominated in sectors and must NOT be multiplied by 512")
	assert.NotEqual(t, fxDiscardSectors*512, got["node_disk_discarded_sectors_total"]["nvme1n1"],
		"a 512x conversion here would be invisible to any name or label comparison")
}

func TestDiskstatsTicksConvertedToSeconds(t *testing.T) {
	// Every tick field is milliseconds. The fixture uses values that are exact
	// thousands so a missing division is obvious, but distinct from each other so a
	// positional swap is too.
	got := collectDiskstats(t)

	for metric, ticks := range map[string]float64{
		"node_disk_read_time_seconds_total":           fxReadTicks,
		"node_disk_write_time_seconds_total":          fxWriteTicks,
		"node_disk_io_time_seconds_total":             fxIOsTotalTicks,
		"node_disk_io_time_weighted_seconds_total":    fxWeightedIOTicks,
		"node_disk_discard_time_seconds_total":        fxDiscardTicks,
		"node_disk_flush_requests_time_seconds_total": fxFlushTicks,
	} {
		assert.Equal(t, ticks/1000.0, got[metric]["nvme1n1"], "%s must be ticks/1000", metric)
	}
}

func TestDiskstatsRawCountsPassThroughUnconverted(t *testing.T) {
	// Counts must not be scaled at all. Grouped separately from the converted
	// fields so that a conversion accidentally applied to a count shows up here.
	got := collectDiskstats(t)

	for metric, want := range map[string]float64{
		"node_disk_reads_completed_total":    fxReadIOs,
		"node_disk_reads_merged_total":       fxReadMerges,
		"node_disk_writes_completed_total":   fxWriteIOs,
		"node_disk_writes_merged_total":      fxWriteMerges,
		"node_disk_io_now":                   fxIOsInProgress,
		"node_disk_discards_completed_total": fxDiscardIOs,
		"node_disk_discards_merged_total":    fxDiscardMerges,
		"node_disk_flush_requests_total":     fxFlushRequests,
	} {
		assert.Equal(t, want, got[metric]["nvme1n1"], "%s must pass through unscaled", metric)
	}
}

// --- the statCount truncation ---------------------------------------------

func TestDiskstatsShortLinesTruncateRatherThanZeroFill(t *testing.T) {
	// /proc/diskstats has 14 fields pre-4.18, 18 on 4.18+, 20 on 5.5+. The fixture
	// has one device at each count. A kernel that does not report discards must
	// produce NO discard metrics -- emitting 0 would assert "this disk has never
	// discarded" when the truth is "this kernel does not say", and rate() would
	// graph that lie as a confident flat line.
	got := collectDiskstats(t)

	// nvme1n1: 20 fields -> 17 stats -> everything including flush.
	assert.Contains(t, got["node_disk_flush_requests_total"], "nvme1n1")
	assert.Contains(t, got["node_disk_discards_completed_total"], "nvme1n1")

	// dm-0: 18 fields -> 15 stats -> discards but NOT flush.
	assert.Contains(t, got["node_disk_discards_completed_total"], "dm-0",
		"an 18-field kernel reports discards")
	assert.NotContains(t, got["node_disk_flush_requests_total"], "dm-0",
		"an 18-field kernel does not report flush; a zero here would be fabricated")
	assert.NotContains(t, got["node_disk_flush_requests_time_seconds_total"], "dm-0")

	// sdz: 14 fields -> 11 stats -> neither discards nor flush.
	assert.Contains(t, got["node_disk_reads_completed_total"], "sdz",
		"a 14-field kernel still reports the basic counters")
	assert.Contains(t, got["node_disk_io_time_weighted_seconds_total"], "sdz",
		"field 14 is the last one a pre-4.18 kernel reports and must be included")
	for _, absent := range []string{
		"node_disk_discards_completed_total",
		"node_disk_discards_merged_total",
		"node_disk_discarded_sectors_total",
		"node_disk_discard_time_seconds_total",
		"node_disk_flush_requests_total",
		"node_disk_flush_requests_time_seconds_total",
	} {
		assert.NotContains(t, got[absent], "sdz",
			"%s must be absent for a 14-field kernel, not zero", absent)
	}
}

func TestDiskstatsTruncationBoundaryIsExact(t *testing.T) {
	// Off-by-one at the boundary is the likely failure: the last included field for
	// a 14-field line is io_time_weighted (index 10), and the first excluded is
	// discards_completed (index 11). Asserted as a count so an off-by-one in either
	// direction fails.
	got := collectDiskstats(t)

	present := 0
	for metric := range got {
		if _, ok := got[metric]["sdz"]; ok {
			present++
		}
	}
	// 11 positional stats + node_disk_info.
	assert.Equal(t, 12, present,
		"a 14-field line must yield exactly 11 stat metrics plus info")
}

// --- the positional pairing -----------------------------------------------

func TestDiskstatsDescsAndValuesHaveEqualLength(t *testing.T) {
	// descs and values are paired BY INDEX. If they ever differ in length, the
	// shorter one silently truncates the other and metrics get wrong values with no
	// error anywhere.
	assert.Len(t, diskstatsDescs(), 17,
		"/proc/diskstats has 17 statistic fields on kernel 5.5+")
	assert.Len(t, diskstatsValues(&blockdevice.Diskstats{}), len(diskstatsDescs()),
		"values and descs must stay the same length or they mispair by index")
}

func TestDiskstatsDescOrderMatchesUpstream(t *testing.T) {
	// The order is load-bearing twice over: values pair by index, and truncation
	// drops from the END. Compared against upstream's descs slice so a reordering
	// is caught mechanically rather than by eye.
	data, err := os.ReadFile("../../../node_exporter/collector/diskstats_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}

	// Upstream's descs slice mixes inline NewDesc calls with references to shared
	// vars in diskstats_common.go, so resolve both forms to a metric name.
	common, err := os.ReadFile("../../../node_exporter/collector/diskstats_common.go")
	require.NoError(t, err)
	sharedNames := map[string]string{}
	for _, m := range regexp.MustCompile(
		`(\w+Desc)\s*=\s*prometheus\.NewDesc\(\s*prometheus\.BuildFQName\(namespace,\s*diskSubsystem,\s*"([^"]+)"`,
	).FindAllStringSubmatch(string(common), -1) {
		sharedNames[m[1]] = m[2]
	}
	require.NotEmpty(t, sharedNames, "extracted no shared descs from upstream; the regexp may be stale")

	body := regexp.MustCompile(`(?s)descs: \[\]typedDesc\{(.*?)\n\t\t\},\n\t\tfilesystemInfoDesc`).
		FindStringSubmatch(string(data))
	require.NotNil(t, body, "failed to locate upstream's descs slice; the regexp may be stale")

	// One entry per `desc:` in order, resolved to its metric name.
	var upstream []string
	for _, m := range regexp.MustCompile(
		`desc:\s*(?:(\w+Desc)|prometheus\.NewDesc\(\s*\n?\s*prometheus\.BuildFQName\(namespace,\s*diskSubsystem,\s*"([^"]+)"\))`,
	).FindAllStringSubmatch(body[1], -1) {
		if m[1] != "" {
			name, ok := sharedNames[m[1]]
			require.True(t, ok, "unresolved shared desc %q", m[1])
			upstream = append(upstream, name)
			continue
		}
		upstream = append(upstream, m[2])
	}
	require.Len(t, upstream, 17, "expected 17 positional descs upstream, got %v", upstream)

	var ours []string
	for _, d := range diskstatsDescs() {
		ours = append(ours, fqName(d.desc.String()))
	}

	assert.Equal(t, upstream, ours,
		"positional desc ORDER must match upstream exactly: values pair by index and truncation drops from the end")
}

func TestDiskstatsIoNowIsAGaugeNotACounter(t *testing.T) {
	// io_now is a queue depth: it goes up AND down. Typed as a counter, rate() would
	// treat every decrease as a counter reset and produce garbage. It is the only
	// gauge in the positional set, so it is the one likely to be typed wrong.
	for i, d := range diskstatsDescs() {
		name := fqName(d.desc.String())
		if name == "io_now" {
			assert.Equal(t, prometheus.GaugeValue, d.valueType, "io_now must be a gauge")
			continue
		}
		assert.Equal(t, prometheus.CounterValue, d.valueType,
			"positional desc %d (%s) must be a counter", i, name)
	}
}

// --- device filtering -----------------------------------------------------

func TestDiskstatsDefaultExcludeMatchesUpstreamVerbatim(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/diskstats_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	m := regexp.MustCompile(`diskstatsDefaultIgnoredDevices\s*=\s*"([^"]+)"`).FindSubmatch(data)
	require.NotNil(t, m, "failed to extract upstream's exclude default; the regexp may be stale")

	// Upstream writes it as an interpreted string literal with escaped backslashes;
	// ours is a raw literal. Unquote so the comparison is of the PATTERN, not of
	// the source syntax.
	assert.Equal(t, strings.ReplaceAll(string(m[1]), `\\`, `\`), defDiskstatsDeviceExclude,
		"our copy of upstream's exclude default must be verbatim")
}

func TestDiskstatsDefaultExcludeDropsPartitionsKeepsWholeDisks(t *testing.T) {
	// The distinction that matters: partitions are excluded because their counters
	// double-count the whole disk's, but the whole disk must be kept. The trailing
	// \d+$ is what separates them, so an over-eager simplification of this regexp
	// silently stops reporting disk I/O entirely.
	rx := regexp.MustCompile(defDiskstatsDeviceExclude)

	for _, dev := range []string{
		"nvme0n1p1", "nvme0n1p127", "sda1", "vda2", "xvda1", "hdb3",
		"loop0", "ram0", "zram0", "fd0",
	} {
		assert.True(t, rx.MatchString(dev), "%q must be excluded", dev)
	}
	for _, dev := range []string{
		"nvme0n1", "nvme1n1", "sda", "xvda", "vda", "dm-0", "md0", "sdz",
	} {
		assert.False(t, rx.MatchString(dev), "%q is a real device and must NOT be excluded", dev)
	}
}

func TestDiskstatsExcludedDevicesAreAbsentFromOutput(t *testing.T) {
	// End to end through the collector rather than just the regexp, so a filter
	// that is compiled but never applied fails.
	got := collectDiskstats(t)
	for _, dev := range []string{"loop0", "ram0", "nvme1n1p1"} {
		assert.NotContains(t, got["node_disk_reads_completed_total"], dev,
			"%q must be filtered out", dev)
	}
	assert.Contains(t, got["node_disk_reads_completed_total"], "nvme1n1")
}

func TestDiskstatsIncludeFilterIsExclusive(t *testing.T) {
	// With an include pattern, everything not matching is dropped -- including
	// devices the default exclude would have kept.
	c := newDiskstatsFixtureCollector(t, "", "^dm-")
	got := gatherDiskstats(t, c)

	assert.Contains(t, got["node_disk_reads_completed_total"], "dm-0")
	assert.NotContains(t, got["node_disk_reads_completed_total"], "nvme1n1",
		"an include pattern must drop non-matching devices")
}

func TestDiskstatsExcludeAndIncludeAreMutuallyExclusive(t *testing.T) {
	// Failing at construction rather than picking a precedence: an operator who set
	// both cannot tell which metric set they got, and the two differ.
	_, err := newDiskstatsCollectorWithFilter(quietLogger(), fixtureDiskstatsPaths(t), "^loop", "^dm-")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestDiskstatsInvalidFilterPatternsRejectedAtConstruction(t *testing.T) {
	// Upstream uses regexp.MustCompile here and PANICS on a bad pattern, which is
	// tolerable because kingpin parses before any collector is built. These patterns
	// can come from a Helm value, and a panic on first scrape inside a DaemonSet is
	// a crash loop with no useful message.
	_, err := newDiskstatsCollectorWithFilter(quietLogger(), fixtureDiskstatsPaths(t), "([unclosed", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid device exclude pattern")

	_, err = newDiskstatsCollectorWithFilter(quietLogger(), fixtureDiskstatsPaths(t), "", "([unclosed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid device include pattern")
}

// --- udev labels ----------------------------------------------------------

func TestDiskstatsUdevSerialFallbackOrder(t *testing.T) {
	// The order changes the label VALUE on real hardware, so it breaks any
	// dashboard joining on serial. Each key is the one udev populates for a
	// different device class: SCSI, generic, virtio.
	assert.Equal(t, "scsi-serial", udevSerial(udevInfo{
		udevSCSIIdentSerial: "scsi-serial",
		udevIDSerialShort:   "short",
		udevIDSerial:        "long",
	}), "SCSI_IDENT_SERIAL wins: it is the serial printed on the disk label")

	assert.Equal(t, "short", udevSerial(udevInfo{
		udevIDSerialShort: "short",
		udevIDSerial:      "long",
	}), "ID_SERIAL_SHORT is second")

	assert.Equal(t, "long", udevSerial(udevInfo{
		udevIDSerial: "long",
	}), "ID_SERIAL is the virtio fallback")

	assert.Empty(t, udevSerial(udevInfo{}))
	// An empty value must fall through rather than win, or a device with an empty
	// SCSI serial would report no serial at all despite having one.
	assert.Equal(t, "short", udevSerial(udevInfo{
		udevSCSIIdentSerial: "",
		udevIDSerialShort:   "short",
	}))
}

func TestDiskstatsUdevPropertiesParsing(t *testing.T) {
	dir := t.TempDir()
	// Real udev data files interleave device properties ("E:") with bookkeeping
	// lines ("S:", "W:", "I:", "G:"), which must be skipped.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b259:0"), []byte(
		"S:disk/by-id/nvme-Amazon_Elastic_Block_Store\n"+
			"W:0\n"+
			"I:1234567\n"+
			"E:ID_MODEL=Amazon Elastic Block Store\n"+
			"E:ID_SERIAL_SHORT=vol0123456789abcdef\n"+
			"E:ID_FS_TYPE=xfs\n"+
			"G:systemd\n"+
			"E:MALFORMED_NO_EQUALS\n"+
			"E:ID_PATH=pci-0000:00:04.0-nvme-1\n",
	), 0o644))

	info, err := readUdevProperties(dir, 259, 0)
	require.NoError(t, err)

	assert.Equal(t, "Amazon Elastic Block Store", info[udevIDModel])
	assert.Equal(t, "vol0123456789abcdef", info[udevIDSerialShort])
	assert.Equal(t, "xfs", info[udevIDFSType])
	assert.Equal(t, "pci-0000:00:04.0-nvme-1", info[udevIDPath],
		"a malformed line must not stop parsing of subsequent ones")
	// A value containing "=" must keep it: Cut splits on the FIRST separator only.
	assert.NotContains(t, info, "MALFORMED_NO_EQUALS",
		"a property line without '=' must be skipped, not stored with an empty value")
	// Non-property lines must not leak in under any key.
	assert.NotContains(t, info, "S")
	assert.NotContains(t, info, "disk/by-id/nvme-Amazon_Elastic_Block_Store")
}

func TestDiskstatsUdevPropertyValueMayContainEquals(t *testing.T) {
	// strings.Cut splits on the first "=" only. Splitting on all of them would
	// truncate any value containing one, and udev writes base64 and paths.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b8:0"),
		[]byte("E:ID_WWN=nvme.1d0f-abc=def==\n"), 0o644))

	info, err := readUdevProperties(dir, 8, 0)
	require.NoError(t, err)
	assert.Equal(t, "nvme.1d0f-abc=def==", info[udevIDWWN])
}

func TestDiskstatsUdevMissingFileIsNotACollectorFailure(t *testing.T) {
	// A device with no udev entry is normal. The info labels come out empty and the
	// counters must be unaffected -- losing disk I/O because udev has no record of
	// a device would be a far worse failure than an empty label.
	_, err := readUdevProperties(t.TempDir(), 1, 2)
	require.Error(t, err)

	c := newDiskstatsFixtureCollector(t, defDiskstatsDeviceExclude, "")
	c.udevProperties = func(uint32, uint32) (udevInfo, error) { return nil, assert.AnError }

	got := gatherDiskstats(t, c)
	assert.Equal(t, fxReadIOs, got["node_disk_reads_completed_total"]["nvme1n1"],
		"counters must survive a udev read failure")
	assert.Contains(t, got["node_disk_info"], "nvme1n1", "info is still emitted with empty labels")
}

func TestDiskstatsUdevDirectoryAbsentDisablesLookupWithoutFailing(t *testing.T) {
	paths := fixtureDiskstatsPaths(t)
	paths.UdevData = filepath.Join(t.TempDir(), "absent")

	c, err := newDiskstatsCollectorWithFilter(quietLogger(), paths, defDiskstatsDeviceExclude, "")
	require.NoError(t, err, "a missing udev directory must not fail construction")
	assert.Nil(t, c.(*diskstatsCollector).udevProperties,
		"lookup must be disabled entirely rather than failing an open per device per scrape")

	got := gatherDiskstats(t, c.(*diskstatsCollector))
	assert.Equal(t, fxReadIOs, got["node_disk_reads_completed_total"]["nvme1n1"])
}

func TestDiskstatsUdevPathIsAFileNotADirectory(t *testing.T) {
	// os.Stat succeeds but IsDir is false. Distinct from the absent case and
	// reached through the || in the same condition.
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	paths := fixtureDiskstatsPaths(t)
	paths.UdevData = file

	c, err := newDiskstatsCollectorWithFilter(quietLogger(), paths, defDiskstatsDeviceExclude, "")
	require.NoError(t, err)
	assert.Nil(t, c.(*diskstatsCollector).udevProperties)
}

func TestDiskstatsUdevTruncatedReadIsReported(t *testing.T) {
	// A line longer than the scanner's buffer makes Scan fail. Upstream ignores
	// scanner.Err() and returns the partial map silently; here the error is
	// returned so a disk reporting a missing serial is distinguishable from a disk
	// that has none. The caller logs it and keeps the counters either way.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b9:0"),
		[]byte("E:ID_MODEL="+strings.Repeat("x", 128*1024)+"\n"), 0o644))

	info, err := readUdevProperties(dir, 9, 0)
	require.Error(t, err, "a truncated udev read must be reported, not silently partial")
	assert.Contains(t, err.Error(), "failed to read udev data")
	assert.NotNil(t, info, "the partial map is still returned so labels degrade rather than vanish")
}

// --- info labels ----------------------------------------------------------

func TestDiskstatsInfoLabelsPopulatedFromUdev(t *testing.T) {
	c := newDiskstatsFixtureCollector(t, defDiskstatsDeviceExclude, "")
	c.udevProperties = func(major, minor uint32) (udevInfo, error) {
		return udevInfo{
			udevIDPath:        "pci-0000:00:04.0-nvme-1",
			udevIDWWN:         "nvme.1d0f-abc",
			udevIDModel:       "Amazon Elastic Block Store",
			udevIDSerialShort: "vol0123",
			udevIDRevision:    "1.0",
		}, nil
	}
	c.rotational = func(string) string { return "0" }

	labels := diskInfoLabels(t, c, "nvme1n1")
	assert.Equal(t, "pci-0000:00:04.0-nvme-1", labels["path"])
	assert.Equal(t, "nvme.1d0f-abc", labels["wwn"])
	assert.Equal(t, "Amazon Elastic Block Store", labels["model"])
	assert.Equal(t, "vol0123", labels["serial"])
	assert.Equal(t, "1.0", labels["revision"])
	assert.Equal(t, "0", labels["rotational"])
	// major/minor come from /proc/diskstats, not udev.
	assert.Equal(t, "259", labels["major"])
	assert.Equal(t, "0", labels["minor"])
}

func TestDiskstatsRotationalLabel(t *testing.T) {
	// Every EKS device is non-rotational, so the "1" branch is unreachable on any
	// host this runs on and is only testable through the seam.
	c := newDiskstatsFixtureCollector(t, defDiskstatsDeviceExclude, "")
	c.rotational = func(string) string { return "1" }
	assert.Equal(t, "1", diskInfoLabels(t, c, "nvme1n1")["rotational"])
}

func TestDiskstatsRotationalDefaultsToZeroOnError(t *testing.T) {
	// The production closure, not an injected one: a fixture sysfs has no
	// queue/rotational file, so the read fails. Upstream previously
	// zero-initialised the struct on error, which also yielded "0"; preserved so
	// the label does not change for a device with no queue directory.
	c := newDiskstatsFixtureCollector(t, defDiskstatsDeviceExclude, "")
	assert.Equal(t, "0", c.rotational("nvme1n1"),
		"an unreadable rotational flag must report 0, matching upstream")
	assert.Equal(t, "0", c.rotational("no-such-device"))
}

func TestDiskstatsRotationalReadsRealSysfsValue(t *testing.T) {
	// Drives the real SysBlockDeviceRotational path with both values present, so
	// the "1" branch of the production closure is covered rather than only the
	// error default.
	paths := fixtureDiskstatsPaths(t)
	for dev, value := range map[string]string{"spinner": "1", "ssd": "0"} {
		dir := filepath.Join(paths.SysFS, "block", dev, "queue")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "rotational"), []byte(value+"\n"), 0o644))
	}

	c, err := newDiskstatsCollectorWithFilter(quietLogger(), paths, defDiskstatsDeviceExclude, "")
	require.NoError(t, err)

	assert.Equal(t, "1", c.(*diskstatsCollector).rotational("spinner"))
	assert.Equal(t, "0", c.(*diskstatsCollector).rotational("ssd"))
}

// --- conditional families -------------------------------------------------

func TestDiskstatsFilesystemInfoOnlyWhenUdevReportsFSType(t *testing.T) {
	// Gated on ID_FS_TYPE so a raw block device does not report an empty
	// filesystem type as though it had one.
	c := newDiskstatsFixtureCollector(t, defDiskstatsDeviceExclude, "")
	c.udevProperties = func(uint32, uint32) (udevInfo, error) { return udevInfo{}, nil }
	assert.NotContains(t, gatherDiskstats(t, c), "node_disk_filesystem_info")

	c.udevProperties = func(uint32, uint32) (udevInfo, error) {
		return udevInfo{
			udevIDFSType:    "xfs",
			udevIDFSUsage:   "filesystem",
			udevIDFSUUID:    "1234-5678",
			udevIDFSVersion: "5",
		}, nil
	}
	got := gatherDiskstats(t, c)
	require.Contains(t, got, "node_disk_filesystem_info")
	assert.Contains(t, got["node_disk_filesystem_info"], "nvme1n1")
}

func TestDiskstatsDeviceMapperInfoOnlyWhenUdevReportsDMName(t *testing.T) {
	c := newDiskstatsFixtureCollector(t, defDiskstatsDeviceExclude, "")
	c.udevProperties = func(uint32, uint32) (udevInfo, error) { return udevInfo{}, nil }
	assert.NotContains(t, gatherDiskstats(t, c), "node_disk_device_mapper_info")

	c.udevProperties = func(uint32, uint32) (udevInfo, error) {
		return udevInfo{
			udevDMName:    "vg0-lv0",
			udevDMUUID:    "LVM-abc",
			udevDMVGName:  "vg0",
			udevDMLVName:  "lv0",
			udevDMLVLayer: "",
		}, nil
	}
	assert.Contains(t, gatherDiskstats(t, c), "node_disk_device_mapper_info")
}

func TestDiskstatsATAMetricsGatedOnIDATA(t *testing.T) {
	// Without the ID_ATA gate an NVMe device would report
	// ata_rotation_rate_rpm=0, which reads as a measurement ("this disk does not
	// spin") rather than as "this is not an ATA disk".
	c := newDiskstatsFixtureCollector(t, defDiskstatsDeviceExclude, "")
	c.udevProperties = func(uint32, uint32) (udevInfo, error) {
		// Attributes present but ID_ATA absent: must emit nothing.
		return udevInfo{
			udevIDATAWriteCache:      "1",
			udevIDATARotationRateRPM: "7200",
		}, nil
	}
	got := gatherDiskstats(t, c)
	assert.NotContains(t, got, "node_disk_ata_write_cache")
	assert.NotContains(t, got, "node_disk_ata_rotation_rate_rpm")

	c.udevProperties = func(uint32, uint32) (udevInfo, error) {
		return udevInfo{
			udevIDATA:                  "1",
			udevIDATAWriteCache:        "1",
			udevIDATAWriteCacheEnabled: "0",
			udevIDATARotationRateRPM:   "7200",
		}, nil
	}
	got = gatherDiskstats(t, c)
	assert.Equal(t, 1.0, got["node_disk_ata_write_cache"]["nvme1n1"])
	assert.Equal(t, 0.0, got["node_disk_ata_write_cache_enabled"]["nvme1n1"])
	assert.Equal(t, 7200.0, got["node_disk_ata_rotation_rate_rpm"]["nvme1n1"])
}

func TestDiskstatsATAMissingAttributeIsSkipped(t *testing.T) {
	c := newDiskstatsFixtureCollector(t, defDiskstatsDeviceExclude, "")
	c.udevProperties = func(uint32, uint32) (udevInfo, error) {
		return udevInfo{udevIDATA: "1", udevIDATAWriteCache: "1"}, nil
	}
	got := gatherDiskstats(t, c)

	assert.Contains(t, got, "node_disk_ata_write_cache")
	assert.NotContains(t, got, "node_disk_ata_rotation_rate_rpm",
		"an absent attribute must be skipped, not emitted as zero")
}

func TestDiskstatsATAUnparseableValueIsSkipped(t *testing.T) {
	// A present-but-unparseable attribute means udev wrote something unexpected.
	// Skip the metric rather than emit a fabricated number, and log at Error --
	// unlike a missing attribute, this is worth surfacing.
	c := newDiskstatsFixtureCollector(t, defDiskstatsDeviceExclude, "")
	c.udevProperties = func(uint32, uint32) (udevInfo, error) {
		return udevInfo{
			udevIDATA:                "1",
			udevIDATAWriteCache:      "not-a-number",
			udevIDATARotationRateRPM: "7200",
		}, nil
	}
	got := gatherDiskstats(t, c)

	assert.NotContains(t, got, "node_disk_ata_write_cache")
	assert.Equal(t, 7200.0, got["node_disk_ata_rotation_rate_rpm"]["nvme1n1"],
		"one bad attribute must not suppress the others")
}

// --- failure paths --------------------------------------------------------

func TestDiskstatsMissingProcfsFile(t *testing.T) {
	paths := fixtureDiskstatsPaths(t)
	require.NoError(t, os.Remove(filepath.Join(paths.ProcFS, "diskstats")))

	c, err := newDiskstatsCollectorWithFilter(quietLogger(), paths, defDiskstatsDeviceExclude, "")
	require.NoError(t, err, "construction must not read diskstats")

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get diskstats")
}

func TestDiskstatsConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newDiskstatsCollectorWithFilter(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent"), SysFS: t.TempDir()}.withDefaults(),
		defDiskstatsDeviceExclude, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs/sysfs")
}

func TestDiskstatsRegisteredWithDefaultsFromPaths(t *testing.T) {
	// The registered constructor, not the seam, so the default wiring is covered:
	// a typo in defDiskstatsDeviceExclude at that call site would otherwise be
	// invisible to every other test here.
	c, err := newDiskstatsCollector(quietLogger(), fixtureDiskstatsPaths(t))
	require.NoError(t, err)
	got := gatherDiskstats(t, c.(*diskstatsCollector))
	assert.Contains(t, got["node_disk_reads_completed_total"], "nvme1n1")
	assert.NotContains(t, got["node_disk_reads_completed_total"], "loop0",
		"the default exclude must be applied by the registered constructor")
}

// --- helpers --------------------------------------------------------------

// fixtureDiskstatsPaths builds Paths over a copy of the checked-in
// /proc/diskstats fixture, plus an empty sysfs and udev directory. Copied to a
// temp dir per test so a test that mutates it cannot affect another.
func fixtureDiskstatsPaths(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	procDir := filepath.Join(root, "proc")
	sysDir := filepath.Join(root, "sys")
	udevDir := filepath.Join(root, "udev")
	for _, d := range []string{procDir, sysDir, udevDir} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	data, err := os.ReadFile("testdata/proc/diskstats")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "diskstats"), data, 0o644))

	return Paths{ProcFS: procDir, SysFS: sysDir, RootFS: root, UdevData: udevDir}
}

func newDiskstatsFixtureCollector(t *testing.T, excludeExpr, includeExpr string) *diskstatsCollector {
	t.Helper()
	c, err := newDiskstatsCollectorWithFilter(quietLogger(), fixtureDiskstatsPaths(t), excludeExpr, includeExpr)
	require.NoError(t, err)
	return c.(*diskstatsCollector)
}

// collectDiskstats runs the collector over the fixture with default filters.
func collectDiskstats(t *testing.T) map[string]map[string]float64 {
	t.Helper()
	return gatherDiskstats(t, newDiskstatsFixtureCollector(t, defDiskstatsDeviceExclude, ""))
}

// gatherDiskstats returns metric name -> device -> value.
//
// Keyed by the DEVICE label rather than by position so an assertion cannot pass
// by reading the wrong device's value.
func gatherDiskstats(t *testing.T, c *diskstatsCollector) map[string]map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 4096)
	require.NoError(t, c.Update(ch))
	close(ch)

	out := map[string]map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))

		name := metricName(t, m)
		device := ""
		for _, l := range pb.GetLabel() {
			if l.GetName() == "device" {
				device = l.GetValue()
			}
		}
		if out[name] == nil {
			out[name] = map[string]float64{}
		}
		if pb.Gauge != nil {
			out[name][device] = pb.GetGauge().GetValue()
			continue
		}
		out[name][device] = pb.GetCounter().GetValue()
	}
	return out
}

// diskInfoLabels returns node_disk_info's labels for one device.
func diskInfoLabels(t *testing.T, c *diskstatsCollector, device string) map[string]string {
	t.Helper()

	ch := make(chan prometheus.Metric, 4096)
	require.NoError(t, c.Update(ch))
	close(ch)

	for m := range ch {
		if metricName(t, m) != "node_disk_info" {
			continue
		}
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))

		labels := map[string]string{}
		for _, l := range pb.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		if labels["device"] == device {
			return labels
		}
	}
	t.Fatalf("no node_disk_info for device %q", device)
	return nil
}

// metricName extracts the fully-qualified name from a metric.
//
// Parsed from Desc().String() because prometheus.Desc exposes no name accessor.
// Anchored on `fqName: "` rather than on the first quoted string in the
// description, which would match the help text.
func metricName(t *testing.T, m prometheus.Metric) string {
	t.Helper()
	match := regexp.MustCompile(`fqName: "([^"]+)"`).FindStringSubmatch(m.Desc().String())
	require.NotNil(t, match, "could not parse fqName from %q", m.Desc().String())
	return match[1]
}

// fqName extracts the metric name from a Desc string and strips the
// "node_disk_" prefix, for comparing against upstream's BuildFQName arguments.
func fqName(descString string) string {
	match := regexp.MustCompile(`fqName: "node_disk_([^"]+)"`).FindStringSubmatch(descString)
	if match == nil {
		return ""
	}
	return match[1]
}
