package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/softnet_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// A near-verbatim port: procfs does the parsing and there is no filtering, no unit
// conversion and no flag surface, so there is very little room to diverge.
//
// WHY THIS ONE MATTERS ON EKS. node_softnet_dropped_total and
// node_softnet_times_squeezed_total are the two counters that show packet loss in
// the kernel's receive path — the backlog overflowing, or NAPI running out of
// budget before draining the queue. On a node running hundreds of pods behind the
// VPC CNI, that is a real and otherwise invisible failure mode, and it is why the
// per-CPU cardinality (7 metrics x nCPU) is worth paying for.
//
// backlog_len is the only gauge: it is a queue depth, not a cumulative count.

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
)

const softnetSubsystem = "softnet"

func init() {
	register("softnet", true, newSoftnetCollector)
}

type softnetCollector struct {
	fs     procfs.FS
	logger *slog.Logger

	processed         *prometheus.Desc
	dropped           *prometheus.Desc
	timeSqueezed      *prometheus.Desc
	cpuCollision      *prometheus.Desc
	receivedRps       *prometheus.Desc
	flowLimitCount    *prometheus.Desc
	softnetBacklogLen *prometheus.Desc
}

func newSoftnetCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}

	desc := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(
			prometheus.BuildFQName(namespace, softnetSubsystem, name), help,
			[]string{"cpu"}, nil,
		)
	}

	return &softnetCollector{
		fs:     fs,
		logger: logger,
		// Help strings are upstream's verbatim, including their wording, so a diff
		// against the reference endpoint is empty rather than noisy.
		processed:         desc("processed_total", "Number of processed packets"),
		dropped:           desc("dropped_total", "Number of dropped packets"),
		timeSqueezed:      desc("times_squeezed_total", "Number of times processing packets ran out of quota"),
		cpuCollision:      desc("cpu_collision_total", "Number of collision occur while obtaining device lock while transmitting"),
		receivedRps:       desc("received_rps_total", "Number of times cpu woken up received_rps"),
		flowLimitCount:    desc("flow_limit_count_total", "Number of times flow limit has been reached"),
		softnetBacklogLen: desc("backlog_len", "Softnet backlog status"),
	}, nil
}

func (c *softnetCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := c.fs.NetSoftnetStat()
	if err != nil {
		return fmt.Errorf("could not get softnet statistics: %w", err)
	}

	for _, s := range stats {
		// The CPU label is the row index in /proc/net/softnet_stat, which is the
		// CPU number. Formatted rather than fmt.Sprint'd to keep it allocation-cheap
		// on a high-core node, where this runs nCPU times per scrape.
		cpu := strconv.FormatUint(uint64(s.Index), 10)

		for _, m := range []struct {
			desc      *prometheus.Desc
			valueType prometheus.ValueType
			value     uint32
		}{
			{c.processed, prometheus.CounterValue, s.Processed},
			{c.dropped, prometheus.CounterValue, s.Dropped},
			{c.timeSqueezed, prometheus.CounterValue, s.TimeSqueezed},
			{c.cpuCollision, prometheus.CounterValue, s.CPUCollision},
			{c.receivedRps, prometheus.CounterValue, s.ReceivedRps},
			{c.flowLimitCount, prometheus.CounterValue, s.FlowLimitCount},
			// backlog_len is a queue depth: it goes up and down, so as a counter
			// rate() would read every decrease as a reset.
			{c.softnetBacklogLen, prometheus.GaugeValue, s.SoftnetBacklogLen},
		} {
			ch <- prometheus.MustNewConstMetric(m.desc, m.valueType, float64(m.value), cpu)
		}
	}
	return nil
}
