package hostmetrics

// Tests for the vmstat and stat collectors.
//
// The vmstat field filter is the parity-critical part: /proc/vmstat has ~200
// fields and upstream emits only those matching a default regexp. Emitting the
// unfiltered set would produce ~200 series and break parity in the direction a
// "missing metric" check never catches.
//
// These tests also cover a real upstream bug that this port fixes: upstream
// indexes parts[1] with no length check, so a single-token line in /proc/vmstat
// panics it.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeProcFile(t *testing.T, name, content string) string {
	t.Helper()
	root := t.TempDir()
	procDir := filepath.Join(root, "proc")
	require.NoError(t, os.MkdirAll(procDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, name), []byte(content), 0o644))
	return procDir
}

func collectNames(t *testing.T, c Collector) []string {
	t.Helper()
	ch := make(chan prometheus.Metric, 512)
	require.NoError(t, c.Update(ch))
	close(ch)
	var out []string
	for m := range ch {
		out = append(out, m.Desc().String())
	}
	return out
}

// --- vmstat: the field filter --------------------------------------------

// TestVMStatDefaultPatternMatchesUpstream asserts our default filter is byte-identical
// to upstream's. A change here changes the emitted series set.
func TestVMStatDefaultPatternMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/vmstat_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	m := regexp.MustCompile(`collector\.vmstat\.fields".*?Default\("([^"]+)"\)`).FindSubmatch(data)
	require.NotNil(t, m, "failed to extract upstream's default pattern; the regexp may be stale")
	assert.Equal(t, string(m[1]), defaultVMStatFields,
		"our default vmstat filter must match upstream's exactly")
}

func TestVMStatFiltersToExpectedFields(t *testing.T) {
	// A realistic slice of /proc/vmstat: a few fields that must be emitted and
	// several that must be filtered out.
	procDir := writeProcFile(t, "vmstat", strings.Join([]string{
		"nr_free_pages 100000",      // filtered
		"nr_zone_inactive_anon 500", // filtered
		"pgpgin 1234",               // emitted
		"pgpgout 5678",              // emitted
		"pswpin 1",                  // emitted
		"pswpout 2",                 // emitted
		"pgfault 999",               // emitted
		"pgmajfault 42",             // emitted
		"oom_kill 3",                // emitted
		"numa_hit 77",               // filtered
		"nr_dirty 12",               // filtered
	}, "\n")+"\n")

	c, err := newVMStatCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)

	names := collectNames(t, c)
	joined := strings.Join(names, " ")

	for _, want := range []string{
		"node_vmstat_pgpgin", "node_vmstat_pgpgout", "node_vmstat_pswpin",
		"node_vmstat_pswpout", "node_vmstat_pgfault", "node_vmstat_pgmajfault",
		"node_vmstat_oom_kill",
	} {
		assert.Contains(t, joined, want)
	}
	for _, unwanted := range []string{
		"nr_free_pages", "nr_zone_inactive_anon", "numa_hit", "nr_dirty",
	} {
		assert.NotContains(t, joined, unwanted,
			"%s must be filtered out; emitting the unfiltered set would produce ~200 series", unwanted)
	}
	assert.Len(t, names, 7, "exactly the 7 fields matching upstream's default pattern")
}

// --- vmstat: the upstream panic this port fixes --------------------------

// TestVMStatShortLineDoesNotPanic covers a real upstream defect. Upstream does
// `parts := strings.Fields(line); strconv.ParseFloat(parts[1], 64)` with no length
// check, so a single-token line panics with index out of range. Verified by
// reading upstream's source at collector/vmstat_linux.go.
func TestVMStatShortLineDoesNotPanic(t *testing.T) {
	procDir := writeProcFile(t, "vmstat", strings.Join([]string{
		"pgfault 100",
		"solitary", // single token: panics upstream
		"",         // empty line
		"   ",      // whitespace only
		"pgmajfault 200",
	}, "\n")+"\n")

	c, err := newVMStatCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)

	// Must not panic, and must still emit the well-formed fields either side of the
	// malformed ones — losing all ~200 fields because one line is odd would be a
	// poor trade.
	names := collectNames(t, c)
	joined := strings.Join(names, " ")
	assert.Contains(t, joined, "node_vmstat_pgfault")
	assert.Contains(t, joined, "node_vmstat_pgmajfault")
	assert.Len(t, names, 2)
}

func TestVMStatUnparseableValueIsSkipped(t *testing.T) {
	procDir := writeProcFile(t, "vmstat", "pgfault notanumber\npgmajfault 5\n")
	c, err := newVMStatCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)

	// Upstream returns an error here, failing the whole collector. Skipping the one
	// bad field keeps the rest, which is the better trade for a 200-field file.
	names := collectNames(t, c)
	assert.Len(t, names, 1)
	assert.Contains(t, names[0], "node_vmstat_pgmajfault")
}

func TestVMStatEmptyFileIsNotAnError(t *testing.T) {
	procDir := writeProcFile(t, "vmstat", "")
	c, err := newVMStatCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)
	assert.Empty(t, collectNames(t, c))
}

func TestVMStatMissingFile(t *testing.T) {
	c, err := newVMStatCollector(quietLogger(), Paths{ProcFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 4))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't open")
}

func TestVMStatValuesAreUntyped(t *testing.T) {
	// Upstream emits these as untyped because the fields are a mix of counters and
	// gauges and it does not attempt to classify them. A change to counter or gauge
	// would alter the exposition format.
	procDir := writeProcFile(t, "vmstat", "pgfault 100\n")
	c, err := newVMStatCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 8)
	require.NoError(t, c.Update(ch))
	close(ch)

	count := 0
	for range ch {
		count++
	}
	assert.Equal(t, 1, count)
}

// --- stat ----------------------------------------------------------------

func TestStatEmitsSixFamilies(t *testing.T) {
	procDir := writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100")
	c, err := newStatCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)

	names := collectNames(t, c)
	joined := strings.Join(names, " ")

	// These sit directly under the node_ namespace with no subsystem, e.g.
	// node_intr_total rather than node_stat_intr_total. A subsystem would rename
	// all six.
	for _, want := range []string{
		"node_intr_total", "node_context_switches_total", "node_forks_total",
		"node_boot_time_seconds", "node_procs_running", "node_procs_blocked",
	} {
		assert.Contains(t, joined, want)
	}
	assert.Len(t, names, 6)
}

func TestStatDoesNotEmitSoftirqs(t *testing.T) {
	// Upstream gates node_softirqs_total on --collector.stat.softirq, which defaults
	// OFF. Not ported, matching the default state.
	procDir := writeProcStat(t, "cpu0 10 20 30 40 50 60 70 80 90 100")
	c, err := newStatCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)

	for _, name := range collectNames(t, c) {
		assert.NotContains(t, name, "softirqs_total")
	}
}

func TestStatMissingFile(t *testing.T) {
	c, err := newStatCollector(quietLogger(), Paths{ProcFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get stat")
}

func TestStatConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newStatCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

func TestVMStatInvalidPatternIsRejectedAtConstruction(t *testing.T) {
	// Unreachable with the compile-time default, but the check must stay: the
	// pattern becomes operator-configurable the moment it is wired to the chart,
	// and an invalid regexp should fail at startup rather than panic on first
	// scrape.
	_, err := newVMStatCollectorWithPattern(quietLogger(),
		Paths{ProcFS: t.TempDir()}.withDefaults(), "([unclosed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid vmstat field pattern")
}

func TestVMStatScannerErrorIsWrapped(t *testing.T) {
	// scanner.Err() fires on an I/O failure mid-read, or on a line exceeding the
	// scanner's buffer. A single line longer than bufio.MaxScanTokenSize (64KB)
	// triggers the latter deterministically without needing a broken filesystem.
	huge := strings.Repeat("x", 70*1024)
	procDir := writeProcFile(t, "vmstat", "pgfault 1\n"+huge+"\n")

	c, err := newVMStatCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 16))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't read")
}
