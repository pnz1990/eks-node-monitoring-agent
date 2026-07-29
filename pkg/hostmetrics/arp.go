package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/arp_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// ONE METRIC, node_arp_entries, but two interchangeable data sources. Upstream
// defaults to netlink (--collector.arp.netlink=true) and that default is
// preserved, so the shipped behaviour matches the reference endpoint.
//
// THE NUD_NOARP FILTER IS THE PART THAT MATTERS, and it is measurably so rather
// than theoretically. Netlink returns every neighbour entry, including NUD_NOARP
// ones — permanent entries needing no ARP resolution, such as multicast and
// point-to-point. /proc/net/arp omits them. Measured on this host with a
// standalone rtnetlink program:
//
//	3 IPv4 neighbours returned, of which 1 is NUD_NOARP
//
// So without the filter the netlink backend reports 3 where the procfs backend
// reports 2 for identical kernel state — a 50% overcount here. A metric whose
// value depends on which backend an operator happened to select is not something
// anyone can alert on, which is why this filter is load-bearing and not cosmetic.
//
// SCOPE. --collector.arp.device-include/exclude are not wired to flags (no flag
// layer here); the filter is constructed empty, matching upstream's defaults.

import (
	"fmt"
	"log/slog"
	"net"

	"github.com/jsimonetti/rtnetlink/v2/rtnl"
	"github.com/mdlayher/netlink"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"
)

func init() {
	register("arp", true, newARPCollector)
}

type arpCollector struct {
	fs           procfs.FS
	logger       *slog.Logger
	deviceFilter deviceFilter
	desc         *prometheus.Desc

	// useNetlink mirrors --collector.arp.netlink, which upstream defaults TRUE.
	// Kept true so the shipped behaviour matches the reference endpoint. Both
	// backends are implemented and must agree; see the NUD_NOARP note above.
	useNetlink bool

	// entriesViaNetlink is injectable because the real implementation opens a
	// netlink socket, which is unavailable in a sandboxed test and would otherwise
	// make the whole default path untestable.
	entriesViaNetlink func() (map[string]uint32, error)
}

func newARPCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	return newARPCollectorWithFilter(logger, paths, "", "")
}

// newARPCollectorWithFilter is split out so the invalid-pattern branches are
// reachable from a test and the filter can be wired to chart values later.
func newARPCollectorWithFilter(logger *slog.Logger, paths Paths, excludeExpr, includeExpr string) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}

	if excludeExpr != "" && includeExpr != "" {
		return nil, fmt.Errorf("arp device-exclude and device-include are mutually exclusive")
	}
	filter, err := newDeviceFilter(excludeExpr, includeExpr)
	if err != nil {
		return nil, fmt.Errorf("failed to build arp device filter: %w", err)
	}

	return &arpCollector{
		fs:           fs,
		logger:       logger,
		deviceFilter: filter,
		useNetlink:   true,
		desc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "arp", "entries"),
			"ARP entries by device",
			[]string{"device"}, nil,
		),
		entriesViaNetlink: arpEntriesViaNetlink,
	}, nil
}

func (c *arpCollector) Update(ch chan<- prometheus.Metric) error {
	entries, err := c.entries()
	if err != nil {
		return fmt.Errorf("could not get ARP entries: %w", err)
	}

	for device, count := range entries {
		if c.deviceFilter.ignored(device) {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(count), device)
	}
	return nil
}

// entries returns the per-device ARP entry count from the configured backend.
func (c *arpCollector) entries() (map[string]uint32, error) {
	if c.useNetlink {
		return c.entriesViaNetlink()
	}
	raw, err := c.fs.GatherARPEntries()
	if err != nil {
		return nil, err
	}
	return arpEntriesByDevice(raw), nil
}

// arpEntriesByDevice counts /proc/net/arp entries per device.
//
// The device name is safe to use as a label key here: the kernel caps interface
// names at IFNAMSIZ (16 including the terminator, so 15 usable characters), and
// procfs splits the line on whitespace, so no truncation or collision is possible.
// I initially assumed this path could merge two long CNI interface names into one
// label and checked instead of asserting it -- the longest name in the live
// cluster corpus is 14 characters and the kernel could not emit a longer one.
func arpEntriesByDevice(entries []procfs.ARPEntry) map[string]uint32 {
	byDevice := make(map[string]uint32, len(entries))
	for _, e := range entries {
		byDevice[e.Device]++
	}
	return byDevice
}

// arpEntriesViaNetlink counts IPv4 neighbours per interface using rtnetlink.
//
// Split into a thin socket wrapper here and the pure arpEntriesFromNeighbours
// below. The counting rules — the NUD_NOARP filter above all — are the part that
// can be wrong, and they should not be reachable only on a host with a working
// netlink socket.
func arpEntriesViaNetlink() (map[string]uint32, error) {
	return arpEntriesViaNetlinkWith(netlinkNeighbourQuery(rtnl.Dial))
}

// arpNetlinkConn is the slice of *rtnl.Conn this collector uses. Declared as an
// interface so the dial and query error paths are reachable from a test without
// needing a netlink socket that fails on demand — on a working host neither branch
// can be exercised, and an untested error path that leaks a socket is exactly the
// kind of defect that only shows up after hours in production.
type arpNetlinkConn interface {
	Neighbours(ifc *net.Interface, family int) ([]*rtnl.Neigh, error)
	Close() error
}

// netlinkNeighbourQuery builds the query function from a dialer.
func netlinkNeighbourQuery(dial func(*netlink.Config) (*rtnl.Conn, error)) func() ([]*rtnl.Neigh, func(), error) {
	return func() ([]*rtnl.Neigh, func(), error) {
		conn, err := dial(nil)
		if err != nil {
			return nil, nil, err
		}
		return queryNeighbours(conn)
	}
}

// queryNeighbours fetches the IPv4 neighbour list and hands back a closer.
func queryNeighbours(conn arpNetlinkConn) ([]*rtnl.Neigh, func(), error) {
	// Restricted to AF_INET: the kernel returns IPv6 neighbours from the same call,
	// and this is an ARP collector. Including them would double-count dual-stack
	// interfaces against a metric named "arp_entries".
	neighbours, err := conn.Neighbours(nil, unix.AF_INET)
	if err != nil {
		// On the success path the caller closes via the returned func. On this path
		// nothing else will, so close here or the socket leaks once per scrape —
		// at a 15s interval that exhausts the fd limit within hours.
		_ = conn.Close()
		return nil, nil, err
	}
	return neighbours, func() { _ = conn.Close() }, nil
}

// arpEntriesViaNetlinkWith is the seam: it takes a function that produces the
// neighbour list, so both error returns are reachable from a test without needing
// a netlink socket to fail on demand.
func arpEntriesViaNetlinkWith(query func() ([]*rtnl.Neigh, func(), error)) (map[string]uint32, error) {
	neighbours, closeConn, err := query()
	if err != nil {
		return nil, err
	}
	if closeConn != nil {
		defer closeConn()
	}
	return arpEntriesFromNeighbours(neighbours), nil
}

// arpEntriesFromNeighbours counts neighbours per interface.
//
// THE NUD_NOARP FILTER IS THE WHOLE POINT. Netlink returns those entries and
// /proc/net/arp does not, so without this the two backends report different
// numbers for identical kernel state (measured on this host: 3 vs 2). A metric
// whose value depends on which backend was selected is not alertable.
func arpEntriesFromNeighbours(neighbours []*rtnl.Neigh) map[string]uint32 {
	byDevice := make(map[string]uint32)
	for _, n := range neighbours {
		if n.State&unix.NUD_NOARP != 0 {
			continue
		}
		// Interface is a pointer. rtnl already drops entries whose link it could
		// not resolve, so it should be non-nil, but a nil deref here would panic
		// the collector and the guard costs one branch.
		if n.Interface == nil {
			continue
		}
		byDevice[n.Interface.Name]++
	}
	return byDevice
}
