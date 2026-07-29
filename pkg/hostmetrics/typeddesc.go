package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/collector.go (typedDesc)
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// Kept as a near-verbatim copy deliberately: ported collector bodies read the same
// as their upstream originals, which is what makes them diffable against the
// reference during review and when porting upstream fixes.

import "github.com/prometheus/client_golang/prometheus"

// typedDesc pairs a metric descriptor with its value type, so collectors can emit
// a metric without restating the type at every call site.
type typedDesc struct {
	desc      *prometheus.Desc
	valueType prometheus.ValueType
}

// mustNewConstMetric builds a metric from the descriptor.
//
// It panics on a label-count mismatch, exactly as upstream does. That is safe
// here despite being a panic in library code, because the resilience layer
// recovers per-collector panics on the goroutine that raises them — a mismatch
// degrades one collector instead of killing the agent. Without that guard this
// would be unacceptable.
func (d *typedDesc) mustNewConstMetric(value float64, labels ...string) prometheus.Metric {
	return prometheus.MustNewConstMetric(d.desc, d.valueType, value, labels...)
}
