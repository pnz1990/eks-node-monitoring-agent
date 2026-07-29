package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/pressure_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2019 The Prometheus Authors, Apache-2.0
//
// PSI (Pressure Stall Information) is the highest-signal collector in this whole
// set for a Kubernetes node, because it measures the thing that actually matters —
// time lost to contention — rather than a utilisation percentage that has to be
// interpreted. node_pressure_memory_stalled_seconds_total rising means processes
// made NO progress waiting for memory, which is the precursor to an OOM kill; the
// standard memory-utilisation gauge looks identical at 95% whether the node is fine
// or thrashing.
//
// THREE THINGS HERE ARE EASY TO GET WRONG AND INVISIBLE IF YOU DO:
//
//  1. THE UNIT. The kernel reports totals in MICROseconds; the metric is seconds,
//     so the divisor is 1e6. Using 1e9 (nanoseconds, which schedstat DOES use, in
//     the same package) makes every value 1000x too small — and it still looks like
//     a plausible near-zero pressure reading on a healthy node.
//
//  2. THE some/full ASYMMETRY, WHICH IS NOT UNIFORM ACROSS RESOURCES.
//     - cpu has "some" but NO "full" (a fully-stalled CPU is not a meaningful state)
//     - irq has "full" but NO "some"
//     - io and memory have both
//     So the emitted metric set is 6 series from 4 resources, not 8. Emitting a
//     zero for a missing one would be fabricating a measurement.
//
//  3. ENOTSUP vs ENOENT. A kernel without CONFIG_PSI returns ENOENT (the file is
//     absent); a kernel with PSI compiled in but disabled at boot returns ENOTSUP.
//     Both are legitimate configurations and neither is a scrape failure, but they
//     are reported differently and the IRQ resource needs a newer kernel (6.1) than
//     the others (4.20) — so a missing irq file on a 5.x kernel must not suppress
//     the resources that ARE available.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
)

const (
	psiResourceCPU    = "cpu"
	psiResourceIO     = "io"
	psiResourceMemory = "memory"
	psiResourceIRQ    = "irq"

	// psiMicrosecondsPerSecond converts the kernel's microsecond totals.
	//
	// NOT 1e9. schedstat in this same package uses nanoseconds, which makes
	// copying the wrong constant between them easy and the result a silent 1000x
	// error that reads as "almost no pressure".
	psiMicrosecondsPerSecond = 1000.0 * 1000.0
)

// psiResources is the collection order. Kept as a slice rather than iterating a map
// so the order is deterministic, which matters only for log output but costs
// nothing.
var psiResources = []string{psiResourceCPU, psiResourceIO, psiResourceMemory, psiResourceIRQ}

func init() {
	register("pressure", true, newPressureCollector)
}

type pressureCollector struct {
	fs     procfs.FS
	logger *slog.Logger

	cpu     *prometheus.Desc
	io      *prometheus.Desc
	ioFull  *prometheus.Desc
	mem     *prometheus.Desc
	memFull *prometheus.Desc
	irqFull *prometheus.Desc

	// psiStats is injectable because the interesting branches — ENOTSUP, a
	// resource missing on an older kernel, a nil Some or Full — cannot be produced
	// on a host whose kernel supports PSI fully.
	psiStats func(resource string) (procfs.PSIStats, error)
}

func newPressureCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}

	desc := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "pressure", name), help, nil, nil)
	}

	c := &pressureCollector{
		fs:     fs,
		logger: logger,
		cpu: desc("cpu_waiting_seconds_total",
			"Total time in seconds that processes have waited for CPU time"),
		io: desc("io_waiting_seconds_total",
			"Total time in seconds that processes have waited due to IO congestion"),
		ioFull: desc("io_stalled_seconds_total",
			"Total time in seconds no process could make progress due to IO congestion"),
		mem: desc("memory_waiting_seconds_total",
			"Total time in seconds that processes have waited for memory"),
		memFull: desc("memory_stalled_seconds_total",
			"Total time in seconds no process could make progress due to memory congestion"),
		irqFull: desc("irq_stalled_seconds_total",
			"Total time in seconds no process could make progress due to IRQ congestion"),
	}
	c.psiStats = c.fs.PSIStatsForResource
	return c, nil
}

func (c *pressureCollector) Update(ch chan<- prometheus.Metric) error {
	found := 0

	for _, res := range psiResources {
		vals, err := c.psiStats(res)
		if err != nil {
			handled, hErr := c.classify(res, err)
			if hErr != nil {
				return hErr
			}
			if handled {
				continue
			}
		}

		// IRQ pressure has no "some" data by design; every other resource must
		// have it. See torvalds/linux include/linux/psi_types.h.
		if vals.Some == nil && res != psiResourceIRQ {
			c.logger.Debug("pressure information returned no 'some' data", "resource", res)
			return ErrNoData
		}
		// CPU has no "full" data by design: a CPU on which no process can make
		// progress is not a meaningful state.
		if vals.Full == nil && res != psiResourceCPU {
			c.logger.Debug("pressure information returned no 'full' data", "resource", res)
			return ErrNoData
		}

		switch res {
		case psiResourceCPU:
			ch <- c.metric(c.cpu, vals.Some.Total)
		case psiResourceIO:
			ch <- c.metric(c.io, vals.Some.Total)
			ch <- c.metric(c.ioFull, vals.Full.Total)
		case psiResourceMemory:
			ch <- c.metric(c.mem, vals.Some.Total)
			ch <- c.metric(c.memFull, vals.Full.Total)
		case psiResourceIRQ:
			ch <- c.metric(c.irqFull, vals.Full.Total)
		}
		found++
	}

	if found == 0 {
		// Every resource was unavailable. Distinct from a failure: this is what a
		// kernel without CONFIG_PSI looks like, and alerting on it would be alerting
		// on a supported configuration.
		c.logger.Debug("pressure information is unavailable; needs Linux >= 4.20 with CONFIG_PSI")
		return ErrNoData
	}
	return nil
}

// metric converts a microsecond total to a seconds counter.
func (c *pressureCollector) metric(desc *prometheus.Desc, microseconds uint64) prometheus.Metric {
	return prometheus.MustNewConstMetric(desc, prometheus.CounterValue,
		float64(microseconds)/psiMicrosecondsPerSecond)
}

// classify decides what a PSI read error means.
//
// Returns (skip, err): skip means this resource is unavailable but others may not
// be, err means the whole collector should stop.
//
// The distinction between the two ErrNotExist cases is only in the log message, but
// it is worth keeping: IRQ pressure needs kernel 6.1 while the rest need 4.20, so
// an operator on a 5.x kernel seeing "irq unavailable" should be told that is
// expected rather than left to wonder.
func (c *pressureCollector) classify(res string, err error) (bool, error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		if res == psiResourceIRQ {
			c.logger.Debug("IRQ pressure information is unavailable; needs Linux >= 6.1 with CONFIG_PSI",
				"resource", res)
		} else {
			c.logger.Debug("pressure information is unavailable; needs Linux >= 4.20 with CONFIG_PSI",
				"resource", res)
		}
		return true, nil

	case errors.Is(err, syscall.ENOTSUP):
		// PSI compiled in but disabled at boot. Unlike ErrNotExist this is
		// system-wide rather than per-resource, so there is no point trying the
		// remaining resources.
		c.logger.Debug("pressure information is disabled; add psi=1 to the kernel command line")
		return false, ErrNoData

	default:
		return false, fmt.Errorf("failed to retrieve pressure stats: %w", err)
	}
}
