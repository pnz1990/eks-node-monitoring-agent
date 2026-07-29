package hostmetrics

// Tests for the netstat collector.
//
// Two focal points:
//
//   - the empty-line panic. TestParseNetStatsRejectsEmptyLines is the regression
//     test for the fourth upstream crash found on this branch. It is a robustness
//     fix rather than a field bug -- a healthy kernel never emits a blank line in
//     these files -- and the test comment says so, so nobody later mistakes it for
//     an observed outage.
//   - the field filter. It is a curated allowlist that keeps this collector to ~60
//     series instead of the ~600 the raw files contain, so both halves are
//     asserted: the fields it must keep AND the volume it must drop.

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

// --- the empty-line panic (upstream crash) --------------------------------

// TestParseNetStatsRejectsEmptyLines is the regression test for the crash.
//
// Upstream computes the protocol as nameParts[0][:len(nameParts[0])-1] to strip
// the trailing ":". On an empty line that is [:-1] and panics with "slice bounds
// out of range [:-1]". Verified against a verbatim copy of upstream's function:
// a trailing, leading, or interior blank line all panic, as does a blank-only file.
//
// SEVERITY, stated honestly: not reachable on a healthy kernel.
// /proc/net/{netstat,snmp} are generated in strict header/value pairs with no
// blank lines (live node: 6 and 12 lines, zero blank, both even). This is a
// robustness gap, not an observed outage. Fixed anyway because the cost is three
// lines and a panic would take the whole scrape down rather than degrade it.
func TestParseNetStatsRejectsEmptyLines(t *testing.T) {
	for name, input := range map[string]string{
		"trailing empty line":  "TcpExt: A B\nTcpExt: 1 2\n\n",
		"leading empty line":   "\nTcpExt: A B\nTcpExt: 1 2\n",
		"empty line in middle": "TcpExt: A B\nTcpExt: 1 2\n\nIp: C\nIp: 3\n",
		"blank-only file":      "\n",
		"whitespace-only line": "TcpExt: A B\nTcpExt: 1 2\n   \n",
	} {
		t.Run(name, func(t *testing.T) {
			// The assertion is that this does not panic. Upstream panics on every
			// one of these inputs.
			stats, err := parseNetStats(strings.NewReader(input), "test")
			require.NoError(t, err, "a blank line must be skipped, not fail")

			if strings.Contains(input, "TcpExt") {
				require.Contains(t, stats, "TcpExt", "real protocols must still parse")
				assert.Equal(t, "1", stats["TcpExt"]["A"])
				assert.Equal(t, "2", stats["TcpExt"]["B"])
			}
		})
	}
}

func TestParseNetStatsHeaderWithoutValueLine(t *testing.T) {
	// A header at EOF with no value line. Upstream reaches the field-count check
	// with valueParts derived from "" and reports a count mismatch, which is a
	// confusing way to say the file was truncated.
	_, err := parseNetStats(strings.NewReader("TcpExt: A B\n"), "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no matching value line")
}

func TestParseNetStatsMalformedHeaderIsRejected(t *testing.T) {
	// A non-empty header that does not end in ":". Without this check the last
	// character is truncated regardless, so "TcpExt" would silently become "TcpEx"
	// and every metric under it would carry a wrong protocol name -- which is
	// worse than an error, because it looks like data.
	_, err := parseNetStats(strings.NewReader("TcpExt A B\nTcpExt 1 2\n"), "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed header line")
}

func TestParseNetStatsFieldCountMismatch(t *testing.T) {
	_, err := parseNetStats(strings.NewReader("TcpExt: A B C\nTcpExt: 1 2\n"), "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "field count mismatch")
	// The message must name the counts, or diagnosing a real mismatch means
	// reading the file by hand.
	assert.Contains(t, err.Error(), "3 names, 2 values")
}

func TestParseNetStatsRepeatedProtocolAccumulates(t *testing.T) {
	// /proc/net/snmp lists Ip, Icmp, Tcp, Udp as separate pairs, but a protocol
	// could legitimately appear twice. Upstream reassigns the inner map, dropping
	// the first block's fields; accumulating keeps both.
	stats, err := parseNetStats(strings.NewReader(
		"Tcp: A B\nTcp: 1 2\nTcp: C\nTcp: 3\n"), "test")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "1", "B": "2", "C": "3"}, stats["Tcp"],
		"a repeated protocol block must not discard the earlier one")
}

// --- the real fixtures ----------------------------------------------------

func TestNetStatParsesUpstreamFixtures(t *testing.T) {
	stats, err := readNetStats("testdata/proc/net/netstat")
	require.NoError(t, err)
	require.Contains(t, stats, "TcpExt")
	require.Contains(t, stats, "IpExt")
	// Spot-check a value against the fixture rather than only the key set, so a
	// column misalignment fails.
	assert.Equal(t, "2", stats["TcpExt"]["SyncookiesFailed"])
	assert.Equal(t, "6286396970", stats["IpExt"]["InOctets"])

	snmp, err := readNetStats("testdata/proc/net/snmp")
	require.NoError(t, err)
	assert.Contains(t, snmp, "Tcp")
	assert.Contains(t, snmp, "Udp")
}

func TestSNMP6ParsingSplitsOnTheSix(t *testing.T) {
	// snmp6 has a different shape: "NameValue" per line, not paired lines. The
	// protocol is everything up to and including the "6", so "Ip6InReceives"
	// becomes protocol "Ip6", name "InReceives".
	stats, err := readSNMP6Stats("testdata/proc/net/snmp6")
	require.NoError(t, err)
	require.Contains(t, stats, "Ip6")
	assert.Equal(t, "7", stats["Ip6"]["InReceives"])
}

func TestSNMP6SkipsLinesWithoutASix(t *testing.T) {
	// A line with no "6" cannot be attributed to an IPv6 protocol, so it is
	// skipped rather than filed under a truncated name.
	stats, err := parseSNMP6Stats(strings.NewReader(
		"Ip6InReceives\t7\nNoSixHere\t9\nUdp6InDatagrams\t3\n"))
	require.NoError(t, err)

	assert.Equal(t, "7", stats["Ip6"]["InReceives"])
	assert.Equal(t, "3", stats["Udp6"]["InDatagrams"])
	for protocol := range stats {
		assert.Contains(t, protocol, "6", "every protocol key must contain the 6")
	}
	assert.Len(t, stats, 2)
}

func TestSNMP6SkipsShortLines(t *testing.T) {
	stats, err := parseSNMP6Stats(strings.NewReader("Ip6InReceives\nIp6OutOctets\t5\n\n"))
	require.NoError(t, err)
	assert.Equal(t, map[string]map[string]string{"Ip6": {"OutOctets": "5"}}, stats)
}

func TestSNMP6AbsentFileIsNotAnError(t *testing.T) {
	// Absent when IPv6 is disabled in the kernel. The v6 metrics are simply not
	// reported; treating it as an error would alert on a normal configuration.
	stats, err := readSNMP6Stats(filepath.Join(t.TempDir(), "no-such-snmp6"))
	require.NoError(t, err)
	assert.Nil(t, stats)
}

func TestSNMP6UnreadableFileIsAnError(t *testing.T) {
	// Distinct from absent: a file that exists but cannot be read means something
	// is wrong, and must not be silently reported as "IPv6 disabled".
	dir := t.TempDir()
	path := filepath.Join(dir, "snmp6")
	require.NoError(t, os.WriteFile(path, []byte("Ip6InReceives\t1\n"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	if os.Geteuid() == 0 {
		t.Skip("running as root, mode 000 is still readable")
	}
	_, err := readSNMP6Stats(path)
	require.Error(t, err)
	assert.False(t, os.IsNotExist(err), "an unreadable file must not be treated as absent")
}

// --- the field filter -----------------------------------------------------

func TestNetStatDefaultFieldsMatchUpstreamVerbatim(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/netstat_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	// Anchored on .Default("...") rather than on proximity to the variable name --
	// the same mistake cost a failing test on netclass.
	m := regexp.MustCompile(`netStatFields\s*=[^\n]*\.Default\("([^"]*)"\)`).FindSubmatch(data)
	require.NotNil(t, m, "failed to extract upstream's field default; the regexp may be stale")
	assert.Equal(t, string(m[1]), defNetStatFields,
		"our copy of upstream's field allowlist must be verbatim")
}

func TestNetStatFilterKeepsTheActionableFields(t *testing.T) {
	// The half that matters: these are what dashboards and alerts join on, so a
	// narrowed pattern silently breaks them.
	rx := regexp.MustCompile(defNetStatFields)
	for _, key := range []string{
		"Tcp_ActiveOpens", "Tcp_PassiveOpens", "Tcp_RetransSegs", "Tcp_CurrEstab",
		"Tcp_InSegs", "Tcp_OutSegs", "Tcp_OutRsts",
		"Udp_InDatagrams", "Udp_OutDatagrams", "Udp_NoPorts",
		"Udp_RcvbufErrors", "Udp_SndbufErrors", "Udp6_InDatagrams",
		"TcpExt_ListenOverflows", "TcpExt_ListenDrops", "TcpExt_SyncookiesSent",
		"TcpExt_TCPSynRetrans", "TcpExt_TCPTimeouts", "TcpExt_TCPOFOQueue",
		"TcpExt_TCPRcvQDrop",
		"Ip_Forwarding", "IpExt_InOctets", "IpExt_OutOctets",
		"Ip6_InOctets", "Ip6_OutOctets",
		"Icmp_InMsgs", "Icmp_OutMsgs", "Icmp6_InMsgs", "Icmp6_OutMsgs",
		"Udp_InErrors", "Tcp_InErrs", "Icmp_InErrors",
	} {
		assert.True(t, rx.MatchString(key), "%q must be kept", key)
	}
}

func TestNetStatFilterDropsTheBulk(t *testing.T) {
	// The other half. Without a working filter this collector emits several hundred
	// series instead of ~60, so a filter that is compiled but not applied must fail
	// a test rather than just inflate cardinality quietly.
	rx := regexp.MustCompile(defNetStatFields)
	for _, key := range []string{
		"Ip_DefaultTTL", "Ip_InReceives", "Ip_OutRequests", "Ip_FragOKs",
		"Tcp_MaxConn", "Tcp_RtoAlgorithm", "Tcp_RtoMin", "Tcp_EstabResets",
		"TcpExt_DelayedACKs", "TcpExt_TCPHystartTrainDetect", "TcpExt_PruneCalled",
		"Icmp_InDestUnreachs", "Icmp_OutTimeExcds",
		"IpExt_InMcastPkts", "IpExt_InBcastPkts",
	} {
		assert.False(t, rx.MatchString(key), "%q must be filtered out", key)
	}
}

func TestNetStatFilterIsAnchored(t *testing.T) {
	// The pattern is ^...$ anchored. Unanchored, a substring match would let
	// hundreds of unintended fields through, and the collector would still "work".
	rx := regexp.MustCompile(defNetStatFields)
	assert.True(t, rx.MatchString("Tcp_InSegs"))
	assert.False(t, rx.MatchString("XTcp_InSegs"), "the pattern must be anchored at the start")
	assert.False(t, rx.MatchString("Tcp_InSegsX"), "the pattern must be anchored at the end")
}

func TestNetStatInvalidFieldPatternRejectedAtConstruction(t *testing.T) {
	_, err := newNetStatCollectorWithFields(quietLogger(), Paths{}.withDefaults(), "([unclosed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid netstat field pattern")
}

// --- end to end -----------------------------------------------------------

func TestNetStatEmitsFilteredMetricsFromFixtures(t *testing.T) {
	c := newNetStatFixtureCollector(t, defNetStatFields)

	ch := make(chan prometheus.Metric, 4096)
	require.NoError(t, c.Update(ch))
	close(ch)

	names := map[string]bool{}
	for m := range ch {
		names[metricName(t, m)] = true
	}

	for _, want := range []string{
		"node_netstat_Tcp_ActiveOpens",
		"node_netstat_Tcp_CurrEstab",
		"node_netstat_Udp_InDatagrams",
		"node_netstat_TcpExt_ListenDrops",
		"node_netstat_IpExt_InOctets",
		"node_netstat_Ip6_InOctets",
	} {
		assert.True(t, names[want], "%s must be emitted", want)
	}
	for _, unwanted := range []string{
		"node_netstat_Ip_DefaultTTL",
		"node_netstat_TcpExt_DelayedACKs",
	} {
		assert.False(t, names[unwanted], "%s must be filtered out", unwanted)
	}

	// A rough volume assertion: the raw fixtures contain several hundred fields and
	// the filter must cut that to tens. A filter silently not applied would blow
	// past this.
	assert.Less(t, len(names), 120,
		"the field filter must keep the series count low; got %d", len(names))
	assert.Greater(t, len(names), 20, "but it must not filter almost everything out")
}

func TestNetStatMetricsAreUntyped(t *testing.T) {
	// Untyped matching upstream, and it is not an oversight: these fields mix
	// cumulative counters with instantaneous gauges (Tcp_CurrEstab is the current
	// number of established connections). Typing them all as counters would make
	// rate() produce nonsense for the gauges.
	c := newNetStatFixtureCollector(t, "^Tcp_CurrEstab$")

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	found := false
	for m := range ch {
		if metricName(t, m) != "node_netstat_Tcp_CurrEstab" {
			continue
		}
		found = true
		assert.Contains(t, m.Desc().String(), "node_netstat_Tcp_CurrEstab")
	}
	assert.True(t, found, "Tcp_CurrEstab must be emitted; it is the gauge that justifies UntypedValue")
}

func TestNetStatUnparseableValueSkipsOnlyThatField(t *testing.T) {
	// Upstream returns an error here, discarding every protocol already collected.
	// One corrupt field should cost one metric, not the whole scrape -- the same
	// reasoning as the netclass fix.
	dir := writeNetStatFixtures(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "net", "netstat"),
		[]byte("TcpExt: TCPTimeouts TCPOFOQueue\nTcpExt: not-a-number 42\n"), 0o644))

	c, err := newNetStatCollectorWithFields(quietLogger(),
		Paths{ProcFS: dir}.withDefaults(), defNetStatFields)
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 512)
	require.NoError(t, err)
	require.NoError(t, c.Update(ch), "one bad value must not fail the collector")
	close(ch)

	names := map[string]bool{}
	for m := range ch {
		names[metricName(t, m)] = true
	}
	assert.True(t, names["node_netstat_TcpExt_TCPOFOQueue"],
		"the parseable field beside a bad one must still be reported")
	assert.False(t, names["node_netstat_TcpExt_TCPTimeouts"],
		"the unparseable field must be skipped rather than emitted as zero")
}

func TestNetStatMissingFilesReportWhichOneFailed(t *testing.T) {
	// Three files are read and the error must say which was missing, or diagnosing
	// it means guessing.
	dir := writeNetStatFixtures(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "net", "netstat")))

	c, err := newNetStatCollectorWithFields(quietLogger(), Paths{ProcFS: dir}.withDefaults(), defNetStatFields)
	require.NoError(t, err)
	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get netstats")

	dir = writeNetStatFixtures(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "net", "snmp")))
	c, err = newNetStatCollectorWithFields(quietLogger(), Paths{ProcFS: dir}.withDefaults(), defNetStatFields)
	require.NoError(t, err)
	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get SNMP stats")
}

func TestNetStatMalformedSNMP6IsReported(t *testing.T) {
	dir := writeNetStatFixtures(t)
	require.NoError(t, os.Chmod(filepath.Join(dir, "net", "snmp6"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "net", "snmp6"), 0o644) })
	if os.Geteuid() == 0 {
		t.Skip("running as root, mode 000 is still readable")
	}

	c, err := newNetStatCollectorWithFields(quietLogger(), Paths{ProcFS: dir}.withDefaults(), defNetStatFields)
	require.NoError(t, err)
	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get SNMP6 stats")
}

func TestNetStatRegisteredConstructorUsesTheDefaultFilter(t *testing.T) {
	// Covers the default wiring: a typo in defNetStatFields at that call site would
	// otherwise be invisible to every other test here.
	c, err := newNetStatCollector(quietLogger(), Paths{ProcFS: writeNetStatFixtures(t)}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 4096)
	require.NoError(t, c.Update(ch))
	close(ch)

	names := map[string]bool{}
	for m := range ch {
		names[metricName(t, m)] = true
	}
	assert.True(t, names["node_netstat_Tcp_ActiveOpens"])
	assert.False(t, names["node_netstat_Ip_DefaultTTL"])
}

// --- helpers --------------------------------------------------------------

// writeNetStatFixtures copies the checked-in /proc/net fixtures into a temp
// procfs, so a test that mutates one cannot affect another.
func writeNetStatFixtures(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	netDir := filepath.Join(root, "net")
	require.NoError(t, os.MkdirAll(netDir, 0o755))

	for _, name := range []string{"netstat", "snmp", "snmp6"} {
		data, err := os.ReadFile(filepath.Join("testdata", "proc", "net", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(netDir, name), data, 0o644))
	}
	return root
}

func newNetStatFixtureCollector(t *testing.T, fields string) Collector {
	t.Helper()
	c, err := newNetStatCollectorWithFields(quietLogger(),
		Paths{ProcFS: writeNetStatFixtures(t)}.withDefaults(), fields)
	require.NoError(t, err)
	return c
}
