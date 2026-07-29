package metrics_test

import (
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/eks-node-monitoring-agent/pkg/metrics"
)

// testLogger returns a logger that discards output so tests stay quiet.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestHostPathArgs(t *testing.T) {
	tests := []struct {
		name     string
		hostRoot string
		expected []string
	}{
		{
			name:     "empty host root returns no args",
			hostRoot: "",
			expected: nil,
		},
		{
			name:     "root host root returns no args",
			hostRoot: "/",
			expected: nil,
		},
		{
			name:     "host mount rebases every path",
			hostRoot: "/host",
			expected: []string{
				"--path.procfs=/host/proc",
				"--path.sysfs=/host/sys",
				"--path.rootfs=/host",
				"--path.udev.data=/host/run/udev/data",
			},
		},
		{
			name:     "trailing slash is trimmed",
			hostRoot: "/host/",
			expected: []string{
				"--path.procfs=/host/proc",
				"--path.sysfs=/host/sys",
				"--path.rootfs=/host",
				"--path.udev.data=/host/run/udev/data",
			},
		},
		{
			name:     "nested mount point is honoured",
			hostRoot: "/mnt/hostfs",
			expected: []string{
				"--path.procfs=/mnt/hostfs/proc",
				"--path.sysfs=/mnt/hostfs/sys",
				"--path.rootfs=/mnt/hostfs",
				"--path.udev.data=/mnt/hostfs/run/udev/data",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, metrics.HostPathArgs(tc.hostRoot))
		})
	}
}

func TestResolveUpstreamFlags(t *testing.T) {
	// ResolveUpstreamFlags latches via sync.Once for the lifetime of the
	// process, so the first call in this package decides the outcome. Assert it
	// succeeds and that repeat calls are consistent rather than re-parsing.
	require.NoError(t, metrics.ResolveUpstreamFlags(nil))
	require.NoError(t, metrics.ResolveUpstreamFlags([]string{"--this-flag-does-not-exist"}),
		"subsequent calls must return the first result, not re-parse")
}

func TestNewCollectorDefaultSet(t *testing.T) {
	require.NoError(t, metrics.ResolveUpstreamFlags(nil))

	nc, err := metrics.NewCollector(testLogger())
	require.NoError(t, err)
	require.NotNil(t, nc)

	names := metrics.EnabledCollectorNames(nc)
	assert.NotEmpty(t, names, "default collector set must not be empty")

	// The default set is what gives node_exporter parity, so spot check the
	// collectors every node dashboard depends on.
	for _, want := range []string{"cpu", "meminfo", "loadavg", "filesystem", "netdev", "diskstats"} {
		assert.Contains(t, names, want)
	}
}

func TestNewCollectorWithFilters(t *testing.T) {
	require.NoError(t, metrics.ResolveUpstreamFlags(nil))

	nc, err := metrics.NewCollector(testLogger(), "cpu", "loadavg")
	require.NoError(t, err)

	names := metrics.EnabledCollectorNames(nc)
	assert.ElementsMatch(t, []string{"cpu", "loadavg"}, names)
}

func TestNewCollectorUnknownFilter(t *testing.T) {
	require.NoError(t, metrics.ResolveUpstreamFlags(nil))

	_, err := metrics.NewCollector(testLogger(), "definitely-not-a-collector")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create node collector")
}

func TestEnabledCollectorNamesNil(t *testing.T) {
	assert.Nil(t, metrics.EnabledCollectorNames(nil))
}

func TestEnabledCollectorNamesSorted(t *testing.T) {
	require.NoError(t, metrics.ResolveUpstreamFlags(nil))

	nc, err := metrics.NewCollector(testLogger(), "loadavg", "cpu")
	require.NoError(t, err)

	names := metrics.EnabledCollectorNames(nc)
	require.Len(t, names, 2)
	assert.Equal(t, []string{"cpu", "loadavg"}, names, "names must be sorted for deterministic logging")
}

func TestApplyEKSDefaultsPreservesOperatorPrecedence(t *testing.T) {
	// kingpin is last-wins, so defaults must come first for an operator flag to
	// override them. If this order ever flips, user configuration is silently
	// ignored.
	got := metrics.ApplyEKSDefaultsForTest([]string{"--collector.netclass.ignored-devices=^custom$"})
	require.NotEmpty(t, got)
	assert.Equal(t, "--collector.netclass.ignored-devices=^custom$", got[len(got)-1],
		"operator flags must be last so they win")
}

func TestEKSDefaultsAreParityNeutral(t *testing.T) {
	// Each EKS default must be parity-neutral: it may change WHICH series appear
	// within a family, but must never add or remove a metric family. Verified
	// against upstream by hack/parity-test.sh (298/298 with these defaults).
	//
	// Two candidates were measured and REJECTED for failing this bar:
	//   --collector.netclass.ignored-devices  -> 297 names (loses
	//        node_network_speed_bytes; the only speed-reporting interfaces on an
	//        EKS node are the pod-side eni* halves)
	//   --collector.netclass.netlink          -> 299 names (adds
	//        node_network_altnames_info)
	//
	// If you add a default here, re-run hack/parity-test.sh and record the result
	// in docs/parity-exceptions.md before changing this list.
	allowed := map[string]bool{
		"--collector.filesystem.mount-points-exclude": true,
	}
	for _, arg := range metrics.ApplyEKSDefaultsForTest(nil) {
		flag := arg
		if i := strings.Index(arg, "="); i >= 0 {
			flag = arg[:i]
		}
		assert.True(t, allowed[flag],
			"EKS default %q is not in the reviewed allowlist; confirm it is parity-neutral with hack/parity-test.sh first", flag)
	}
}

func TestFilesystemExclusionCoversPodEphemeralMounts(t *testing.T) {
	// Regression test for the pod-scaling cardinality defect: the agent runs with
	// hostPID, so the filesystem collector reads the host init mount namespace and
	// sees every per-pod mount. Those paths embed unique pod UIDs and sandbox IDs,
	// so leaving them in grows series count with pod density indefinitely.
	rx := regexp.MustCompile(metrics.EKSExcludedMountPointsForTest())

	mustExclude := []string{
		"/var/lib/kubelet/pods/1b450692-be04-479e-a795-94a8ec472c20/volumes/kubernetes.io~projected/kube-api-access-r9psr",
		"/run/containerd/io.containerd.grpc.v1.cri/sandboxes/20c938ab0c375c430150ca8ebbc45fd9371a5aaedb089de2beea68d1efb16e80/shm",
		// upstream's own defaults must survive being replaced by ours
		"/dev", "/proc", "/sys",
		"/var/lib/docker/overlay2/abc",
		"/var/lib/containers/storage/x",
		"/run/credentials/systemd-sysctl.service",
	}
	for _, mp := range mustExclude {
		assert.True(t, rx.MatchString(mp), "must exclude pod/runtime mount %q", mp)
	}

	mustKeep := []string{
		"/", "/boot/efi", "/run", "/tmp", "/var", "/var/lib", "/var/lib/kubelet",
		"/home", "/var/log",
	}
	for _, mp := range mustKeep {
		assert.False(t, rx.MatchString(mp), "must NOT exclude real filesystem %q", mp)
	}
}

func TestTestOnlyHelpersExposeInternals(t *testing.T) {
	// The two ForTest helpers exist so the external test package can assert on
	// internals without widening the real API. Cover them so the package stays at
	// full statement coverage without a .covignore entry.
	assert.NotEmpty(t, metrics.EKSExcludedMountPointsForTest())
	assert.NotNil(t, metrics.ApplyEKSDefaultsForTest(nil))
}
