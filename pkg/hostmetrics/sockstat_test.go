package hostmetrics

// Tests for the sockstat collector.
//
// The focus is mem_bytes. /proc/net/sockstat reports "mem" in PAGES, and the
// derived *_mem_bytes metric multiplies by the page size. Hardcoding 4096 would be
// correct on x86_64 and 16x wrong on an arm64 kernel with 64K pages -- and Graviton
// nodes are common on EKS. The metric would exist with the right name and type
// either way, so only an explicit assertion catches it.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- the page-size conversion ---------------------------------------------

func TestSockStatMemBytesUsesTheRuntimePageSize(t *testing.T) {
	// The assertion is against os.Getpagesize(), NOT against 4096: writing 4096
	// here would make the test agree with a hardcoded bug on x86_64 and fail
	// legitimately on a 64K-page arm64 host. Asserting against the runtime value
	// is the only form that is correct on both.
	c := newSockStatFixtureCollector(t)

	pages := 42
	c.sockstat = func() (*procfs.NetSockstat, error) {
		return &procfs.NetSockstat{
			Protocols: []procfs.NetSockstatProtocol{
				{Protocol: "TCP", InUse: 1, Mem: &pages},
			},
		}, nil
	}
	c.sockstat6 = func() (*procfs.NetSockstat, error) { return nil, os.ErrNotExist }

	got := gatherSockStat(t, c)

	assert.Equal(t, 42.0, got["node_sockstat_TCP_mem"],
		"mem is reported in pages, unconverted")
	assert.Equal(t, float64(42*os.Getpagesize()), got["node_sockstat_TCP_mem_bytes"],
		"mem_bytes must be pages * the runtime page size")
}

func TestSockStatPageSizeIsNotHardcoded(t *testing.T) {
	// Guards the constructor rather than the arithmetic: a literal 4096 assigned to
	// c.pageSize would pass the conversion test above on this host but fail here on
	// any kernel with a different page size.
	c := newSockStatFixtureCollector(t)
	assert.Equal(t, os.Getpagesize(), c.pageSize,
		"the page size must come from the runtime, not a constant")
}

func TestSockStatMemBytesAbsentWhenMemIsAbsent(t *testing.T) {
	// A protocol that does not report mem must not get a fabricated mem_bytes of 0.
	c := newSockStatFixtureCollector(t)
	c.sockstat = func() (*procfs.NetSockstat, error) {
		return &procfs.NetSockstat{
			Protocols: []procfs.NetSockstatProtocol{{Protocol: "RAW", InUse: 3}},
		}, nil
	}
	c.sockstat6 = func() (*procfs.NetSockstat, error) { return nil, os.ErrNotExist }

	got := gatherSockStat(t, c)
	assert.Equal(t, 3.0, got["node_sockstat_RAW_inuse"])
	assert.NotContains(t, got, "node_sockstat_RAW_mem")
	assert.NotContains(t, got, "node_sockstat_RAW_mem_bytes")
}

// --- nil field handling ---------------------------------------------------

func TestSockStatNilFieldsAreOmittedNotZeroed(t *testing.T) {
	// Only InUse is a value; the rest are pointers, and nil means "this protocol
	// does not report that field". Emitting zero would be a claim the kernel never
	// made -- and for a field like orphan, a fabricated 0 reads as healthy.
	c := newSockStatFixtureCollector(t)
	orphan := 7
	c.sockstat = func() (*procfs.NetSockstat, error) {
		return &procfs.NetSockstat{
			Protocols: []procfs.NetSockstatProtocol{
				{Protocol: "TCP", InUse: 5, Orphan: &orphan},
			},
		}, nil
	}
	c.sockstat6 = func() (*procfs.NetSockstat, error) { return nil, os.ErrNotExist }

	got := gatherSockStat(t, c)
	assert.Equal(t, 5.0, got["node_sockstat_TCP_inuse"])
	assert.Equal(t, 7.0, got["node_sockstat_TCP_orphan"])
	for _, absent := range []string{"tw", "alloc", "mem", "memory", "mem_bytes"} {
		assert.NotContains(t, got, "node_sockstat_TCP_"+absent,
			"%s is nil and must be omitted, not zeroed", absent)
	}
}

func TestSockStatFieldNamesArePreservedForCompatibility(t *testing.T) {
	// These names were originally generated from the file's own field names, and
	// upstream keeps the mapping for compatibility. So "tw" stays "tw" rather than
	// becoming something more readable like "time_wait": renaming it would break
	// every dashboard that already selects it.
	c := newSockStatFixtureCollector(t)
	tw, alloc, mem, memory, orphan := 1, 2, 3, 4, 5
	c.sockstat = func() (*procfs.NetSockstat, error) {
		return &procfs.NetSockstat{
			Protocols: []procfs.NetSockstatProtocol{{
				Protocol: "TCP", InUse: 6,
				Orphan: &orphan, TW: &tw, Alloc: &alloc, Mem: &mem, Memory: &memory,
			}},
		}, nil
	}
	c.sockstat6 = func() (*procfs.NetSockstat, error) { return nil, os.ErrNotExist }

	got := gatherSockStat(t, c)
	assert.Equal(t, 6.0, got["node_sockstat_TCP_inuse"])
	assert.Equal(t, 5.0, got["node_sockstat_TCP_orphan"])
	assert.Equal(t, 1.0, got["node_sockstat_TCP_tw"], "the field is named tw, not time_wait")
	assert.Equal(t, 2.0, got["node_sockstat_TCP_alloc"])
	assert.Equal(t, 3.0, got["node_sockstat_TCP_mem"])
	assert.Equal(t, 4.0, got["node_sockstat_TCP_memory"],
		"mem and memory are DIFFERENT fields and both must be reported")
}

// --- sockets_used is IPv4-only --------------------------------------------

func TestSockStatSocketsUsedIsIPv4OnlyAndUnlabelled(t *testing.T) {
	// sockets_used exists only in the IPv4 file and carries no protocol label.
	// Emitting it for IPv6 as well would produce a DUPLICATE label set (both
	// unlabelled), and Prometheus rejects the entire scrape on a duplicate -- so
	// this would not degrade the metric, it would break everything.
	c := newSockStatFixtureCollector(t)
	used4, used6 := 100, 200
	c.sockstat = func() (*procfs.NetSockstat, error) {
		return &procfs.NetSockstat{Used: &used4}, nil
	}
	// A hypothetical future kernel exporting Used in sockstat6 must still not
	// produce a second series.
	c.sockstat6 = func() (*procfs.NetSockstat, error) {
		return &procfs.NetSockstat{Used: &used6}, nil
	}

	c2 := c
	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c2.Update(ch))
	close(ch)

	count := 0
	var value float64
	for m := range ch {
		if metricName(t, m) != "node_sockstat_sockets_used" {
			continue
		}
		count++
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		value = pb.GetGauge().GetValue()
	}
	assert.Equal(t, 1, count,
		"exactly one sockets_used series; a second would be a duplicate label set and Prometheus would reject the scrape")
	assert.Equal(t, 100.0, value, "it must be the IPv4 value")
}

func TestSockStatSocketsUsedOmittedWhenAbsent(t *testing.T) {
	c := newSockStatFixtureCollector(t)
	c.sockstat = func() (*procfs.NetSockstat, error) {
		return &procfs.NetSockstat{Protocols: []procfs.NetSockstatProtocol{
			{Protocol: "TCP", InUse: 1},
		}}, nil
	}
	c.sockstat6 = func() (*procfs.NetSockstat, error) { return nil, os.ErrNotExist }

	assert.NotContains(t, gatherSockStat(t, c), "node_sockstat_sockets_used")
}

// --- IPv4 / IPv6 independence --------------------------------------------

func TestSockStatIPv6AbsentStillReportsIPv4(t *testing.T) {
	// The common case on a v4-only cluster: a disabled family must not cost the
	// other family's metrics, and must not be a scrape failure.
	c := newSockStatFixtureCollector(t)
	c.sockstat = func() (*procfs.NetSockstat, error) {
		return &procfs.NetSockstat{Protocols: []procfs.NetSockstatProtocol{
			{Protocol: "TCP", InUse: 11},
		}}, nil
	}
	c.sockstat6 = func() (*procfs.NetSockstat, error) { return nil, os.ErrNotExist }

	got := gatherSockStat(t, c)
	assert.Equal(t, 11.0, got["node_sockstat_TCP_inuse"])
	assert.NotContains(t, got, "node_sockstat_TCP6_inuse")
}

func TestSockStatIPv4AbsentStillReportsIPv6(t *testing.T) {
	c := newSockStatFixtureCollector(t)
	c.sockstat = func() (*procfs.NetSockstat, error) { return nil, os.ErrNotExist }
	c.sockstat6 = func() (*procfs.NetSockstat, error) {
		return &procfs.NetSockstat{Protocols: []procfs.NetSockstatProtocol{
			{Protocol: "TCP6", InUse: 22},
		}}, nil
	}

	got := gatherSockStat(t, c)
	assert.Equal(t, 22.0, got["node_sockstat_TCP6_inuse"])
}

func TestSockStatBothAbsentSucceedsWithNoMetrics(t *testing.T) {
	// Upstream returns nil here rather than ErrNoData, unlike udp_queues. Preserved
	// for parity even though it is inconsistent between the two collectors, and
	// asserted so the difference is deliberate rather than accidental.
	c := newSockStatFixtureCollector(t)
	c.sockstat = func() (*procfs.NetSockstat, error) { return nil, os.ErrNotExist }
	c.sockstat6 = func() (*procfs.NetSockstat, error) { return nil, os.ErrNotExist }

	ch := make(chan prometheus.Metric, 8)
	require.NoError(t, c.Update(ch), "upstream returns nil, not ErrNoData; kept for parity")
	close(ch)
	assert.Empty(t, ch)
}

func TestSockStatRealIPv4ErrorIsAFailure(t *testing.T) {
	c := newSockStatFixtureCollector(t)
	c.sockstat = func() (*procfs.NetSockstat, error) { return nil, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get IPv4 sockstat data")
}

func TestSockStatRealIPv6ErrorIsAFailure(t *testing.T) {
	// Unreadable is not the same as absent: absent means IPv6 is disabled,
	// unreadable means something is wrong and must be surfaced.
	c := newSockStatFixtureCollector(t)
	c.sockstat = func() (*procfs.NetSockstat, error) { return &procfs.NetSockstat{}, nil }
	c.sockstat6 = func() (*procfs.NetSockstat, error) { return nil, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get IPv6 sockstat data")
}

// --- real fixture ---------------------------------------------------------

func TestSockStatParsesTheRealFile(t *testing.T) {
	// Against the real host file rather than a stub, so the procfs field mapping is
	// exercised end to end. Metric names are built at runtime from the protocol
	// names in the file, so they cannot be enumerated from the source -- the same
	// reason the whole parity methodology uses live scrape diffing.
	c, err := newSockStatCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	got := gatherSockStat(t, c.(*sockStatCollector))
	require.NotEmpty(t, got)

	assert.Contains(t, got, "node_sockstat_sockets_used")
	assert.Contains(t, got, "node_sockstat_TCP_inuse")
	// TCP reports mem on every modern kernel, so mem_bytes must be derived.
	if _, ok := got["node_sockstat_TCP_mem"]; ok {
		assert.Contains(t, got, "node_sockstat_TCP_mem_bytes")
		assert.Equal(t, got["node_sockstat_TCP_mem"]*float64(os.Getpagesize()),
			got["node_sockstat_TCP_mem_bytes"])
	}
}

func TestSockStatAllMetricsAreGauges(t *testing.T) {
	// Including the fields that sound cumulative: inuse, orphan, tw and alloc are
	// all CURRENT counts of sockets in a state, not totals over time.
	c, err := newSockStatCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 1024)
	require.NoError(t, c.Update(ch))
	close(ch)

	n := 0
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.NotNil(t, pb.Gauge, "%s must be a gauge", metricName(t, m))
		assert.Nil(t, pb.Counter)
		n++
	}
	assert.Positive(t, n, "no metrics were checked")
}

func TestSockStatConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newSockStatCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

func TestSockStatRegisteredConstructorWiresTheRealReaders(t *testing.T) {
	c, err := newSockStatCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	sc := c.(*sockStatCollector)
	require.NotNil(t, sc.sockstat)
	require.NotNil(t, sc.sockstat6)
}

// --- helpers --------------------------------------------------------------

func newSockStatFixtureCollector(t *testing.T) *sockStatCollector {
	t.Helper()
	c, err := newSockStatCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	return c.(*sockStatCollector)
}

// gatherSockStat returns metric name -> value. Every sockstat metric is
// unlabelled, so the name alone is a unique key.
func gatherSockStat(t *testing.T, c *sockStatCollector) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 2048)
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
