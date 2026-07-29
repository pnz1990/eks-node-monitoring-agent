package hostmetrics

// Tests for the netdev collector.
//
// The legacy() transformation is the focus. It does not merely rename fields — it
// SUMS several kernel error counters into one metric. Omitting a contributor
// produces a metric with the right name, type and labels and the WRONG VALUE,
// which no name or label comparison catches. Every summing rule is asserted
// individually, and the whole table is diffed against upstream's source.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLegacyRulesMatchUpstream parses upstream's legacy() and compares it to our
// table, including the summed contributors.
//
// Upstream lists the "multicast" rule twice; the second occurrence is a no-op
// because the first pop() already removed the key. Compared as sets so that
// harmless redundancy does not read as a mismatch.
func TestLegacyRulesMatchUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/netdev_common.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}

	body := regexp.MustCompile(`(?s)func legacy\(metrics map\[string\]uint64\) \{(.*?)\n\}`).
		FindSubmatch(data)
	require.NotNil(t, body, "failed to locate upstream's legacy(); the regexp may be stale")

	type rule struct {
		from, to string
		plus     string
	}
	upstream := map[rule]bool{}
	ruleRe := regexp.MustCompile(`pop\(metrics,\s*"([^"]+)"\)[\s\S]*?\n\s*metrics\["([^"]+)"\]\s*=\s*metric([^\n]*)`)
	popzRe := regexp.MustCompile(`popz\(metrics,\s*"([^"]+)"\)`)
	for _, m := range ruleRe.FindAllSubmatch(body[1], -1) {
		var plus []string
		for _, p := range popzRe.FindAllSubmatch(m[3], -1) {
			plus = append(plus, string(p[1]))
		}
		upstream[rule{string(m[1]), string(m[2]), strings.Join(plus, ",")}] = true
	}
	require.NotEmpty(t, upstream, "extracted no rules from upstream")

	ours := map[rule]bool{}
	for _, r := range legacyRules() {
		ours[rule{r.from, r.to, strings.Join(r.plus, ",")}] = true
	}

	for r := range upstream {
		assert.True(t, ours[r],
			"upstream rule %s -> %s (plus %q) is missing; the metric would report a wrong value", r.from, r.to, r.plus)
	}
	for r := range ours {
		assert.True(t, upstream[r],
			"we have a rule %s -> %s (plus %q) that upstream does not", r.from, r.to, r.plus)
	}
}

// --- the summing rules, individually -------------------------------------

func TestLegacySumsReceiveFrameContributors(t *testing.T) {
	// node_network_receive_frame_total is the sum of four kernel counters. Omitting
	// any one yields a plausible wrong number.
	fields := map[string]uint64{
		"receive_frame_errors":  1,
		"receive_length_errors": 2,
		"receive_over_errors":   4,
		"receive_crc_errors":    8,
	}
	applyLegacyNames(fields)

	assert.Equal(t, uint64(15), fields["receive_frame"], "must be 1+2+4+8")
	// Contributors must not survive as separate metrics.
	for _, gone := range []string{
		"receive_frame_errors", "receive_length_errors", "receive_over_errors", "receive_crc_errors",
	} {
		assert.NotContains(t, fields, gone)
	}
}

func TestLegacySumsTransmitCarrierContributors(t *testing.T) {
	fields := map[string]uint64{
		"transmit_carrier_errors":   1,
		"transmit_aborted_errors":   2,
		"transmit_heartbeat_errors": 4,
		"transmit_window_errors":    8,
	}
	applyLegacyNames(fields)

	assert.Equal(t, uint64(15), fields["transmit_carrier"])
	for _, gone := range []string{
		"transmit_carrier_errors", "transmit_aborted_errors",
		"transmit_heartbeat_errors", "transmit_window_errors",
	} {
		assert.NotContains(t, fields, gone)
	}
}

func TestLegacySumsReceiveDropContributors(t *testing.T) {
	fields := map[string]uint64{"receive_dropped": 10, "receive_missed_errors": 5}
	applyLegacyNames(fields)

	assert.Equal(t, uint64(15), fields["receive_drop"])
	assert.NotContains(t, fields, "receive_dropped")
	assert.NotContains(t, fields, "receive_missed_errors")
}

func TestLegacyPlainRenames(t *testing.T) {
	fields := map[string]uint64{
		"receive_errors":       1,
		"receive_fifo_errors":  2,
		"multicast":            3,
		"transmit_errors":      4,
		"transmit_dropped":     5,
		"transmit_fifo_errors": 6,
		"collisions":           7,
	}
	applyLegacyNames(fields)

	assert.Equal(t, uint64(1), fields["receive_errs"])
	assert.Equal(t, uint64(2), fields["receive_fifo"])
	assert.Equal(t, uint64(3), fields["receive_multicast"])
	assert.Equal(t, uint64(4), fields["transmit_errs"])
	assert.Equal(t, uint64(5), fields["transmit_drop"])
	assert.Equal(t, uint64(6), fields["transmit_fifo"])
	assert.Equal(t, uint64(7), fields["transmit_colls"])
}

func TestLegacyMissingSourceFieldEmitsNothing(t *testing.T) {
	// A kernel that does not report a field must not produce a zero-valued metric:
	// "zero errors" and "unknown errors" are different claims, and upstream makes
	// the same distinction by gating on the pop() succeeding.
	fields := map[string]uint64{"receive_bytes": 100}
	applyLegacyNames(fields)

	assert.NotContains(t, fields, "receive_errs")
	assert.NotContains(t, fields, "receive_frame")
	assert.Equal(t, uint64(100), fields["receive_bytes"], "untouched fields must pass through")
}

func TestLegacyContributorWithoutSourceIsStillRemoved(t *testing.T) {
	// If only a contributor is present but not the source, upstream leaves the
	// contributor in place because the rule never fires. Reproduced: the contributor
	// surfaces as its own metric, which is upstream's behaviour rather than ours.
	fields := map[string]uint64{"receive_length_errors": 7}
	applyLegacyNames(fields)

	assert.Equal(t, uint64(7), fields["receive_length_errors"],
		"with no receive_frame_errors the rule does not fire, matching upstream")
	assert.NotContains(t, fields, "receive_frame")
}

func TestLegacyIsIdempotent(t *testing.T) {
	// Applying twice must not double-count, in case a future refactor calls it more
	// than once per scrape.
	fields := map[string]uint64{"receive_dropped": 10, "receive_missed_errors": 5}
	applyLegacyNames(fields)
	applyLegacyNames(fields)
	assert.Equal(t, uint64(15), fields["receive_drop"])
}

// --- emitted metric set ---------------------------------------------------

func TestNetDevEmitsTotalSuffixedCounters(t *testing.T) {
	c := newTestNetDevCollector(t)

	ch := make(chan prometheus.Metric, 1024)
	require.NoError(t, c.Update(ch))
	close(ch)

	names := map[string]bool{}
	for m := range ch {
		names[m.Desc().String()] = true
	}
	require.NotEmpty(t, names, "the host must have at least one network device")

	joined := strings.Join(keysOf(names), " ")
	// The "_total" suffix is appended by desc(), not stored in the field names.
	for _, want := range []string{
		"node_network_receive_bytes_total", "node_network_transmit_bytes_total",
		"node_network_receive_packets_total", "node_network_transmit_packets_total",
	} {
		assert.Contains(t, joined, want)
	}
	// Pre-legacy names must never appear. Matched with the full metric prefix,
	// because a bare substring like "multicast_total" also matches the CORRECT
	// name "node_network_receive_multicast_total" -- an earlier version of this
	// assertion failed for exactly that reason.
	for _, unwanted := range []string{
		"node_network_receive_errors_total",
		"node_network_transmit_errors_total",
		"node_network_multicast_total",
		"node_network_collisions_total",
		"node_network_receive_dropped_total",
		"node_network_receive_fifo_errors_total",
		"node_network_transmit_carrier_errors_total",
	} {
		assert.NotContains(t, joined, unwanted,
			"%s is a pre-legacy field name and must have been renamed", unwanted)
	}
}

func TestNetDevDescriptorsAreCachedAndStable(t *testing.T) {
	c := newTestNetDevCollector(t)

	first := c.desc("receive_bytes")
	second := c.desc("receive_bytes")
	assert.Same(t, first, second, "descriptors must be cached, not rebuilt per scrape")
	assert.Contains(t, first.String(), "node_network_receive_bytes_total")
}

func TestNetDevConcurrentUpdatesAreSafe(t *testing.T) {
	// The descriptor cache is shared mutable state and Collect runs collectors
	// concurrently. Run under -race.
	c := newTestNetDevCollector(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := make(chan prometheus.Metric, 1024)
			assert.NoError(t, c.Update(ch))
		}()
	}
	wg.Wait()
}

func TestNetDevFieldsCoversEveryProcfsCounter(t *testing.T) {
	// Every field procfs exposes on a NetDevLine should be mapped, or we silently
	// drop a counter upstream reports.
	fields := netDevFields(&procfs.NetDevLine{})
	assert.Len(t, fields, 16, "procfs NetDevLine has 16 counters; all must be mapped")
}

func TestNetDevMissingProcfs(t *testing.T) {
	c, err := newNetDevCollector(quietLogger(), Paths{ProcFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get netdev stats")
}

func TestNetDevConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newNetDevCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

// --- helpers --------------------------------------------------------------

func newTestNetDevCollector(t *testing.T) *netDevCollector {
	t.Helper()
	c, err := newNetDevCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	return c.(*netDevCollector)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
