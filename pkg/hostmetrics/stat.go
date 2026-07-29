package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/stat_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// SCOPE. Upstream also exposes node_softirqs_total from this collector, gated on
// --collector.stat.softirq which defaults to OFF. Not ported, matching the default
// state; recorded in docs/parity-exceptions-nodep.md.
//
// NOTE ON node_boot_time_seconds. This metric is in the harness skip list because
// it is constant per boot and identical across implementations, so it carries no
// comparison signal. It is still emitted because dashboards use it for uptime.

import (
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
)

func init() {
	register("stat", true, newStatCollector)
}

type statCollector struct {
	fs     procfs.FS
	logger *slog.Logger

	intr            *prometheus.Desc
	contextSwitches *prometheus.Desc
	forks           *prometheus.Desc
	bootTime        *prometheus.Desc
	procsRunning    *prometheus.Desc
	procsBlocked    *prometheus.Desc
}

func newStatCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	// Empty subsystem: these metrics sit directly under the node_ namespace, e.g.
	// node_intr_total rather than node_stat_intr_total. Reproducing that exactly
	// matters — a subsystem here would rename all six.
	return &statCollector{
		fs:     fs,
		logger: logger,
		intr: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "intr_total"),
			"Total number of interrupts serviced.",
			nil, nil,
		),
		contextSwitches: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "context_switches_total"),
			"Total number of context switches.",
			nil, nil,
		),
		forks: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "forks_total"),
			"Total number of forks.",
			nil, nil,
		),
		bootTime: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "boot_time_seconds"),
			"Node boot time, in unixtime.",
			nil, nil,
		),
		procsRunning: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "procs_running"),
			"Number of processes in runnable state.",
			nil, nil,
		),
		procsBlocked: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "procs_blocked"),
			"Number of processes blocked waiting for I/O to complete.",
			nil, nil,
		),
	}, nil
}

func (c *statCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := c.fs.Stat()
	if err != nil {
		return fmt.Errorf("couldn't get stat: %w", err)
	}

	ch <- prometheus.MustNewConstMetric(c.intr, prometheus.CounterValue, float64(stats.IRQTotal))
	ch <- prometheus.MustNewConstMetric(c.contextSwitches, prometheus.CounterValue, float64(stats.ContextSwitches))
	ch <- prometheus.MustNewConstMetric(c.forks, prometheus.CounterValue, float64(stats.ProcessCreated))
	ch <- prometheus.MustNewConstMetric(c.bootTime, prometheus.GaugeValue, float64(stats.BootTime))
	ch <- prometheus.MustNewConstMetric(c.procsRunning, prometheus.GaugeValue, float64(stats.ProcessesRunning))
	ch <- prometheus.MustNewConstMetric(c.procsBlocked, prometheus.GaugeValue, float64(stats.ProcessesBlocked))

	return nil
}
