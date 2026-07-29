package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/diskstats_linux.go
//                 node_exporter/collector/diskstats_common.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015, 2019 The Prometheus Authors, Apache-2.0
//
// THE TRAPS IN THIS COLLECTOR are all value-correctness rather than shape. The
// metric names come out right no matter what you do; the numbers do not.
//
//  1. UNIT CONVERSIONS. Sectors are UNIX 512-byte sectors regardless of the
//     device's actual block size, and ticks are milliseconds. Get either wrong and
//     the metric exists with the right name, type and labels and reports a number
//     that is off by a constant factor. `read_bytes_total` off by 8x looks exactly
//     like a busy disk.
//
//  2. THE DISCARD-SECTORS ASYMMETRY. ReadSectors and WriteSectors are multiplied
//     by 512 to become *_bytes_total. DiscardSectors is NOT — it is emitted raw as
//     `discarded_sectors_total`, because the metric is named in sectors. Both are
//     "sectors" in /proc/diskstats and only one is converted. This is the single
//     easiest field in the file to "fix" into a bug, so it is asserted explicitly.
//
//  3. THE statCount TRUNCATION. /proc/diskstats has 14 fields on kernels before
//     4.18, 18 on 4.18+, and 20 on 5.5+. Upstream emits only as many metrics as
//     the kernel actually reported, so a short line must NOT produce zero-valued
//     discard and flush counters. Zero discards and unknown discards are different
//     claims, and a zero would be a lie that rate() happily graphs as a flat line.
//     The ordering of the value slice is therefore load-bearing: it must match
//     /proc/diskstats field order exactly, because truncation is positional.
//
// SCOPE. --collector.diskstats.device-include and the deprecated
// --collector.diskstats.ignored-devices alias are not wired to flags (this package
// has no flag layer by design); the filter is constructed directly. The exclude
// default is upstream's, verbatim.
//
// EKS NOTE. Unlike the filesystem collector, no EKS-specific exclusion is added.
// Verified against the live cluster golden corpus: a node reports exactly one
// block device (nvme0n1), because EBS volumes are whole devices and containers do
// not create block devices. There is no churn cardinality problem here to solve,
// and upstream's default already excludes the loop/ram/partition devices that
// would cause one.

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs/blockdevice"
)

const (
	diskSubsystem = "disk"

	// secondsPerTick converts /proc/diskstats tick fields, which are milliseconds.
	secondsPerTick = 1.0 / 1000.0

	// unixSectorSize is 512 because the kernel reports "standard UNIX 512-byte
	// sectors, not any device- or filesystem-specific block size".
	// See https://www.kernel.org/doc/Documentation/block/stat.txt
	//
	// It is NOT the device's logical or physical block size. An NVMe device with
	// 4096-byte blocks still reports 512-byte sectors here, so reading the real
	// block size from sysfs and using it would produce numbers 8x too large.
	unixSectorSize = 512.0

	// defDiskstatsDeviceExclude is upstream's device-exclude default, verbatim.
	// It drops ram/zram, loopback, floppy, and PARTITIONS of whole disks
	// (sda1, nvme0n1p1) while keeping the whole disks themselves — note the
	// trailing \d+$, which is what makes "nvme0n1p1" match and "nvme0n1" not.
	defDiskstatsDeviceExclude = `^(z?ram|loop|fd|(h|s|v|xv)d[a-z]|nvme\d+n\d+p)\d+$`

	// udevDevicePropertyPrefix marks a device property line in a udev data file.
	// See udevadm(8).
	udevDevicePropertyPrefix = "E:"
)

// Udev device property keys. Named constants rather than inline strings so a typo
// is a compile error instead of a permanently empty label.
const (
	udevDMLVLayer              = "DM_LV_LAYER"
	udevDMLVName               = "DM_LV_NAME"
	udevDMName                 = "DM_NAME"
	udevDMUUID                 = "DM_UUID"
	udevDMVGName               = "DM_VG_NAME"
	udevIDATA                  = "ID_ATA"
	udevIDATARotationRateRPM   = "ID_ATA_ROTATION_RATE_RPM"
	udevIDATAWriteCache        = "ID_ATA_WRITE_CACHE"
	udevIDATAWriteCacheEnabled = "ID_ATA_WRITE_CACHE_ENABLED"
	udevIDFSType               = "ID_FS_TYPE"
	udevIDFSUsage              = "ID_FS_USAGE"
	udevIDFSUUID               = "ID_FS_UUID"
	udevIDFSVersion            = "ID_FS_VERSION"
	udevIDModel                = "ID_MODEL"
	udevIDPath                 = "ID_PATH"
	udevIDRevision             = "ID_REVISION"
	udevIDSerial               = "ID_SERIAL"
	udevIDSerialShort          = "ID_SERIAL_SHORT"
	udevIDWWN                  = "ID_WWN"
	udevSCSIIdentSerial        = "SCSI_IDENT_SERIAL"
)

// udevInfo is one device's udev properties.
type udevInfo map[string]string

func init() {
	register("diskstats", true, newDiskstatsCollector)
}

type diskstatsCollector struct {
	fs           blockdevice.FS
	logger       *slog.Logger
	deviceFilter deviceFilter

	infoDesc             typedDesc
	filesystemInfoDesc   typedDesc
	deviceMapperInfoDesc typedDesc
	ataDescs             map[string]typedDesc

	// descs is indexed positionally against /proc/diskstats field order. See
	// diskstatsValues: the pairing is by index, so reordering either one silently
	// swaps two metrics' values.
	descs []typedDesc

	// udevProperties is nil when the udev data directory is unreadable, which
	// makes the info labels empty rather than failing the collector. Injectable so
	// tests can drive the label-fallback logic without a real udev database.
	udevProperties func(major, minor uint32) (udevInfo, error)

	// rotational reads the device's rotational flag. Injectable for the same
	// reason: every EKS device is non-rotational, so the "1" branch is otherwise
	// unreachable on any host this runs on.
	rotational func(dev string) string
}

func newDiskstatsCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	return newDiskstatsCollectorWithFilter(logger, paths, defDiskstatsDeviceExclude, "")
}

// newDiskstatsCollectorWithFilter is split out so the invalid-pattern branches are
// reachable from a test, and so the filter can be wired to chart values later
// without restructuring.
func newDiskstatsCollectorWithFilter(logger *slog.Logger, paths Paths, excludeExpr, includeExpr string) (Collector, error) {
	fs, err := blockdevice.NewFS(paths.ProcFS, paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs/sysfs at %s, %s: %w", paths.ProcFS, paths.SysFS, err)
	}

	// Upstream treats exclude and include as mutually exclusive at the flag layer.
	// Reproduced here as a construction error rather than a silent precedence rule,
	// because "include wins" and "exclude wins" give different metric sets and an
	// operator who set both cannot tell which they got.
	if excludeExpr != "" && includeExpr != "" {
		return nil, fmt.Errorf("device-exclude and device-include are mutually exclusive")
	}

	filter, err := newDeviceFilter(excludeExpr, includeExpr)
	if err != nil {
		return nil, fmt.Errorf("failed to build diskstats device filter: %w", err)
	}

	c := &diskstatsCollector{
		fs:           fs,
		logger:       logger,
		deviceFilter: filter,
		infoDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, diskSubsystem, "info"),
				"Info of /sys/block/<block_device>.",
				[]string{"device", "major", "minor", "path", "wwn", "model", "serial", "revision", "rotational"},
				nil,
			),
			valueType: prometheus.GaugeValue,
		},
		filesystemInfoDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, diskSubsystem, "filesystem_info"),
				"Info about disk filesystem.",
				[]string{"device", "type", "usage", "uuid", "version"},
				nil,
			),
			valueType: prometheus.GaugeValue,
		},
		deviceMapperInfoDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, diskSubsystem, "device_mapper_info"),
				"Info about disk device mapper.",
				[]string{"device", "name", "uuid", "vg_name", "lv_name", "lv_layer"},
				nil,
			),
			valueType: prometheus.GaugeValue,
		},
		ataDescs: map[string]typedDesc{
			udevIDATAWriteCache: {
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "ata_write_cache"),
					"ATA disk has a write cache.",
					[]string{"device"}, nil,
				),
				valueType: prometheus.GaugeValue,
			},
			udevIDATAWriteCacheEnabled: {
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "ata_write_cache_enabled"),
					"ATA disk has its write cache enabled.",
					[]string{"device"}, nil,
				),
				valueType: prometheus.GaugeValue,
			},
			udevIDATARotationRateRPM: {
				desc: prometheus.NewDesc(
					prometheus.BuildFQName(namespace, diskSubsystem, "ata_rotation_rate_rpm"),
					"ATA disk rotation rate in RPMs (0 for SSDs).",
					[]string{"device"}, nil,
				),
				valueType: prometheus.GaugeValue,
			},
		},
		descs: diskstatsDescs(),
	}

	c.rotational = func(dev string) string {
		// Reads exactly one sysfs file per device. Upstream switched from
		// SysBlockDeviceQueueStats (~30 reads/device) to this after #3282, a 10x
		// scrape regression on hosts with many block devices. Keeping the cheap
		// call is not an optimisation to revisit — it is the fix.
		rot, err := c.fs.SysBlockDeviceRotational(dev)
		if err != nil {
			// Upstream previously zero-initialised the struct on error, which also
			// produced rotational="0". Preserved so the label does not change for a
			// device whose queue directory is missing.
			return "0"
		}
		if rot == 1 {
			return "1"
		}
		return "0"
	}

	// Only read udev properties if the directory is actually readable, to avoid a
	// failed open per device per scrape. Upstream logs this at Error; demoted to
	// Warn because it is an expected configuration on a host without udev (and on
	// EKS the directory IS present — verified in the golden corpus, where path,
	// model, serial and wwn are all populated — so this branch firing in
	// production means something is wrong with the host mount, not with the code).
	if stat, err := os.Stat(paths.UdevData); err != nil || !stat.IsDir() {
		logger.Warn("udev data directory not readable, disk info labels will be empty",
			"path", paths.UdevData)
	} else {
		c.udevProperties = func(major, minor uint32) (udevInfo, error) {
			return readUdevProperties(paths.UdevData, major, minor)
		}
	}

	return c, nil
}

// diskstatsDescs returns the positional descriptors, in /proc/diskstats field
// order. The order is load-bearing: diskstatsValues pairs with it by index, and
// short-line truncation drops from the END, so a reordering both mislabels values
// and truncates the wrong ones.
func diskstatsDescs() []typedDesc {
	counter := func(name, help string) typedDesc {
		return typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, diskSubsystem, name), help,
				[]string{"device"}, nil,
			),
			valueType: prometheus.CounterValue,
		}
	}
	gauge := func(name, help string) typedDesc {
		return typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, diskSubsystem, name), help,
				[]string{"device"}, nil,
			),
			valueType: prometheus.GaugeValue,
		}
	}

	return []typedDesc{
		counter("reads_completed_total", "The total number of reads completed successfully."),
		counter("reads_merged_total", "The total number of reads merged."),
		counter("read_bytes_total", "The total number of bytes read successfully."),
		counter("read_time_seconds_total", "The total number of seconds spent by all reads."),
		counter("writes_completed_total", "The total number of writes completed successfully."),
		counter("writes_merged_total", "The number of writes merged."),
		counter("written_bytes_total", "The total number of bytes written successfully."),
		counter("write_time_seconds_total", "This is the total number of seconds spent by all writes."),
		// io_now is the only gauge in the positional set: it is a queue depth, not
		// a cumulative count. Typing it as a counter would make rate() produce
		// nonsense from a value that goes up and down.
		gauge("io_now", "The number of I/Os currently in progress."),
		counter("io_time_seconds_total", "Total seconds spent doing I/Os."),
		counter("io_time_weighted_seconds_total", "The weighted # of seconds spent doing I/Os."),
		// Fields 15-18: kernel 4.18+.
		counter("discards_completed_total", "The total number of discards completed successfully."),
		counter("discards_merged_total", "The total number of discards merged."),
		counter("discarded_sectors_total", "The total number of sectors discarded successfully."),
		counter("discard_time_seconds_total", "This is the total number of seconds spent by all discards."),
		// Fields 19-20: kernel 5.5+.
		counter("flush_requests_total", "The total number of flush requests completed successfully"),
		counter("flush_requests_time_seconds_total", "This is the total number of seconds spent by all flush requests."),
	}
}

// diskstatsValues returns the metric values in the same order as diskstatsDescs.
//
// The unit conversions live here, together, so they can be read as a group:
//   - sectors -> bytes via unixSectorSize, for READS and WRITES only
//   - ticks -> seconds via secondsPerTick, for every *_time_* field
//   - DiscardSectors passes through RAW, because the metric is named
//     discarded_sectors_total and is denominated in sectors, not bytes
//   - IOsInProgress passes through raw, being a count
func diskstatsValues(s *blockdevice.Diskstats) []float64 {
	return []float64{
		float64(s.ReadIOs),
		float64(s.ReadMerges),
		float64(s.ReadSectors) * unixSectorSize,
		float64(s.ReadTicks) * secondsPerTick,
		float64(s.WriteIOs),
		float64(s.WriteMerges),
		float64(s.WriteSectors) * unixSectorSize,
		float64(s.WriteTicks) * secondsPerTick,
		float64(s.IOsInProgress),
		float64(s.IOsTotalTicks) * secondsPerTick,
		float64(s.WeightedIOTicks) * secondsPerTick,
		float64(s.DiscardIOs),
		float64(s.DiscardMerges),
		// NOT multiplied by unixSectorSize. The metric is discarded_SECTORS_total.
		float64(s.DiscardSectors),
		float64(s.DiscardTicks) * secondsPerTick,
		float64(s.FlushRequestsCompleted),
		float64(s.TimeSpentFlushing) * secondsPerTick,
	}
}

func (c *diskstatsCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := c.fs.ProcDiskstats()
	if err != nil {
		return fmt.Errorf("couldn't get diskstats: %w", err)
	}

	for i := range stats {
		s := &stats[i]
		dev := s.DeviceName
		if c.deviceFilter.ignored(dev) {
			continue
		}

		var info udevInfo
		if c.udevProperties != nil {
			info, err = c.udevProperties(s.MajorNumber, s.MinorNumber)
			if err != nil {
				// A device with no udev entry is normal, not a failure. The info
				// labels come out empty and the counters are unaffected.
				c.logger.Debug("failed to read udev info", "device", dev, "err", err)
			}
		}

		ch <- c.infoDesc.mustNewConstMetric(1.0, dev,
			strconv.FormatUint(uint64(s.MajorNumber), 10),
			strconv.FormatUint(uint64(s.MinorNumber), 10),
			info[udevIDPath],
			info[udevIDWWN],
			info[udevIDModel],
			udevSerial(info),
			info[udevIDRevision],
			c.rotational(dev),
		)

		// IoStatsCount counts the fields Sscanf actually filled, including
		// major, minor and the device name, so subtract those three to get the
		// number of STATISTIC fields present. Kernels report 14, 18 or 20 total.
		//
		// Truncating rather than zero-filling is the point: a pre-4.18 kernel
		// reports no discard fields, and emitting discards_completed_total=0 would
		// assert "this disk has never discarded" when the truth is "this kernel
		// does not say".
		statCount := s.IoStatsCount - 3
		for i, value := range diskstatsValues(s) {
			if i >= statCount {
				break
			}
			ch <- c.descs[i].mustNewConstMetric(value, dev)
		}

		if fsType := info[udevIDFSType]; fsType != "" {
			ch <- c.filesystemInfoDesc.mustNewConstMetric(1.0, dev,
				fsType,
				info[udevIDFSUsage],
				info[udevIDFSUUID],
				info[udevIDFSVersion],
			)
		}

		if name := info[udevDMName]; name != "" {
			ch <- c.deviceMapperInfoDesc.mustNewConstMetric(1.0, dev,
				name,
				info[udevDMUUID],
				info[udevDMVGName],
				info[udevDMLVName],
				info[udevDMLVLayer],
			)
		}

		// The ATA metrics are gated on ID_ATA being present, so an NVMe device
		// does not report a rotation rate of 0 as though it were a measurement.
		if info[udevIDATA] != "" {
			c.pushATA(ch, dev, info)
		}
	}
	return nil
}

// pushATA emits the ATA-specific gauges for one device.
func (c *diskstatsCollector) pushATA(ch chan<- prometheus.Metric, dev string, info udevInfo) {
	for attr, desc := range c.ataDescs {
		str, ok := info[attr]
		if !ok {
			c.logger.Debug("udev attribute does not exist", "device", dev, "attribute", attr)
			continue
		}
		value, err := strconv.ParseFloat(str, 64)
		if err != nil {
			// Upstream logs this at Error. Kept at Error: unlike a missing
			// attribute, a present-but-unparseable one means udev wrote something
			// unexpected, which is worth surfacing.
			c.logger.Error("failed to parse ATA value", "device", dev,
				"attribute", attr, "value", str, "err", err)
			continue
		}
		ch <- desc.mustNewConstMetric(value, dev)
	}
}

// udevSerial picks the serial label, in upstream's fallback order.
//
// The order matters for label stability: SCSI_IDENT_SERIAL is the serial printed
// on the physical disk label, ID_SERIAL_SHORT is udev's short form, and ID_SERIAL
// is what virtio devices set. Reordering these changes the label value on real
// hardware, which breaks any dashboard or alert that joins on it.
func udevSerial(info udevInfo) string {
	for _, key := range []string{udevSCSIIdentSerial, udevIDSerialShort, udevIDSerial} {
		if v := info[key]; v != "" {
			return v
		}
	}
	return ""
}

// readUdevProperties parses one device's udev data file.
//
// Format is one "KEY:value" per line; only "E:" lines are device properties, the
// rest are udev bookkeeping. A line without "=" after the prefix is skipped rather
// than stored as an empty-valued key.
func readUdevProperties(udevDataPath string, major, minor uint32) (udevInfo, error) {
	name := fmt.Sprintf("b%d:%d", major, minor)
	file, err := os.Open(fmt.Sprintf("%s/%s", strings.TrimSuffix(udevDataPath, "/"), name))
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info := make(udevInfo)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, udevDevicePropertyPrefix) {
			continue
		}
		line = strings.TrimPrefix(line, udevDevicePropertyPrefix)
		if key, value, found := strings.Cut(line, "="); found {
			info[key] = value
		}
	}
	// Upstream ignores the scanner error here. Returned instead: a truncated read
	// yields partial labels, and silently reporting a disk with a missing serial
	// is worse than saying the read failed. The caller logs at debug and keeps the
	// counters either way, so this cannot fail the collector.
	if err := scanner.Err(); err != nil {
		return info, fmt.Errorf("failed to read udev data for %s: %w", name, err)
	}
	return info, nil
}
