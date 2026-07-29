package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/btrfs_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2017 The Prometheus Authors, Apache-2.0
//
// No btrfs on EKS -- Amazon Linux 2023 uses xfs for the root volume -- so this reports
// success with zero series on the live cluster, and the golden corpus contains no
// node_btrfs_* series at all. Ported so a customer running btrfs on a self-managed
// nodegroup does not silently lose it.
//
// SCOPE EXCEPTION: THE ioctl DEVICE-STATS PATH IS NOT PORTED.
//
// Upstream has two ways of reporting per-device stats. The procfs path
// (/sys/fs/btrfs/<uuid>/devices) yields device_size_bytes. The ioctl path opens every
// btrfs mount with BTRFS_IOC_FS_INFO / BTRFS_IOC_GET_DEV_STATS and additionally yields
// device_unused_bytes and device_errors_total{type=write|read|flush|corruption|
// generation}. Only the procfs path is ported here, which means three metric FAMILIES
// are absent on a btrfs host: device_unused_bytes, device_errors_total, and the
// btrfs_dev_uuid label on device_size_bytes.
//
// Recorded in docs/parity-exceptions-nodep.md rather than glossed, because it is the
// single largest deliberate parity gap on this branch. The reasons:
//
//   - It needs CAP_SYS_ADMIN to open the filesystem for ioctl. The shipped DaemonSet
//     does not have it, so on the actual deployment target this path would fail and
//     fall back to procfs anyway -- upstream logs at Debug and continues.
//   - It pulls in github.com/dennwc/btrfs, a cgo-adjacent ioctl wrapper, for metrics
//     that are unreachable in our deployment.
//   - Zero btrfs filesystems exist on any EKS node, so the gap is currently
//     unobservable. That is an argument for deferring it, NOT for pretending it does
//     not exist.
//
// If a customer needs device error counters on btrfs, this is the work to do, and it
// is a bounded ~80 lines plus the dependency.

import (
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs/btrfs"
)

const btrfsSubsystem = "btrfs"

func init() {
	register("btrfs", true, newBtrfsCollector)
}

// btrfsMetric is one emitted series. Built as a value rather than emitted directly so
// the whole set for a filesystem can be assembled and then converted, matching
// upstream's structure and keeping the label bookkeeping in one place.
type btrfsMetric struct {
	name            string
	help            string
	metricType      prometheus.ValueType
	value           float64
	extraLabel      []string
	extraLabelValue []string
}

type btrfsCollector struct {
	fs     btrfs.FS
	logger *slog.Logger

	stats func() ([]*btrfs.Stats, error)
}

func newBtrfsCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := btrfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}
	c := &btrfsCollector{fs: fs, logger: logger}
	c.stats = c.fs.Stats
	return c, nil
}

func (c *btrfsCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := c.stats()
	if err != nil {
		return fmt.Errorf("failed to retrieve Btrfs stats from procfs: %w", err)
	}

	for _, s := range stats {
		for _, m := range btrfsMetrics(s) {
			// The uuid label is always first, with any extras appended. Built per
			// metric because the extra label SET varies by metric -- info carries
			// "label", the layout metrics carry "block_group_type" and "mode" -- so
			// these Descs genuinely cannot be cached by name alone.
			labels := append([]string{"uuid"}, m.extraLabel...)
			values := append([]string{s.UUID}, m.extraLabelValue...)

			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					prometheus.BuildFQName(namespace, btrfsSubsystem, m.name),
					m.help, labels, nil,
				),
				m.metricType, m.value, values...)
		}
	}
	return nil
}

// btrfsMetrics assembles every series for one filesystem.
func btrfsMetrics(s *btrfs.Stats) []btrfsMetric {
	metrics := []btrfsMetric{
		{
			name: "info", help: "Filesystem information",
			metricType: prometheus.GaugeValue, value: 1,
			extraLabel: []string{"label"}, extraLabelValue: []string{s.Label},
		},
		{
			name: "global_rsv_size_bytes", help: "Size of global reserve.",
			metricType: prometheus.GaugeValue,
			value:      float64(s.Allocation.GlobalRsvSize),
		},
		{
			name: "commits_total", help: "The total number of commits that have occurred.",
			metricType: prometheus.CounterValue,
			value:      float64(s.CommitStats.Commits),
		},
		// THE THREE COMMIT DURATIONS ARE MILLISECONDS in the kernel and seconds in the
		// metric. That is the sixth distinct unit convention in this package, and note
		// that last/max are GAUGES while the total is a COUNTER despite all three
		// sharing the same divisor -- a "consistency" cleanup that made them all
		// counters would break rate() on last_commit_seconds.
		{
			name: "last_commit_seconds", help: "Duration of the most recent commit, in seconds.",
			metricType: prometheus.GaugeValue,
			value:      float64(s.CommitStats.LastCommitMs) / 1000,
		},
		{
			name: "max_commit_seconds", help: "Duration of the slowest commit, in seconds.",
			metricType: prometheus.GaugeValue,
			value:      float64(s.CommitStats.MaxCommitMs) / 1000,
		},
		{
			name: "commit_seconds_total", help: "Sum of the duration of all commits, in seconds.",
			metricType: prometheus.CounterValue,
			value:      float64(s.CommitStats.TotalCommitMs) / 1000,
		},
	}

	// Order is upstream's: data, metadata, system. It affects only readability.
	for _, group := range []struct {
		name  string
		stats *btrfs.AllocationStats
	}{
		{"data", s.Allocation.Data},
		{"metadata", s.Allocation.Metadata},
		{"system", s.Allocation.System},
	} {
		metrics = append(metrics, btrfsAllocationMetrics(group.name, group.stats)...)
	}

	// The procfs device path. Upstream prefers ioctl when available and falls back to
	// this; only this branch is ported (see the scope note at the top of the file).
	for name, dev := range s.Devices {
		metrics = append(metrics, btrfsMetric{
			name:       "device_size_bytes",
			help:       "Size of a device that is part of the filesystem.",
			metricType: prometheus.GaugeValue,
			value:      float64(dev.Size),
			extraLabel: []string{"device"}, extraLabelValue: []string{name},
		})
	}

	return metrics
}

// btrfsAllocationMetrics returns the reserved-bytes metric plus per-layout metrics for
// one block-group type.
func btrfsAllocationMetrics(groupType string, s *btrfs.AllocationStats) []btrfsMetric {
	if s == nil {
		// A filesystem may not report every block-group type. Upstream would nil-deref
		// here; guarded because a panic in a collector is far worse than a missing
		// metric.
		return nil
	}

	metrics := []btrfsMetric{{
		name: "reserved_bytes", help: "Amount of space reserved for a data type",
		metricType: prometheus.GaugeValue,
		value:      float64(s.ReservedBytes),
		extraLabel: []string{"block_group_type"}, extraLabelValue: []string{groupType},
	}}

	for layout, usage := range s.Layouts {
		metrics = append(metrics, btrfsLayoutMetrics(groupType, layout, usage)...)
	}
	return metrics
}

// btrfsLayoutMetrics returns the three per-layout metrics.
func btrfsLayoutMetrics(groupType, layout string, s *btrfs.LayoutUsage) []btrfsMetric {
	if s == nil {
		return nil
	}

	labels := []string{"block_group_type", "mode"}
	values := []string{groupType, layout}

	return []btrfsMetric{
		{
			name: "used_bytes", help: "Amount of used space by a layout/data type",
			metricType: prometheus.GaugeValue, value: float64(s.UsedBytes),
			extraLabel: labels, extraLabelValue: values,
		},
		{
			name: "size_bytes", help: "Amount of space allocated for a layout/data type",
			metricType: prometheus.GaugeValue, value: float64(s.TotalBytes),
			extraLabel: labels, extraLabelValue: values,
		},
		{
			// Already a ratio, so no conversion -- unlike every other numeric here.
			name: "allocation_ratio", help: "Data allocation ratio for a layout/data type",
			metricType: prometheus.GaugeValue, value: s.Ratio,
			extraLabel: labels, extraLabelValue: values,
		},
	}
}
