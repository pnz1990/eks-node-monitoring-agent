package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/filesystem_linux.go
//                 node_exporter/collector/filesystem_common.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// THIS COLLECTOR CARRIES TWO FIXES, both recorded in OPEN-QUESTIONS.md Q7.
//
// FIX 1 — upstream data race. Upstream's GetStats declares `stats := []filesystemStats{}`
// on the calling goroutine and then appends to it from TWO goroutines with no
// mutex: the spawned goroutine appends a deviceError entry for each already-stuck
// mount, while the caller appends everything drained from statChan. Those overlap,
// so a racing append can lose entries or tear the slice header. It only fires when
// a stuck mount is already recorded, which is why it survives on healthy hosts —
// `go test -race` would not reach it without a hung mount. Here the stuck-mount
// entries are sent through the same channel as everything else, so there is
// exactly one writer to the result slice.
//
// FIX 2 — pod-mount cardinality. The mount table read is PID 1's, and the agent
// runs with hostPID: true, so PID 1 is host init and its namespace contains every
// per-pod mount. Measured on the dependency branch: 17 filesystem series versus
// upstream's 4 on an idle 20-pod node, the extra 13 being containerd sandbox shm
// and kubelet projected volumes whose paths embed unique pod UIDs. That is
// unbounded high-churn cardinality. The default exclusion below extends upstream's
// own defMountPointsExcluded, which already excludes var/lib/docker/.+ for exactly
// this reason and simply has no containerd-on-Kubernetes entry.
//
// THE STUCK-MOUNT WATCHER IS NOT OPTIONAL. statfs() on a dead NFS or network mount
// blocks in the kernel and cannot be interrupted. Without a timeout the collector
// hangs forever, which is upstream issue #1353. The watcher marks such a mount and
// subsequent scrapes skip it, reporting device_error=1 instead of blocking.

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"
)

const filesystemSubsystem = "filesystem"

// Upstream's defaults, copied verbatim. Asserted against upstream's source in
// tests, because a change here silently changes which filesystems are reported.
const (
	defMountPointsExcluded = "^/(dev|proc|run/credentials/.+|sys|var/lib/docker/.+|var/lib/containers/storage/.+)($|/)"
	defFSTypesExcluded     = "^(autofs|binfmt_misc|bpf|cgroup2?|configfs|debugfs|devpts|devtmpfs|fusectl|hugetlbfs|iso9660|mqueue|nsfs|overlay|proc|procfs|pstore|rpc_pipefs|securityfs|selinuxfs|squashfs|erofs|sysfs|tracefs)$"
)

// eksMountPointsExcluded extends upstream's default with the per-pod paths a
// Kubernetes node creates. Supplying the flag REPLACES upstream's default rather
// than appending, so upstream's alternatives are repeated here verbatim.
const eksMountPointsExcluded = "^/(dev|proc|run/credentials/.+|sys|var/lib/docker/.+|var/lib/containers/storage/.+" +
	// containerd pod sandbox shm: /run/containerd/.../sandboxes/<id>/shm
	"|run/containerd/.+/sandboxes/.+" +
	// kubelet per-pod volumes, including projected service-account tokens
	"|var/lib/kubelet/pods/.+" +
	")($|/)"

// defaultMountTimeout bounds a single statfs call. Upstream's default, kept
// identical.
const defaultMountTimeout = 5 * time.Second

// defaultStatWorkers is how many mounts are stat'ed concurrently. Upstream's
// default is 4.
const defaultStatWorkers = 4

func init() {
	register("filesystem", true, newFilesystemCollector)
}

// filesystemLabels identifies one mount.
type filesystemLabels struct {
	device      string
	mountPoint  string
	fsType      string
	options     string
	deviceError string
	major       string
	minor       string
}

// filesystemStats is one mount's measurements.
type filesystemStats struct {
	labels      filesystemLabels
	size        float64
	free        float64
	avail       float64
	files       float64
	filesFree   float64
	purgeable   float64
	ro          float64
	deviceError float64
}

type filesystemCollector struct {
	paths        Paths
	logger       *slog.Logger
	mountFilter  *regexp.Regexp
	fsTypeFilter *regexp.Regexp
	mountTimeout time.Duration
	statWorkers  int

	// stuckMounts records mounts whose statfs previously timed out, so later
	// scrapes skip them instead of blocking again. Shared across scrapes and
	// guarded, since Collect runs collectors concurrently.
	stuckMu     sync.Mutex
	stuckMounts map[string]struct{}

	// mountSource overrides the mount table read, so tests can drive the fallback
	// and error paths without an unreadable /proc/1. Nil in production.
	mountSource func() ([]filesystemLabels, error)

	// readPID1Mounts and readSelfMounts are the two mount-table reads, injectable
	// so the hidepid fallback and its failure are reachable in tests. On a healthy
	// host PID 1 is always readable, so neither branch executes in production
	// tests. Nil means use the real procfs reads.
	readPID1Mounts func(procfs.FS) ([]*procfs.MountInfo, error)
	readSelfMounts func() ([]*procfs.MountInfo, error)

	sizeDesc, freeDesc, availDesc  *prometheus.Desc
	filesDesc, filesFreeDesc       *prometheus.Desc
	purgeableDesc, roDesc          *prometheus.Desc
	deviceErrorDesc, mountInfoDesc *prometheus.Desc
}

func newFilesystemCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	return newFilesystemCollectorWithFilters(logger, paths, eksMountPointsExcluded, defFSTypesExcluded)
}

// newFilesystemCollectorWithFilters is split out so the invalid-regexp branches are
// reachable in tests. With the compile-time constants they can never fail, but the
// checks must stay: both patterns become operator-configurable the moment they are
// wired to the chart, and an invalid regexp should fail at startup rather than
// panic on first scrape.
func newFilesystemCollectorWithFilters(logger *slog.Logger, paths Paths, mountExpr, fsTypeExpr string) (Collector, error) {
	mountFilter, err := regexp.Compile(mountExpr)
	if err != nil {
		return nil, fmt.Errorf("invalid mount point exclusion %q: %w", mountExpr, err)
	}
	fsTypeFilter, err := regexp.Compile(fsTypeExpr)
	if err != nil {
		return nil, fmt.Errorf("invalid fs type exclusion %q: %w", fsTypeExpr, err)
	}

	labels := []string{"device", "mountpoint", "fstype", "device_error"}
	desc := func(name, help string, labelNames []string) *prometheus.Desc {
		return prometheus.NewDesc(
			prometheus.BuildFQName(namespace, filesystemSubsystem, name), help, labelNames, nil)
	}

	return &filesystemCollector{
		paths:        paths,
		logger:       logger,
		mountFilter:  mountFilter,
		fsTypeFilter: fsTypeFilter,
		mountTimeout: defaultMountTimeout,
		statWorkers:  defaultStatWorkers,
		stuckMounts:  map[string]struct{}{},

		sizeDesc:      desc("size_bytes", "Filesystem size in bytes.", labels),
		freeDesc:      desc("free_bytes", "Filesystem free space in bytes.", labels),
		availDesc:     desc("avail_bytes", "Filesystem space available to non-root users in bytes.", labels),
		filesDesc:     desc("files", "Filesystem total file nodes.", labels),
		filesFreeDesc: desc("files_free", "Filesystem total free file nodes.", labels),
		purgeableDesc: desc("purgeable_bytes", "Filesystem space available including purgeable space (MacOS specific).", labels),
		roDesc:        desc("readonly", "Filesystem read-only status.", labels),
		deviceErrorDesc: desc("device_error",
			"Whether an error occurred while getting statistics for the given device.", labels),
		mountInfoDesc: desc("mount_info", "Filesystem mount information.",
			[]string{"device", "major", "minor", "mountpoint"}),
	}, nil
}

func (c *filesystemCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := c.stats()
	if err != nil {
		return fmt.Errorf("couldn't get filesystem stats: %w", err)
	}

	// Deduplicate by mount point. Upstream does this because a device can appear
	// under several mount points and Prometheus rejects duplicate label sets.
	seenDevices := map[string]struct{}{}
	for _, s := range stats {
		l := []string{s.labels.device, s.labels.mountPoint, s.labels.fsType, s.labels.deviceError}

		if _, dup := seenDevices[s.labels.device+s.labels.mountPoint]; !dup {
			seenDevices[s.labels.device+s.labels.mountPoint] = struct{}{}
			ch <- prometheus.MustNewConstMetric(c.mountInfoDesc, prometheus.GaugeValue, 1.0,
				s.labels.device, s.labels.major, s.labels.minor, s.labels.mountPoint)
		}

		ch <- prometheus.MustNewConstMetric(c.deviceErrorDesc, prometheus.GaugeValue, s.deviceError, l...)
		if s.deviceError > 0 {
			// A mount we could not stat has no meaningful sizes; emitting zeros
			// would read as a full disk.
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.sizeDesc, prometheus.GaugeValue, s.size, l...)
		ch <- prometheus.MustNewConstMetric(c.freeDesc, prometheus.GaugeValue, s.free, l...)
		ch <- prometheus.MustNewConstMetric(c.availDesc, prometheus.GaugeValue, s.avail, l...)
		ch <- prometheus.MustNewConstMetric(c.filesDesc, prometheus.GaugeValue, s.files, l...)
		ch <- prometheus.MustNewConstMetric(c.filesFreeDesc, prometheus.GaugeValue, s.filesFree, l...)
		ch <- prometheus.MustNewConstMetric(c.purgeableDesc, prometheus.GaugeValue, s.purgeable, l...)
		ch <- prometheus.MustNewConstMetric(c.roDesc, prometheus.GaugeValue, s.ro, l...)
	}
	return nil
}

// stats collects every non-excluded mount, bounding each statfs call.
//
// Unlike upstream, every result — including the skip entry for an already-stuck
// mount — travels through one channel and is appended by a single goroutine. That
// removes the data race described in the file header: upstream appends to the
// result slice from both the producer goroutine and the consumer loop.
func (c *filesystemCollector) stats() ([]filesystemStats, error) {
	mounts, err := c.mountPoints()
	if err != nil {
		return nil, err
	}

	labelCh := make(chan filesystemLabels)
	statCh := make(chan filesystemStats)

	var workers sync.WaitGroup
	for i := 0; i < c.statWorkers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for labels := range labelCh {
				statCh <- c.statMount(labels)
			}
		}()
	}

	go func() {
		defer close(labelCh)
		for _, labels := range mounts {
			if c.mountFilter.MatchString(labels.mountPoint) {
				c.logger.Debug("ignoring mount point", "mountpoint", labels.mountPoint)
				continue
			}
			if c.fsTypeFilter.MatchString(labels.fsType) {
				c.logger.Debug("ignoring fs type", "type", labels.fsType)
				continue
			}

			if c.isStuck(labels.mountPoint) {
				// Send rather than append: this is the fix for upstream's race.
				labels.deviceError = "mountpoint timeout"
				statCh <- filesystemStats{labels: labels, deviceError: 1}
				c.logger.Debug("mount point is in an unresponsive state, skipping",
					"mountpoint", labels.mountPoint)
				continue
			}
			labelCh <- labels
		}
	}()

	go func() {
		workers.Wait()
		close(statCh)
	}()

	// Single writer to the result slice.
	var out []filesystemStats
	for s := range statCh {
		out = append(out, s)
	}
	return out, nil
}

// statMount runs statfs with a timeout.
//
// statfs on a dead network mount blocks in the kernel and cannot be interrupted,
// so it runs on its own goroutine that is abandoned if it does not return in time.
// The mount is then recorded as stuck so later scrapes skip it rather than
// accumulating blocked goroutines — upstream issue #1353.
func (c *filesystemCollector) statMount(labels filesystemLabels) filesystemStats {
	result := filesystemStats{labels: labels}
	if c.isReadOnly(labels) {
		result.ro = 1
	}

	done := make(chan *unix.Statfs_t, 1)
	go func() {
		var buf unix.Statfs_t
		if err := unix.Statfs(c.paths.rootPath(labels.mountPoint), &buf); err != nil {
			done <- nil
			return
		}
		done <- &buf
	}()

	timer := time.NewTimer(c.mountTimeout)
	defer timer.Stop()

	select {
	case buf := <-done:
		c.clearStuck(labels.mountPoint)
		if buf == nil {
			result.labels.deviceError = "statfs failed"
			result.deviceError = 1
			return result
		}
		result.size = float64(buf.Blocks) * float64(buf.Bsize)
		result.free = float64(buf.Bfree) * float64(buf.Bsize)
		result.avail = float64(buf.Bavail) * float64(buf.Bsize)
		result.files = float64(buf.Files)
		result.filesFree = float64(buf.Ffree)
		return result

	case <-timer.C:
		// The statfs goroutine is left running; it cannot be cancelled. Recording
		// the mount as stuck is what stops the next scrape from spawning another.
		c.markStuck(labels.mountPoint)
		c.logger.Error("statfs timed out; marking mount as stuck",
			"mountpoint", labels.mountPoint, "timeout", c.mountTimeout)
		result.labels.deviceError = "mountpoint timeout"
		result.deviceError = 1
		return result
	}
}

func (c *filesystemCollector) isStuck(mountPoint string) bool {
	c.stuckMu.Lock()
	defer c.stuckMu.Unlock()
	_, stuck := c.stuckMounts[mountPoint]
	return stuck
}

func (c *filesystemCollector) markStuck(mountPoint string) {
	c.stuckMu.Lock()
	defer c.stuckMu.Unlock()
	c.stuckMounts[mountPoint] = struct{}{}
}

// clearStuck forgets a mount that has started responding again, so a transient
// hang does not permanently suppress a filesystem.
func (c *filesystemCollector) clearStuck(mountPoint string) {
	c.stuckMu.Lock()
	defer c.stuckMu.Unlock()
	delete(c.stuckMounts, mountPoint)
}

// isReadOnly reports whether the mount options mark the filesystem read-only.
func (c *filesystemCollector) isReadOnly(labels filesystemLabels) bool {
	for _, opt := range strings.Split(labels.options, ",") {
		if opt == "ro" {
			return true
		}
	}
	return false
}

// mountPoints reads the mount table.
//
// It reads PID 1's mountinfo, falling back to self's if PID 1 is unreadable due to
// hidepid — upstream's behaviour. Note that under hostPID this is the HOST init
// namespace, which is what makes the pod-mount exclusion necessary.
func (c *filesystemCollector) mountPoints() ([]filesystemLabels, error) {
	fs, err := procfs.NewFS(c.paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", c.paths.ProcFS, err)
	}

	if c.mountSource != nil {
		return c.mountSource()
	}

	// PID 1's mount table is the authoritative one. Under hostPID that is the HOST
	// init namespace, which is exactly why the pod-mount exclusion above is needed.
	//
	// Falls back to this process's own mountinfo if PID 1 is unreadable, which
	// happens under hidepid. Upstream does the same; the fallback sees fewer mounts
	// but is better than reporting none.
	readPID1 := c.readPID1Mounts
	if readPID1 == nil {
		readPID1 = func(fs procfs.FS) ([]*procfs.MountInfo, error) {
			proc, err := fs.Proc(1)
			if err != nil {
				return nil, err
			}
			return proc.MountInfo()
		}
	}
	readSelf := c.readSelfMounts
	if readSelf == nil {
		readSelf = procfs.GetMounts
	}

	mounts, err := readPID1(fs)
	if err != nil {
		c.logger.Debug("could not read PID 1 mountinfo, falling back to self", "err", err)
		mounts, err = readSelf()
		if err != nil {
			return nil, fmt.Errorf("failed to read mount info: %w", err)
		}
	}

	out := make([]filesystemLabels, 0, len(mounts))
	for _, m := range mounts {
		major, minor := splitMajorMinor(m.MajorMinorVer)
		// fstab escapes spaces and tabs; decode them so the label reads correctly.
		mountPoint := strings.ReplaceAll(m.MountPoint, `\040`, " ")
		mountPoint = strings.ReplaceAll(mountPoint, `\011`, "\t")

		out = append(out, filesystemLabels{
			device:     m.Source,
			mountPoint: c.paths.stripRootFS(mountPoint),
			fsType:     m.FSType,
			options:    strings.Join(mountOptions(m.Options), ","),
			major:      major,
			minor:      minor,
		})
	}
	return out, nil
}

// splitMajorMinor parses procfs's "major:minor" device version.
func splitMajorMinor(v string) (string, string) {
	major, minor, found := strings.Cut(v, ":")
	if !found {
		return "0", "0"
	}
	// Validated rather than passed through, so a malformed value cannot produce a
	// nonsense label.
	if _, err := strconv.Atoi(major); err != nil {
		return "0", "0"
	}
	if _, err := strconv.Atoi(minor); err != nil {
		return "0", "0"
	}
	return major, minor
}

// mountOptions flattens procfs's option map into a stable, sorted list.
func mountOptions(opts map[string]string) []string {
	out := make([]string, 0, len(opts))
	for k := range opts {
		out = append(out, k)
	}
	// Sorted so the read-only check and any future option comparison are
	// deterministic rather than depending on map iteration order.
	sort.Strings(out)
	return out
}
