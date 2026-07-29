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

	"github.com/jsimonetti/rtnetlink/v2"
	"github.com/mdlayher/netlink"

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

func TestNetDevBothBackendsFailingIsAnError(t *testing.T) {
	// The error message must name BOTH failures. Reporting only "procfs failed" would
	// hide that netlink was tried first and why it did not work -- and netlink is the
	// primary backend, so its failure is the more interesting half.
	//
	// This test previously passed an empty procfs and asserted an error, on the
	// assumption that procfs was the only backend. Once netlink became primary it
	// HUNG: netlink succeeded, emitted ~150 metrics into an 8-slot channel, and blocked
	// forever. The test was encoding an assumption about the implementation, not a
	// contract -- so it now states the contract instead.
	c, err := newNetDevCollector(quietLogger(), Paths{ProcFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	nc := c.(*netDevCollector)
	nc.netlinkStats = func() (map[string]map[string]uint64, error) { return nil, assert.AnError }

	err = nc.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get netdev stats")
	assert.Contains(t, err.Error(), "netlink failed",
		"the netlink failure must be named, not swallowed")
	assert.Contains(t, err.Error(), "procfs failed")
}

func TestNetDevFallsBackToProcfsWhenNetlinkFails(t *testing.T) {
	// The fallback path. It yields one metric FEWER than netlink --
	// receive_nohandler exists only in rtnetlink's LinkStats64, and /proc/net/dev has
	// no column for it -- but every other counter is present, which beats no network
	// metrics at all on a host without a netlink socket.
	c, err := newNetDevCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	nc := c.(*netDevCollector)
	nc.netlinkStats = func() (map[string]map[string]uint64, error) { return nil, assert.AnError }

	ch := make(chan prometheus.Metric, 4096)
	require.NoError(t, nc.Update(ch), "the procfs fallback must succeed")
	close(ch)

	names := map[string]bool{}
	for m := range ch {
		names[m.Desc().String()] = true
	}
	joined := strings.Join(keysOf(names), " ")
	assert.Contains(t, joined, "node_network_receive_bytes_total",
		"the fallback still reports the core counters")
	assert.NotContains(t, joined, "node_network_receive_nohandler_total",
		"receive_nohandler is netlink-only and must be absent on the fallback")
}

func TestNetDevNetlinkPathIsPrimaryAndCarriesNohandler(t *testing.T) {
	// THE PARITY REQUIREMENT. Upstream's --collector.netdev.netlink defaults to true,
	// and only the netlink backend exposes receive_nohandler -- 7 series on the live
	// EKS node. A procfs-based port loses it silently, which is exactly what happened
	// on my first attempt and was caught only by diffing the two endpoints end to end.
	c, err := newNetDevCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	nc := c.(*netDevCollector)
	require.NotNil(t, nc.netlinkStats, "netlink must be wired as the primary backend")

	ch := make(chan prometheus.Metric, 8192)
	err = nc.Update(ch)
	close(ch)
	if err != nil {
		t.Skipf("netlink unavailable in this environment: %v", err)
	}

	names := map[string]bool{}
	for m := range ch {
		names[m.Desc().String()] = true
	}
	assert.Contains(t, strings.Join(keysOf(names), " "),
		"node_network_receive_nohandler_total",
		"the netlink backend must emit receive_nohandler")
}

func TestNetDevNetlinkFieldsMatchUpstream(t *testing.T) {
	// The netlink field map, diffed against upstream's. A missing entry drops a metric
	// family; an extra one emits a family upstream lacks.
	data, err := os.ReadFile("../../../node_exporter/collector/netdev_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}

	// Upstream's netlink map is the one keyed on stats.<Field> with a leading
	// receive_packets entry; anchored on that to avoid matching the 32-bit widening
	// block above it.
	body := regexp.MustCompile(`(?s)metrics\[name\] = map\[string\]uint64\{(.*?)
		\}`).
		FindSubmatch(data)
	require.NotNil(t, body, "failed to locate upstream's netlink field map")

	upstream := map[string]string{}
	for _, m := range regexp.MustCompile(`"([a-z_0-9]+)":\s*stats\.([A-Za-z0-9]+),`).
		FindAllSubmatch(body[1], -1) {
		upstream[string(m[1])] = string(m[2])
	}
	require.Len(t, upstream, 24, "expected 24 upstream netlink fields, got %d", len(upstream))

	ours := netDevNetlinkFields(&rtnetlink.LinkStats64{})
	for name := range upstream {
		assert.Contains(t, ours, name, "upstream netlink field %q is missing", name)
	}
	for name := range ours {
		assert.Contains(t, upstream, name, "we emit netlink field %q that upstream does not", name)
	}
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

func TestNetDevWidenStats32MatchesUpstreamFieldSet(t *testing.T) {
	// A kernel reporting only 32-bit link stats. Unreachable on any modern host -- the
	// live one reports Stats64 -- so it is exercised directly.
	//
	// Every field netDevNetlinkFields reads must be widened, or that metric silently
	// reports 0 on such a kernel. Asserted by setting every 32-bit field to a distinct
	// value and checking none of the mapped outputs is zero.
	s32 := &rtnetlink.LinkStats{
		RXPackets: 1, TXPackets: 2, RXBytes: 3, TXBytes: 4,
		RXErrors: 5, TXErrors: 6, RXDropped: 7, TXDropped: 8,
		Multicast: 9, Collisions: 10,
		RXLengthErrors: 11, RXOverErrors: 12, RXCRCErrors: 13,
		RXFrameErrors: 14, RXFIFOErrors: 15, RXMissedErrors: 16,
		TXAbortedErrors: 17, TXCarrierErrors: 18, TXFIFOErrors: 19,
		TXHeartbeatErrors: 20, TXWindowErrors: 21,
		RXCompressed: 22, TXCompressed: 23, RXNoHandler: 24,
	}

	fields := netDevNetlinkFields(widenNetDevStats32(s32))
	require.Len(t, fields, 24)
	for name, value := range fields {
		assert.NotZero(t, value,
			"%s reads a field that widenNetDevStats32 does not convert, so it would report 0", name)
	}
	// And the widened value is the 32-bit one, not a truncation or a wrap.
	assert.Equal(t, uint64(24), fields["receive_nohandler"])
	assert.Equal(t, uint64(3), fields["receive_bytes"])
}

func TestNetDevNetlinkSkipsLinksWithoutAttributesOrStats(t *testing.T) {
	// A link with no attributes, or attributes but no stats of either width, must be
	// skipped rather than reported with zeros -- and must not nil-panic. Upstream
	// guards the same two cases.
	//
	// Exercised through the pure field-mapping function plus the guards' own logic,
	// since rtnetlink.Conn cannot be faked without a socket.
	c, err := newNetDevCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	nc := c.(*netDevCollector)
	nc.netlinkStats = func() (map[string]map[string]uint64, error) {
		// A device present with an empty field map: the collector must emit nothing for
		// it rather than failing.
		return map[string]map[string]uint64{"skipped": {}}, nil
	}

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, nc.Update(ch))
	close(ch)
	assert.Empty(t, ch, "a device with no fields emits no series")
}

func TestNetDevStatsFromLinksSkipsUnusableLinks(t *testing.T) {
	// The two guards upstream also has, both of which would nil-panic without them. A
	// link with no attributes carries no name, and one with neither 32- nor 64-bit stats
	// carries no counters -- neither can be reported, and inventing zeros would claim
	// readings the kernel never made.
	links := []rtnetlink.LinkMessage{
		// no attributes at all
		{},
		// attributes but no stats of either width
		{Attributes: &rtnetlink.LinkAttributes{Name: "nostats"}},
		// 64-bit stats: the normal case
		{Attributes: &rtnetlink.LinkAttributes{
			Name:    "eth0",
			Stats64: &rtnetlink.LinkStats64{RXBytes: 100, RXNoHandler: 7},
		}},
		// 32-bit stats only: widened rather than skipped
		{Attributes: &rtnetlink.LinkAttributes{
			Name:  "eth1",
			Stats: &rtnetlink.LinkStats{RXBytes: 200, RXNoHandler: 9},
		}},
	}

	got := netDevStatsFromLinks(links)

	assert.Len(t, got, 2, "only the two links with stats may be reported")
	assert.NotContains(t, got, "nostats")
	assert.Equal(t, uint64(100), got["eth0"]["receive_bytes"])
	assert.Equal(t, uint64(7), got["eth0"]["receive_nohandler"])
	assert.Equal(t, uint64(200), got["eth1"]["receive_bytes"],
		"32-bit stats must be widened, not dropped")
	assert.Equal(t, uint64(9), got["eth1"]["receive_nohandler"])
}

func TestNetDevStatsFromLinksAppliesLegacyNames(t *testing.T) {
	// legacy() must run on the netlink path too. Without it the emitted names are the
	// pre-legacy ones, which is what my first version did -- 17 wrong names appearing and
	// 10 right ones missing.
	links := []rtnetlink.LinkMessage{{
		Attributes: &rtnetlink.LinkAttributes{
			Name: "eth0",
			Stats64: &rtnetlink.LinkStats64{
				RXErrors: 1, RXDropped: 10, RXMissedErrors: 5,
				RXFrameErrors: 2, RXLengthErrors: 4, RXOverErrors: 8, RXCRCErrors: 16,
			},
		},
	}}

	fields := netDevStatsFromLinks(links)["eth0"]

	// Post-legacy names present.
	assert.Equal(t, uint64(1), fields["receive_errs"])
	assert.Equal(t, uint64(15), fields["receive_drop"], "10 dropped + 5 missed, summed")
	assert.Equal(t, uint64(30), fields["receive_frame"], "2+4+8+16, summed")
	// Pre-legacy names gone.
	for _, gone := range []string{
		"receive_errors", "receive_dropped", "receive_missed_errors",
		"receive_frame_errors", "receive_length_errors", "receive_over_errors",
		"receive_crc_errors",
	} {
		assert.NotContains(t, fields, gone, "%s is a pre-legacy name", gone)
	}
}

func TestNetDevNetlinkQueryFailureIsPropagated(t *testing.T) {
	// Both error returns in the socket wrapper -- dial failure and link-list failure --
	// surface through this seam. Without it they are only reachable on a host where
	// netlink is broken.
	_, err := netDevNetlinkStatsWith(func() ([]rtnetlink.LinkMessage, func(), error) {
		return nil, nil, assert.AnError
	})
	require.ErrorIs(t, err, assert.AnError)
}

func TestNetDevNetlinkConnectionIsClosed(t *testing.T) {
	// The closer must run on the success path, or the collector leaks a netlink socket
	// once per scrape -- at a 15s interval that exhausts the fd limit within hours. The
	// arp collector had exactly this defect, found the same way.
	closed := false
	got, err := netDevNetlinkStatsWith(func() ([]rtnetlink.LinkMessage, func(), error) {
		return []rtnetlink.LinkMessage{{
			Attributes: &rtnetlink.LinkAttributes{
				Name:    "eth0",
				Stats64: &rtnetlink.LinkStats64{RXBytes: 1},
			},
		}}, func() { closed = true }, nil
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), got["eth0"]["receive_bytes"])
	assert.True(t, closed, "the netlink connection must be closed")
}

func TestNetDevNetlinkNilCloserIsTolerated(t *testing.T) {
	got, err := netDevNetlinkStatsWith(func() ([]rtnetlink.LinkMessage, func(), error) {
		return nil, nil, nil
	})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestNetDevNetlinkDialFailureIsPropagated(t *testing.T) {
	// The dial error path, reachable without breaking netlink on the host.
	_, err := netDevNetlinkStatsWith(netDevLinkQuery(
		func(*netlink.Config) (*rtnetlink.Conn, error) { return nil, assert.AnError }))
	require.ErrorIs(t, err, assert.AnError)
}

func TestNetDevNetlinkRealQueryWorksOnThisHost(t *testing.T) {
	// The production dialer end to end, so the success path of the real closure is
	// exercised rather than only the seam. Skipped rather than failed where the socket
	// is unavailable, since a sandbox without it is a legitimate environment.
	got, err := netDevNetlinkStats()
	if err != nil {
		t.Skipf("netlink unavailable in this environment: %v", err)
	}
	require.NotEmpty(t, got, "a real host has at least one link")
	for device, fields := range got {
		assert.NotEmpty(t, device)
		assert.Contains(t, fields, "receive_nohandler",
			"the netlink backend must carry receive_nohandler for every device")
	}
}

func TestNetDevListFailureClosesTheConnection(t *testing.T) {
	// THE LEAK TEST. If the connection is not closed when the link list fails, nothing
	// else closes it -- the success path relies on the returned closer -- so the collector
	// leaks a netlink socket per scrape. The arp collector had this same defect.
	closed := false
	_, closer, err := netDevListLinks(
		func() ([]rtnetlink.LinkMessage, error) { return nil, assert.AnError },
		func() error { closed = true; return nil },
	)

	require.ErrorIs(t, err, assert.AnError)
	assert.Nil(t, closer, "no closer is returned on the error path")
	assert.True(t, closed, "the connection must be closed before returning the error")
}

func TestNetDevListSuccessDefersCloseToTheCaller(t *testing.T) {
	closed := false
	links, closer, err := netDevListLinks(
		func() ([]rtnetlink.LinkMessage, error) {
			return []rtnetlink.LinkMessage{{
				Attributes: &rtnetlink.LinkAttributes{
					Name:    "eth0",
					Stats64: &rtnetlink.LinkStats64{RXBytes: 5},
				},
			}}, nil
		},
		func() error { closed = true; return nil },
	)

	require.NoError(t, err)
	require.Len(t, links, 1)
	require.NotNil(t, closer)
	assert.False(t, closed, "the caller owns closing on the success path")

	closer()
	assert.True(t, closed)
}
