package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/vmstat_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// PARITY-CRITICAL DETAIL. /proc/vmstat has ~200 fields, but upstream emits only
// those matching a default regexp: "^(oom_kill|pgpg|pswp|pg.*fault).*". Measured
// on a live EKS node that yields exactly 7 series (oom_kill, pgfault, pgmajfault,
// pgpgin, pgpgout, pswpin, pswpout). Emitting the unfiltered set would produce
// ~200 series and break parity in the direction a "missing metric" check never
// catches — the same failure mode as the Hugetlb_bytes mistake in meminfo.
//
// Differences from upstream:
//   - the pattern is a struct field rather than a kingpin global
//   - a malformed line is skipped with a debug log rather than failing the whole
//     collector; see the comment on Update for why

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

const vmStatSubsystem = "vmstat"

// defaultVMStatFields is upstream's default filter, copied verbatim. Changing it
// changes the emitted series set, so it is asserted against upstream in tests.
const defaultVMStatFields = "^(oom_kill|pgpg|pswp|pg.*fault).*"

func init() {
	register("vmstat", true, newVMStatCollector)
}

type vmStatCollector struct {
	fieldPattern *regexp.Regexp
	path         string
	logger       *slog.Logger
}

func newVMStatCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	return newVMStatCollectorWithPattern(logger, paths, defaultVMStatFields)
}

// newVMStatCollectorWithPattern is split out so the invalid-pattern branch is
// reachable in tests. With the compile-time default constant it can never fail,
// but the check must stay: the pattern becomes operator-configurable the moment
// someone wires it to the chart, and an invalid regexp should fail at startup
// rather than panic on first scrape.
func newVMStatCollectorWithPattern(logger *slog.Logger, paths Paths, expr string) (Collector, error) {
	pattern, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid vmstat field pattern %q: %w", expr, err)
	}
	return &vmStatCollector{
		fieldPattern: pattern,
		path:         paths.procPath("vmstat"),
		logger:       logger,
	}, nil
}

func (c *vmStatCollector) Update(ch chan<- prometheus.Metric) error {
	file, err := os.Open(c.path)
	if err != nil {
		return fmt.Errorf("couldn't open %s: %w", c.path, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())

		// Upstream indexes parts[1] without checking length, which panics on a
		// short line. That is contained by the resilience layer here, but a
		// malformed line should degrade one field rather than the whole collector:
		// /proc/vmstat is ~200 lines and losing all of them because one is odd
		// would be a poor trade. Skipped with a debug log instead.
		if len(fields) < 2 {
			c.logger.Debug("skipping malformed vmstat line", "line", scanner.Text())
			continue
		}
		if !c.fieldPattern.MatchString(fields[0]) {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			c.logger.Debug("skipping unparseable vmstat value",
				"field", fields[0], "value", fields[1], "err", err)
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, vmStatSubsystem, fields[0]),
				fmt.Sprintf("/proc/vmstat information field %s.", fields[0]),
				nil, nil,
			),
			// Untyped, matching upstream: these fields are a mix of counters and
			// gauges and upstream does not attempt to classify them.
			prometheus.UntypedValue,
			value,
		)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("couldn't read %s: %w", c.path, err)
	}
	return nil
}
