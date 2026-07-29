package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/dmmultipath_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2025 The Prometheus Authors, Apache-2.0
//
// Device-mapper multipath. Nothing on a normal EKS node -- EBS presents a single path
// -- but relevant for self-managed nodes with FC or iSCSI SAN storage, where a failed
// path is exactly the silent degradation this metric exists to surface.
//
// TWO THINGS WORTH PRESERVING CAREFULLY:
//
//  1. isPathActive accepts BOTH "running" (SCSI) and "live" (NVMe). Those are two
//     different kernel subsystems using different words for the same healthy state.
//     Handling only "running" would make every NVMe path count as FAILED -- a metric
//     reporting total path failure on a healthy machine, which is worse than no metric.
//
//  2. device_active is INVERTED from the underlying field: the struct reports
//     Suspended, the metric reports active. So active = !Suspended. Getting the
//     polarity wrong reports every healthy device as suspended and every suspended one
//     as healthy — and the metric looks entirely plausible either way.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs/blockdevice"
)

func init() {
	register("dmmultipath", true, newDMMultipathCollector)
}

var (
	dmmultipathDeviceLabels = []string{"device", "sysfs_name"}

	dmmultipathDeviceInfoDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "dmmultipath", "device_info"),
		"Non-numeric information about a DM-multipath device.",
		[]string{"device", "sysfs_name", "uuid"}, nil,
	)
	dmmultipathDeviceActiveDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "dmmultipath", "device_active"),
		"Whether the multipath device-mapper device is active (1) or suspended (0).",
		dmmultipathDeviceLabels, nil,
	)
	dmmultipathDeviceSizeBytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "dmmultipath", "device_size_bytes"),
		"Size of the multipath device in bytes, read from /sys/block/<dm>/size.",
		dmmultipathDeviceLabels, nil,
	)
	dmmultipathDevicePathsDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "dmmultipath", "device_paths"),
		"Number of paths for a multipath device.",
		dmmultipathDeviceLabels, nil,
	)
	dmmultipathDevicePathsActiveDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "dmmultipath", "device_paths_active"),
		"Number of paths in active state (SCSI running or NVMe live) for a multipath device.",
		dmmultipathDeviceLabels, nil,
	)
	dmmultipathDevicePathsFailedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "dmmultipath", "device_paths_failed"),
		"Number of paths not in active state for a multipath device.",
		dmmultipathDeviceLabels, nil,
	)
	dmmultipathPathStateDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "dmmultipath", "path_state"),
		"Reports the underlying device state for a multipath path, as read from /sys/block/<dev>/device/state.",
		[]string{"device", "path", "state"}, nil,
	)
)

// dmMultipathActiveStates are the device states that mean a path is healthy and
// usable.
//
// BOTH are required: "running" is SCSI, "live" is NVMe. A table rather than an
// inline || so the set is diffable against upstream and so adding a third subsystem
// later is a data change.
func dmMultipathActiveStates() []string {
	return []string{"running", "live"}
}

// isDMPathActive reports whether a path's device state is healthy.
func isDMPathActive(state string) bool {
	for _, active := range dmMultipathActiveStates() {
		if state == active {
			return true
		}
	}
	return false
}

type dmMultipathCollector struct {
	fs     blockdevice.FS
	logger *slog.Logger

	multipathDevices func() ([]blockdevice.DMMultipathDevice, error)
}

func newDMMultipathCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := blockdevice.NewFS(paths.ProcFS, paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs/sysfs at %s, %s: %w",
			paths.ProcFS, paths.SysFS, err)
	}
	c := &dmMultipathCollector{fs: fs, logger: logger}
	c.multipathDevices = c.fs.DMMultipathDevices
	return c, nil
}

func (c *dmMultipathCollector) Update(ch chan<- prometheus.Metric) error {
	devices, err := c.multipathDevices()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			c.logger.Debug("could not read DM-multipath devices", "err", err)
			return ErrNoData
		}
		return fmt.Errorf("failed to scan DM-multipath devices: %w", err)
	}

	for i := range devices {
		dev := &devices[i]

		ch <- prometheus.MustNewConstMetric(dmmultipathDeviceInfoDesc, prometheus.GaugeValue, 1,
			dev.Name, dev.SysfsName, dev.UUID)

		// INVERTED: the struct reports Suspended, the metric reports active.
		active := 0.0
		if !dev.Suspended {
			active = 1.0
		}
		ch <- prometheus.MustNewConstMetric(dmmultipathDeviceActiveDesc, prometheus.GaugeValue,
			active, dev.Name, dev.SysfsName)

		ch <- prometheus.MustNewConstMetric(dmmultipathDeviceSizeBytesDesc, prometheus.GaugeValue,
			float64(dev.SizeBytes), dev.Name, dev.SysfsName)

		var activePaths, failedPaths float64
		for _, p := range dev.Paths {
			if isDMPathActive(p.State) {
				activePaths++
			} else {
				failedPaths++
			}
			// One series per path, with the raw state as a label. The value is always 1;
			// the information is in the label.
			ch <- prometheus.MustNewConstMetric(dmmultipathPathStateDesc, prometheus.GaugeValue, 1,
				dev.Name, p.Device, p.State)
		}

		// active + failed always equals len(Paths): every path is counted exactly once,
		// so a path in an unrecognised state counts as FAILED rather than vanishing.
		// That is the safe direction -- an unknown state is not evidence of health.
		ch <- prometheus.MustNewConstMetric(dmmultipathDevicePathsDesc, prometheus.GaugeValue,
			float64(len(dev.Paths)), dev.Name, dev.SysfsName)
		ch <- prometheus.MustNewConstMetric(dmmultipathDevicePathsActiveDesc, prometheus.GaugeValue,
			activePaths, dev.Name, dev.SysfsName)
		ch <- prometheus.MustNewConstMetric(dmmultipathDevicePathsFailedDesc, prometheus.GaugeValue,
			failedPaths, dev.Name, dev.SysfsName)
	}
	return nil
}
