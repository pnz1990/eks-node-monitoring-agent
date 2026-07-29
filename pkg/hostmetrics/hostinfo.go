package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/uname.go, uname_linux.go
//                 node_exporter/collector/entropy_linux.go
//                 node_exporter/collector/filefd_linux.go
//                 node_exporter/collector/schedstat_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015, 2018, 2019 The Prometheus Authors, Apache-2.0
//
// Four small collectors that each read one thing. Grouped in one file because each
// is under 40 lines of real logic and splitting them would be four files of
// boilerplate; they are still four independently registered collectors, so an
// operator can disable them individually and a failure in one cannot affect another.
//
// THE ONE TRAP WORTH FLAGGING UP FRONT: schedstat uses NANOseconds (1e9) while
// pressure, in this same package, uses MICROseconds (1e6). Copying the constant
// between them is a silent 1000x error in either direction, and both produce
// plausible-looking values. Each divisor is named and asserted.
//
// WHY filefd MATTERS ON EKS: node_filefd_allocated against node_filefd_maximum is
// the fd exhaustion signal. A node running hundreds of pods, each with its own
// sockets and log pipes, can approach fs.file-max — and when it does, everything
// fails at once in ways that look unrelated to file descriptors.

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// uname
// ---------------------------------------------------------------------------

func init() {
	register("uname", true, newUnameCollector)
}

var unameDesc = prometheus.NewDesc(
	prometheus.BuildFQName(namespace, "uname", "info"),
	"Labeled system information as provided by the uname system call.",
	// Label ORDER is part of the contract only insofar as it must match the values
	// passed at emit time; the names themselves are what queries select on. Both are
	// upstream's, verbatim.
	[]string{"sysname", "release", "version", "machine", "nodename", "domainname"},
	nil,
)

type unameCollector struct {
	logger *slog.Logger

	// unameInfo is injectable because the syscall cannot be made to fail on a
	// working host, and because the emitted labels are otherwise untestable without
	// asserting against this specific machine's kernel version.
	unameInfo func() (unameFields, error)
}

// unameFields is the subset of struct utsname that becomes labels.
type unameFields struct {
	SysName    string
	Release    string
	Version    string
	Machine    string
	NodeName   string
	DomainName string
}

func newUnameCollector(logger *slog.Logger, _ Paths) (Collector, error) {
	// NOTE: uname(2) is a syscall, so this collector ignores Paths entirely. It
	// therefore reports the CONTAINER's uname, not the host's, if they ever differ
	// — but they cannot: the UTS namespace is shared when hostNetwork/hostPID are
	// set, which the shipped DaemonSet does. Recorded because it is the one
	// collector a rebased procfs path would not fix.
	return &unameCollector{logger: logger, unameInfo: readUname}, nil
}

func (c *unameCollector) Update(ch chan<- prometheus.Metric) error {
	u, err := c.unameInfo()
	if err != nil {
		return fmt.Errorf("failed to read uname: %w", err)
	}
	ch <- prometheus.MustNewConstMetric(unameDesc, prometheus.GaugeValue, 1,
		u.SysName, u.Release, u.Version, u.Machine, u.NodeName, u.DomainName)
	return nil
}

// readUname calls uname(2).
func readUname() (unameFields, error) {
	return readUnameWith(unix.Uname)
}

// readUnameWith is the seam. uname(2) cannot fail on a working host -- it takes no
// arguments that could be invalid -- so the error branch is otherwise unreachable
// and would ship untested.
func readUnameWith(uname func(*unix.Utsname) error) (unameFields, error) {
	var utsname unix.Utsname
	if err := uname(&utsname); err != nil {
		return unameFields{}, fmt.Errorf("uname syscall failed: %w", err)
	}
	// ByteSliceToString stops at the first NUL. Using string(buf[:]) directly would
	// embed the trailing NULs in the label value, which Prometheus accepts and every
	// dashboard then fails to match.
	return unameFields{
		SysName:    unix.ByteSliceToString(utsname.Sysname[:]),
		Release:    unix.ByteSliceToString(utsname.Release[:]),
		Version:    unix.ByteSliceToString(utsname.Version[:]),
		Machine:    unix.ByteSliceToString(utsname.Machine[:]),
		NodeName:   unix.ByteSliceToString(utsname.Nodename[:]),
		DomainName: unix.ByteSliceToString(utsname.Domainname[:]),
	}, nil
}

// ---------------------------------------------------------------------------
// entropy
// ---------------------------------------------------------------------------

func init() {
	register("entropy", true, newEntropyCollector)
}

var (
	// Empty subsystem: node_entropy_available_bits, not
	// node_entropy_entropy_available_bits.
	entropyAvailDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "entropy_available_bits"),
		"Bits of available entropy.", nil, nil,
	)
	entropyPoolSizeDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "entropy_pool_size_bits"),
		"Bits of entropy pool.", nil, nil,
	)
)

type entropyCollector struct {
	fs     procfs.FS
	logger *slog.Logger

	// kernelRandom is injectable so the nil-field branches are reachable: on a real
	// host both values are always present.
	kernelRandom func() (procfs.KernelRandom, error)
}

func newEntropyCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	c := &entropyCollector{fs: fs, logger: logger}
	c.kernelRandom = c.fs.KernelRandom
	return c, nil
}

func (c *entropyCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := c.kernelRandom()
	if err != nil {
		return fmt.Errorf("failed to get kernel random stats: %w", err)
	}

	// Both fields are pointers, and upstream returns an ERROR rather than ErrNoData
	// when either is nil. Preserved: a kernel that exposes /proc/sys/kernel/random
	// but not entropy_avail is genuinely unexpected, unlike a kernel that lacks the
	// directory entirely.
	if stats.EntropyAvaliable == nil {
		return errors.New("couldn't get entropy_avail")
	}
	ch <- prometheus.MustNewConstMetric(entropyAvailDesc, prometheus.GaugeValue,
		float64(*stats.EntropyAvaliable))

	if stats.PoolSize == nil {
		return errors.New("couldn't get entropy poolsize")
	}
	ch <- prometheus.MustNewConstMetric(entropyPoolSizeDesc, prometheus.GaugeValue,
		float64(*stats.PoolSize))

	return nil
}

// ---------------------------------------------------------------------------
// filefd
// ---------------------------------------------------------------------------

const fileFDSubsystem = "filefd"

func init() {
	register(fileFDSubsystem, true, newFileFDCollector)
}

type fileFDCollector struct {
	logger *slog.Logger
	paths  Paths
}

func newFileFDCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	return &fileFDCollector{logger: logger, paths: paths}, nil
}

func (c *fileFDCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := parseFileFDStats(c.paths.procPath("sys", "fs", "file-nr"))
	if err != nil {
		return fmt.Errorf("couldn't get file-nr: %w", err)
	}

	for name, value := range stats {
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("invalid value %s in file-nr: %w", value, err)
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, fileFDSubsystem, name),
				fmt.Sprintf("File descriptor statistics: %s.", name),
				nil, nil,
			),
			prometheus.GaugeValue, v,
		)
	}
	return nil
}

// parseFileFDStats reads /proc/sys/fs/file-nr, a single line of three
// TAB-separated values.
//
// THE MIDDLE VALUE IS DELIBERATELY SKIPPED. It is "free file handles", which has
// been hardcoded to 0 since Linux 2.6 — the kernel no longer tracks it. Emitting it
// would publish a permanent zero that looks like a measurement, and an operator
// computing "allocated + free" would get a number that is wrong by construction.
func parseFileFDStats(filename string) (map[string]string, error) {
	content, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	// Split on TAB specifically, not on whitespace: the values are tab-separated and
	// splitting on spaces would return one field and fail the length check below.
	parts := bytes.Split(bytes.TrimSpace(content), []byte("\t"))
	if len(parts) < 3 {
		return nil, fmt.Errorf("unexpected number of file stats in %q: got %d fields, want 3",
			filename, len(parts))
	}

	return map[string]string{
		"allocated": string(parts[0]),
		"maximum":   string(parts[2]),
	}, nil
}

// ---------------------------------------------------------------------------
// schedstat
// ---------------------------------------------------------------------------

// schedstatNanosecondsPerSecond converts the kernel's nanosecond totals.
//
// NANOseconds here, but MICROseconds in pressure.go. Copying either constant to the
// other collector is a silent 1000x error that still produces plausible values.
const schedstatNanosecondsPerSecond = 1e9

func init() {
	register("schedstat", true, newSchedstatCollector)
}

var (
	schedstatRunningDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "schedstat", "running_seconds_total"),
		"Number of seconds CPU spent running a process.",
		[]string{"cpu"}, nil,
	)
	schedstatWaitingDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "schedstat", "waiting_seconds_total"),
		"Number of seconds spent by processing waiting for this CPU.",
		[]string{"cpu"}, nil,
	)
	schedstatTimeslicesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "schedstat", "timeslices_total"),
		"Number of timeslices executed by CPU.",
		[]string{"cpu"}, nil,
	)
)

type schedstatCollector struct {
	fs     procfs.FS
	logger *slog.Logger

	// schedstat is injectable so the ErrNotExist and hard-error branches are
	// reachable: /proc/schedstat exists on every kernel this runs on.
	schedstat func() (*procfs.Schedstat, error)
}

func newSchedstatCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	c := &schedstatCollector{fs: fs, logger: logger}
	c.schedstat = c.fs.Schedstat
	return c, nil
}

func (c *schedstatCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := c.schedstat()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// CONFIG_SCHEDSTATS not enabled. A supported configuration, so not a
			// failure.
			c.logger.Debug("schedstat file does not exist")
			return ErrNoData
		}
		return err
	}

	for _, cpu := range stats.CPUs {
		// The cpu label comes from the file rather than the loop index: /proc/schedstat
		// lists CPUs by name ("cpu0", "cpu12") and offline CPUs are absent, so an
		// index would silently renumber the remaining ones.
		ch <- prometheus.MustNewConstMetric(schedstatRunningDesc, prometheus.CounterValue,
			float64(cpu.RunningNanoseconds)/schedstatNanosecondsPerSecond, cpu.CPUNum)
		ch <- prometheus.MustNewConstMetric(schedstatWaitingDesc, prometheus.CounterValue,
			float64(cpu.WaitingNanoseconds)/schedstatNanosecondsPerSecond, cpu.CPUNum)
		// Timeslices is a plain count, not a duration: no conversion.
		ch <- prometheus.MustNewConstMetric(schedstatTimeslicesDesc, prometheus.CounterValue,
			float64(cpu.RunTimeslices), cpu.CPUNum)
	}
	return nil
}
