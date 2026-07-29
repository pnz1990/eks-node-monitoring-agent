package hostmetrics

// Tests for path resolution. These matter more than they look: every collector
// reads through Paths, so a wrong join means the agent silently reports the
// *container's* metrics instead of the host's — which looks plausible and is
// completely wrong.

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPathsWithDefaults(t *testing.T) {
	got := Paths{}.withDefaults()
	assert.Equal(t, "/proc", got.ProcFS)
	assert.Equal(t, "/sys", got.SysFS)
	assert.Equal(t, "/", got.RootFS)
	assert.Equal(t, "/run/udev/data", got.UdevData)
}

func TestPathsWithDefaultsPreservesExplicitValues(t *testing.T) {
	got := Paths{ProcFS: "/a", SysFS: "/b", RootFS: "/c", UdevData: "/d"}.withDefaults()
	assert.Equal(t, Paths{ProcFS: "/a", SysFS: "/b", RootFS: "/c", UdevData: "/d"}, got)
}

func TestPathsWithDefaultsFillsOnlyMissing(t *testing.T) {
	got := Paths{ProcFS: "/custom/proc"}.withDefaults()
	assert.Equal(t, "/custom/proc", got.ProcFS)
	assert.Equal(t, "/sys", got.SysFS, "unset fields must still get defaults")
}

func TestForHostRoot(t *testing.T) {
	tests := []struct {
		name     string
		hostRoot string
		want     Paths
	}{
		{
			name:     "empty means running on the host",
			hostRoot: "",
			want:     Paths{ProcFS: "/proc", SysFS: "/sys", RootFS: "/", UdevData: "/run/udev/data"},
		},
		{
			name:     "root means running on the host",
			hostRoot: "/",
			want:     Paths{ProcFS: "/proc", SysFS: "/sys", RootFS: "/", UdevData: "/run/udev/data"},
		},
		{
			name:     "the shipped DaemonSet mount point",
			hostRoot: "/host",
			want:     Paths{ProcFS: "/host/proc", SysFS: "/host/sys", RootFS: "/host", UdevData: "/host/run/udev/data"},
		},
		{
			name:     "trailing slash is trimmed",
			hostRoot: "/host/",
			want:     Paths{ProcFS: "/host/proc", SysFS: "/host/sys", RootFS: "/host", UdevData: "/host/run/udev/data"},
		},
		{
			name:     "nested mount point",
			hostRoot: "/mnt/hostfs",
			want: Paths{ProcFS: "/mnt/hostfs/proc", SysFS: "/mnt/hostfs/sys", RootFS: "/mnt/hostfs",
				UdevData: "/mnt/hostfs/run/udev/data"},
		},
		{
			name:     "path with spaces survives intact",
			hostRoot: "/mnt/host fs",
			want: Paths{ProcFS: "/mnt/host fs/proc", SysFS: "/mnt/host fs/sys", RootFS: "/mnt/host fs",
				UdevData: "/mnt/host fs/run/udev/data"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ForHostRoot(tc.hostRoot))
		})
	}
}

func TestPathJoiners(t *testing.T) {
	p := Paths{ProcFS: "/host/proc", SysFS: "/host/sys", RootFS: "/host"}

	assert.Equal(t, "/host/proc/loadavg", p.procPath("loadavg"))
	assert.Equal(t, "/host/proc/net/dev", p.procPath("net", "dev"))
	assert.Equal(t, "/host/sys/class/net", p.sysPath("class", "net"))
	assert.Equal(t, "/host/var/lib/kubelet", p.rootPath("var", "lib", "kubelet"))

	// No elements returns the root itself, which callers rely on for directory
	// listings.
	assert.Equal(t, "/host/proc", p.procPath())
	assert.Equal(t, "/host/sys", p.sysPath())
}

func TestStripRootFS(t *testing.T) {
	tests := []struct {
		name   string
		rootFS string
		path   string
		want   string
	}{
		{
			name:   "host rootfs is a no-op",
			rootFS: "/",
			path:   "/var/lib/kubelet",
			want:   "/var/lib/kubelet",
		},
		{
			// The reason this exists: mount points must be reported as the host
			// sees them, or every filesystem dashboard label changes.
			name:   "container mount is stripped",
			rootFS: "/host",
			path:   "/host/var/lib/kubelet",
			want:   "/var/lib/kubelet",
		},
		{
			name:   "the rootfs itself becomes /",
			rootFS: "/host",
			path:   "/host",
			want:   "/",
		},
		{
			name:   "a path outside the rootfs is left alone",
			rootFS: "/host",
			path:   "/elsewhere/thing",
			want:   "/elsewhere/thing",
		},
		{
			name:   "nested rootfs",
			rootFS: "/mnt/hostfs",
			path:   "/mnt/hostfs/boot/efi",
			want:   "/boot/efi",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Paths{RootFS: tc.rootFS}
			assert.Equal(t, tc.want, p.stripRootFS(tc.path))
		})
	}
}
