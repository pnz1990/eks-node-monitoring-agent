package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/cpu_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// SCOPE. Upstream's cpu collector has ten metric families. On an EKS node only
// two produce data — node_cpu_seconds_total and node_cpu_guest_seconds_total —
// because EC2 does not expose the sysfs that backs the others (core and package
// throttles, frequency, flag/bug info, isolated, online, info). Verified against a
// live node: PNE emits exactly the same two families with success=1, so this is
// absent hardware rather than a gap in either implementation.
//
// The sysfs-backed families are therefore NOT ported. Recorded in
// docs/parity-exceptions-nodep.md. If this ever runs on hardware that exposes
// them, they will be missing relative to upstream and that is a known limitation.
//
// THE PART THAT MATTERS MOST. The monotonicity cache below is not an optimisation
// and must not be simplified away. Kernel CPU counters can jump *backwards* — on
// CPU hotplug, and on some hypervisors — and a Prometheus counter that decreases
// makes rate() produce a spike or a gap. Upstream carries a per-CPU cache that
// only ever moves a counter forward, plus a heuristic that resets a CPU's stats
// entirely if idle jumps back by more than a threshold. A port that dropped this
// would look correct in every unit test and produce wrong graphs in production.

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
)

const cpuSubsystem = "cpu"

// jumpBackSeconds is the idle-counter regression that triggers a full reset of a
// CPU's cached stats, on the assumption that the CPU was hotplugged. Upstream's
// value, kept identical: a different threshold would change which events reset.
const jumpBackSeconds = 3.0

func init() {
	register("cpu", true, newCPUCollector)
}

type cpuCollector struct {
	fs     procfs.FS
	logger *slog.Logger

	cpu      *prometheus.Desc
	cpuGuest *prometheus.Desc

	// includeGuest mirrors upstream's --collector.cpu.guest, default on. Guest time
	// is also counted inside user and nice, so it is exposed separately rather than
	// added.
	includeGuest bool

	// cpuStats caches the last-seen value per CPU so counters only move forward.
	// Guarded by mu because Collect runs collectors concurrently.
	mu       sync.Mutex
	cpuStats map[int64]procfs.CPUStat
}

func newCPUCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	return &cpuCollector{
		fs:     fs,
		logger: logger,
		cpu: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, cpuSubsystem, "seconds_total"),
			"Seconds the CPUs spent in each mode.",
			[]string{"cpu", "mode"}, nil,
		),
		cpuGuest: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, cpuSubsystem, "guest_seconds_total"),
			"Seconds the CPUs spent in guests (VMs) for each mode.",
			[]string{"cpu", "mode"}, nil,
		),
		includeGuest: true,
		cpuStats:     map[int64]procfs.CPUStat{},
	}, nil
}

func (c *cpuCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := c.fs.Stat()
	if err != nil {
		return fmt.Errorf("couldn't get cpu stats: %w", err)
	}

	c.updateCache(stats.CPU)

	c.mu.Lock()
	defer c.mu.Unlock()
	for cpuID, stat := range c.cpuStats {
		cpuNum := strconv.Itoa(int(cpuID))
		// Mode label values are part of the contract: recording rules select on
		// mode!="idle". Any rename breaks them.
		ch <- prometheus.MustNewConstMetric(c.cpu, prometheus.CounterValue, stat.User, cpuNum, "user")
		ch <- prometheus.MustNewConstMetric(c.cpu, prometheus.CounterValue, stat.Nice, cpuNum, "nice")
		ch <- prometheus.MustNewConstMetric(c.cpu, prometheus.CounterValue, stat.System, cpuNum, "system")
		ch <- prometheus.MustNewConstMetric(c.cpu, prometheus.CounterValue, stat.Idle, cpuNum, "idle")
		ch <- prometheus.MustNewConstMetric(c.cpu, prometheus.CounterValue, stat.Iowait, cpuNum, "iowait")
		ch <- prometheus.MustNewConstMetric(c.cpu, prometheus.CounterValue, stat.IRQ, cpuNum, "irq")
		ch <- prometheus.MustNewConstMetric(c.cpu, prometheus.CounterValue, stat.SoftIRQ, cpuNum, "softirq")
		ch <- prometheus.MustNewConstMetric(c.cpu, prometheus.CounterValue, stat.Steal, cpuNum, "steal")

		if c.includeGuest {
			// Guest time is already included in User and Nice; exposed separately so
			// it can be subtracted, not added.
			ch <- prometheus.MustNewConstMetric(c.cpuGuest, prometheus.CounterValue, stat.Guest, cpuNum, "user")
			ch <- prometheus.MustNewConstMetric(c.cpuGuest, prometheus.CounterValue, stat.GuestNice, cpuNum, "nice")
		}
	}
	return nil
}

// updateCache merges freshly read stats into the cache, never letting a counter
// move backwards.
//
// Upstream implements this as ten near-identical if-blocks. Expressed here as a
// table over field accessors so the monotonicity rule is stated once instead of
// ten times — the behaviour is identical, and a table cannot develop a
// copy-paste inconsistency between fields, which ten blocks can.
func (c *cpuCollector) updateCache(fresh map[int64]procfs.CPUStat) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for id, next := range fresh {
		cached := c.cpuStats[id]

		// A large backwards jump in idle means this CPU was almost certainly
		// hotplugged and its counters restarted. Keeping the old values would
		// freeze the series; resetting lets it climb again from the new baseline.
		if (cached.Idle - next.Idle) >= jumpBackSeconds {
			c.logger.Debug("CPU idle counter jumped backwards far enough to assume a hotplug event; resetting",
				"cpu", id, "old_value", cached.Idle, "new_value", next.Idle)
			cached = procfs.CPUStat{}
		}

		for _, f := range cpuStatFields(&cached, &next) {
			if *f.next >= *f.cached {
				*f.cached = *f.next
			} else {
				// A small regression is not a hotplug: keep the higher value so the
				// counter never decreases, and log it, exactly as upstream does.
				c.logger.Debug("CPU counter jumped backwards",
					"cpu", id, "mode", f.name, "old_value", *f.cached, "new_value", *f.next)
			}
		}

		c.cpuStats[id] = cached
	}

	// Drop CPUs that have gone offline, or their stale series would be reported
	// forever.
	if len(fresh) != len(c.cpuStats) {
		online := slices.Collect(maps.Keys(fresh))
		maps.DeleteFunc(c.cpuStats, func(id int64, _ procfs.CPUStat) bool {
			return !slices.Contains(online, id)
		})
	}
}

// cpuStatField pairs a cached field with its fresh counterpart for the
// monotonicity check.
type cpuStatField struct {
	name   string
	cached *float64
	next   *float64
}

// cpuStatFields lists every counter subject to the monotonicity rule. Adding a
// field to the emitted set without adding it here would let that counter move
// backwards, so the two lists are asserted equal in tests.
func cpuStatFields(cached, next *procfs.CPUStat) []cpuStatField {
	return []cpuStatField{
		{"idle", &cached.Idle, &next.Idle},
		{"user", &cached.User, &next.User},
		{"nice", &cached.Nice, &next.Nice},
		{"system", &cached.System, &next.System},
		{"iowait", &cached.Iowait, &next.Iowait},
		{"irq", &cached.IRQ, &next.IRQ},
		{"softirq", &cached.SoftIRQ, &next.SoftIRQ},
		{"steal", &cached.Steal, &next.Steal},
		{"guest", &cached.Guest, &next.Guest},
		{"guest_nice", &cached.GuestNice, &next.GuestNice},
	}
}
