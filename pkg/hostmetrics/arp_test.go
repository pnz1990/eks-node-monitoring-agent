package hostmetrics

// Tests for the arp collector.
//
// One metric, two backends, and the thing worth testing is that they AGREE. The
// NUD_NOARP filter is what makes them agree: netlink returns those entries and
// /proc/net/arp does not. Measured on this host with a standalone rtnetlink
// program: 3 IPv4 neighbours, 1 of them NUD_NOARP -- so without the filter netlink
// reports 3 where procfs reports 2, a 50% overcount for identical kernel state.

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/jsimonetti/rtnetlink/v2/rtnl"
	"github.com/mdlayher/netlink"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// --- the NUD_NOARP filter -------------------------------------------------

func TestARPNetlinkExcludesNUDNOARP(t *testing.T) {
	// Against the REAL arpEntriesFromNeighbours, not a mirrored copy of its logic.
	// An earlier version of this test reimplemented the filter inline and compared
	// it to itself, which would have passed against a broken collector -- the same
	// "asserting against a loosely-specified artifact" family as four prior test
	// bugs on this branch.
	//
	// NUD_NOARP entries are permanent ones needing no ARP resolution (multicast,
	// point-to-point) and /proc/net/arp omits them. Measured live on this host: 3
	// IPv4 neighbours, 1 NUD_NOARP -- so without the filter netlink reports 3 where
	// procfs reports 2, a 50% overcount for identical kernel state.
	got := arpEntriesFromNeighbours([]*rtnl.Neigh{
		{Interface: &net.Interface{Name: "eth0"}, State: unix.NUD_REACHABLE},
		{Interface: &net.Interface{Name: "eth0"}, State: unix.NUD_REACHABLE},
		{Interface: &net.Interface{Name: "eth0"}, State: unix.NUD_NOARP},
		{Interface: &net.Interface{Name: "br0"}, State: unix.NUD_NOARP},
	})

	assert.Equal(t, uint32(2), got["eth0"], "only the two reachable entries count")
	assert.NotContains(t, got, "br0",
		"a device whose only neighbour is NUD_NOARP must not appear at all")
	assert.Len(t, got, 1)
}

func TestARPNetlinkCountsEveryNonNOARPState(t *testing.T) {
	// Only NUD_NOARP is excluded. A filter that also dropped, say, NUD_STALE or
	// NUD_PERMANENT would undercount silently -- the metric would still exist with a
	// plausible value.
	for _, state := range []uint16{
		unix.NUD_INCOMPLETE, unix.NUD_REACHABLE, unix.NUD_STALE,
		unix.NUD_DELAY, unix.NUD_PROBE, unix.NUD_FAILED, unix.NUD_PERMANENT,
	} {
		got := arpEntriesFromNeighbours([]*rtnl.Neigh{
			{Interface: &net.Interface{Name: "eth0"}, State: state},
		})
		assert.Equal(t, uint32(1), got["eth0"], "state 0x%02x must be counted", state)
	}

	got := arpEntriesFromNeighbours([]*rtnl.Neigh{
		{Interface: &net.Interface{Name: "eth0"}, State: unix.NUD_NOARP},
	})
	assert.Empty(t, got, "NUD_NOARP is the only excluded state")
}

func TestARPNetlinkSkipsUnresolvedInterface(t *testing.T) {
	// rtnl drops entries whose link it could not resolve, so this should not occur
	// in practice -- but Interface is a pointer and a nil deref would panic the
	// whole collector. The guard is asserted rather than assumed.
	got := arpEntriesFromNeighbours([]*rtnl.Neigh{
		{Interface: nil, State: unix.NUD_REACHABLE},
		{Interface: &net.Interface{Name: "eth0"}, State: unix.NUD_REACHABLE},
	})
	assert.Equal(t, map[string]uint32{"eth0": 1}, got,
		"a neighbour with no resolved interface is skipped, not counted under an empty name")
}

func TestARPNetlinkEmptyNeighbourListIsEmptyMap(t *testing.T) {
	got := arpEntriesFromNeighbours(nil)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestARPNetlinkQueryFailureIsPropagated(t *testing.T) {
	// Both error returns in the socket wrapper -- dial failure and neighbour-query
	// failure -- surface through the same seam. Without this the branches are only
	// reachable on a host where netlink is broken.
	_, err := arpEntriesViaNetlinkWith(func() ([]*rtnl.Neigh, func(), error) {
		return nil, nil, assert.AnError
	})
	require.ErrorIs(t, err, assert.AnError)
}

func TestARPNetlinkConnectionIsClosed(t *testing.T) {
	// The close func must run even though the happy path returns normally, or the
	// collector leaks a netlink socket once per scrape -- which on a 15s scrape
	// interval exhausts the fd limit in hours.
	closed := false
	got, err := arpEntriesViaNetlinkWith(func() ([]*rtnl.Neigh, func(), error) {
		return []*rtnl.Neigh{
			{Interface: &net.Interface{Name: "eth0"}, State: unix.NUD_REACHABLE},
		}, func() { closed = true }, nil
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]uint32{"eth0": 1}, got)
	assert.True(t, closed, "the netlink connection must be closed")
}

func TestARPNetlinkDialFailureIsPropagated(t *testing.T) {
	// The dial error path, reachable without breaking netlink on the host.
	_, err := arpEntriesViaNetlinkWith(netlinkNeighbourQuery(
		func(*netlink.Config) (*rtnl.Conn, error) { return nil, assert.AnError }))
	require.ErrorIs(t, err, assert.AnError)
}

// fakeARPConn stands in for *rtnl.Conn so the query error path is reachable.
type fakeARPConn struct {
	neighbours []*rtnl.Neigh
	queryErr   error
	closed     bool
}

func (f *fakeARPConn) Neighbours(_ *net.Interface, family int) ([]*rtnl.Neigh, error) {
	// The family must be AF_INET: an ARP collector that also counted IPv6
	// neighbours would double-count dual-stack interfaces.
	if family != unix.AF_INET {
		return nil, assert.AnError
	}
	return f.neighbours, f.queryErr
}

func (f *fakeARPConn) Close() error {
	f.closed = true
	return nil
}

func TestARPNetlinkQueryFailureClosesTheConnection(t *testing.T) {
	// THE LEAK TEST. If the connection is not closed on the query error path,
	// nothing else closes it -- the success path relies on the returned func -- so
	// the collector leaks one netlink socket per scrape. At a 15s interval that
	// exhausts the fd limit within hours, and it would present as an unrelated
	// failure long after the cause.
	conn := &fakeARPConn{queryErr: assert.AnError}

	_, closeFn, err := queryNeighbours(conn)
	require.ErrorIs(t, err, assert.AnError)
	assert.Nil(t, closeFn, "no closer is returned on the error path")
	assert.True(t, conn.closed, "the connection must be closed before returning the error")
}

func TestARPNetlinkQuerySucceedsAndDefersClose(t *testing.T) {
	conn := &fakeARPConn{neighbours: []*rtnl.Neigh{
		{Interface: &net.Interface{Name: "eth0"}, State: unix.NUD_REACHABLE},
	}}

	neighbours, closeFn, err := queryNeighbours(conn)
	require.NoError(t, err)
	require.Len(t, neighbours, 1)
	require.NotNil(t, closeFn)
	assert.False(t, conn.closed, "the caller owns closing on the success path")

	closeFn()
	assert.True(t, conn.closed)
}

func TestARPNetlinkQueryAsksForIPv4Only(t *testing.T) {
	// The fake returns an error for any family other than AF_INET, so a change to
	// AF_UNSPEC or AF_INET6 fails here rather than silently doubling the counts on a
	// dual-stack node.
	conn := &fakeARPConn{neighbours: []*rtnl.Neigh{
		{Interface: &net.Interface{Name: "eth0"}, State: unix.NUD_REACHABLE},
	}}
	_, _, err := queryNeighbours(conn)
	assert.NoError(t, err, "the query must use AF_INET")
}

func TestARPNetlinkNilCloseFuncIsTolerated(t *testing.T) {
	// A query that returns no close func must not nil-panic in the defer.
	got, err := arpEntriesViaNetlinkWith(func() ([]*rtnl.Neigh, func(), error) {
		return nil, nil, nil
	})
	require.NoError(t, err)
	assert.Empty(t, got)
}

// --- backend selection ----------------------------------------------------

func TestARPDefaultsToNetlink(t *testing.T) {
	// Upstream defaults --collector.arp.netlink to TRUE. Flipping this would change
	// which backend ships, and the two can disagree if the NUD_NOARP filter ever
	// regresses.
	c := newARPFixtureCollector(t, "", "")
	assert.True(t, c.useNetlink, "upstream's default is netlink=true")
}

func TestARPDefaultMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/arp_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	m := regexp.MustCompile(`arpNetlink\s*=[^\n]*\.Default\("([^"]*)"\)`).FindSubmatch(data)
	require.NotNil(t, m, "failed to extract upstream's netlink default; the regexp may be stale")
	assert.Equal(t, "true", string(m[1]), "upstream's default must be true, matching ours")
}

func TestARPNetlinkBackendIsUsedWhenEnabled(t *testing.T) {
	c := newARPFixtureCollector(t, "", "")
	c.entriesViaNetlink = func() (map[string]uint32, error) {
		return map[string]uint32{"eth0": 4, "eni123": 2}, nil
	}

	got := gatherARP(t, c)
	assert.Equal(t, map[string]float64{"eth0": 4, "eni123": 2}, got)
}

func TestARPProcfsBackendIsUsedWhenNetlinkDisabled(t *testing.T) {
	c := newARPFixtureCollector(t, "", "")
	c.useNetlink = false
	c.entriesViaNetlink = func() (map[string]uint32, error) {
		t.Fatal("netlink must not be called when disabled")
		return nil, nil
	}

	got := gatherARP(t, c)
	// The fixture has entries for two devices; assert the aggregation rather than
	// exact counts, since the fixture is upstream's.
	require.NotEmpty(t, got, "the procfs fixture must yield entries")
	for device, count := range got {
		assert.Positive(t, count, "device %s must have a positive count", device)
	}
}

func TestARPNetlinkFailureIsReported(t *testing.T) {
	c := newARPFixtureCollector(t, "", "")
	c.entriesViaNetlink = func() (map[string]uint32, error) { return nil, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not get ARP entries")
}

func TestARPProcfsFailureIsReported(t *testing.T) {
	c, err := newARPCollectorWithFilter(quietLogger(), Paths{ProcFS: t.TempDir()}.withDefaults(), "", "")
	require.NoError(t, err)
	ac := c.(*arpCollector)
	ac.useNetlink = false

	err = ac.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not get ARP entries")
}

// --- per-device aggregation -----------------------------------------------

func TestARPEntriesByDeviceCountsPerDevice(t *testing.T) {
	got := arpEntriesByDevice([]procfs.ARPEntry{
		{Device: "eth0"}, {Device: "eth0"}, {Device: "eth0"},
		{Device: "eni51bec58f2f1"},
	})
	assert.Equal(t, map[string]uint32{"eth0": 3, "eni51bec58f2f1": 1}, got)
}

func TestARPEntriesByDeviceOnEmptyInput(t *testing.T) {
	// No ARP entries is a valid state (a freshly booted node), and must yield an
	// empty map rather than a nil that a caller might range over differently.
	got := arpEntriesByDevice(nil)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestARPLongCNIDeviceNamesAreNotTruncated(t *testing.T) {
	// I initially assumed /proc/net/arp could truncate long CNI interface names and
	// merge two devices into one label. It cannot: the kernel caps interface names
	// at IFNAMSIZ (15 usable characters) so a longer name cannot exist, and procfs
	// splits on whitespace rather than fixed columns. Asserted rather than left as
	// a comment, since the claim is what justifies either backend being safe.
	got := arpEntriesByDevice([]procfs.ARPEntry{
		{Device: "eni51bec58f2f1"},  // 14 chars, the longest in the live corpus
		{Device: "enia08cb446736"},  // also 14, differs only in the middle
		{Device: "vethc8a9f68abcd"}, // 15 chars, the kernel maximum
	})
	assert.Len(t, got, 3, "three distinct devices must remain three distinct labels")
	for _, device := range []string{"eni51bec58f2f1", "enia08cb446736", "vethc8a9f68abcd"} {
		assert.Equal(t, uint32(1), got[device], "%s must keep its own count", device)
	}
}

// --- device filter --------------------------------------------------------

func TestARPDeviceFilterDefaultsToEverything(t *testing.T) {
	// Upstream's include and exclude both default empty, so no device is filtered.
	c := newARPFixtureCollector(t, "", "")
	for _, device := range []string{"eth0", "ens5", "eni51bec58f2f1", "lo", "docker0"} {
		assert.False(t, c.deviceFilter.ignored(device), "%q must not be filtered by default", device)
	}
}

func TestARPExcludeFilterDropsMatches(t *testing.T) {
	c := newARPFixtureCollector(t, "^eni", "")
	c.entriesViaNetlink = func() (map[string]uint32, error) {
		return map[string]uint32{"ens5": 1, "eni51bec58f2f1": 2, "eniabc": 3}, nil
	}

	got := gatherARP(t, c)
	assert.Equal(t, map[string]float64{"ens5": 1}, got)
}

func TestARPIncludeFilterIsExclusive(t *testing.T) {
	c := newARPFixtureCollector(t, "", "^ens")
	c.entriesViaNetlink = func() (map[string]uint32, error) {
		return map[string]uint32{"ens5": 1, "eni51bec58f2f1": 2}, nil
	}

	got := gatherARP(t, c)
	assert.Equal(t, map[string]float64{"ens5": 1}, got)
}

func TestARPExcludeAndIncludeAreMutuallyExclusive(t *testing.T) {
	_, err := newARPCollectorWithFilter(quietLogger(), Paths{}.withDefaults(), "^eni", "^ens")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestARPInvalidFilterPatternsRejectedAtConstruction(t *testing.T) {
	_, err := newARPCollectorWithFilter(quietLogger(), Paths{}.withDefaults(), "([unclosed", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid device exclude pattern")

	_, err = newARPCollectorWithFilter(quietLogger(), Paths{}.withDefaults(), "", "([unclosed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid device include pattern")
}

// --- metric shape ---------------------------------------------------------

func TestARPEntriesIsAGauge(t *testing.T) {
	// An entry count is a current value, not cumulative: entries expire.
	c := newARPFixtureCollector(t, "", "")
	c.entriesViaNetlink = func() (map[string]uint32, error) {
		return map[string]uint32{"eth0": 1}, nil
	}

	ch := make(chan prometheus.Metric, 16)
	require.NoError(t, c.Update(ch))
	close(ch)

	n := 0
	for m := range ch {
		assert.Equal(t, "node_arp_entries", metricName(t, m))
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.NotNil(t, pb.Gauge, "arp_entries must be a gauge; entries expire")
		n++
	}
	assert.Equal(t, 1, n)
}

func TestARPConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newARPCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

func TestARPRegisteredConstructorWiresNetlink(t *testing.T) {
	c, err := newARPCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	ac := c.(*arpCollector)
	require.NotNil(t, ac.entriesViaNetlink, "the real netlink reader must be wired")
	assert.True(t, ac.useNetlink)
}

func TestARPRealNetlinkBackendWorksOnThisHost(t *testing.T) {
	// Exercises the actual rtnetlink implementation rather than the seam. Verified
	// to work in this environment (3 IPv4 neighbours), but skipped rather than
	// failed if the socket is unavailable, since a sandbox without CAP_NET_ADMIN is
	// a legitimate test environment.
	entries, err := arpEntriesViaNetlink()
	if err != nil {
		t.Skipf("netlink unavailable in this environment: %v", err)
	}
	for device, count := range entries {
		assert.NotEmpty(t, device, "every entry must be attributed to a named device")
		assert.Positive(t, count)
	}
}

// --- helpers --------------------------------------------------------------

func newARPFixtureCollector(t *testing.T, excludeExpr, includeExpr string) *arpCollector {
	t.Helper()

	root := t.TempDir()
	netDir := filepath.Join(root, "net")
	require.NoError(t, os.MkdirAll(netDir, 0o755))
	data, err := os.ReadFile("testdata/proc/net/arp")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(netDir, "arp"), data, 0o644))

	c, err := newARPCollectorWithFilter(quietLogger(),
		Paths{ProcFS: root}.withDefaults(), excludeExpr, includeExpr)
	require.NoError(t, err)
	return c.(*arpCollector)
}

// gatherARP returns device -> entry count.
func gatherARP(t *testing.T, c *arpCollector) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)

	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		for _, l := range pb.GetLabel() {
			if l.GetName() == "device" {
				out[l.GetValue()] = pb.GetGauge().GetValue()
			}
		}
	}
	return out
}
