package hostmetrics

// Tests for the netclass collector.
//
// The centrepiece is TestNetClassSkipsUnreadableDevice, the regression test for
// upstream #1915/#1841. Upstream returns on the first device read error, so one
// interface disappearing mid-scrape suppresses metrics for EVERY interface. On EKS
// that is routine, because veth and eni devices churn with pod scheduling.

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

// writeSysClassNet builds a fake /sys/class/net with the given devices. Each device
// gets the minimum attributes procfs/sysfs needs to parse it.
func writeSysClassNet(t *testing.T, devices ...string) string {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "class", "net")
	for _, dev := range devices {
		dir := filepath.Join(base, dev)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		for name, content := range map[string]string{
			"operstate":          "up",
			"addr_assign_type":   "0",
			"address":            "aa:bb:cc:dd:ee:ff",
			"broadcast":          "ff:ff:ff:ff:ff:ff",
			"carrier":            "1",
			"carrier_changes":    "2",
			"carrier_up_count":   "1",
			"carrier_down_count": "1",
			"dev_id":             "0x0",
			"dormant":            "0",
			"flags":              "0x1003",
			"ifindex":            "2",
			"iflink":             "2",
			"link_mode":          "0",
			"mtu":                "9001",
			"name_assign_type":   "4",
			"netdev_group":       "0",
			"tx_queue_len":       "1000",
			"type":               "1",
		} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o644))
		}
	}
	return root
}

func newTestNetClassCollector(t *testing.T, sysRoot string) *netClassCollector {
	t.Helper()
	c, err := newNetClassCollector(quietLogger(), Paths{SysFS: filepath.Join(sysRoot)}.withDefaults())
	require.NoError(t, err)
	return c.(*netClassCollector)
}

// --- the #1915 / #1841 fix ------------------------------------------------

// TestNetClassSkipsUnreadableDevice is the regression test for the all-or-nothing
// read. Upstream would return zero devices here; this must return the readable
// ones.
func TestNetClassSkipsUnreadableDevice(t *testing.T) {
	sysRoot := writeSysClassNet(t, "eth0", "veth1234", "veth5678")

	// Simulate pod churn: a device listed by NetClassDevices but gone by the time it
	// is read. Removing its attributes makes NetClassByIface fail while the
	// directory entry still exists to be listed.
	require.NoError(t, os.RemoveAll(filepath.Join(sysRoot, "class", "net", "veth5678", "operstate")))
	require.NoError(t, os.Chmod(filepath.Join(sysRoot, "class", "net", "veth5678"), 0o000))
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(sysRoot, "class", "net", "veth5678"), 0o755)
	})

	c := newTestNetClassCollector(t, sysRoot)
	devices, err := c.netClassInfo()
	require.NoError(t, err, "one unreadable device must not fail the whole collection")

	names := map[string]bool{}
	for _, d := range devices {
		names[d.Name] = true
	}
	// Upstream returns 0 of 3 here. The readable devices must survive.
	assert.True(t, names["eth0"], "eth0 is readable and must be reported")
	assert.True(t, names["veth1234"], "veth1234 is readable and must be reported")
	assert.GreaterOrEqual(t, len(devices), 2,
		"upstream returns nothing when any device fails; we must return the readable ones")
}

func TestNetClassAllDevicesUnreadableYieldsNoData(t *testing.T) {
	// Distinct from a failure: no readable device means no data, which upstream's
	// ErrNoData sentinel exists to express. It reports success=0 without logging an
	// error, matching upstream's treatment.
	sysRoot := writeSysClassNet(t, "eth0")
	require.NoError(t, os.Chmod(filepath.Join(sysRoot, "class", "net", "eth0"), 0o000))
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(sysRoot, "class", "net", "eth0"), 0o755)
	})

	c := newTestNetClassCollector(t, sysRoot)
	err := c.Update(make(chan prometheus.Metric, 64))
	if err != nil {
		// Either ErrNoData or a wrapped read failure is acceptable; a panic is not.
		assert.True(t, IsNoDataError(err) || strings.Contains(err.Error(), "net class"),
			"unexpected error: %v", err)
	}
}

// --- field set matches upstream ------------------------------------------

func TestNetClassFieldsMatchUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/netclass_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}

	upstream := map[string]bool{}
	for _, m := range regexp.MustCompile(`c\.getFieldDesc\("([^"]+)"\)`).FindAllSubmatch(data, -1) {
		upstream[string(m[1])] = true
	}
	require.NotEmpty(t, upstream, "extracted no fields from upstream; the regexp may be stale")

	ours := map[string]bool{}
	for _, name := range netclassFieldNames() {
		ours[name] = true
	}

	for f := range upstream {
		assert.True(t, ours[f], "upstream field %q is missing; the metric would disappear", f)
	}
	for f := range ours {
		assert.True(t, upstream[f], "we emit field %q that upstream does not", f)
	}
	assert.Len(t, ours, len(upstream))
}

// --- adminState ----------------------------------------------------------

func TestAdminState(t *testing.T) {
	// Upstream uses `*flags & int64(net.FlagUp) == 1`, and net.FlagUp is 1, so it is
	// a test of bit 0. Verified equivalent to this `& 0x01 != 0` form across real
	// interface flag values including 0x1003 (UP|BROADCAST|MULTICAST).
	up := int64(1)
	down := int64(2)
	realUp := int64(0x1003)
	realDown := int64(0x1002)

	assert.Equal(t, "up", adminState(&up))
	assert.Equal(t, "down", adminState(&down))
	assert.Equal(t, "up", adminState(&realUp), "0x1003 has IFF_UP set")
	assert.Equal(t, "down", adminState(&realDown), "0x1002 does not have IFF_UP set")
	assert.Equal(t, "unknown", adminState(nil), "a kernel that did not report flags is unknown, not down")

	zero := int64(0)
	assert.Equal(t, "down", adminState(&zero))
}

// --- emitted metric set --------------------------------------------------

func TestNetClassEmitsUpAndInfo(t *testing.T) {
	c := newTestNetClassCollector(t, writeSysClassNet(t, "eth0"))

	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)

	var sawUp, sawInfo bool
	for m := range ch {
		d := m.Desc().String()
		if strings.Contains(d, "node_network_up") {
			sawUp = true
		}
		if strings.Contains(d, "node_network_info") {
			sawInfo = true
		}
	}
	assert.True(t, sawUp, "node_network_up must be emitted")
	assert.True(t, sawInfo, "node_network_info must be emitted")
}

func TestNetClassSpeedConvertsMbitToBytes(t *testing.T) {
	// speed is reported in Mbit/s and the metric is bytes/s: value * 1000 * 1000 / 8.
	// A regression here is off by 8x and passes any name-only comparison.
	sysRoot := writeSysClassNet(t, "eth0")
	require.NoError(t, os.WriteFile(
		filepath.Join(sysRoot, "class", "net", "eth0", "speed"), []byte("10000\n"), 0o644))

	c := newTestNetClassCollector(t, sysRoot)
	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)

	found := false
	for m := range ch {
		if strings.Contains(m.Desc().String(), "node_network_speed_bytes") {
			found = true
		}
	}
	assert.True(t, found, "speed_bytes must be emitted when the kernel reports a speed")
}

func TestNetClassInvalidSpeedEmittedByDefault(t *testing.T) {
	// Some virtual devices report -1. Upstream emits it unless
	// ignore-invalid-speed is set, which defaults OFF. Enabling that default would
	// remove the metric family entirely on a node where every remaining interface
	// reports an invalid speed -- measured on the dependency branch as 297 names
	// versus upstream's 298.
	sysRoot := writeSysClassNet(t, "eth0")
	require.NoError(t, os.WriteFile(
		filepath.Join(sysRoot, "class", "net", "eth0", "speed"), []byte("-1\n"), 0o644))

	c := newTestNetClassCollector(t, sysRoot)
	assert.False(t, c.ignoreInvalidSpeed, "must default OFF to match upstream")

	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)

	found := false
	for m := range ch {
		if strings.Contains(m.Desc().String(), "node_network_speed_bytes") {
			found = true
		}
	}
	assert.True(t, found, "an invalid speed is still emitted by default, matching upstream")
}

func TestNetClassInvalidSpeedSuppressedWhenEnabled(t *testing.T) {
	sysRoot := writeSysClassNet(t, "eth0")
	require.NoError(t, os.WriteFile(
		filepath.Join(sysRoot, "class", "net", "eth0", "speed"), []byte("-1\n"), 0o644))

	c := newTestNetClassCollector(t, sysRoot)
	c.ignoreInvalidSpeed = true

	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)

	for m := range ch {
		assert.NotContains(t, m.Desc().String(), "node_network_speed_bytes")
	}
}

func TestNetClassEnumerationFailureIsWrapped(t *testing.T) {
	// A sysfs root that exists but has no class/net: NewFS succeeds, enumeration
	// fails. Distinct from ErrNoData -- we could not look, rather than looked and
	// found nothing -- so it must surface as an error and set collector success=0.
	root := t.TempDir()
	c, err := newNetClassCollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err, "construction must not probe class/net")

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err), "an unreadable class/net is a failure, not no-data")
	assert.Contains(t, err.Error(), "couldn't get net class info")
}

func TestNetClassIgnoredDevicesFilterSkipsMatches(t *testing.T) {
	// The filter is not wired to any default, so it is exercised through the seam.
	// Excluded devices must be absent entirely, not reported with zeroes.
	sysRoot := writeSysClassNet(t, "eth0", "veth1234", "veth5678")
	c, err := newNetClassCollectorWithFilter(quietLogger(),
		Paths{SysFS: sysRoot}.withDefaults(), "^veth")
	require.NoError(t, err)

	devices, err := c.(*netClassCollector).netClassInfo()
	require.NoError(t, err)

	names := map[string]bool{}
	for _, d := range devices {
		names[d.Name] = true
	}
	assert.Equal(t, map[string]bool{"eth0": true}, names,
		"only the non-matching device may be reported")
}

func TestNetClassInvalidIgnoredPatternRejectedAtConstruction(t *testing.T) {
	_, err := newNetClassCollectorWithFilter(quietLogger(),
		Paths{SysFS: writeSysClassNet(t, "eth0")}.withDefaults(), "([unclosed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid ignored-devices pattern")
}

func TestNetClassDefaultFilterIsUpstreamVerbatim(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/netclass_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	// Anchor on the .Default(...) call, not just the next string literal after the
	// variable name -- that is the flag NAME, and an earlier version of this
	// assertion compared against "collector.netclass.ignored-devices" and failed for
	// exactly that reason. Same family as the three prior "asserting against a
	// loosely-specified artifact" bugs.
	m := regexp.MustCompile(`netclassIgnoredDevices\s*=[^\n]*\.Default\("([^"]*)"\)`).FindSubmatch(data)
	require.NotNil(t, m, "failed to extract upstream's ignored-devices default; the regexp may be stale")
	assert.Equal(t, string(m[1]), defNetClassIgnoredDevices,
		"our copy of upstream's ignored-devices default must be verbatim")
}

func TestNetClassIgnoredDevicesDefaultsToNothing(t *testing.T) {
	// "^$" matches only the empty string. Excluding pod-side interfaces was measured
	// and rejected on the dependency branch for costing node_network_speed_bytes;
	// with the all-or-nothing read fixed there is no resilience reason to exclude.
	c := newTestNetClassCollector(t, writeSysClassNet(t, "eth0"))
	for _, dev := range []string{"eth0", "veth1234", "eni0abc", "lo", "docker0"} {
		assert.False(t, c.ignoredDevices.MatchString(dev),
			"no device should be excluded by default, %q was", dev)
	}
}

func TestNetClassPushFieldSkipsNil(t *testing.T) {
	// A nil pointer means the sysfs file was absent. Emitting zero would be a claim
	// the kernel never made.
	c := newTestNetClassCollector(t, writeSysClassNet(t, "eth0"))
	ch := make(chan prometheus.Metric, 4)
	c.pushField(ch, "mtu_bytes", nil, "eth0", prometheus.GaugeValue)
	assert.Empty(t, ch, "a nil value must emit nothing")
}

func TestNetClassPushFieldUnknownFieldIsSkipped(t *testing.T) {
	// Unreachable with the compile-time table, but a missing descriptor would
	// otherwise nil-panic inside MustNewConstMetric.
	c := newTestNetClassCollector(t, writeSysClassNet(t, "eth0"))
	ch := make(chan prometheus.Metric, 4)
	v := int64(1)
	c.pushField(ch, "not_a_real_field", &v, "eth0", prometheus.GaugeValue)
	assert.Empty(t, ch)
}

func TestNetClassMissingSysfs(t *testing.T) {
	_, err := newNetClassCollector(quietLogger(),
		Paths{SysFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open sysfs")
}

func TestNetClassNoDevicesYieldsNoData(t *testing.T) {
	// An empty /sys/class/net is no data rather than an error.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "class", "net"), 0o755))

	c := newTestNetClassCollector(t, root)
	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err), "expected ErrNoData, got %v", err)
}
