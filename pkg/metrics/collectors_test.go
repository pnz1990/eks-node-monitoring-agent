package metrics_test

import (
	"io"
	"log/slog"
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
