package hostmetrics

// Tests for dmi, nvme and selinux -- the three collectors in the hardware group that
// actually emit on EKS (1, 6 and 3 series respectively on the live node).
//
// The centrepiece is the dmi label set, which is HOST-DEPENDENT: upstream builds the
// descriptor's label list at construction from whichever DMI fields the platform
// exposes, omitting the nil ones. On the live EKS node that yields 16 of 20 --
// board_serial, chassis_serial, product_serial and product_uuid are absent because
// those sysfs files are mode 0400. So node_dmi_info is literally a different metric
// on different hosts, and "fixing" that by emitting all 20 with empty strings would
// change the series identity on every node.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs/sysfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- dmi: the host-dependent label set ------------------------------------

func TestDMIOmitsAbsentFieldsRatherThanEmittingThemEmpty(t *testing.T) {
	// THE EKS CASE, reproduced exactly. The four serial/uuid files are mode 0400 and
	// unreadable as a non-root user, so procfs returns nil for them and the labels
	// must be ABSENT -- not present with an empty value.
	//
	// Verified against the live cluster golden, which carries exactly these 16 label
	// keys on node_dmi_info.
	root := writeDMITree(t, map[string]string{
		"bios_date":         "10/16/2017",
		"bios_release":      "1.0",
		"bios_vendor":       "Amazon EC2",
		"bios_version":      "1.0",
		"board_asset_tag":   "i-0334db72aac2027f8",
		"board_name":        "",
		"board_vendor":      "Amazon EC2",
		"board_version":     "",
		"chassis_asset_tag": "Amazon EC2",
		"chassis_vendor":    "Amazon EC2",
		"chassis_version":   "",
		"product_family":    "",
		"product_name":      "t3.large",
		"product_sku":       "",
		"product_version":   "",
		"sys_vendor":        "Amazon EC2",
		// deliberately NOT written: board_serial, chassis_serial, product_serial,
		// product_uuid -- exactly the four the EKS node lacks.
	})

	c, err := newDMICollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	labels := singleMetricLabels(t, c, "node_dmi_info")

	for _, want := range []string{
		"bios_date", "bios_release", "bios_vendor", "bios_version",
		"board_asset_tag", "board_name", "board_vendor", "board_version",
		"chassis_asset_tag", "chassis_vendor", "chassis_version",
		"product_family", "product_name", "product_sku", "product_version",
		"system_vendor",
	} {
		assert.Contains(t, labels, want, "label %q must be present", want)
	}
	for _, absent := range []string{
		"board_serial", "chassis_serial", "product_serial", "product_uuid",
	} {
		assert.NotContains(t, labels, absent,
			"%q is unreadable here and its label must be OMITTED, not empty -- "+
				"emitting it would change the series identity", absent)
	}
	assert.Len(t, labels, 16, "16 of 20, matching the live EKS node exactly")
}

func TestDMIEmptyStringIsNotTheSameAsAbsent(t *testing.T) {
	// A field that EXISTS but is empty (board_name on EC2) must still get its label,
	// with an empty value. Only a nil pointer -- an unreadable file -- omits the label.
	// Conflating the two would silently drop labels on real hosts.
	root := writeDMITree(t, map[string]string{
		"product_name": "t3.large",
		"board_name":   "",
	})

	c, err := newDMICollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	labels := singleMetricLabels(t, c, "node_dmi_info")
	assert.Equal(t, "t3.large", labels["product_name"])
	require.Contains(t, labels, "board_name", "an empty-but-present file keeps its label")
	assert.Equal(t, "", labels["board_name"])
}

func TestDMILabelOrderIsDeterministic(t *testing.T) {
	// Upstream ranges over a Go map, so its Desc's label order varies between process
	// starts. Harmless for series identity (Prometheus sorts) but it makes a golden
	// corpus non-diffable, so ours is sorted. Asserted across repeated construction.
	root := writeDMITree(t, map[string]string{
		"product_name": "t3.large", "bios_vendor": "Amazon EC2", "sys_vendor": "Amazon EC2",
	})

	first := dmiDescString(t, root)
	for i := 0; i < 5; i++ {
		assert.Equal(t, first, dmiDescString(t, root),
			"the descriptor must be identical across constructions")
	}
	// And specifically sorted.
	labels := regexp.MustCompile(`variableLabels: \{([^}]*)\}`).FindStringSubmatch(first)
	require.NotNil(t, labels, "could not parse variableLabels from %q", first)
	got := regexp.MustCompile(`[a-z_]+`).FindAllString(labels[1], -1)
	sorted := append([]string(nil), got...)
	sort.Strings(sorted)
	assert.Equal(t, sorted, got, "labels must be in sorted order")
}

func TestDMIInvalidUTF8IsReplaced(t *testing.T) {
	// DMI strings come from firmware and are not guaranteed valid UTF-8, but the
	// Prometheus text format requires it -- an invalid byte would make the whole
	// exposition unparseable, not just this metric.
	root := t.TempDir()
	dir := filepath.Join(root, "class", "dmi", "id")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "product_name"),
		[]byte("bad\xff\xfebyte\n"), 0o644))

	c, err := newDMICollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	value := singleMetricLabels(t, c, "node_dmi_info")["product_name"]
	assert.True(t, strings.ToValidUTF8(value, "") == value || strings.Contains(value, "�"),
		"invalid bytes must be replaced, got %q", value)
	assert.NotContains(t, value, "\xff")
}

func TestDMINoFieldsIsNoData(t *testing.T) {
	// ErrNoData here, unlike the rest of the hardware group -- and that IS upstream's
	// behaviour, because a dmi_info with zero labels is a bare "1" carrying no
	// information at all.
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "class", "dmi", "id"), 0o755))

	c, err := newDMICollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err), "expected ErrNoData, got %v", err)
}

func TestDMIAbsentClassDirectoryStillConstructs(t *testing.T) {
	// A platform without DMI (most ARM boards). Construction must SUCCEED with an
	// empty struct rather than failing -- otherwise the whole agent refuses to start
	// over absent firmware tables. Update then reports ErrNoData.
	c, err := newDMICollector(quietLogger(), Paths{SysFS: t.TempDir()}.withDefaults())
	require.NoError(t, err, "a platform without DMI must not fail construction")

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
}

func TestDMIConstructionFailsOnMissingSysfs(t *testing.T) {
	_, err := newDMICollector(quietLogger(),
		Paths{SysFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open sysfs")
}

func TestDMIUnreadableClassDirectoryFailsConstruction(t *testing.T) {
	// A DMI directory that EXISTS but cannot be read is NOT the same as an absent
	// platform. Absent (ENOENT) means "no DMI here" and construction proceeds with an
	// empty struct; unreadable (EACCES) means something is wrong and must surface,
	// rather than silently reporting a node with no firmware information.
	if os.Geteuid() == 0 {
		t.Skip("running as root, mode 000 is still readable")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "class", "dmi", "id")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "product_name"), []byte("x\n"), 0o644))
	require.NoError(t, os.Chmod(dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, err := newDMICollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read DMI information")
	assert.False(t, os.IsNotExist(err),
		"an unreadable directory must not be treated as an absent platform")
}

func TestDMIClassPathIsAFileNotADirectory(t *testing.T) {
	// ENOTDIR rather than ENOENT: another non-absent failure that must not be
	// laundered into "this platform has no DMI".
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "class", "dmi"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "class", "dmi", "id"), []byte("x"), 0o644))

	_, err := newDMICollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read DMI information")
}

func TestDMIFieldSetMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/dmi.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	// Anchored on the label->accessor map literal.
	body := regexp.MustCompile(`(?s)for label, value := range map\[string\]\*string\{(.*?)\n\t\}`).
		FindSubmatch(data)
	require.NotNil(t, body, "failed to locate upstream's DMI field map; the regexp may be stale")

	upstream := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([a-z_]+)":`).FindAllSubmatch(body[1], -1) {
		upstream[string(m[1])] = true
	}
	require.Len(t, upstream, 20, "expected 20 upstream DMI fields, got %d", len(upstream))

	ours := dmiFields(&sysfs.DMIClass{})
	for field := range upstream {
		assert.Contains(t, ours, field, "upstream DMI field %q is missing", field)
	}
	for field := range ours {
		assert.Contains(t, upstream, field, "we have a DMI field %q that upstream does not", field)
	}
}

func TestDMILiveOnThisHost(t *testing.T) {
	c, err := newDMICollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 8)
	err = c.Update(ch)
	close(ch)
	if err != nil && IsNoDataError(err) {
		t.Skip("no DMI on this host")
	}
	require.NoError(t, err)
	assert.Len(t, ch, 1, "exactly one dmi_info series")
}

// --- nvme -----------------------------------------------------------------

func TestNVMeEmitsDeviceAndNamespaceMetrics(t *testing.T) {
	// EBS volumes present as NVMe on EC2, so this collector DOES emit on EKS -- the
	// live node reports 6 series. Values are distinct so a misrouted descriptor fails.
	c := newNVMeStub(t, sysfs.NVMeClass{
		"nvme0": sysfs.NVMeDevice{
			Name: "nvme0", FirmwareRevision: "1.0", Model: "Amazon Elastic Block Store",
			Serial: "vol023eb1d2f25982ce5", State: "live", ControllerID: "0",
			Namespaces: []sysfs.NVMeNamespace{{
				ID: "1", ANAState: "optimized",
				CapacityBytes: 8589934592, SizeBytes: 8589934592,
				UsedBytes: 1073741824, LogicalBlockSize: 512,
			}},
		},
	})

	got := gatherAllLabels(t, c)
	require.Contains(t, got, "node_nvme_info")
	assert.Contains(t, got["node_nvme_namespace_capacity_bytes"], "device=nvme0,nsid=1")
	assert.Equal(t, 8589934592.0, got["node_nvme_namespace_capacity_bytes"]["device=nvme0,nsid=1"])
	assert.Equal(t, 8589934592.0, got["node_nvme_namespace_size_bytes"]["device=nvme0,nsid=1"])
	assert.Equal(t, 1073741824.0, got["node_nvme_namespace_used_bytes"]["device=nvme0,nsid=1"])
	assert.Equal(t, 512.0, got["node_nvme_namespace_logical_block_size_bytes"]["device=nvme0,nsid=1"])
}

func TestNVMeInfoLabelsAreNotTransposed(t *testing.T) {
	// Six string labels emitted positionally, and cntlid is LAST in the descriptor
	// even though it reads first alphabetically. A transposition yields a metric with
	// the right name and label keys and wrong values.
	c := newNVMeStub(t, sysfs.NVMeClass{
		"nvme0": sysfs.NVMeDevice{
			Name: "DEVICE", FirmwareRevision: "FIRMWARE", Model: "MODEL",
			Serial: "SERIAL", State: "STATE", ControllerID: "CNTLID",
		},
	})

	labels := singleMetricLabels(t, c, "node_nvme_info")
	assert.Equal(t, map[string]string{
		"device": "DEVICE", "firmware_revision": "FIRMWARE", "model": "MODEL",
		"serial": "SERIAL", "state": "STATE", "cntlid": "CNTLID",
	}, labels)
}

func TestNVMeMultipleNamespacesEachGetSeries(t *testing.T) {
	c := newNVMeStub(t, sysfs.NVMeClass{
		"nvme0": sysfs.NVMeDevice{
			Name: "nvme0",
			Namespaces: []sysfs.NVMeNamespace{
				{ID: "1", SizeBytes: 100},
				{ID: "2", SizeBytes: 200},
			},
		},
	})

	got := gatherAllLabels(t, c)
	assert.Equal(t, 100.0, got["node_nvme_namespace_size_bytes"]["device=nvme0,nsid=1"])
	assert.Equal(t, 200.0, got["node_nvme_namespace_size_bytes"]["device=nvme0,nsid=2"])
	assert.Len(t, got["node_nvme_namespace_size_bytes"], 2)
}

func TestNVMeDeviceWithNoNamespacesStillEmitsInfo(t *testing.T) {
	c := newNVMeStub(t, sysfs.NVMeClass{
		"nvme0": sysfs.NVMeDevice{Name: "nvme0", State: "live"},
	})

	got := gatherAllLabels(t, c)
	assert.Contains(t, got, "node_nvme_info")
	assert.NotContains(t, got, "node_nvme_namespace_info")
}

func TestNVMeAbsentClassIsNoData(t *testing.T) {
	c := newNVMeStub(t, nil)
	c.nvmeClass = func() (sysfs.NVMeClass, error) { return nil, os.ErrNotExist }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err))
}

func TestNVMeRealErrorIsAFailure(t *testing.T) {
	c := newNVMeStub(t, nil)
	c.nvmeClass = func() (sysfs.NVMeClass, error) { return nil, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err))
	assert.Contains(t, err.Error(), "error obtaining NVMe class info")
}

func TestNVMeAllMetricsAreGauges(t *testing.T) {
	// Namespace sizes change only on resize, and info is a constant 1, so gauges
	// throughout -- none of these is cumulative.
	c := newNVMeStub(t, sysfs.NVMeClass{
		"nvme0": sysfs.NVMeDevice{
			Name:       "nvme0",
			Namespaces: []sysfs.NVMeNamespace{{ID: "1", SizeBytes: 1}},
		},
	})

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	n := 0
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.NotNil(t, pb.Gauge, "%s must be a gauge", metricName(t, m))
		n++
	}
	assert.Equal(t, 6, n, "1 device info + 1 namespace info + 4 namespace values")
}

func TestNVMeConstructionFailsOnMissingSysfs(t *testing.T) {
	_, err := newNVMeCollector(quietLogger(),
		Paths{SysFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open sysfs")
}

func TestNVMeRegisteredConstructorWiresTheRealReader(t *testing.T) {
	c, err := newNVMeCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	require.NotNil(t, c.(*nvmeCollector).nvmeClass)
}

// --- selinux --------------------------------------------------------------

func TestSELinuxDisabledEmitsOnlyEnabled(t *testing.T) {
	// config_mode and current_mode are meaningless without SELinux, and emitting 0
	// for them would read as "permissive" -- a specific claim rather than an absence.
	c := newSELinuxStub(t, false, 1, 1)

	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, map[string]float64{"node_selinux_enabled": 0}, got)
}

func TestSELinuxEnabledEmitsAllThree(t *testing.T) {
	// The modes are an enum: -1 disabled, 0 permissive, 1 enforcing. Three states, so
	// a gauge rather than a boolean. Distinct values so a swap between config and
	// current fails.
	c := newSELinuxStub(t, true, 1, 0)

	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, 1.0, got["node_selinux_enabled"])
	assert.Equal(t, 1.0, got["node_selinux_config_mode"], "configured enforcing")
	assert.Equal(t, 0.0, got["node_selinux_current_mode"],
		"currently permissive -- a real and important divergence from the config")
	assert.Len(t, got, 3)
}

func TestSELinuxModesCoverTheFullEnum(t *testing.T) {
	// -1 is a legitimate value (disabled), so the metric must not be clamped to 0/1.
	c := newSELinuxStub(t, true, -1, -1)

	got := gatherUnlabelledMixed(t, c)
	assert.Equal(t, -1.0, got["node_selinux_config_mode"])
	assert.Equal(t, -1.0, got["node_selinux_current_mode"])
}

func TestSELinuxLiveOnThisHost(t *testing.T) {
	c, err := newSELinuxCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	got := gatherUnlabelledMixed(t, c)
	require.Contains(t, got, "node_selinux_enabled")
	assert.Contains(t, []float64{0, 1}, got["node_selinux_enabled"])
	if got["node_selinux_enabled"] == 1 {
		assert.Len(t, got, 3, "an enabled host reports all three")
		return
	}
	assert.Len(t, got, 1, "a disabled host reports only enabled=0")
}

func TestSELinuxRegisteredConstructorWiresTheRealFunctions(t *testing.T) {
	c, err := newSELinuxCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	sc := c.(*selinuxCollector)
	require.NotNil(t, sc.enabled)
	require.NotNil(t, sc.defaultEnforce)
	require.NotNil(t, sc.currentEnforce)
}

// --- helpers --------------------------------------------------------------

// writeDMITree builds a /sys/class/dmi/id directory.
//
// NOTE the filename for system_vendor is "sys_vendor" in sysfs -- the label is
// system_vendor but the file is not. Getting that wrong makes the label silently
// absent, which is exactly the failure this test file exists to catch.
func writeDMITree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "class", "dmi", "id")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o644))
	}
	return root
}

func dmiDescString(t *testing.T, root string) string {
	t.Helper()
	c, err := newDMICollector(quietLogger(), Paths{SysFS: root}.withDefaults())
	require.NoError(t, err)
	return c.(*dmiCollector).infoDesc.String()
}

func newNVMeStub(t *testing.T, class sysfs.NVMeClass) *nvmeCollector {
	t.Helper()
	c, err := newNVMeCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	nc := c.(*nvmeCollector)
	nc.nvmeClass = func() (sysfs.NVMeClass, error) { return class, nil }
	return nc
}

func newSELinuxStub(t *testing.T, enabled bool, config, current int) *selinuxCollector {
	t.Helper()
	c, err := newSELinuxCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	sc := c.(*selinuxCollector)
	sc.enabled = func() bool { return enabled }
	sc.defaultEnforce = func() int { return config }
	sc.currentEnforce = func() int { return current }
	return sc
}
