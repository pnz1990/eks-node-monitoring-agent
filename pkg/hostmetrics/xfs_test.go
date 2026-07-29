package hostmetrics

// Tests for the xfs collector.
//
// 39 counters across 11 nested structs with deliberately repetitive names
// (AllocationBTree.Lookups, BlockMapBTree.Lookups, DirectoryOperation.Lookup,
// InodeOperation.Found...). The failure mode is one entry pointing at its
// NEIGHBOUR's field, which produces a metric with the correct name, type and labels
// reporting a plausible number from the wrong counter. That is invisible to any
// structural comparison and to the three-way harness.
//
// So the central test does not spot-check values: it re-extracts upstream's whole
// (name, help, accessor) table from source and compares all 39 pairs. A hand-check of
// 39 entries is not something I would trust, and not something a reviewer should have
// to trust either.

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs/xfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- the whole table, compared against upstream ----------------------------

// TestXFSTableMatchesUpstreamNameHelpAndField is the test that actually matters.
//
// It parses upstream's metrics table into (name, help, struct field path) triples and
// compares against ours. The struct field path is recovered from OUR table by
// reflection over a probe: each accessor is called against a Stats value in which
// exactly one leaf field is set to a unique sentinel, so the accessor that returns
// that sentinel identifies which field it reads.
//
// That means a mis-wired accessor fails here even though the metric name is right.
func TestXFSTableMatchesUpstreamNameHelpAndField(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/xfs_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}

	body := regexp.MustCompile(`(?s)metrics := \[\]struct \{.*?\n\t\}\{(.*?)\n\t\}\n`).
		FindSubmatch(data)
	require.NotNil(t, body, "failed to locate upstream's metrics table; the regexp may be stale")

	type entry struct{ help, field string }
	upstream := map[string]entry{}
	for _, m := range regexp.MustCompile(
		`\{\s*name:\s*"([^"]+)",\s*desc:\s*"([^"]*)",\s*value:\s*float64\(s\.([A-Za-z.]+)\),\s*\}`,
	).FindAllSubmatch(body[1], -1) {
		upstream[string(m[1])] = entry{help: string(m[2]), field: string(m[3])}
	}
	require.Len(t, upstream, 39, "expected 39 upstream xfs metrics, got %d", len(upstream))

	ours := xfsMetrics()
	require.Len(t, ours, len(upstream), "table length must match upstream")

	// Map each of our accessors to the struct field it reads, by probing.
	fieldOf := map[string]string{}
	for _, m := range ours {
		field := probeXFSAccessor(t, m.value)
		require.NotEmpty(t, field,
			"could not determine which field %q reads; the probe may need updating", m.name)
		fieldOf[m.name] = field
	}

	for name, want := range upstream {
		got, ok := fieldOf[name]
		require.True(t, ok, "upstream metric %q is missing from our table", name)
		assert.Equal(t, want.field, got,
			"%s reads the WRONG FIELD: upstream uses s.%s, we use s.%s", name, want.field, got)
	}
	for name := range fieldOf {
		assert.Contains(t, upstream, name, "we emit %q which upstream does not", name)
	}

	// Help text is part of the exposition output, so it is compared too.
	descs := xfsDescs(t)
	for name, want := range upstream {
		assert.Contains(t, descs[name], want.help,
			"help text for %s must be upstream's verbatim", name)
	}
}

// probeXFSAccessor determines which leaf field an accessor reads.
//
// Walks every leaf uint32/uint64 field of xfs.Stats, sets exactly one to a sentinel,
// and checks whether the accessor returns it. Returns the dotted field path.
func probeXFSAccessor(t *testing.T, accessor func(*xfs.Stats) float64) string {
	t.Helper()

	const sentinel = 987654321

	var found string
	walkXFSLeaves(reflect.ValueOf(&xfs.Stats{}).Elem(), "", func(path string, set func(uint64)) {
		var s xfs.Stats
		var target func(uint64)
		walkXFSLeaves(reflect.ValueOf(&s).Elem(), "", func(p string, setter func(uint64)) {
			if p == path {
				target = setter
			}
		})
		if target == nil {
			return
		}
		target(sentinel)
		if accessor(&s) == sentinel && found == "" {
			found = path
		}
	})
	return found
}

// walkXFSLeaves visits every settable numeric leaf field, passing its dotted path.
func walkXFSLeaves(v reflect.Value, prefix string, visit func(path string, set func(uint64))) {
	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		name := typ.Field(i).Name
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}

		switch field.Kind() {
		case reflect.Struct:
			walkXFSLeaves(field, path, visit)
		case reflect.Uint32, reflect.Uint64:
			f := field
			visit(path, func(x uint64) { f.SetUint(x) })
		}
	}
}

func TestXFSTableHasNoDuplicateNames(t *testing.T) {
	// A duplicated name would make Prometheus reject the scrape on a duplicate label
	// set, and with 39 repetitive names a copy-paste duplicate is plausible.
	seen := map[string]bool{}
	for _, m := range xfsMetrics() {
		assert.False(t, seen[m.name], "duplicate metric name %q", m.name)
		seen[m.name] = true
	}
	assert.Len(t, seen, 39)
}

func TestXFSTableHasNoDuplicateAccessors(t *testing.T) {
	// Two entries reading the SAME field is the copy-paste failure this collector is
	// most exposed to: both metrics exist with plausible values and nothing complains.
	byField := map[string]string{}
	for _, m := range xfsMetrics() {
		field := probeXFSAccessor(t, m.value)
		require.NotEmpty(t, field, "could not probe %q", m.name)
		if prior, dup := byField[field]; dup {
			t.Errorf("%s and %s both read s.%s", m.name, prior, field)
		}
		byField[field] = m.name
	}
	assert.Len(t, byField, 39, "39 metrics must read 39 distinct fields")
}

// --- emitted metrics ------------------------------------------------------

func TestXFSEmitsAllMetricsPerDevice(t *testing.T) {
	c := newXFSStub(t, []*xfs.Stats{
		{Name: "sda1"},
		{Name: "nvme0n1p1"},
	})

	got := gatherLabelled(t, c, "device")
	assert.Len(t, got, 39, "39 metric families")
	for name, byDevice := range got {
		assert.Len(t, byDevice, 2, "%s must be reported for both devices", name)
	}
}

func TestXFSValuesComeFromTheRightFields(t *testing.T) {
	// A handful of spot checks with distinct values, as a cheap guard alongside the
	// exhaustive table comparison. Chosen from the structs whose field names collide
	// across parents -- Lookups appears on both B-trees.
	c := newXFSStub(t, []*xfs.Stats{{
		Name: "sda1",
		ExtentAllocation: xfs.ExtentAllocationStats{
			ExtentsAllocated: 11, BlocksAllocated: 12, ExtentsFreed: 13, BlocksFreed: 14,
		},
		AllocationBTree: xfs.BTreeStats{
			Lookups: 21, Compares: 22, RecordsInserted: 23, RecordsDeleted: 24,
		},
		BlockMapBTree: xfs.BTreeStats{
			Lookups: 31, Compares: 32, RecordsInserted: 33, RecordsDeleted: 34,
		},
	}})

	got := gatherLabelled(t, c, "device")
	assert.Equal(t, 11.0, got["node_xfs_extent_allocation_extents_allocated_total"]["sda1"])
	assert.Equal(t, 12.0, got["node_xfs_extent_allocation_blocks_allocated_total"]["sda1"])
	assert.Equal(t, 13.0, got["node_xfs_extent_allocation_extents_freed_total"]["sda1"])
	assert.Equal(t, 14.0, got["node_xfs_extent_allocation_blocks_freed_total"]["sda1"])
	// The pair that would silently swap: both B-trees have a Lookups field.
	assert.Equal(t, 21.0, got["node_xfs_allocation_btree_lookups_total"]["sda1"])
	assert.Equal(t, 31.0, got["node_xfs_block_map_btree_lookups_total"]["sda1"],
		"block-map B-tree lookups must not come from the allocation B-tree")
	assert.Equal(t, 22.0, got["node_xfs_allocation_btree_compares_total"]["sda1"])
	assert.Equal(t, 32.0, got["node_xfs_block_map_btree_compares_total"]["sda1"])
}

func TestXFSAllMetricsAreCounters(t *testing.T) {
	// Every one is a monotonic kernel counter since mount, so rate() is the useful
	// query. As gauges they would be nearly useless.
	c := newXFSStub(t, []*xfs.Stats{{Name: "sda1"}})

	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)

	n := 0
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.NotNil(t, pb.Counter, "%s must be a counter", metricName(t, m))
		assert.Nil(t, pb.Gauge)
		n++
	}
	assert.Equal(t, 39, n)
}

func TestXFSDeviceLabelComesFromStatsName(t *testing.T) {
	c := newXFSStub(t, []*xfs.Stats{{Name: "nvme0n1p1"}})

	got := gatherLabelled(t, c, "device")
	for name, byDevice := range got {
		assert.Contains(t, byDevice, "nvme0n1p1", "%s must be labelled with the device", name)
	}
}

func TestXFSNoFilesystemsSucceedsWithZeroSeries(t *testing.T) {
	// A node with no XFS. Upstream returns nil, not ErrNoData, so the contract is
	// success-with-no-series -- the same as the rest of the hardware group.
	c := newXFSStub(t, nil)

	ch := make(chan prometheus.Metric, 16)
	require.NoError(t, c.Update(ch), "no XFS filesystems must not be a failure")
	close(ch)
	assert.Empty(t, ch)
}

func TestXFSReadFailureIsWrapped(t *testing.T) {
	c := newXFSStub(t, nil)
	c.sysStats = func() ([]*xfs.Stats, error) { return nil, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err))
	assert.Contains(t, err.Error(), "failed to retrieve XFS stats")
}

func TestXFSDescriptorsAreBuiltOnceAtConstruction(t *testing.T) {
	// The metric set is fixed at compile time, unlike netdev where the kernel supplies
	// the field names. So the descriptors are built once; rebuilding them per scrape
	// would allocate 39 Descs every 15 seconds for no reason.
	c := newXFSStub(t, []*xfs.Stats{{Name: "sda1"}})
	require.Len(t, c.descs, 39)

	first := c.descs[0]
	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)
	assert.Same(t, first, c.descs[0], "descriptors must not be rebuilt")
}

func TestXFSConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newXFSCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent"), SysFS: t.TempDir()}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs/sysfs")
}

func TestXFSLiveOnThisHost(t *testing.T) {
	// The root volume is XFS on Amazon Linux 2023, so the live EKS node reports 40
	// series. This dev host may differ, so an empty result is tolerated -- but a
	// failure is not.
	c, err := newXFSCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 4096)
	err = c.Update(ch)
	close(ch)
	if err != nil && IsNoDataError(err) {
		t.Skip("no XFS on this host")
	}
	require.NoError(t, err, "xfs must not fail on a real host")

	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.GreaterOrEqual(t, pb.GetCounter().GetValue(), 0.0,
			"%s must not be negative", metricName(t, m))
	}
}

func TestXFSRegisteredConstructorWiresTheRealReader(t *testing.T) {
	c, err := newXFSCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	require.NotNil(t, c.(*xfsCollector).sysStats)
}

// --- helpers --------------------------------------------------------------

func newXFSStub(t *testing.T, stats []*xfs.Stats) *xfsCollector {
	t.Helper()
	c, err := newXFSCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	xc := c.(*xfsCollector)
	xc.sysStats = func() ([]*xfs.Stats, error) { return stats, nil }
	return xc
}

// xfsDescs returns metric name -> descriptor string.
func xfsDescs(t *testing.T) map[string]string {
	t.Helper()

	c := newXFSStub(t, []*xfs.Stats{{Name: "sda1"}})
	out := make(map[string]string, len(c.descs))
	for i, m := range xfsMetrics() {
		out[m.name] = c.descs[i].String()
	}
	return out
}
