package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/dmi.go
//                 node_exporter/collector/nvme_linux.go
//                 node_exporter/collector/selinux_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2021-2023 The Prometheus Authors, Apache-2.0
//
// UNLIKE the rest of the hardware group, these three DO emit on EKS. Measured
// against the live cluster:
//
//	node_dmi_info      1 series, 16 labels
//	node_nvme_*        6 series (EBS presents as NVMe)
//	node_selinux_*     3 series
//
// THE INTERESTING ONE IS dmi, BECAUSE ITS LABEL SET IS HOST-DEPENDENT.
//
// Upstream builds the descriptor's label list AT CONSTRUCTION from whichever DMI
// fields the platform actually exposes, and skips the nil ones. On the live EKS node
// that yields 16 of the 20 possible labels — board_serial, chassis_serial,
// product_serial and product_uuid are absent because those sysfs files are
// root-readable only (mode 0400).
//
// That means node_dmi_info is a DIFFERENT METRIC on different hosts, which is
// unusual and would be easy to "fix" by always emitting all 20 with empty strings
// for the missing ones. Doing so would change the series identity on every node and
// break any query joining on it. Preserved exactly, and asserted.
//
// It also means the label list must be built ONCE at construction, not per scrape:
// a Desc whose label names varied between scrapes would make Prometheus reject the
// second one.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/opencontainers/selinux/go-selinux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs/sysfs"
)

// ---------------------------------------------------------------------------
// dmi
// ---------------------------------------------------------------------------

func init() {
	register("dmi", true, newDMICollector)
}

// dmiFields maps the label name to its accessor on sysfs.DMIClass, in a fixed
// order.
//
// ORDER MATTERS AND UPSTREAM'S IS NOT DETERMINISTIC. Upstream iterates a Go map, so
// the label order in its Desc varies between process starts. That happens to be
// harmless because Prometheus sorts labels for series identity, but it means the
// label ORDER cannot be compared against upstream — only the SET. Sorted here so
// the descriptor is at least stable across our own restarts, which makes the golden
// corpus diffable.
func dmiFields(dmi *sysfs.DMIClass) map[string]*string {
	return map[string]*string{
		"bios_date":         dmi.BiosDate,
		"bios_release":      dmi.BiosRelease,
		"bios_vendor":       dmi.BiosVendor,
		"bios_version":      dmi.BiosVersion,
		"board_asset_tag":   dmi.BoardAssetTag,
		"board_name":        dmi.BoardName,
		"board_serial":      dmi.BoardSerial,
		"board_vendor":      dmi.BoardVendor,
		"board_version":     dmi.BoardVersion,
		"chassis_asset_tag": dmi.ChassisAssetTag,
		"chassis_serial":    dmi.ChassisSerial,
		"chassis_vendor":    dmi.ChassisVendor,
		"chassis_version":   dmi.ChassisVersion,
		"product_family":    dmi.ProductFamily,
		"product_name":      dmi.ProductName,
		"product_serial":    dmi.ProductSerial,
		"product_sku":       dmi.ProductSKU,
		"product_uuid":      dmi.ProductUUID,
		"product_version":   dmi.ProductVersion,
		"system_vendor":     dmi.SystemVendor,
	}
}

type dmiCollector struct {
	logger   *slog.Logger
	infoDesc *prometheus.Desc
	values   []string
}

func newDMICollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := sysfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}

	dmi, err := fs.DMIClass()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("failed to read DMI information: %w", err)
		}
		// A platform without DMI (most ARM boards, some VMs). Upstream substitutes an
		// empty struct so construction still succeeds and Update reports ErrNoData,
		// rather than failing the whole agent at startup over absent firmware tables.
		logger.Debug("platform does not support DMI information", "err", err)
		dmi = &sysfs.DMIClass{}
	}

	fields := dmiFields(dmi)

	// Sorted so the descriptor is stable across restarts. Upstream ranges over the
	// map directly, so its label order varies run to run.
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	var labels, values []string
	for _, name := range names {
		value := fields[name]
		// nil means the sysfs file was absent or unreadable. The label is OMITTED
		// entirely rather than emitted empty -- which is what makes this metric
		// host-dependent, and is exactly the behaviour to preserve.
		if value == nil {
			continue
		}
		labels = append(labels, name)
		// DMI strings come from firmware and are not guaranteed valid UTF-8; the
		// Prometheus text format requires it. Upstream substitutes U+FFFD.
		values = append(values, strings.ToValidUTF8(*value, "�"))
	}

	return &dmiCollector{
		logger: logger,
		// Built ONCE: DMI cannot change without a reboot, and a Desc whose label
		// names varied between scrapes would make Prometheus reject the second one.
		infoDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "dmi", "info"),
			"A metric with a constant '1' value labeled by bios_date, bios_release, bios_vendor, bios_version, "+
				"board_asset_tag, board_name, board_serial, board_vendor, board_version, chassis_asset_tag, "+
				"chassis_serial, chassis_vendor, chassis_version, product_family, product_name, product_serial, "+
				"product_sku, product_uuid, product_version, system_vendor if provided by DMI.",
			labels, nil,
		),
		values: values,
	}, nil
}

func (c *dmiCollector) Update(ch chan<- prometheus.Metric) error {
	if len(c.values) == 0 {
		// No DMI fields at all. ErrNoData here, unlike the rest of the hardware group
		// -- and that IS upstream's behaviour, because a dmi_info metric with zero
		// labels would be a bare "1" carrying no information.
		return ErrNoData
	}
	ch <- prometheus.MustNewConstMetric(c.infoDesc, prometheus.GaugeValue, 1.0, c.values...)
	return nil
}

// ---------------------------------------------------------------------------
// nvme
// ---------------------------------------------------------------------------

func init() {
	register("nvme", true, newNVMeCollector)
}

var (
	// NOTE the label ORDER: cntlid is LAST in the descriptor even though it reads
	// first alphabetically. The emit call must match this order positionally, and a
	// transposition would silently swap two label values.
	nvmeInfoDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "nvme", "info"),
		"Non-numeric data from /sys/class/nvme/<device>, value is always 1.",
		[]string{"device", "firmware_revision", "model", "serial", "state", "cntlid"}, nil,
	)
	nvmeNamespaceInfoDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "nvme", "namespace_info"),
		"Information about NVMe namespaces. Value is always 1",
		[]string{"device", "nsid", "ana_state"}, nil,
	)
	nvmeNamespaceCapacityBytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "nvme", "namespace_capacity_bytes"),
		"Capacity of the NVMe namespace in bytes. Computed as namespace_size * namespace_logical_block_size",
		[]string{"device", "nsid"}, nil,
	)
	nvmeNamespaceSizeBytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "nvme", "namespace_size_bytes"),
		"Size of the NVMe namespace in bytes. Available in /sys/class/nvme/<device>/<namespace>/size",
		[]string{"device", "nsid"}, nil,
	)
	nvmeNamespaceUsedBytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "nvme", "namespace_used_bytes"),
		"Used space of the NVMe namespace in bytes. Available in /sys/class/nvme/<device>/<namespace>/nuse",
		[]string{"device", "nsid"}, nil,
	)
	nvmeNamespaceLogicalBlockSizeBytesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "nvme", "namespace_logical_block_size_bytes"),
		"Logical block size of the NVMe namespace in bytes. Usually 4Kb. Available in /sys/class/nvme/<device>/<namespace>/queue/logical_block_size",
		[]string{"device", "nsid"}, nil,
	)
)

type nvmeCollector struct {
	fs     sysfs.FS
	logger *slog.Logger

	nvmeClass func() (sysfs.NVMeClass, error)
}

func newNVMeCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := sysfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}
	c := &nvmeCollector{fs: fs, logger: logger}
	c.nvmeClass = c.fs.NVMeClass
	return c, nil
}

func (c *nvmeCollector) Update(ch chan<- prometheus.Metric) error {
	devices, err := c.nvmeClass()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No /sys/class/nvme at all. On EKS this branch does NOT fire: EBS volumes
			// present as NVMe devices, so the live node reports 6 series.
			c.logger.Debug("nvme statistics not found, skipping")
			return ErrNoData
		}
		return fmt.Errorf("error obtaining NVMe class info: %w", err)
	}

	for _, device := range devices {
		ch <- prometheus.MustNewConstMetric(nvmeInfoDesc, prometheus.GaugeValue, 1.0,
			device.Name, device.FirmwareRevision, device.Model,
			device.Serial, device.State, device.ControllerID)

		for _, ns := range device.Namespaces {
			ch <- prometheus.MustNewConstMetric(nvmeNamespaceInfoDesc, prometheus.GaugeValue, 1.0,
				device.Name, ns.ID, ns.ANAState)

			// All four are raw byte counts from sysfs -- no unit conversion. capacity is
			// size * logical_block_size, computed by procfs rather than here.
			for desc, value := range map[*prometheus.Desc]uint64{
				nvmeNamespaceCapacityBytesDesc:         ns.CapacityBytes,
				nvmeNamespaceSizeBytesDesc:             ns.SizeBytes,
				nvmeNamespaceUsedBytesDesc:             ns.UsedBytes,
				nvmeNamespaceLogicalBlockSizeBytesDesc: ns.LogicalBlockSize,
			} {
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue,
					float64(value), device.Name, ns.ID)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// selinux
// ---------------------------------------------------------------------------

func init() {
	register("selinux", true, newSELinuxCollector)
}

// The three SELinux metrics. config_mode and current_mode are the enforcement mode
// as an enum (-1 disabled, 0 permissive, 1 enforcing), which is why they are gauges
// rather than booleans: three states do not fit in a 0/1.
var (
	selinuxConfigModeDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "selinux", "config_mode"),
		"Configured SELinux enforcement mode",
		nil, nil,
	)
	selinuxCurrentModeDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "selinux", "current_mode"),
		"Current SELinux enforcement mode",
		nil, nil,
	)
	selinuxEnabledDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "selinux", "enabled"),
		"SELinux is enabled, 1 is true, 0 is false",
		nil, nil,
	)
)

type selinuxCollector struct {
	logger *slog.Logger

	// The go-selinux package reads global state through package-level functions, so
	// these are the only seams available. Injectable because a single host is either
	// enforcing or not, and the other branch is otherwise untestable.
	enabled        func() bool
	defaultEnforce func() int
	currentEnforce func() int
}

func newSELinuxCollector(logger *slog.Logger, _ Paths) (Collector, error) {
	// NOTE: go-selinux reads /sys/fs/selinux directly and has no configurable root,
	// so this collector ignores Paths. In the shipped DaemonSet that is correct --
	// /sys is mounted from the host -- but it is the second collector after uname
	// that a rebased path would not fix. Recorded in docs/parity-exceptions-nodep.md.
	return &selinuxCollector{
		logger:         logger,
		enabled:        selinux.GetEnabled,
		defaultEnforce: selinux.DefaultEnforceMode,
		currentEnforce: selinux.EnforceMode,
	}, nil
}

func (c *selinuxCollector) Update(ch chan<- prometheus.Metric) error {
	if !c.enabled() {
		// Only the enabled=0 metric. config_mode and current_mode are meaningless
		// without SELinux, and emitting 0 for them would read as "permissive" -- a
		// specific claim rather than an absence.
		ch <- prometheus.MustNewConstMetric(selinuxEnabledDesc, prometheus.GaugeValue, 0)
		return nil
	}

	ch <- prometheus.MustNewConstMetric(selinuxEnabledDesc, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(selinuxConfigModeDesc, prometheus.GaugeValue,
		float64(c.defaultEnforce()))
	ch <- prometheus.MustNewConstMetric(selinuxCurrentModeDesc, prometheus.GaugeValue,
		float64(c.currentEnforce()))
	return nil
}
