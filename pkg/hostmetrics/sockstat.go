package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/sockstat_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// THE SUBTLE PART IS mem_bytes. /proc/net/sockstat reports the "mem" field in
// PAGES, and upstream additionally exposes it multiplied by the page size as
// node_sockstat_TCP_mem_bytes. Hardcoding 4096 would be right on x86_64 and WRONG
// on arm64 with 64K pages — a 16x error, on a metric that exists with the correct
// name, type and labels either way. Graviton nodes are common on EKS, so this is
// not a theoretical concern: os.Getpagesize() is load-bearing.
//
// SHAPE. Metric names are built at runtime from the protocol names in the file
// (TCP, UDP, UDPLITE, RAW, FRAG, and the IPv6 variants), so the metric set cannot
// be enumerated from the source — the same reason the whole parity methodology is
// based on live scrape diffing rather than a name list.
//
// Everything is a GAUGE, including the fields that sound cumulative. "inuse",
// "orphan", "tw" and "alloc" are all current counts of sockets in a state, not
// totals over time.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
)

const sockStatSubsystem = "sockstat"

func init() {
	register(sockStatSubsystem, true, newSockStatCollector)
}

type sockStatCollector struct {
	fs     procfs.FS
	logger *slog.Logger

	// pageSize is captured at construction rather than read per scrape. It cannot
	// change for the life of the process.
	//
	// NOT hardcoded: arm64 kernels can use 64K pages, where a hardcoded 4096 would
	// make every *_mem_bytes metric 16x too small. Graviton is common on EKS.
	pageSize int

	// Injected seams. On any real host both files exist, so the ErrNotExist and
	// hard-error branches are otherwise unreachable.
	sockstat  func() (*procfs.NetSockstat, error)
	sockstat6 func() (*procfs.NetSockstat, error)
}

func newSockStatCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	c := &sockStatCollector{
		fs:       fs,
		logger:   logger,
		pageSize: os.Getpagesize(),
	}
	c.sockstat = c.fs.NetSockstat
	c.sockstat6 = c.fs.NetSockstat6
	return c, nil
}

func (c *sockStatCollector) Update(ch chan<- prometheus.Metric) error {
	// IPv4 and IPv6 are handled independently: a kernel with either disabled must
	// still report the other, rather than losing both.
	stat4, err := c.sockstat()
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		c.logger.Debug("IPv4 sockstat statistics not found, skipping")
	default:
		return fmt.Errorf("failed to get IPv4 sockstat data: %w", err)
	}

	stat6, err := c.sockstat6()
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		c.logger.Debug("IPv6 sockstat statistics not found, skipping")
	default:
		return fmt.Errorf("failed to get IPv6 sockstat data: %w", err)
	}

	c.emit(ch, false, stat4)
	c.emit(ch, true, stat6)
	return nil
}

// emit sends the metrics for one address family.
func (c *sockStatCollector) emit(ch chan<- prometheus.Metric, isIPv6 bool, s *procfs.NetSockstat) {
	if s == nil {
		// That family is disabled. Nothing to report, which is not the same as zero.
		return
	}

	// sockets_used is only present in the IPv4 file, and is unlabelled rather than
	// per-protocol. Emitting it for IPv6 too would produce a duplicate label set
	// and Prometheus would reject the whole scrape.
	if !isIPv6 && s.Used != nil {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, sockStatSubsystem, "sockets_used"),
				"Number of IPv4 sockets in use.",
				nil, nil,
			),
			prometheus.GaugeValue, float64(*s.Used),
		)
	}

	for _, p := range s.Protocols {
		// Upstream's comment notes these names were once generated straight from the
		// file's own field names, and the mapping is preserved for compatibility.
		// So "tw" stays "tw" rather than becoming something more readable like
		// "time_wait": renaming it would break every existing dashboard.
		pairs := []struct {
			name  string
			value *int
		}{
			{"inuse", &p.InUse},
			{"orphan", p.Orphan},
			{"tw", p.TW},
			{"alloc", p.Alloc},
			{"mem", p.Mem},
			{"memory", p.Memory},
		}

		if p.Mem != nil {
			// The mem field is in PAGES; this is the same value in bytes. Both are
			// emitted, matching upstream.
			bytes := *p.Mem * c.pageSize
			pairs = append(pairs, struct {
				name  string
				value *int
			}{"mem_bytes", &bytes})
		}

		for _, pair := range pairs {
			// A nil pointer means the protocol does not report that field. Emitting
			// zero would claim the kernel said something it did not.
			if pair.value == nil {
				continue
			}
			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					prometheus.BuildFQName(namespace, sockStatSubsystem,
						fmt.Sprintf("%s_%s", p.Protocol, pair.name)),
					fmt.Sprintf("Number of %s sockets in state %s.", p.Protocol, pair.name),
					nil, nil,
				),
				// Gauges throughout, including the fields that sound cumulative:
				// inuse/orphan/tw/alloc are current counts, not totals.
				prometheus.GaugeValue, float64(*pair.value),
			)
		}
	}
}
