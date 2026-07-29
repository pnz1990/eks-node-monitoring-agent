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
// NOTE ON DEVICE FILTERING. Unlike the filesystem collector, netdev reads a single
// file (/proc/net/dev), so it has neither the listing-then-read race nor a
// per-device cost. No EKS-specific device exclusion is applied here, matching the
// dependency branch's decision: excluding interfaces from netdev would shrink the
// device label space for no resilience benefit.

import (
	"fmt"
	"log/slog"
	"sync"

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
}

func newNetDevCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	return &netDevCollector{
		fs:     fs,
		logger: logger,
		descs:  map[string]*prometheus.Desc{},
	}, nil
}

func (c *netDevCollector) Update(ch chan<- prometheus.Metric) error {
	lines, err := c.fs.NetDev()
	if err != nil {
		return fmt.Errorf("couldn't get netdev stats: %w", err)
	}

	for device, stats := range lines {
		fields := netDevFields(&stats)
		applyLegacyNames(fields)

		for key, value := range fields {
			ch <- prometheus.MustNewConstMetric(
				c.desc(key), prometheus.CounterValue, float64(value), device)
		}
	}
	return nil
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
