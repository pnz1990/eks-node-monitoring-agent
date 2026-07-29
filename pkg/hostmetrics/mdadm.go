package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/mdadm_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// No software RAID on EKS nodes, so this reports success with zero series on the live
// cluster. Ported for completeness -- a customer running mdraid on a self-managed
// nodegroup would otherwise silently lose these metrics.
//
// THE TRAP HERE IS A CONST-LABEL / MAP-KEY MISMATCH, and it is upstream's, not mine.
//
// node_md_state is emitted five times per device, each from a DIFFERENT descriptor
// carrying a different constant label. The value for each comes from a map keyed by
// the ActivityState string procfs reports. And two of those keys DO NOT MATCH their
// label:
//
//	label "resync" <- stateVals["resyncing"]
//	label "check"  <- stateVals["checking"]
//
// So the label says "resync" while the lookup key is "resyncing". Getting this
// "consistent" by using the label as the key would make node_md_state{state="resync"}
// permanently 0 on a device that IS resyncing -- which is precisely when someone is
// looking at it. Reproduced exactly, and the mismatch is asserted so a future tidy-up
// fails rather than silently breaking the one case the metric exists for.
//
// TWO SOURCES, TWO ErrNoData PATHS: /proc/mdstat via procfs and /sys/block/md*/md via
// sysfs. Either being absent yields ErrNoData, and upstream checks them separately.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"github.com/prometheus/procfs/sysfs"
)

func init() {
	register("mdadm", true, newMDAdmCollector)
}

// mdStateMetric pairs a node_md_state descriptor with the map key its value comes
// from.
//
// The key and the label are held SEPARATELY on purpose: for "resync" and "check" they
// differ, and collapsing them is the bug described above.
type mdStateMetric struct {
	label string
	// stateKey is the ActivityState value procfs reports. NOT always equal to label.
	stateKey string
	desc     *prometheus.Desc
}

func mdStateMetrics() []mdStateMetric {
	desc := func(state string) *prometheus.Desc {
		return prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "md", "state"),
			"Indicates the state of md-device.",
			[]string{"device"},
			// A CONSTANT label, not a variable one: five descriptors share the metric
			// name and are distinguished by this.
			prometheus.Labels{"state": state},
		)
	}
	return []mdStateMetric{
		{label: "active", stateKey: "active", desc: desc("active")},
		{label: "inactive", stateKey: "inactive", desc: desc("inactive")},
		{label: "recovering", stateKey: "recovering", desc: desc("recovering")},
		// THE MISMATCHES. procfs reports "resyncing"/"checking"; the label is
		// "resync"/"check".
		{label: "resync", stateKey: "resyncing", desc: desc("resync")},
		{label: "check", stateKey: "checking", desc: desc("check")},
	}
}

var (
	mdDisksDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "md", "disks"),
		"Number of active/failed/spare disks of device.",
		[]string{"device", "state"}, nil,
	)
	mdDisksRequiredDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "md", "disks_required"),
		"Total number of disks of device.",
		[]string{"device"}, nil,
	)
	mdBlocksDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "md", "blocks"),
		"Total number of blocks on device.",
		[]string{"device"}, nil,
	)
	mdBlocksSyncedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "md", "blocks_synced"),
		"Number of blocks synced on device.",
		[]string{"device"}, nil,
	)
	mdRaidDisksDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "md", "raid_disks"),
		"Number of raid disks on device.",
		[]string{"device"}, nil,
	)
	mdDegradedDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "md", "degraded"),
		"Number of degraded disks on device.",
		[]string{"device"}, nil,
	)
)

type mdAdmCollector struct {
	logger *slog.Logger

	// Two independent sources, each with its own ErrNoData path.
	mdStat  func() ([]procfs.MDStat, error)
	mdRaids func() ([]sysfs.Mdraid, error)
}

func newMDAdmCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	procFS, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	sysFS, err := sysfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}

	// Upstream opens BOTH filesystems inside Update, on every scrape. Moved to
	// construction: a bad path should fail at startup, not every 15 seconds.
	return &mdAdmCollector{
		logger:  logger,
		mdStat:  procFS.MDStat,
		mdRaids: sysFS.Mdraids,
	}, nil
}

func (c *mdAdmCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := c.mdStat()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No /proc/mdstat: the md module is not loaded. A supported configuration.
			c.logger.Debug("not collecting mdstat, file does not exist")
			return ErrNoData
		}
		return fmt.Errorf("error parsing mdstatus: %w", err)
	}

	for _, stat := range stats {
		// stateVals has exactly ONE entry -- the device's current activity state --
		// and every other state's lookup misses and yields the zero value. That is how
		// the five node_md_state series get 1 for the active state and 0 for the rest.
		stateVals := map[string]float64{stat.ActivityState: 1}

		ch <- prometheus.MustNewConstMetric(mdDisksRequiredDesc, prometheus.GaugeValue,
			float64(stat.DisksTotal), stat.Name)

		for state, value := range map[string]int64{
			"active": stat.DisksActive,
			"failed": stat.DisksFailed,
			"spare":  stat.DisksSpare,
		} {
			ch <- prometheus.MustNewConstMetric(mdDisksDesc, prometheus.GaugeValue,
				float64(value), stat.Name, state)
		}

		for _, m := range mdStateMetrics() {
			// Keyed by stateKey, NOT by label. See the note at the top of this file.
			ch <- prometheus.MustNewConstMetric(m.desc, prometheus.GaugeValue,
				stateVals[m.stateKey], stat.Name)
		}

		ch <- prometheus.MustNewConstMetric(mdBlocksDesc, prometheus.GaugeValue,
			float64(stat.BlocksTotal), stat.Name)
		ch <- prometheus.MustNewConstMetric(mdBlocksSyncedDesc, prometheus.GaugeValue,
			float64(stat.BlocksSynced), stat.Name)
	}

	raids, err := c.mdRaids()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.logger.Debug("not collecting mdraids, path does not exist")
			return ErrNoData
		}
		return fmt.Errorf("error parsing mdraids: %w", err)
	}

	for _, raid := range raids {
		// Disks is a pointer: absent on some kernels. Emitting zero would report a
		// RAID array with no disks.
		if raid.Disks != nil {
			ch <- prometheus.MustNewConstMetric(mdRaidDisksDesc, prometheus.GaugeValue,
				float64(*raid.Disks), raid.Device)
		}
		// DegradedDisks is NOT a pointer, so zero is a real measurement here: a
		// healthy array genuinely has zero degraded disks.
		ch <- prometheus.MustNewConstMetric(mdDegradedDesc, prometheus.GaugeValue,
			float64(raid.DegradedDisks), raid.Device)
	}

	return nil
}
