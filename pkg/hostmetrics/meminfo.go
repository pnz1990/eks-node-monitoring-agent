package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/meminfo.go
//                 node_exporter/collector/meminfo_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// PARITY NOTE — this is the collector that proves why parity must be measured
// rather than enumerated. Metric names here are built at runtime as
// "node_memory_" + a field key, so node_memory_MemAvailable_bytes appears nowhere
// in any source file, upstream's or ours. Grepping for it returns zero hits.
//
// COVERAGE NOTE — upstream hand-maps ~51 fields from procfs.Meminfo, and
// procfs.Meminfo only models the keys it knows about. Measured on the test host:
// /proc/meminfo has 55 keys, upstream maps 51, and 49 were emitted. So upstream
// silently drops kernel fields procfs does not model. Reusing procfs reproduces
// that exactly, including the blind spot, which is what parity requires. Writing
// our own /proc/meminfo parser would emit MORE metrics than upstream and break
// parity in the other direction.
//
// Differences from upstream:
//   - reads through Paths rather than a kingpin global
//   - the field mapping is a table rather than 51 sequential if-blocks, which is
//     the same logic in a form that can be diffed and tested as data

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
)

const meminfoSubsystem = "memory"

func init() {
	register("meminfo", true, newMeminfoCollector)
}

type meminfoCollector struct {
	fs     procfs.FS
	logger *slog.Logger
	// fields maps metric-name suffixes to procfs values. Injectable so tests can
	// reach the counter branch: no current upstream meminfo field has a "_total"
	// suffix, so that branch is unreachable with the real table. The branch is kept
	// because it is upstream's rule, and a future procfs field named *_total would
	// otherwise be silently typed as a gauge, breaking rate() on it.
	fields func(*procfs.Meminfo) map[string]*uint64
}

func newMeminfoCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	return &meminfoCollector{fs: fs, logger: logger, fields: meminfoFields}, nil
}

func (c *meminfoCollector) Update(ch chan<- prometheus.Metric) error {
	fields, err := c.memInfo()
	if err != nil {
		return fmt.Errorf("couldn't get meminfo: %w", err)
	}
	for key, value := range fields {
		// Upstream's rule: a "_total" suffix means counter, everything else gauge.
		// Reproduced exactly, because a type change breaks rate() queries.
		valueType := prometheus.GaugeValue
		if strings.HasSuffix(key, "_total") {
			valueType = prometheus.CounterValue
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, meminfoSubsystem, key),
				fmt.Sprintf("Memory information field %s.", key),
				nil, nil,
			),
			valueType, value,
		)
	}
	return nil
}

// memInfo reads /proc/meminfo and returns the metric-name suffix for each field
// that the kernel reported.
//
// The keys are the exact strings upstream uses, so the resulting metric names
// match byte for byte. A nil field means the kernel did not report it and it is
// omitted rather than emitted as zero — emitting zero would be a lie about the
// kernel's state and would differ from upstream.
func (c *meminfoCollector) memInfo() (map[string]float64, error) {
	mi, err := c.fs.Meminfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get memory info: %w", err)
	}

	out := make(map[string]float64, 64)
	for key, ptr := range c.fields(&mi) {
		if ptr != nil {
			out[key] = float64(*ptr)
		}
	}
	return out, nil
}

// meminfoFields maps each metric-name suffix to the corresponding procfs field.
//
// Upstream writes this as 51 sequential `if x != nil` blocks. Expressed as a table
// here so it can be diffed against upstream's list mechanically and asserted in
// tests as data, rather than reviewed line by line. The keys are copied verbatim
// from upstream — any deviation, including capitalisation, renames a metric.
func meminfoFields(mi *procfs.Meminfo) map[string]*uint64 {
	return map[string]*uint64{
		"Active_bytes":            mi.ActiveBytes,
		"Active_anon_bytes":       mi.ActiveAnonBytes,
		"Active_file_bytes":       mi.ActiveFileBytes,
		"AnonHugePages_bytes":     mi.AnonHugePagesBytes,
		"AnonPages_bytes":         mi.AnonPagesBytes,
		"Bounce_bytes":            mi.BounceBytes,
		"Buffers_bytes":           mi.BuffersBytes,
		"Cached_bytes":            mi.CachedBytes,
		"CmaFree_bytes":           mi.CmaFreeBytes,
		"CmaTotal_bytes":          mi.CmaTotalBytes,
		"CommitLimit_bytes":       mi.CommitLimitBytes,
		"Committed_AS_bytes":      mi.CommittedASBytes,
		"DirectMap1G_bytes":       mi.DirectMap1GBytes,
		"DirectMap2M_bytes":       mi.DirectMap2MBytes,
		"DirectMap4k_bytes":       mi.DirectMap4kBytes,
		"Dirty_bytes":             mi.DirtyBytes,
		"HardwareCorrupted_bytes": mi.HardwareCorruptedBytes,
		"HugePages_Free":          mi.HugePagesFree,
		"HugePages_Rsvd":          mi.HugePagesRsvd,
		"HugePages_Surp":          mi.HugePagesSurp,
		"HugePages_Total":         mi.HugePagesTotal,
		"Hugepagesize_bytes":      mi.HugepagesizeBytes,
		"Inactive_bytes":          mi.InactiveBytes,
		"Inactive_anon_bytes":     mi.InactiveAnonBytes,
		"Inactive_file_bytes":     mi.InactiveFileBytes,
		"KernelStack_bytes":       mi.KernelStackBytes,
		"Mapped_bytes":            mi.MappedBytes,
		"MemAvailable_bytes":      mi.MemAvailableBytes,
		"MemFree_bytes":           mi.MemFreeBytes,
		"MemTotal_bytes":          mi.MemTotalBytes,
		"Mlocked_bytes":           mi.MlockedBytes,
		"NFS_Unstable_bytes":      mi.NFSUnstableBytes,
		"PageTables_bytes":        mi.PageTablesBytes,
		"Percpu_bytes":            mi.PercpuBytes,
		"SReclaimable_bytes":      mi.SReclaimableBytes,
		"SUnreclaim_bytes":        mi.SUnreclaimBytes,
		"ShmemHugePages_bytes":    mi.ShmemHugePagesBytes,
		"ShmemPmdMapped_bytes":    mi.ShmemPmdMappedBytes,
		"Shmem_bytes":             mi.ShmemBytes,
		"Slab_bytes":              mi.SlabBytes,
		"SwapCached_bytes":        mi.SwapCachedBytes,
		"SwapFree_bytes":          mi.SwapFreeBytes,
		"SwapTotal_bytes":         mi.SwapTotalBytes,
		"Unevictable_bytes":       mi.UnevictableBytes,
		"VmallocChunk_bytes":      mi.VmallocChunkBytes,
		"VmallocTotal_bytes":      mi.VmallocTotalBytes,
		"VmallocUsed_bytes":       mi.VmallocUsedBytes,
		"WritebackTmp_bytes":      mi.WritebackTmpBytes,
		"Writeback_bytes":         mi.WritebackBytes,
		"Zswap_bytes":             mi.ZswapBytes,
		"Zswapped_bytes":          mi.ZswappedBytes,
	}
}
