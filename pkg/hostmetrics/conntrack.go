package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/conntrack_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// THE MOST OPERATIONALLY IMPORTANT COLLECTOR IN THIS SET FOR EKS, and the reason
// is the ratio between two of its metrics. node_nf_conntrack_entries against
// node_nf_conntrack_entries_limit is THE signal for conntrack table exhaustion,
// which on a Kubernetes node presents as random connection failures and DNS
// timeouts rather than as anything that looks like a network problem. kube-proxy in
// iptables mode creates a conntrack entry per connection, so a node running
// hundreds of pods can approach nf_conntrack_max under ordinary traffic.
//
// NOTE ON METRIC NAMES: the subsystem is EMPTY, so these are node_nf_conntrack_*
// and NOT node_conntrack_nf_conntrack_*. Passing "conntrack" as the subsystem
// would produce plausible-looking names that no existing dashboard matches — the
// same trap as the stat collector's node_intr_total.
//
// TWO PROCFS LOCATIONS, deliberately: the count and limit come from
// /proc/sys/net/netfilter/* (sysctl values, read as plain integers) while the
// stat_* counters come from /proc/net/stat/nf_conntrack (per-CPU rows that must be
// SUMMED). Reading the sysctls through procfs's typed API is not possible, hence
// the direct file reads.
//
// EVERYTHING IS A GAUGE, including the stat_* fields whose names sound cumulative.
// That is upstream's choice and it is preserved for parity, but it is worth being
// explicit that it is arguably wrong: stat_drop and stat_early_drop ARE monotonic
// kernel counters, and typing them as gauges means rate() over them is unsupported
// by the type even though the underlying data would support it. Changing it would
// break the contract, so it stays; recorded in docs/parity-exceptions-nodep.md.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
)

func init() {
	register("conntrack", true, newConntrackCollector)
}

type conntrackCollector struct {
	fs     procfs.FS
	logger *slog.Logger
	paths  Paths

	// conntrackStat is injectable so the stat-file failure path is reachable: on a
	// host with conntrack loaded the file always parses.
	conntrackStat func() ([]procfs.ConntrackStatEntry, error)

	current *prometheus.Desc
	limit   *prometheus.Desc
	stats   []conntrackStatDesc
}

// conntrackStatDesc pairs a descriptor with the field it sums out of the per-CPU
// rows. Held as a table so the set can be diffed against upstream and so adding a
// field cannot silently skip its accumulation.
type conntrackStatDesc struct {
	desc  *prometheus.Desc
	value func(*procfs.ConntrackStatEntry) uint64
}

func newConntrackCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}

	// Empty subsystem: the names are node_nf_conntrack_*, not
	// node_conntrack_nf_conntrack_*.
	desc := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "", name), help, nil, nil)
	}

	c := &conntrackCollector{
		fs:     fs,
		logger: logger,
		paths:  paths,
		current: desc("nf_conntrack_entries",
			"Number of currently allocated flow entries for connection tracking."),
		limit: desc("nf_conntrack_entries_limit",
			"Maximum size of connection tracking table."),
		stats: []conntrackStatDesc{
			{desc("nf_conntrack_stat_found",
				"Number of searched entries which were successful."),
				func(e *procfs.ConntrackStatEntry) uint64 { return e.Found }},
			{desc("nf_conntrack_stat_invalid",
				"Number of packets seen which can not be tracked."),
				func(e *procfs.ConntrackStatEntry) uint64 { return e.Invalid }},
			{desc("nf_conntrack_stat_ignore",
				"Number of packets seen which are already connected to a conntrack entry."),
				func(e *procfs.ConntrackStatEntry) uint64 { return e.Ignore }},
			{desc("nf_conntrack_stat_insert",
				"Number of entries inserted into the list."),
				func(e *procfs.ConntrackStatEntry) uint64 { return e.Insert }},
			{desc("nf_conntrack_stat_insert_failed",
				"Number of entries for which list insertion was attempted but failed."),
				func(e *procfs.ConntrackStatEntry) uint64 { return e.InsertFailed }},
			{desc("nf_conntrack_stat_drop",
				"Number of packets dropped due to conntrack failure."),
				func(e *procfs.ConntrackStatEntry) uint64 { return e.Drop }},
			{desc("nf_conntrack_stat_early_drop",
				"Number of dropped conntrack entries to make room for new ones, if maximum table size was reached."),
				func(e *procfs.ConntrackStatEntry) uint64 { return e.EarlyDrop }},
			{desc("nf_conntrack_stat_search_restart",
				"Number of conntrack table lookups which had to be restarted due to hashtable resizes."),
				func(e *procfs.ConntrackStatEntry) uint64 { return e.SearchRestart }},
		},
	}
	c.conntrackStat = c.fs.ConntrackStat
	return c, nil
}

func (c *conntrackCollector) Update(ch chan<- prometheus.Metric) error {
	current, err := readUintFromFile(c.paths.procPath("sys", "net", "netfilter", "nf_conntrack_count"))
	if err != nil {
		return c.handleErr(err)
	}
	ch <- prometheus.MustNewConstMetric(c.current, prometheus.GaugeValue, float64(current))

	limit, err := readUintFromFile(c.paths.procPath("sys", "net", "netfilter", "nf_conntrack_max"))
	if err != nil {
		return c.handleErr(err)
	}
	ch <- prometheus.MustNewConstMetric(c.limit, prometheus.GaugeValue, float64(limit))

	entries, err := c.conntrackStat()
	if err != nil {
		return c.handleErr(err)
	}

	// /proc/net/stat/nf_conntrack has one row PER CPU. The exported metrics are
	// node-wide, so each field is summed across rows. Reporting a single row (or
	// the last one) would silently report one CPU's share of the traffic.
	for _, s := range c.stats {
		var total uint64
		for i := range entries {
			total += s.value(&entries[i])
		}
		ch <- prometheus.MustNewConstMetric(s.desc, prometheus.GaugeValue, float64(total))
	}
	return nil
}

// handleErr maps a read failure to either ErrNoData or a real error.
//
// The distinction is the whole point: the nf_conntrack module being absent is a
// legitimate configuration (a node with no iptables-based networking), and
// reporting it as a scrape failure would alert on a working system. Anything else
// is a genuine failure.
func (c *conntrackCollector) handleErr(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		c.logger.Debug("conntrack probably not loaded")
		return ErrNoData
	}
	return fmt.Errorf("failed to retrieve conntrack stats: %w", err)
}

// readUintFromFile reads a file containing a single unsigned integer.
//
// PROVENANCE: node_exporter/collector/fixtures... helpers.go readUintFromFile.
// Shared by several collectors upstream; kept here as the first user.
//
// The error is returned unwrapped when the file is missing, because handleErr
// needs errors.Is(err, os.ErrNotExist) to still match — wrapping it in a message
// here would work, but a future refactor that formatted rather than wrapped would
// silently turn "module not loaded" into a reported failure.
func readUintFromFile(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	return value, nil
}
