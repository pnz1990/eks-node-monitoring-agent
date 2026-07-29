package hostmetrics

// Tests for the cpu collector.
//
// The monotonicity cache is the focus. It is the part of this collector that a
// naive port would drop — it looks like an optimisation and is actually
// correctness: kernel CPU counters can jump backwards on hotplug and on some
// hypervisors, and a Prometheus counter that decreases makes rate() produce a
// spike or a gap. Every test below that touches updateCache exists because the
// behaviour is invisible in a single scrape and only shows up as a wrong graph.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeProcStat creates a minimal /proc/stat with the given per-CPU lines.
//
// Field order is user, nice, system, idle, iowait, irq, softirq, steal, guest,
// guest_nice — the kernel's order, which procfs relies on.
func writeProcStat(t *testing.T, lines ...string) string {
	t.Helper()
	root := t.TempDir()
	procDir := filepath.Join(root, "proc")
	require.NoError(t, os.MkdirAll(procDir, 0o755))

	body := "cpu  100 200 300 400 500 600 700 800 900 1000\n"
	for _, l := range lines {
		body += l + "\n"
	}
	body += "intr 0\nctxt 12345\nbtime 1600000000\nprocesses 100\nprocs_running 1\nprocs_blocked 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "stat"), []byte(body), 0o644))
	return procDir
}

func newTestCPUCollector(t *testing.T, procDir string) *cpuCollector {
	t.Helper()
	c, err := newCPUCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)
	return c.(*cpuCollector)
}

// --- emitted metric set ---------------------------------------------------

func TestCPUEmitsExpectedFamilies(t *testing.T) {
	procDir := writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100")
	c := newTestCPUCollector(t, procDir)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	modes := map[string]int{}
	families := map[string]bool{}
	for m := range ch {
		d := m.Desc().String()
		switch {
		case strings.Contains(d, "node_cpu_guest_seconds_total"):
			families["guest"] = true
		case strings.Contains(d, "node_cpu_seconds_total"):
			families["cpu"] = true
		}
		modes[d]++
	}

	// Exactly the two families that produce data on EKS.
	assert.True(t, families["cpu"], "node_cpu_seconds_total must be emitted")
	assert.True(t, families["guest"], "node_cpu_guest_seconds_total must be emitted")
	assert.Len(t, families, 2, "no other families are ported; see the scope note in cpu.go")
}

func TestCPUEmitsEightModesPlusTwoGuest(t *testing.T) {
	procDir := writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100")
	c := newTestCPUCollector(t, procDir)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	count := 0
	for range ch {
		count++
	}
	// 8 modes on node_cpu_seconds_total + 2 on node_cpu_guest_seconds_total.
	// The mode label values are contract: recording rules select mode!="idle".
	assert.Equal(t, 10, count, "one CPU must yield 8 mode series plus 2 guest series")
}

func TestCPUGuestCanBeDisabled(t *testing.T) {
	procDir := writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100")
	c := newTestCPUCollector(t, procDir)
	c.includeGuest = false

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	count := 0
	for m := range ch {
		count++
		assert.NotContains(t, m.Desc().String(), "guest_seconds_total")
	}
	assert.Equal(t, 8, count)
}

func TestCPUMultipleCPUs(t *testing.T) {
	procDir := writeProcStat(t,
		"cpu0 10 20 30 40 50 60 70 80 90 100",
		"cpu1 11 21 31 41 51 61 71 81 91 101",
		"cpu2 12 22 32 42 52 62 72 82 92 102",
	)
	c := newTestCPUCollector(t, procDir)

	ch := make(chan prometheus.Metric, 128)
	require.NoError(t, c.Update(ch))
	close(ch)

	count := 0
	for range ch {
		count++
	}
	assert.Equal(t, 30, count, "three CPUs must yield 3 x 10 series")
}

// --- monotonicity: the part that must not be simplified away -------------

func TestCPUCounterNeverDecreases(t *testing.T) {
	c := newTestCPUCollector(t, writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100"))

	// First scrape establishes the baseline.
	c.updateCache(map[int64]procfs.CPUStat{
		0: {User: 100, Nice: 100, System: 100, Idle: 100, Iowait: 100,
			IRQ: 100, SoftIRQ: 100, Steal: 100, Guest: 100, GuestNice: 100},
	})

	// A small backwards step, as seen on some hypervisors. Every counter must hold
	// its previous value rather than regress: a decreasing counter makes rate()
	// emit a spike.
	c.updateCache(map[int64]procfs.CPUStat{
		0: {User: 99, Nice: 99, System: 99, Idle: 99, Iowait: 99,
			IRQ: 99, SoftIRQ: 99, Steal: 99, Guest: 99, GuestNice: 99},
	})

	got := c.cpuStats[0]
	for _, f := range cpuStatFields(&got, &got) {
		assert.Equal(t, float64(100), *f.cached,
			"%s regressed; a Prometheus counter must never decrease", f.name)
	}
}

func TestCPUCounterAdvancesNormally(t *testing.T) {
	c := newTestCPUCollector(t, writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100"))

	c.updateCache(map[int64]procfs.CPUStat{0: {User: 10, Idle: 10}})
	c.updateCache(map[int64]procfs.CPUStat{0: {User: 20, Idle: 25}})

	assert.Equal(t, float64(20), c.cpuStats[0].User)
	assert.Equal(t, float64(25), c.cpuStats[0].Idle)
}

func TestCPUHotplugResetsStats(t *testing.T) {
	c := newTestCPUCollector(t, writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100"))

	c.updateCache(map[int64]procfs.CPUStat{0: {Idle: 1000, User: 500}})

	// Idle drops by more than jumpBackSeconds: assume the CPU was hotplugged and
	// its counters restarted. Holding the old values would freeze the series
	// forever, so the cache resets and climbs again from the new baseline.
	c.updateCache(map[int64]procfs.CPUStat{0: {Idle: 5, User: 2}})

	assert.Equal(t, float64(5), c.cpuStats[0].Idle, "idle must adopt the post-hotplug value")
	assert.Equal(t, float64(2), c.cpuStats[0].User, "user must reset too, not hold the pre-hotplug value")
}

func TestCPUSmallJumpBackIsNotTreatedAsHotplug(t *testing.T) {
	c := newTestCPUCollector(t, writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100"))

	c.updateCache(map[int64]procfs.CPUStat{0: {Idle: 100, User: 100}})
	// Less than jumpBackSeconds: not a hotplug, so values are held rather than reset.
	c.updateCache(map[int64]procfs.CPUStat{0: {Idle: 98, User: 99}})

	assert.Equal(t, float64(100), c.cpuStats[0].Idle)
	assert.Equal(t, float64(100), c.cpuStats[0].User)
}

func TestCPUJumpBackThresholdBoundary(t *testing.T) {
	// Exactly at the threshold counts as a hotplug (upstream uses >=).
	c := newTestCPUCollector(t, writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100"))
	c.updateCache(map[int64]procfs.CPUStat{0: {Idle: 100, User: 100}})
	c.updateCache(map[int64]procfs.CPUStat{0: {Idle: 100 - jumpBackSeconds, User: 1}})

	assert.Equal(t, float64(1), c.cpuStats[0].User, "at the threshold the stats must reset")
}

func TestCPUOfflineCPUsAreRemoved(t *testing.T) {
	c := newTestCPUCollector(t, writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100"))

	c.updateCache(map[int64]procfs.CPUStat{
		0: {Idle: 10}, 1: {Idle: 10}, 2: {Idle: 10},
	})
	require.Len(t, c.cpuStats, 3)

	// cpu1 goes offline. Its series must stop being reported rather than being
	// frozen at its last value forever.
	c.updateCache(map[int64]procfs.CPUStat{0: {Idle: 20}, 2: {Idle: 20}})

	assert.Len(t, c.cpuStats, 2)
	_, present := c.cpuStats[1]
	assert.False(t, present, "an offline CPU must be dropped from the cache")
}

func TestCPUNewCPUComingOnlineIsAdded(t *testing.T) {
	c := newTestCPUCollector(t, writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100"))

	c.updateCache(map[int64]procfs.CPUStat{0: {Idle: 10}})
	c.updateCache(map[int64]procfs.CPUStat{0: {Idle: 20}, 1: {Idle: 5}})

	assert.Len(t, c.cpuStats, 2)
	assert.Equal(t, float64(5), c.cpuStats[1].Idle)
}

// TestCPUStatFieldsCoversEveryEmittedMode guards the coupling between the two
// lists: every counter that is emitted must also be subject to the monotonicity
// rule, or it can silently move backwards.
func TestCPUStatFieldsCoversEveryEmittedMode(t *testing.T) {
	var cached, next procfs.CPUStat
	fields := cpuStatFields(&cached, &next)

	names := map[string]bool{}
	for _, f := range fields {
		names[f.name] = true
	}

	// The eight modes emitted on node_cpu_seconds_total plus the two guest modes.
	for _, mode := range []string{
		"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal",
		"guest", "guest_nice",
	} {
		assert.True(t, names[mode],
			"mode %q is emitted but not covered by the monotonicity rule, so it could decrease", mode)
	}
	assert.Len(t, fields, 10)
}

// --- error paths ----------------------------------------------------------

func TestCPUUpdateWrapsStatError(t *testing.T) {
	// procfs directory exists but has no stat file.
	procDir := t.TempDir()
	c, err := newCPUCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 4))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get cpu stats")
}

func TestCPUConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newCPUCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

func TestCPUConcurrentUpdatesAreSafe(t *testing.T) {
	// Collect runs collectors concurrently, and the cache is shared mutable state.
	// Run under -race.
	procDir := writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100")
	c := newTestCPUCollector(t, procDir)

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			ch := make(chan prometheus.Metric, 64)
			_ = c.Update(ch)
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
