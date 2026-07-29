package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/netdev_common.go
//                 node_exporter/collector/netdev_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// THE SUBTLE PART. Upstream's legacy() transformation does not merely rename
// fields — it SUMS several kernel error counters into a single metric. For
// example node_network_receive_frame_total is the sum of receive_frame_errors,
// receive_length_errors, receive_over_errors and receive_crc_errors. Getting that
// wrong produces a plausible-but-wrong number that no name comparison would
// catch: the metric exists, has the right type and labels, and reports the wrong
// value. The mapping below is copied field for field and asserted against
// upstream's source in tests.
//
// The transformation is applied unless --collector.netdev.enable-detailed-metrics
// is set, which defaults off. Detailed mode is not ported; it produces
// deliberately incompatible names and nothing on EKS uses it.
//
// SCOPE. --collector.netdev.address-info (node_network_address_info) defaults off
// and is not ported. Recorded in docs/parity-exceptions-nodep.md.
//
// THE BACKEND IS NETLINK, NOT procfs, AND THAT IS A PARITY REQUIREMENT.
//
// Upstream's --collector.netdev.netlink defaults to TRUE, so /proc/net/dev is only the
// fallback. The two backends do NOT expose the same field set: netlink's
// rtnetlink.LinkStats64 carries RXNoHandler, which /proc/net/dev has no column for at
// all. So a procfs-based port silently loses node_network_receive_nohandler_total --
// 7 series on the live EKS node.
//
// I built this on procfs first, and the omission only surfaced when the two
// implementations were diffed end to end: 304 families upstream versus 303 native, with
// exactly that one metric missing. Every per-collector test passed, because they compare
// my port against ITS OWN table rather than against the endpoint upstream serves. That
// is the argument for the three-way comparison existing at all.
//
// legacy() applies to BOTH backends. Upstream calls it in Update, after getNetDevStats,
// so it runs on whichever map was produced. I first assumed netlink names were already
// final and skipped it -- the diff then showed 17 pre-legacy names appearing and 10
// post-legacy names missing, which is exactly what skipping the transformation looks
// like.
//
// NOTE ON DEVICE FILTERING. No EKS-specific device exclusion is applied here, matching
// the dependency branch's decision: excluding interfaces from netdev would shrink the
// device label space for no resilience benefit.

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/jsimonetti/rtnetlink/v2"
	"github.com/mdlayher/netlink"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
)

const netdevSubsystem = "network"

func init() {
	register("netdev", true, newNetDevCollector)
}

type netDevCollector struct {
	fs     procfs.FS
	logger *slog.Logger

	// descs are built lazily because the field set comes from the kernel, so it
	// cannot be enumerated up front. Guarded because Collect runs concurrently.
	descsMu sync.Mutex
	descs   map[string]*prometheus.Desc

	// netlinkStats is the PRIMARY backend, matching upstream's default. Injectable so
	// the fallback path is reachable in a test, and so a sandbox without a netlink
	// socket still exercises the collector.
	netlinkStats func() (map[string]map[string]uint64, error)
}

func newNetDevCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	c := &netDevCollector{
		fs:     fs,
		logger: logger,
		descs:  map[string]*prometheus.Desc{},
	}
	c.netlinkStats = netDevNetlinkStats
	return c, nil
}

func (c *netDevCollector) Update(ch chan<- prometheus.Metric) error {
	byDevice, err := c.stats()
	if err != nil {
		return fmt.Errorf("couldn't get netdev stats: %w", err)
	}

	for device, fields := range byDevice {
		for key, value := range fields {
			ch <- prometheus.MustNewConstMetric(
				c.desc(key), prometheus.CounterValue, float64(value), device)
		}
	}
	return nil
}

// stats returns per-device counters, preferring netlink as upstream does.
//
// A netlink failure falls back to /proc/net/dev rather than failing: the procfs path
// yields one metric fewer (no receive_nohandler) but every other counter, which beats no
// network metrics at all on a host where the netlink socket is unavailable.
func (c *netDevCollector) stats() (map[string]map[string]uint64, error) {
	byDevice, err := c.netlinkStats()
	if err == nil {
		return byDevice, nil
	}
	c.logger.Debug("netlink netdev stats unavailable, falling back to /proc/net/dev", "err", err)

	lines, procErr := c.fs.NetDev()
	if procErr != nil {
		// Report the ORIGINAL netlink error too: "procfs failed" alone would hide that
		// the primary backend was tried first, and why it did not work.
		return nil, fmt.Errorf("netlink failed (%v) and procfs failed: %w", err, procErr)
	}

	byDevice = make(map[string]map[string]uint64, len(lines))
	for device, stats := range lines {
		fields := netDevFields(&stats)
		applyLegacyNames(fields)
		byDevice[device] = fields
	}
	return byDevice, nil
}

// netDevNetlinkStats reads per-device counters over rtnetlink.
func netDevNetlinkStats() (map[string]map[string]uint64, error) {
	return netDevNetlinkStatsWith(netDevLinkQuery(rtnetlink.Dial))
}

// netDevLinkQuery builds the link-list query from a dialer.
//
// The dial and the list are separated so BOTH failure paths are testable. *rtnetlink.Conn
// is a concrete struct with an embedded Link service, so it cannot be faked -- hence the
// list step takes the two operations it needs as functions rather than the connection.
func netDevLinkQuery(dial func(*netlink.Config) (*rtnetlink.Conn, error)) func() ([]rtnetlink.LinkMessage, func(), error) {
	return func() ([]rtnetlink.LinkMessage, func(), error) {
		conn, err := dial(nil)
		if err != nil {
			return nil, nil, err
		}
		return netDevListLinks(conn.Link.List, conn.Close)
	}
}

// netDevListLinks lists links and hands back a closer, closing on failure.
//
// THE CLOSE-ON-ERROR IS THE POINT. On the success path the caller closes via the
// returned func, so nothing else does; left unclosed on the error path this leaks a
// netlink socket per scrape, and at a 15s interval that exhausts the fd limit within
// hours. The arp collector had exactly this defect, found the same way -- by making the
// error path reachable rather than by reading the code.
func netDevListLinks(
	list func() ([]rtnetlink.LinkMessage, error),
	closeConn func() error,
) ([]rtnetlink.LinkMessage, func(), error) {
	links, err := list()
	if err != nil {
		_ = closeConn()
		return nil, nil, err
	}
	return links, func() { _ = closeConn() }, nil
}

// netDevNetlinkStatsWith is the seam: it takes a function producing the link list, so
// both error returns are reachable without a netlink socket that fails on demand.
func netDevNetlinkStatsWith(query func() ([]rtnetlink.LinkMessage, func(), error)) (map[string]map[string]uint64, error) {
	links, closeConn, err := query()
	if err != nil {
		return nil, err
	}
	if closeConn != nil {
		defer closeConn()
	}
	return netDevStatsFromLinks(links), nil
}

// netDevStatsFromLinks maps rtnetlink link messages onto per-device counters.
//
// Split from the socket handling so the two skip conditions -- a link with no
// attributes, and attributes carrying neither 32- nor 64-bit stats -- are reachable
// without a netlink socket. Both are guards upstream also has, and both would nil-panic
// without them; a panic in a collector is far worse than a missing device.
func netDevStatsFromLinks(links []rtnetlink.LinkMessage) map[string]map[string]uint64 {
	byDevice := make(map[string]map[string]uint64, len(links))

	for _, msg := range links {
		if msg.Attributes == nil {
			continue
		}
		stats := msg.Attributes.Stats64
		if stats == nil {
			// A kernel reporting only 32-bit stats. Widened rather than skipped: the
			// counters are still correct, just narrower.
			if s32 := msg.Attributes.Stats; s32 != nil {
				stats = widenNetDevStats32(s32)
			}
		}
		if stats == nil {
			continue
		}

		fields := netDevNetlinkFields(stats)
		// The SAME transformation the procfs path gets, for the reason in the file
		// comment above.
		applyLegacyNames(fields)
		byDevice[msg.Attributes.Name] = fields
	}
	return byDevice
}

// widenNetDevStats32 converts 32-bit link stats to the 64-bit form.
//
// Only the fields netDevNetlinkFields reads are converted. Any field this misses would
// silently report 0 on such a kernel, which is why the test sets every 32-bit field to a
// distinct non-zero value and asserts no mapped output is zero.
func widenNetDevStats32(s *rtnetlink.LinkStats) *rtnetlink.LinkStats64 {
	return &rtnetlink.LinkStats64{
		RXPackets: uint64(s.RXPackets), TXPackets: uint64(s.TXPackets),
		RXBytes: uint64(s.RXBytes), TXBytes: uint64(s.TXBytes),
		RXErrors: uint64(s.RXErrors), TXErrors: uint64(s.TXErrors),
		RXDropped: uint64(s.RXDropped), TXDropped: uint64(s.TXDropped),
		Multicast: uint64(s.Multicast), Collisions: uint64(s.Collisions),
		RXLengthErrors: uint64(s.RXLengthErrors), RXOverErrors: uint64(s.RXOverErrors),
		RXCRCErrors: uint64(s.RXCRCErrors), RXFrameErrors: uint64(s.RXFrameErrors),
		RXFIFOErrors: uint64(s.RXFIFOErrors), RXMissedErrors: uint64(s.RXMissedErrors),
		TXAbortedErrors: uint64(s.TXAbortedErrors), TXCarrierErrors: uint64(s.TXCarrierErrors),
		TXFIFOErrors: uint64(s.TXFIFOErrors), TXHeartbeatErrors: uint64(s.TXHeartbeatErrors),
		TXWindowErrors: uint64(s.TXWindowErrors),
		RXCompressed:   uint64(s.RXCompressed), TXCompressed: uint64(s.TXCompressed),
		RXNoHandler: uint64(s.RXNoHandler),
	}
}

// netDevNetlinkFields maps rtnetlink stats onto the PRE-legacy metric names.
//
// Upstream's map verbatim. NOTE receive_nohandler at the end: it exists ONLY here, not
// in /proc/net/dev, which is why the procfs fallback yields one metric fewer.
// See https://github.com/torvalds/linux/blob/master/include/uapi/linux/if_link.h
func netDevNetlinkFields(s *rtnetlink.LinkStats64) map[string]uint64 {
	return map[string]uint64{
		"receive_packets":  s.RXPackets,
		"transmit_packets": s.TXPackets,
		"receive_bytes":    s.RXBytes,
		"transmit_bytes":   s.TXBytes,
		"receive_errors":   s.RXErrors,
		"transmit_errors":  s.TXErrors,
		"receive_dropped":  s.RXDropped,
		"transmit_dropped": s.TXDropped,
		"multicast":        s.Multicast,
		"collisions":       s.Collisions,

		// detailed rx_errors
		"receive_length_errors": s.RXLengthErrors,
		"receive_over_errors":   s.RXOverErrors,
		"receive_crc_errors":    s.RXCRCErrors,
		"receive_frame_errors":  s.RXFrameErrors,
		"receive_fifo_errors":   s.RXFIFOErrors,
		"receive_missed_errors": s.RXMissedErrors,

		// detailed tx_errors
		"transmit_aborted_errors":   s.TXAbortedErrors,
		"transmit_carrier_errors":   s.TXCarrierErrors,
		"transmit_fifo_errors":      s.TXFIFOErrors,
		"transmit_heartbeat_errors": s.TXHeartbeatErrors,
		"transmit_window_errors":    s.TXWindowErrors,

		// for cslip etc
		"receive_compressed":  s.RXCompressed,
		"transmit_compressed": s.TXCompressed,
		"receive_nohandler":   s.RXNoHandler,
	}
}

// desc returns (and caches) the descriptor for a field.
//
// The "_total" suffix is appended here, not stored in the field names, matching
// upstream. So the field "receive_bytes" becomes node_network_receive_bytes_total.
func (c *netDevCollector) desc(key string) *prometheus.Desc {
	c.descsMu.Lock()
	defer c.descsMu.Unlock()

	if d, ok := c.descs[key]; ok {
		return d
	}
	d := prometheus.NewDesc(
		prometheus.BuildFQName(namespace, netdevSubsystem, key+"_total"),
		fmt.Sprintf("Network device statistic %s.", key),
		[]string{"device"}, nil,
	)
	c.descs[key] = d
	return d
}

// netDevFields flattens procfs's per-device line into upstream's field names.
//
// These are the *pre-legacy* names, matching what upstream's getNetDevStats
// produces before legacy() runs. Keeping that two-step structure means the
// transformation below can be diffed against upstream's directly.
func netDevFields(l *procfs.NetDevLine) map[string]uint64 {
	return map[string]uint64{
		"receive_bytes":           l.RxBytes,
		"receive_packets":         l.RxPackets,
		"receive_errors":          l.RxErrors,
		"receive_dropped":         l.RxDropped,
		"receive_fifo_errors":     l.RxFIFO,
		"receive_frame_errors":    l.RxFrame,
		"receive_compressed":      l.RxCompressed,
		"multicast":               l.RxMulticast,
		"transmit_bytes":          l.TxBytes,
		"transmit_packets":        l.TxPackets,
		"transmit_errors":         l.TxErrors,
		"transmit_dropped":        l.TxDropped,
		"transmit_fifo_errors":    l.TxFIFO,
		"collisions":              l.TxCollisions,
		"transmit_carrier_errors": l.TxCarrier,
		"transmit_compressed":     l.TxCompressed,
	}
}

// legacyRule describes one entry in upstream's legacy() transformation: a source
// field renamed to a target, optionally summing additional fields into it.
type legacyRule struct {
	from string
	to   string
	// plus are fields folded into the target and removed. This is the part that
	// matters: the target is a SUM, not a rename, so omitting a contributor
	// produces a metric that exists with the right name and the wrong value.
	plus []string
}

// legacyRules is upstream's legacy() field for field. Asserted against upstream's
// source in tests, because a missing contributor is invisible to any name or label
// comparison.
func legacyRules() []legacyRule {
	return []legacyRule{
		{from: "receive_errors", to: "receive_errs"},
		{from: "receive_dropped", to: "receive_drop", plus: []string{"receive_missed_errors"}},
		{from: "receive_fifo_errors", to: "receive_fifo"},
		{from: "receive_frame_errors", to: "receive_frame", plus: []string{
			"receive_length_errors", "receive_over_errors", "receive_crc_errors",
		}},
		{from: "multicast", to: "receive_multicast"},
		{from: "transmit_errors", to: "transmit_errs"},
		{from: "transmit_dropped", to: "transmit_drop"},
		{from: "transmit_fifo_errors", to: "transmit_fifo"},
		{from: "collisions", to: "transmit_colls"},
		{from: "transmit_carrier_errors", to: "transmit_carrier", plus: []string{
			"transmit_aborted_errors", "transmit_heartbeat_errors", "transmit_window_errors",
		}},
	}
}

// applyLegacyNames rewrites kernel field names to the stable metric names, summing
// contributors as upstream does.
//
// A rule only fires if its source field is present, matching upstream: a kernel
// that does not report a field must not produce a zero-valued metric, because zero
// errors and unknown errors are different claims.
func applyLegacyNames(fields map[string]uint64) {
	for _, rule := range legacyRules() {
		value, ok := fields[rule.from]
		if !ok {
			continue
		}
		delete(fields, rule.from)
		for _, extra := range rule.plus {
			// Contributors are removed whether or not they were present, so they
			// never surface as separate metrics.
			value += fields[extra]
			delete(fields, extra)
		}
		fields[rule.to] = value
	}
}
