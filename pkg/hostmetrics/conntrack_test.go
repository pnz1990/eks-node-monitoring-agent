package hostmetrics

// Tests for the conntrack collector.
//
// Three focal points:
//
//   - the EMPTY subsystem. These are node_nf_conntrack_*, NOT
//     node_conntrack_nf_conntrack_*. Passing "conntrack" as the subsystem would
//     produce plausible names that no dashboard matches -- the same trap as the
//     stat collector's node_intr_total.
//   - the per-CPU SUM. /proc/net/stat/nf_conntrack has one row per CPU (32 on this
//     host) and the metrics are node-wide. Reporting one row would silently report
//     one CPU's share.
//   - the ErrNoData vs failure distinction. The nf_conntrack module being absent is
//     a legitimate configuration; reporting it as a failure would alert on a working
//     system.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- the empty subsystem --------------------------------------------------

func TestConntrackMetricNamesUseAnEmptySubsystem(t *testing.T) {
	// Verified against the live cluster golden corpus, which contains
	// "node_nf_conntrack_entries". A "conntrack" subsystem would yield
	// "node_conntrack_nf_conntrack_entries" -- a name that looks right in isolation
	// and matches nothing.
	got := gatherConntrack(t, newConntrackFixtureCollector(t))

	for name := range got {
		assert.True(t, strings.HasPrefix(name, "node_nf_conntrack"),
			"%q must start with node_nf_conntrack; an extra subsystem segment breaks every dashboard", name)
		assert.NotContains(t, name, "node_conntrack_",
			"%q has a doubled subsystem", name)
	}

	assert.Contains(t, got, "node_nf_conntrack_entries")
	assert.Contains(t, got, "node_nf_conntrack_entries_limit")
	assert.Contains(t, got, "node_nf_conntrack_stat_found")
}

func TestConntrackEmitsAllTenFamilies(t *testing.T) {
	// Two sysctl gauges plus eight stat_* fields. Matches the live corpus exactly,
	// where the PNE node reports 10 node_nf_conntrack_* series.
	got := gatherConntrack(t, newConntrackFixtureCollector(t))

	for _, name := range []string{
		"node_nf_conntrack_entries",
		"node_nf_conntrack_entries_limit",
		"node_nf_conntrack_stat_found",
		"node_nf_conntrack_stat_invalid",
		"node_nf_conntrack_stat_ignore",
		"node_nf_conntrack_stat_insert",
		"node_nf_conntrack_stat_insert_failed",
		"node_nf_conntrack_stat_drop",
		"node_nf_conntrack_stat_early_drop",
		"node_nf_conntrack_stat_search_restart",
	} {
		assert.Contains(t, got, name)
	}
	assert.Len(t, got, 10)
}

// --- the sysctl values, and the ratio that matters ------------------------

func TestConntrackEntriesAndLimitComeFromTheRightFiles(t *testing.T) {
	// entries vs entries_limit is THE signal for conntrack table exhaustion, which
	// on a Kubernetes node presents as random connection failures and DNS timeouts
	// rather than as anything resembling a network problem. Swapping the two files
	// would invert the ratio and make an exhausted table look empty.
	c := newConntrackFixtureCollector(t)
	got := gatherConntrack(t, c)

	assert.Equal(t, 123.0, got["node_nf_conntrack_entries"],
		"entries must come from nf_conntrack_count")
	assert.Equal(t, 65536.0, got["node_nf_conntrack_entries_limit"],
		"limit must come from nf_conntrack_max, not count")
	assert.Less(t, got["node_nf_conntrack_entries"], got["node_nf_conntrack_entries_limit"],
		"a healthy node has entries below the limit; an inversion here would hide exhaustion")
}

// --- the per-CPU summation ------------------------------------------------

func TestConntrackStatsAreSummedAcrossCPUs(t *testing.T) {
	// /proc/net/stat/nf_conntrack has one row PER CPU -- 32 rows on this host. The
	// exported metrics are node-wide, so reporting a single row (or the last one)
	// would silently report one CPU's share of the traffic and understate drops.
	c := newConntrackFixtureCollector(t)
	c.conntrackStat = func() ([]procfs.ConntrackStatEntry, error) {
		return []procfs.ConntrackStatEntry{
			{Found: 1, Invalid: 10, Ignore: 100, Insert: 1000,
				InsertFailed: 2, Drop: 20, EarlyDrop: 200, SearchRestart: 2000},
			{Found: 3, Invalid: 30, Ignore: 300, Insert: 3000,
				InsertFailed: 4, Drop: 40, EarlyDrop: 400, SearchRestart: 4000},
		}, nil
	}

	got := gatherConntrack(t, c)

	// Each expected value is a distinct sum, so a field reading the wrong struct
	// member fails rather than coincidentally matching.
	assert.Equal(t, 4.0, got["node_nf_conntrack_stat_found"], "1+3")
	assert.Equal(t, 40.0, got["node_nf_conntrack_stat_invalid"], "10+30")
	assert.Equal(t, 400.0, got["node_nf_conntrack_stat_ignore"], "100+300")
	assert.Equal(t, 4000.0, got["node_nf_conntrack_stat_insert"], "1000+3000")
	assert.Equal(t, 6.0, got["node_nf_conntrack_stat_insert_failed"], "2+4")
	assert.Equal(t, 60.0, got["node_nf_conntrack_stat_drop"], "20+40")
	assert.Equal(t, 600.0, got["node_nf_conntrack_stat_early_drop"], "200+400")
	assert.Equal(t, 6000.0, got["node_nf_conntrack_stat_search_restart"], "2000+4000")
}

func TestConntrackSingleCPURowIsNotSpecialCased(t *testing.T) {
	c := newConntrackFixtureCollector(t)
	c.conntrackStat = func() ([]procfs.ConntrackStatEntry, error) {
		return []procfs.ConntrackStatEntry{{Found: 7, Drop: 9}}, nil
	}

	got := gatherConntrack(t, c)
	assert.Equal(t, 7.0, got["node_nf_conntrack_stat_found"])
	assert.Equal(t, 9.0, got["node_nf_conntrack_stat_drop"])
}

func TestConntrackNoStatRowsYieldsZeroesNotAbsence(t *testing.T) {
	// Distinct from the module being absent: the file exists and reports no rows, so
	// zero IS the measured value. Unlike diskstats' short lines, here the kernel has
	// answered.
	c := newConntrackFixtureCollector(t)
	c.conntrackStat = func() ([]procfs.ConntrackStatEntry, error) {
		return nil, nil
	}

	got := gatherConntrack(t, c)
	assert.Equal(t, 0.0, got["node_nf_conntrack_stat_found"])
	assert.Len(t, got, 10, "all ten families are still reported")
}

func TestConntrackStatFieldsAreAllDistinct(t *testing.T) {
	// The accessor table pairs a descriptor with a struct field. Two entries reading
	// the same field would be invisible: both metrics exist with plausible values.
	// Detected by giving every field a unique value and checking the outputs are a
	// permutation of the inputs.
	c := newConntrackFixtureCollector(t)
	c.conntrackStat = func() ([]procfs.ConntrackStatEntry, error) {
		return []procfs.ConntrackStatEntry{{
			Found: 1, Invalid: 2, Ignore: 3, Insert: 4,
			InsertFailed: 5, Drop: 6, EarlyDrop: 7, SearchRestart: 8,
		}}, nil
	}

	got := gatherConntrack(t, c)
	seen := map[float64]string{}
	for name, value := range got {
		if !strings.Contains(name, "_stat_") {
			continue
		}
		if prior, dup := seen[value]; dup {
			t.Errorf("%s and %s both report %v; two descriptors read the same field", name, prior, value)
		}
		seen[value] = name
	}
	assert.Len(t, seen, 8, "all eight stat fields must have distinct values")
}

// --- ErrNoData vs failure -------------------------------------------------

func TestConntrackModuleAbsentIsNoDataNotFailure(t *testing.T) {
	// nf_conntrack not being loaded is a legitimate configuration -- a node with no
	// iptables-based networking. Reporting it as a scrape failure would alert on a
	// working system.
	root := t.TempDir()
	c, err := newConntrackCollector(quietLogger(), Paths{ProcFS: root}.withDefaults())
	require.NoError(t, err, "construction must not read the sysctls")

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err), "expected ErrNoData, got %v", err)
}

func TestConntrackMissingMaxIsNoData(t *testing.T) {
	// count present but max absent. Reached separately because it is a different
	// return site.
	root := conntrackFixtureProcFS(t)
	require.NoError(t, os.Remove(filepath.Join(root, "sys", "net", "netfilter", "nf_conntrack_max")))

	c, err := newConntrackCollector(quietLogger(), Paths{ProcFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
}

func TestConntrackStatReadFailureIsNoDataWhenAbsent(t *testing.T) {
	c := newConntrackFixtureCollector(t)
	c.conntrackStat = func() ([]procfs.ConntrackStatEntry, error) {
		return nil, os.ErrNotExist
	}

	err := c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
}

func TestConntrackStatReadFailureIsAnErrorWhenReal(t *testing.T) {
	// A parse failure is not the module being absent, and must not be laundered into
	// ErrNoData -- that would hide a genuinely broken procfs behind a benign signal.
	c := newConntrackFixtureCollector(t)
	c.conntrackStat = func() ([]procfs.ConntrackStatEntry, error) {
		return nil, assert.AnError
	}

	err := c.Update(make(chan prometheus.Metric, 32))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err), "a real failure must not be reported as no-data")
	assert.Contains(t, err.Error(), "failed to retrieve conntrack stats")
}

func TestConntrackUnparseableSysctlIsAnErrorNotNoData(t *testing.T) {
	// A file that exists but contains garbage is a real failure. Critically it must
	// NOT be ErrNoData, because handleErr keys on os.ErrNotExist and a parse error
	// wrapped the wrong way would match.
	root := conntrackFixtureProcFS(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "sys", "net", "netfilter", "nf_conntrack_count"),
		[]byte("not-a-number\n"), 0o644))

	c, err := newConntrackCollector(quietLogger(), Paths{ProcFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err), "a garbage sysctl must not be reported as no-data")
	assert.Contains(t, err.Error(), "failed to retrieve conntrack stats")
}

// --- readUintFromFile -----------------------------------------------------

func TestReadUintFromFile(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "value")
	require.NoError(t, os.WriteFile(path, []byte("42\n"), 0o644))
	v, err := readUintFromFile(path)
	require.NoError(t, err)
	assert.Equal(t, uint64(42), v, "the trailing newline must be trimmed")

	// Sysctls are written with a newline; some are written without one.
	require.NoError(t, os.WriteFile(path, []byte("131072"), 0o644))
	v, err = readUintFromFile(path)
	require.NoError(t, err)
	assert.Equal(t, uint64(131072), v)

	// Leading/trailing whitespace.
	require.NoError(t, os.WriteFile(path, []byte("  7  \n"), 0o644))
	v, err = readUintFromFile(path)
	require.NoError(t, err)
	assert.Equal(t, uint64(7), v)
}

func TestReadUintFromFileMissingReturnsUnwrappedNotExist(t *testing.T) {
	// handleErr keys on errors.Is(err, os.ErrNotExist). If this were wrapped with
	// fmt.Errorf using %v rather than %w, that match would break and "module not
	// loaded" would be reported as a hard failure -- so the propagation is asserted,
	// not assumed.
	_, err := readUintFromFile(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err),
		"ErrNotExist must survive so handleErr can distinguish absent from broken")
}

func TestReadUintFromFileRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "value")

	for _, content := range []string{"not-a-number", "", "-1", "3.14", "0x10"} {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		_, err := readUintFromFile(path)
		require.Error(t, err, "content %q must not parse", content)
		assert.Contains(t, err.Error(), "failed to parse")
		// Must not be mistakable for an absent file.
		assert.False(t, os.IsNotExist(err))
	}
}

// --- metric types ---------------------------------------------------------

func TestConntrackAllMetricsAreGauges(t *testing.T) {
	// Upstream types everything as a gauge, including stat_drop and stat_early_drop,
	// which ARE monotonic kernel counters. That is arguably wrong -- rate() over them
	// is unsupported by the type even though the data would support it -- but
	// changing it breaks the contract, so it is preserved and recorded in
	// docs/parity-exceptions-nodep.md rather than silently "fixed".
	c := newConntrackFixtureCollector(t)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	n := 0
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.NotNil(t, pb.Gauge, "%s must be a gauge, matching upstream", metricName(t, m))
		assert.Nil(t, pb.Counter)
		n++
	}
	assert.Equal(t, 10, n)
}

func TestConntrackMetricsAreUnlabelled(t *testing.T) {
	// All ten are node-wide with no labels. A stray label would make them a
	// different series than the reference endpoint's.
	c := newConntrackFixtureCollector(t)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.Empty(t, pb.GetLabel(), "%s must carry no labels", metricName(t, m))
	}
}

func TestConntrackConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newConntrackCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

func TestConntrackRegisteredConstructorWiresTheRealStatReader(t *testing.T) {
	c, err := newConntrackCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	require.NotNil(t, c.(*conntrackCollector).conntrackStat)
}

// --- helpers --------------------------------------------------------------

// conntrackFixtureProcFS builds a procfs with the two sysctls and the stat file.
func conntrackFixtureProcFS(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	sysDir := filepath.Join(root, "sys", "net", "netfilter")
	require.NoError(t, os.MkdirAll(sysDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sysDir, "nf_conntrack_count"), []byte("123\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(sysDir, "nf_conntrack_max"), []byte("65536\n"), 0o644))

	statDir := filepath.Join(root, "net", "stat")
	require.NoError(t, os.MkdirAll(statDir, 0o755))
	// Two per-CPU rows so the summation is exercised by the default fixture too.
	require.NoError(t, os.WriteFile(filepath.Join(statDir, "nf_conntrack"), []byte(
		"entries  searched found new invalid ignore delete delete_list insert insert_failed drop early_drop icmp_error  expect_new expect_create expect_delete search_restart\n"+
			"0000007b  00000000 00000001 00000000 00000002 00000003 00000000 00000000 00000004 00000005 00000006 00000007 00000000  00000000 00000000 00000000 00000008\n"+
			"0000007b  00000000 00000001 00000000 00000002 00000003 00000000 00000000 00000004 00000005 00000006 00000007 00000000  00000000 00000000 00000000 00000008\n",
	), 0o644))

	return root
}

func newConntrackFixtureCollector(t *testing.T) *conntrackCollector {
	t.Helper()
	c, err := newConntrackCollector(quietLogger(),
		Paths{ProcFS: conntrackFixtureProcFS(t)}.withDefaults())
	require.NoError(t, err)
	return c.(*conntrackCollector)
}

// gatherConntrack returns metric name -> value. Every conntrack metric is
// unlabelled, so the name alone is a unique key.
func gatherConntrack(t *testing.T, c *conntrackCollector) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 128)
	require.NoError(t, c.Update(ch))
	close(ch)

	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		out[metricName(t, m)] = pb.GetGauge().GetValue()
	}
	return out
}
