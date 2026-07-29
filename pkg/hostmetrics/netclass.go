package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/netclass_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// THIS COLLECTOR FIXES UPSTREAM #1915 / #1841, the third and most consequential
// "fix rather than contain" instance on this branch.
//
// Upstream's getNetClassInfo enumerates /sys/class/net, then reads each device:
//
//	for _, device := range netDevices {
//	    interfaceClass, err := c.fs.NetClassByIface(device)
//	    if err != nil {
//	        return netClass, err     // <-- ONE bad device discards EVERYTHING
//	    }
//	    netClass[device] = *interfaceClass
//	}
//
// A device present at listing time and gone at read time makes the collector emit
// NOTHING — not partial data. On a static host interfaces essentially never
// disappear, which is why this has been open since 2020. On EKS, veth and eni
// interfaces are created and destroyed on every pod schedule, so the
// listing-to-read window is hit routinely rather than rarely.
//
// Reproduced deterministically on the dependency branch: remove a device after
// listing and 0 of 3 devices get reported.
//
// Here a device that fails to read is skipped with a debug log and the rest are
// still reported. That also removes the need for the EKS device-exclusion default
// that was measured and rejected on the dependency branch for costing
// node_network_speed_bytes — the churn hazard is fixed at its cause instead of
// worked around by not looking.

import (
	"fmt"
	"log/slog"
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs/sysfs"
)

const netclassSubsystem = "network"

func init() {
	register("netclass", true, newNetClassCollector)
}

type netClassCollector struct {
	fs     sysfs.FS
	logger *slog.Logger

	// ignoredDevices matches devices to skip entirely. Empty by default, matching
	// upstream: with the all-or-nothing read fixed there is no resilience reason to
	// exclude anything, and excluding pod-side interfaces would cost
	// node_network_speed_bytes (measured on the dependency branch: 297 metric names
	// versus upstream's 298).
	ignoredDevices *regexp.Regexp

	// ignoreInvalidSpeed suppresses speed_bytes for devices reporting a negative
	// speed. Upstream defaults this OFF and intends to flip it in 2.x; kept OFF for
	// parity, since enabling it removes the metric family entirely on a node where
	// every remaining interface reports an invalid speed.
	ignoreInvalidSpeed bool

	upDesc    *prometheus.Desc
	infoDesc  *prometheus.Desc
	fieldDesc map[string]*prometheus.Desc
}

// netclassFields are the numeric sysfs attributes upstream exposes, in the order it
// emits them. The key is the metric-name suffix; the accessor pulls the value.
//
// Held as a table so the set can be asserted against upstream mechanically. A
// missing entry silently drops a metric; an extra one emits a metric upstream
// lacks. Both are invisible to a name-only review.
func netclassFieldNames() []string {
	return []string{
		"address_assign_type",
		"carrier",
		"carrier_changes_total",
		"carrier_up_changes_total",
		"carrier_down_changes_total",
		"device_id",
		"dormant",
		"flags",
		"iface_id",
		"iface_link",
		"iface_link_mode",
		"mtu_bytes",
		"name_assign_type",
		"net_dev_group",
		"speed_bytes",
		"transmit_queue_length",
		"protocol_type",
	}
}

// defNetClassIgnoredDevices matches only the empty string, i.e. nothing. This is
// upstream's default, kept verbatim: with the all-or-nothing read fixed there is no
// resilience reason to exclude anything.
const defNetClassIgnoredDevices = "^$"

func newNetClassCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	return newNetClassCollectorWithFilter(logger, paths, defNetClassIgnoredDevices)
}

// newNetClassCollectorWithFilter is split out so the invalid-pattern branch is
// reachable from a test, and so the device filter can be wired to a chart value
// later without restructuring. Unreachable with the compile-time default, but the
// check must stay: an invalid regexp should fail at startup rather than panic on
// first scrape.
func newNetClassCollectorWithFilter(logger *slog.Logger, paths Paths, ignoredExpr string) (Collector, error) {
	fs, err := sysfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}

	ignored, err := regexp.Compile(ignoredExpr)
	if err != nil {
		return nil, fmt.Errorf("invalid ignored-devices pattern: %w", err)
	}

	fieldDesc := make(map[string]*prometheus.Desc, len(netclassFieldNames()))
	for _, name := range netclassFieldNames() {
		fieldDesc[name] = prometheus.NewDesc(
			prometheus.BuildFQName(namespace, netclassSubsystem, name),
			fmt.Sprintf("Network device property: %s.", name),
			[]string{"device"}, nil,
		)
	}

	return &netClassCollector{
		fs:             fs,
		logger:         logger,
		ignoredDevices: ignored,
		fieldDesc:      fieldDesc,
		upDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, netclassSubsystem, "up"),
			"Value is 1 if operstate is 'up', 0 otherwise.",
			[]string{"device"}, nil,
		),
		infoDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, netclassSubsystem, "info"),
			"Non-numeric data from /sys/class/net/<iface>, value is always 1.",
			[]string{"device", "address", "broadcast", "duplex", "operstate", "adminstate", "ifalias"},
			nil,
		),
	}, nil
}

func (c *netClassCollector) Update(ch chan<- prometheus.Metric) error {
	devices, err := c.netClassInfo()
	if err != nil {
		return fmt.Errorf("couldn't get net class info: %w", err)
	}
	if len(devices) == 0 {
		// No readable devices at all is genuinely no data, distinct from a failure.
		return ErrNoData
	}

	for _, iface := range devices {
		up := 0.0
		if iface.OperState == "up" {
			up = 1.0
		}
		ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, up, iface.Name)

		ch <- prometheus.MustNewConstMetric(c.infoDesc, prometheus.GaugeValue, 1.0,
			iface.Name, iface.Address, iface.Broadcast, iface.Duplex,
			iface.OperState, adminState(iface.Flags), iface.IfAlias)

		c.pushField(ch, "address_assign_type", iface.AddrAssignType, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "carrier", iface.Carrier, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "carrier_changes_total", iface.CarrierChanges, iface.Name, prometheus.CounterValue)
		c.pushField(ch, "carrier_up_changes_total", iface.CarrierUpCount, iface.Name, prometheus.CounterValue)
		c.pushField(ch, "carrier_down_changes_total", iface.CarrierDownCount, iface.Name, prometheus.CounterValue)
		c.pushField(ch, "device_id", iface.DevID, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "dormant", iface.Dormant, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "flags", iface.Flags, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "iface_id", iface.IfIndex, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "iface_link", iface.IfLink, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "iface_link_mode", iface.LinkMode, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "mtu_bytes", iface.MTU, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "name_assign_type", iface.NameAssignType, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "net_dev_group", iface.NetDevGroup, iface.Name, prometheus.GaugeValue)

		if iface.Speed != nil {
			// Some virtual devices report -1. Upstream emits it unless
			// ignore-invalid-speed is set, and converts Mbit/s to bytes/s.
			if *iface.Speed >= 0 || !c.ignoreInvalidSpeed {
				speedBytes := int64(*iface.Speed * 1000 * 1000 / 8)
				c.pushField(ch, "speed_bytes", &speedBytes, iface.Name, prometheus.GaugeValue)
			}
		}

		c.pushField(ch, "transmit_queue_length", iface.TxQueueLen, iface.Name, prometheus.GaugeValue)
		c.pushField(ch, "protocol_type", iface.Type, iface.Name, prometheus.GaugeValue)
	}
	return nil
}

// pushField emits one numeric attribute, skipping it when the kernel did not
// report the value.
//
// A nil pointer means the sysfs file was absent. Emitting zero would be a claim
// about the device that the kernel never made, and upstream omits it too.
func (c *netClassCollector) pushField(ch chan<- prometheus.Metric, name string, value *int64,
	device string, valueType prometheus.ValueType) {
	if value == nil {
		return
	}
	desc, ok := c.fieldDesc[name]
	if !ok {
		// Unreachable with the compile-time table, but a missing descriptor would
		// otherwise nil-panic inside MustNewConstMetric.
		c.logger.Debug("no descriptor for netclass field, skipping", "field", name)
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, valueType, float64(*value), device)
}

// netClassInfo reads every readable network device.
//
// THE FIX: a device that cannot be read is skipped rather than aborting the whole
// collection. Upstream returns on the first error, so one interface disappearing
// mid-scrape suppresses metrics for every interface on the node — routine on EKS,
// where veth and eni devices churn with pod scheduling.
func (c *netClassCollector) netClassInfo() ([]sysfs.NetClassIface, error) {
	names, err := c.fs.NetClassDevices()
	if err != nil {
		return nil, err
	}

	out := make([]sysfs.NetClassIface, 0, len(names))
	skipped := 0
	for _, name := range names {
		if c.ignoredDevices.MatchString(name) {
			continue
		}
		iface, err := c.fs.NetClassByIface(name)
		if err != nil {
			// The common cause is the device being removed between listing and
			// reading. Debug rather than error: on a busy node this is expected
			// churn, and logging it at error level would be noise.
			c.logger.Debug("could not read network device, skipping it",
				"device", name, "err", err)
			skipped++
			continue
		}
		out = append(out, *iface)
	}
	if skipped > 0 {
		c.logger.Debug("some network devices were unreadable and were skipped",
			"skipped", skipped, "reported", len(out))
	}
	return out, nil
}

// adminState renders the IFF_UP flag as upstream does.
//
// Bit 0 of the interface flags is IFF_UP. Upstream reports "up"/"down" as a label
// value on node_network_info.
func adminState(flags *int64) string {
	if flags == nil {
		return "unknown"
	}
	if *flags&0x01 != 0 {
		return "up"
	}
	return "down"
}
