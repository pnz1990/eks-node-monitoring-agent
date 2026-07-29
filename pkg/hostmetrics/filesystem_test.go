package hostmetrics

// Tests for the filesystem collector.
//
// Three things get the most attention, because each is a silent failure mode:
//   - the mount-point exclusion, which must drop per-pod mounts (unbounded churn
//     cardinality) while keeping every real filesystem. The must-keep half matters
//     more: an over-broad regexp would hide a real disk-full condition.
//   - the single-writer result collection, which is the fix for upstream's data
//     race between the producer goroutine and the consumer loop.
//   - the stuck-mount machinery, which is what stops a dead NFS mount from hanging
//     the collector forever (upstream #1353).

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- exclusion defaults match upstream -----------------------------------

func TestFilesystemUpstreamDefaultsMatch(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/filesystem_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	src := string(data)

	mountRe := regexp.MustCompile(`defMountPointsExcluded\s*=\s*"([^"]+)"`).FindStringSubmatch(src)
	require.NotNil(t, mountRe, "failed to extract upstream's mount-point default")
	assert.Equal(t, mountRe[1], defMountPointsExcluded,
		"our copy of upstream's mount-point default must be verbatim")

	fsRe := regexp.MustCompile(`defFSTypesExcluded\s*=\s*"([^"]+)"`).FindStringSubmatch(src)
	require.NotNil(t, fsRe, "failed to extract upstream's fs-type default")
	assert.Equal(t, fsRe[1], defFSTypesExcluded,
		"our copy of upstream's fs-type default must be verbatim")
}

// TestEKSExclusionExtendsUpstreamRatherThanReplacing asserts our EKS default keeps
// every alternative upstream excludes. Supplying the flag REPLACES upstream's
// default rather than appending, so dropping one silently starts reporting
// filesystems upstream never did.
func TestEKSExclusionExtendsUpstreamRatherThanReplacing(t *testing.T) {
	for _, upstreamPath := range []string{
		"/dev", "/proc", "/sys",
		"/run/credentials/systemd-sysctl.service",
		"/var/lib/docker/overlay2/abc",
		"/var/lib/containers/storage/x",
	} {
		assert.Regexp(t, eksMountPointsExcluded, upstreamPath,
			"upstream excludes %q; our replacement default must too", upstreamPath)
	}
}

func TestEKSExclusionDropsPerPodMounts(t *testing.T) {
	// Real paths observed on the live cluster. These embed unique pod UIDs and
	// sandbox IDs, so leaving them in grows series count with pod density forever.
	rx := regexp.MustCompile(eksMountPointsExcluded)
	for _, mp := range []string{
		"/var/lib/kubelet/pods/1b450692-be04-479e-a795-94a8ec472c20/volumes/kubernetes.io~projected/kube-api-access-r9psr",
		"/run/containerd/io.containerd.grpc.v1.cri/sandboxes/20c938ab0c375c430150ca8ebbc45fd9371a5aaedb089de2beea68d1efb16e80/shm",
	} {
		assert.True(t, rx.MatchString(mp), "per-pod mount %q must be excluded", mp)
	}
}

func TestEKSExclusionKeepsRealFilesystems(t *testing.T) {
	// The important half. An over-broad regexp that swallowed /var or
	// /var/lib/kubelet would hide a real disk-full condition, which is a worse
	// failure than the cardinality it was meant to fix.
	rx := regexp.MustCompile(eksMountPointsExcluded)
	for _, mp := range []string{
		"/", "/boot/efi", "/run", "/tmp", "/var", "/var/lib", "/var/lib/kubelet",
		"/var/log", "/home", "/opt", "/var/lib/containerd",
	} {
		assert.False(t, rx.MatchString(mp), "real filesystem %q must NOT be excluded", mp)
	}
}

func TestFSTypeExclusionCoversPseudoFilesystems(t *testing.T) {
	rx := regexp.MustCompile(defFSTypesExcluded)
	for _, fsType := range []string{
		"sysfs", "proc", "devtmpfs", "cgroup2", "overlay", "nsfs", "tracefs", "bpf",
	} {
		assert.True(t, rx.MatchString(fsType), "pseudo filesystem %q must be excluded", fsType)
	}
	for _, fsType := range []string{"xfs", "ext4", "btrfs", "vfat", "tmpfs"} {
		assert.False(t, rx.MatchString(fsType), "real filesystem type %q must NOT be excluded", fsType)
	}
}

// --- the race fix: exactly one writer to the result slice -----------------

// TestStuckMountEntryTravelsThroughTheChannel is the regression test for
// upstream's data race. Upstream appends the stuck-mount entry directly to the
// result slice from the producer goroutine while the consumer loop appends from the
// channel — two unsynchronised writers. Here every result, including the stuck
// entry, goes through statCh so there is a single writer.
//
// Run under -race: if the stuck entry were appended directly, this would report a
// race on the slice.
func TestStuckMountEntryTravelsThroughTheChannel(t *testing.T) {
	c := newTestFilesystemCollector(t)

	// Pre-mark a mount that exists in the table as stuck.
	c.markStuck("/")

	stats, err := c.stats()
	require.NoError(t, err)

	var stuck *filesystemStats
	for i := range stats {
		if stats[i].labels.mountPoint == "/" {
			stuck = &stats[i]
		}
	}
	require.NotNil(t, stuck, "the stuck mount must still appear in the results")
	assert.Equal(t, float64(1), stuck.deviceError,
		"a stuck mount must report device_error=1 rather than being dropped")
	assert.Equal(t, "mountpoint timeout", stuck.labels.deviceError)
	assert.Zero(t, stuck.size, "a mount we could not stat must not report a size")
}

func TestConcurrentStatsAreRaceFree(t *testing.T) {
	// Collect runs collectors concurrently, and stuckMounts is shared mutable
	// state across scrapes. Run under -race.
	c := newTestFilesystemCollector(t)
	c.markStuck("/tmp")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.stats()
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
}

// --- stuck-mount lifecycle ------------------------------------------------

func TestStuckMountLifecycle(t *testing.T) {
	c := newTestFilesystemCollector(t)

	assert.False(t, c.isStuck("/mnt/nfs"))
	c.markStuck("/mnt/nfs")
	assert.True(t, c.isStuck("/mnt/nfs"))

	// A mount that starts responding again must be forgotten, or a transient hang
	// would permanently suppress a filesystem.
	c.clearStuck("/mnt/nfs")
	assert.False(t, c.isStuck("/mnt/nfs"))
}

func TestStatMountTimeoutMarksStuck(t *testing.T) {
	c := newTestFilesystemCollector(t)
	// A timeout short enough that even a fast statfs races it, so the timeout path
	// is exercised deterministically enough to be useful.
	c.mountTimeout = time.Nanosecond

	result := c.statMount(filesystemLabels{mountPoint: "/", device: "test", fsType: "xfs"})

	// Either the statfs won the race or the timeout did; both must be handled
	// without panicking, and a timeout must mark the mount.
	if result.deviceError == 1 && result.labels.deviceError == "mountpoint timeout" {
		assert.True(t, c.isStuck("/"), "a timed-out mount must be recorded as stuck")
	}
}

func TestStatMountOnMissingPathReportsDeviceError(t *testing.T) {
	c := newTestFilesystemCollector(t)

	result := c.statMount(filesystemLabels{
		mountPoint: "/definitely/not/a/real/mount/point",
		device:     "nowhere",
		fsType:     "xfs",
	})

	assert.Equal(t, float64(1), result.deviceError)
	assert.Equal(t, "statfs failed", result.labels.deviceError)
	// A failed statfs must not report zeros for size, which would read as a
	// zero-byte filesystem rather than an unknown one.
	assert.Zero(t, result.size)
}

// --- read-only detection --------------------------------------------------

func TestIsReadOnly(t *testing.T) {
	c := newTestFilesystemCollector(t)

	assert.True(t, c.isReadOnly(filesystemLabels{options: "ro"}))
	assert.True(t, c.isReadOnly(filesystemLabels{options: "nosuid,ro,relatime"}))
	assert.False(t, c.isReadOnly(filesystemLabels{options: "rw,relatime"}))
	assert.False(t, c.isReadOnly(filesystemLabels{options: ""}))
	// "ro" must match as a whole option, not as a substring of another.
	assert.False(t, c.isReadOnly(filesystemLabels{options: "rootcontext,nordirplus"}))
}

// --- major:minor parsing --------------------------------------------------

func TestSplitMajorMinor(t *testing.T) {
	tests := []struct {
		in           string
		major, minor string
	}{
		{"259:1", "259", "1"},
		{"0:24", "0", "24"},
		// Malformed input must not produce a nonsense label.
		{"garbage", "0", "0"},
		{"", "0", "0"},
		{"a:b", "0", "0"},
		{"259:", "0", "0"},
		{":1", "0", "0"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			major, minor := splitMajorMinor(tc.in)
			assert.Equal(t, tc.major, major)
			assert.Equal(t, tc.minor, minor)
		})
	}
}

func TestMountOptionsAreSorted(t *testing.T) {
	// Deterministic order so the read-only check and any future comparison do not
	// depend on map iteration order.
	got := mountOptions(map[string]string{"rw": "", "relatime": "", "attr2": ""})
	assert.Equal(t, []string{"attr2", "relatime", "rw"}, got)
	assert.Empty(t, mountOptions(map[string]string{}))
}

// --- emitted metric set ---------------------------------------------------

func TestFilesystemEmitsExpectedFamilies(t *testing.T) {
	c := newTestFilesystemCollector(t)

	ch := make(chan prometheus.Metric, 1024)
	require.NoError(t, c.Update(ch))
	close(ch)

	families := map[string]bool{}
	for m := range ch {
		d := m.Desc().String()
		for _, name := range []string{
			"node_filesystem_size_bytes", "node_filesystem_free_bytes",
			"node_filesystem_avail_bytes", "node_filesystem_files",
			"node_filesystem_files_free", "node_filesystem_purgeable_bytes",
			"node_filesystem_readonly", "node_filesystem_device_error",
			"node_filesystem_mount_info",
		} {
			if strings.Contains(d, name) {
				families[name] = true
			}
		}
	}

	// All nine families upstream emits on Linux. Missing any breaks a dashboard.
	assert.Len(t, families, 9, "got families: %v", families)
}

func TestFilesystemNoPerPodMountsOnThisHost(t *testing.T) {
	// The regression test for the cardinality fix, run against the real mount
	// table: no reported mount point may be a per-pod path.
	c := newTestFilesystemCollector(t)

	stats, err := c.stats()
	require.NoError(t, err)

	for _, s := range stats {
		assert.NotContains(t, s.labels.mountPoint, "/var/lib/kubelet/pods/")
		assert.NotContains(t, s.labels.mountPoint, "/sandboxes/")
	}
}

func TestFilesystemMissingProcfs(t *testing.T) {
	c, err := newFilesystemCollector(quietLogger(), Paths{ProcFS: "/definitely/absent"}.withDefaults())
	require.NoError(t, err, "construction must not probe procfs")

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get filesystem stats")
}

// --- helper ---------------------------------------------------------------

// newTestFilesystemCollector builds a collector against the real host mount table.
// Using the real table rather than a fixture is deliberate here: the exclusion
// regexps and the mountinfo parsing are the parts most likely to break, and a
// hand-written fixture would only prove the parser agrees with my idea of the
// format.
func newTestFilesystemCollector(t *testing.T) *filesystemCollector {
	t.Helper()
	c, err := newFilesystemCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	return c.(*filesystemCollector)
}

// --- construction failure paths -------------------------------------------

func TestFilesystemInvalidFiltersRejectedAtConstruction(t *testing.T) {
	// Unreachable with the compile-time constants, but the checks must stay: both
	// patterns become operator-configurable the moment they are wired to the chart,
	// and an invalid regexp should fail at startup rather than panic on first scrape.
	_, err := newFilesystemCollectorWithFilters(quietLogger(), Paths{}.withDefaults(),
		"([unclosed", defFSTypesExcluded)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid mount point exclusion")

	_, err = newFilesystemCollectorWithFilters(quietLogger(), Paths{}.withDefaults(),
		eksMountPointsExcluded, "([unclosed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid fs type exclusion")
}

// --- mount source failure and the device_error short-circuit --------------

func TestFilesystemMountSourceErrorIsWrapped(t *testing.T) {
	c := newTestFilesystemCollector(t)
	c.mountSource = func() ([]filesystemLabels, error) {
		return nil, assert.AnError
	}

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get filesystem stats")
}

func TestFilesystemDeviceErrorSuppressesSizeMetrics(t *testing.T) {
	// A mount we could not stat must report device_error=1 and mount_info, but NOT
	// size/free/avail: emitting zeros there would read as a full disk on every
	// dashboard.
	c := newTestFilesystemCollector(t)
	c.mountSource = func() ([]filesystemLabels, error) {
		return []filesystemLabels{{
			device: "dead-nfs", mountPoint: "/mnt/dead", fsType: "nfs4",
			major: "0", minor: "42",
		}}, nil
	}
	c.markStuck("/mnt/dead")

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	var sawDeviceError, sawMountInfo bool
	for m := range ch {
		d := m.Desc().String()
		switch {
		case strings.Contains(d, "node_filesystem_device_error"):
			sawDeviceError = true
		case strings.Contains(d, "node_filesystem_mount_info"):
			sawMountInfo = true
		case strings.Contains(d, "node_filesystem_size_bytes"),
			strings.Contains(d, "node_filesystem_free_bytes"),
			strings.Contains(d, "node_filesystem_avail_bytes"):
			t.Errorf("a mount with device_error must not report sizes: %s", d)
		}
	}
	assert.True(t, sawDeviceError, "device_error must be reported for a stuck mount")
	assert.True(t, sawMountInfo, "mount_info must still be reported for a stuck mount")
}

func TestFilesystemDeduplicatesMountInfoPerDeviceAndMountPoint(t *testing.T) {
	// A device can appear under several mount points, and Prometheus rejects
	// duplicate label sets, so mount_info is emitted once per device+mountpoint.
	c := newTestFilesystemCollector(t)
	c.mountSource = func() ([]filesystemLabels, error) {
		return []filesystemLabels{
			{device: "/dev/x", mountPoint: "/a", fsType: "xfs", major: "1", minor: "2"},
			{device: "/dev/x", mountPoint: "/a", fsType: "xfs", major: "1", minor: "2"},
			{device: "/dev/x", mountPoint: "/b", fsType: "xfs", major: "1", minor: "2"},
		}, nil
	}

	ch := make(chan prometheus.Metric, 128)
	require.NoError(t, c.Update(ch))
	close(ch)

	mountInfo := 0
	for m := range ch {
		if strings.Contains(m.Desc().String(), "node_filesystem_mount_info") {
			mountInfo++
		}
	}
	assert.Equal(t, 2, mountInfo, "two distinct device+mountpoint pairs, so two mount_info series")
}

func TestFilesystemFallsBackToSelfMountsUnderHidepid(t *testing.T) {
	// Under hidepid, /proc/1 is unreadable and upstream falls back to this
	// process's own mountinfo. On a healthy host PID 1 is always readable, so this
	// branch is only reachable with the read injected.
	c := newTestFilesystemCollector(t)
	c.readPID1Mounts = func(procfs.FS) ([]*procfs.MountInfo, error) {
		return nil, assert.AnError
	}
	c.readSelfMounts = func() ([]*procfs.MountInfo, error) {
		return []*procfs.MountInfo{{
			Source: "/dev/fallback", MountPoint: "/fallback", FSType: "xfs",
			MajorMinorVer: "1:2", Options: map[string]string{"rw": ""},
		}}, nil
	}

	labels, err := c.mountPoints()
	require.NoError(t, err)
	require.Len(t, labels, 1)
	assert.Equal(t, "/fallback", labels[0].mountPoint)
	assert.Equal(t, "1", labels[0].major)
	assert.Equal(t, "2", labels[0].minor)
}

func TestFilesystemBothMountReadsFailing(t *testing.T) {
	// If neither read works there is nothing to report, and the error must say so
	// rather than returning an empty list that looks like "no filesystems".
	c := newTestFilesystemCollector(t)
	c.readPID1Mounts = func(procfs.FS) ([]*procfs.MountInfo, error) { return nil, assert.AnError }
	c.readSelfMounts = func() ([]*procfs.MountInfo, error) { return nil, assert.AnError }

	_, err := c.mountPoints()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read mount info")
}

func TestFilesystemMountPointEscapesAreDecoded(t *testing.T) {
	// fstab escapes spaces as \040 and tabs as \011. Left encoded, the label would
	// not match what an operator sees in `mount`.
	c := newTestFilesystemCollector(t)
	c.readPID1Mounts = func(procfs.FS) ([]*procfs.MountInfo, error) {
		return []*procfs.MountInfo{{
			Source: "/dev/x", MountPoint: `/mnt/with\040space`, FSType: "xfs",
			MajorMinorVer: "1:2", Options: map[string]string{},
		}}, nil
	}

	labels, err := c.mountPoints()
	require.NoError(t, err)
	require.Len(t, labels, 1)
	assert.Equal(t, "/mnt/with space", labels[0].mountPoint)
}

func TestFilesystemDefaultPID1ReadHandlesProcLookupFailure(t *testing.T) {
	// Exercises the production readPID1 closure rather than an injected one, so the
	// fs.Proc(1) failure path is covered. A procfs rooted at a directory with no
	// "1" entry makes Proc(1) fail, which then falls back to self mounts.
	root := t.TempDir()
	c, err := newFilesystemCollector(quietLogger(), Paths{ProcFS: root}.withDefaults())
	require.NoError(t, err)
	fc := c.(*filesystemCollector)

	// Self mounts are injected so the test does not depend on the host's table for
	// its assertion; only the PID 1 lookup uses the real code path.
	fc.readSelfMounts = func() ([]*procfs.MountInfo, error) {
		return []*procfs.MountInfo{{
			Source: "/dev/y", MountPoint: "/y", FSType: "ext4",
			MajorMinorVer: "8:1", Options: map[string]string{},
		}}, nil
	}

	labels, err := fc.mountPoints()
	require.NoError(t, err)
	require.Len(t, labels, 1)
	assert.Equal(t, "/y", labels[0].mountPoint)
}
