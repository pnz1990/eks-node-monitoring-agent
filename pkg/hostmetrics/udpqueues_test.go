package hostmetrics

// Tests for the udp_queues collector.
//
// Only four series, so the entire subject is the three-way error distinction:
//
//	IPv6 file absent   -> report v4, say nothing about v6, NOT a failure
//	both files absent  -> ErrNoData
//	any other error    -> a real failure
//
// Collapsing "IPv6 is disabled" into either a failure or a silent success is wrong
// in opposite directions: the first alerts on a normal configuration, the second
// hides a genuinely broken procfs. On a v4-only EKS cluster the "IPv6 absent"
// branch is the COMMON path, not an edge case, which is why it gets this much
// attention for four series.

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUDPQueuesEmitsFourSeriesWhenBothFamiliesPresent(t *testing.T) {
	c := newUDPQueuesFixtureCollector(t, true, true)

	got := gatherUDPQueues(t, c)
	assert.Len(t, got, 4, "tx/rx x v4/v6")
	for _, key := range []string{"tx/v4", "rx/v4", "tx/v6", "rx/v6"} {
		assert.Contains(t, got, key)
	}
}

func TestUDPQueuesIPv6AbsentIsNotAFailure(t *testing.T) {
	// The common path on a v4-only cluster. The v4 series must be reported and the
	// collector must succeed -- treating a disabled-IPv6 kernel as a scrape failure
	// would alert on a normal configuration.
	c := newUDPQueuesFixtureCollector(t, true, false)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch), "a v4-only host must not report a failure")
	close(ch)

	got := collectUDPKeys(t, ch)
	assert.Contains(t, got, "tx/v4")
	assert.Contains(t, got, "rx/v4")
	assert.NotContains(t, got, "tx/v6", "no v6 series may be fabricated")
	assert.NotContains(t, got, "rx/v6")
	assert.Len(t, got, 2)
}

func TestUDPQueuesIPv4AbsentStillReportsIPv6(t *testing.T) {
	// The mirror case. Unusual in practice but the code path is symmetric and a
	// v6-only host must not lose its metrics.
	c := newUDPQueuesFixtureCollector(t, false, true)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	got := collectUDPKeys(t, ch)
	assert.Equal(t, []string{"rx/v6", "tx/v6"}, sortedKeys(got))
}

func TestUDPQueuesBothAbsentIsNoData(t *testing.T) {
	// Nothing to report, but the collector did not break either. ErrNoData means
	// node_scrape_collector_success=0 without an error log, which is the honest
	// signal: procfs told us nothing.
	c := newUDPQueuesFixtureCollector(t, false, false)

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err), "expected ErrNoData, got %v", err)
}

func TestUDPQueuesRealIPv4ErrorIsAFailureNotNoData(t *testing.T) {
	// A non-ErrNotExist error means procfs is broken, not that IPv4 is disabled.
	// Reported as a failure. Reachable only through the seam: on a real host the v4
	// file is always present and parseable.
	c := newUDPQueuesFixtureCollector(t, true, true)
	c.udpSummary = func() (*procfs.NetUDPSummary, error) { return nil, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err), "a broken procfs must not be reported as no-data")
	assert.Contains(t, err.Error(), "couldn't get udp queued bytes")
}

func TestUDPQueuesRealIPv6ErrorIsAFailureNotSilence(t *testing.T) {
	// The distinction that matters most here: an unreadable v6 file is NOT the same
	// as an absent one. Absent means IPv6 is disabled; unreadable means something is
	// wrong and must be surfaced rather than quietly reported as a v4-only host.
	c := newUDPQueuesFixtureCollector(t, true, true)
	c.udp6Summary = func() (*procfs.NetUDPSummary, error) { return nil, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err))
	assert.Contains(t, err.Error(), "couldn't get udp6 queued bytes")
}

func TestUDPQueuesValuesComeFromTheSummary(t *testing.T) {
	// Guards against tx and rx being swapped, which no name or label comparison
	// would catch: both series exist either way.
	c := newUDPQueuesFixtureCollector(t, true, true)
	c.udpSummary = func() (*procfs.NetUDPSummary, error) {
		return &procfs.NetUDPSummary{TxQueueLength: 111, RxQueueLength: 222}, nil
	}
	c.udp6Summary = func() (*procfs.NetUDPSummary, error) {
		return &procfs.NetUDPSummary{TxQueueLength: 333, RxQueueLength: 444}, nil
	}

	got := gatherUDPQueues(t, c)
	assert.Equal(t, 111.0, got["tx/v4"], "tx must come from TxQueueLength")
	assert.Equal(t, 222.0, got["rx/v4"], "rx must come from RxQueueLength, not Tx")
	assert.Equal(t, 333.0, got["tx/v6"])
	assert.Equal(t, 444.0, got["rx/v6"])
}

func TestUDPQueuesAreGauges(t *testing.T) {
	// Queue lengths are instantaneous. As counters, rate() would produce garbage
	// from a value that rises and falls with traffic.
	c := newUDPQueuesFixtureCollector(t, true, true)

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	count := 0
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.NotNil(t, pb.Gauge, "udp queue lengths must be gauges")
		assert.Equal(t, "node_udp_queues", metricName(t, m))
		count++
	}
	assert.Equal(t, 4, count)
}

func TestUDPQueuesConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newUDPQueuesCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

func TestUDPQueuesRegisteredConstructorWiresTheRealSummaries(t *testing.T) {
	// Covers the default wiring rather than the seams, so a constructor that forgot
	// to assign udpSummary would fail here rather than nil-panic in production.
	c, err := newUDPQueuesCollector(quietLogger(), Paths{ProcFS: udpFixtureProcFS(t, true, true)}.withDefaults())
	require.NoError(t, err)

	uc := c.(*udpQueuesCollector)
	require.NotNil(t, uc.udpSummary)
	require.NotNil(t, uc.udp6Summary)

	got := gatherUDPQueues(t, uc)
	assert.Len(t, got, 4)
}

// --- helpers --------------------------------------------------------------

// udpFixtureProcFS builds a procfs containing net/udp and/or net/udp6.
//
// The upstream fixture is copied for udp; udp6 is written from the same content
// because the format is identical and only its PRESENCE is under test.
func udpFixtureProcFS(t *testing.T, v4, v6 bool) string {
	t.Helper()
	root := t.TempDir()
	netDir := filepath.Join(root, "net")
	require.NoError(t, os.MkdirAll(netDir, 0o755))

	data, err := os.ReadFile("testdata/proc/net/udp")
	require.NoError(t, err)

	if v4 {
		require.NoError(t, os.WriteFile(filepath.Join(netDir, "udp"), data, 0o644))
	}
	if v6 {
		require.NoError(t, os.WriteFile(filepath.Join(netDir, "udp6"), data, 0o644))
	}
	return root
}

func newUDPQueuesFixtureCollector(t *testing.T, v4, v6 bool) *udpQueuesCollector {
	t.Helper()
	c, err := newUDPQueuesCollector(quietLogger(),
		Paths{ProcFS: udpFixtureProcFS(t, v4, v6)}.withDefaults())
	require.NoError(t, err)
	return c.(*udpQueuesCollector)
}

// gatherUDPQueues returns "queue/ip" -> value.
func gatherUDPQueues(t *testing.T, c *udpQueuesCollector) map[string]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)
	return collectUDPKeys(t, ch)
}

func collectUDPKeys(t *testing.T, ch chan prometheus.Metric) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))

		labels := map[string]string{}
		for _, l := range pb.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		out[labels["queue"]+"/"+labels["ip"]] = pb.GetGauge().GetValue()
	}
	return out
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
