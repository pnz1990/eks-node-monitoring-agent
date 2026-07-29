package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/loadavg.go
//                 node_exporter/collector/loadavg_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// Metric names, help strings and value types are reproduced exactly so existing
// dashboards keep working. Differences from upstream:
//   - reads through Paths rather than a kingpin global
//   - no init()-time flag registration
//   - parse errors name the offending field, which upstream does too, but the
//     error is wrapped with the file path for a more actionable message

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

func init() {
	register("loadavg", true, newLoadavgCollector)
}

type loadavgCollector struct {
	descs  []typedDesc
	paths  Paths
	logger *slog.Logger
}

func newLoadavgCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	return &loadavgCollector{
		descs: []typedDesc{
			{prometheus.NewDesc(namespace+"_load1", "1m load average.", nil, nil), prometheus.GaugeValue},
			{prometheus.NewDesc(namespace+"_load5", "5m load average.", nil, nil), prometheus.GaugeValue},
			{prometheus.NewDesc(namespace+"_load15", "15m load average.", nil, nil), prometheus.GaugeValue},
		},
		paths:  paths,
		logger: logger,
	}, nil
}

func (c *loadavgCollector) Update(ch chan<- prometheus.Metric) error {
	path := c.paths.procPath("loadavg")
	loads, err := readLoadavg(path)
	if err != nil {
		return fmt.Errorf("couldn't get load: %w", err)
	}
	for i, load := range loads {
		c.logger.Debug("return load", "index", i, "load", load)
		ch <- c.descs[i].mustNewConstMetric(load)
	}
	return nil
}

// readLoadavg reads and parses /proc/loadavg.
func readLoadavg(path string) ([]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseLoadavg(string(data), path)
}

// parseLoadavg extracts the 1m, 5m and 15m averages.
//
// path is passed only to make errors actionable; it does not affect parsing. Split
// from readLoadavg so the parser can be unit tested against upstream's fixtures
// without touching a filesystem.
func parseLoadavg(data, path string) ([]float64, error) {
	fields := strings.Fields(data)
	if len(fields) < 3 {
		return nil, fmt.Errorf("unexpected content in %s: got %d fields, want at least 3", path, len(fields))
	}
	loads := make([]float64, 3)
	for i, raw := range fields[0:3] {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("could not parse load %q from %s: %w", raw, path, err)
		}
		loads[i] = value
	}
	return loads, nil
}
