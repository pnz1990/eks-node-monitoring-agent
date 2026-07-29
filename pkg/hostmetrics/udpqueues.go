package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/udp_queues_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// Four series: {tx,rx} x {v4,v6}. The interesting part is the error handling, and
// it is reproduced exactly because the distinction it draws is the whole point:
//
//   - IPv6 file absent  -> the kernel has IPv6 disabled. Report the v4 series and
//     say nothing about v6. NOT a failure.
//   - BOTH files absent -> ErrNoData. Nothing to report, but the collector did not
//     break either.
//   - any other error   -> a real failure, reported as one.
//
// Collapsing "IPv6 is disabled" into either a failure or a silent success would be
// wrong in opposite directions: the first alerts on a normal configuration, the
// second hides a genuinely broken procfs.
//
// On EKS this reports v4 only on a v4 cluster, so the "IPv6 absent" branch is the
// COMMON path here, not an edge case.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
)

func init() {
	register("udp_queues", true, newUDPQueuesCollector)
}

type udpQueuesCollector struct {
	fs     procfs.FS
	logger *slog.Logger
	desc   *prometheus.Desc

	// Injected seams. On a v4-only host the v6 summary always fails with
	// ErrNotExist, so the non-ErrNotExist error branches are otherwise unreachable
	// without corrupting /proc.
	udpSummary  func() (*procfs.NetUDPSummary, error)
	udp6Summary func() (*procfs.NetUDPSummary, error)
}

func newUDPQueuesCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	c := &udpQueuesCollector{
		fs:     fs,
		logger: logger,
		desc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "udp", "queues"),
			"Number of allocated memory in the kernel for UDP datagrams in bytes.",
			[]string{"queue", "ip"}, nil,
		),
	}
	c.udpSummary = c.fs.NetUDPSummary
	c.udp6Summary = c.fs.NetUDP6Summary
	return c, nil
}

func (c *udpQueuesCollector) Update(ch chan<- prometheus.Metric) error {
	s4, errIPv4 := c.udpSummary()
	if errIPv4 == nil {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s4.TxQueueLength), "tx", "v4")
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s4.RxQueueLength), "rx", "v4")
	} else if errors.Is(errIPv4, os.ErrNotExist) {
		c.logger.Debug("not collecting ipv4 based metrics")
	} else {
		return fmt.Errorf("couldn't get udp queued bytes: %w", errIPv4)
	}

	s6, errIPv6 := c.udp6Summary()
	if errIPv6 == nil {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s6.TxQueueLength), "tx", "v6")
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(s6.RxQueueLength), "rx", "v6")
	} else if errors.Is(errIPv6, os.ErrNotExist) {
		// The normal case on a v4-only cluster.
		c.logger.Debug("not collecting ipv6 based metrics")
	} else {
		return fmt.Errorf("couldn't get udp6 queued bytes: %w", errIPv6)
	}

	// Both absent means procfs told us nothing. Distinct from a failure, and
	// distinct from success with four series.
	if errors.Is(errIPv4, os.ErrNotExist) && errors.Is(errIPv6, os.ErrNotExist) {
		return ErrNoData
	}
	return nil
}
