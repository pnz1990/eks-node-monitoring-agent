package hostmetrics

// Tests for the four small collectors in hostinfo.go: uname, entropy, filefd and
// schedstat.
//
// The one cross-collector hazard gets its own test: schedstat uses NANOseconds
// while pressure, in this same package, uses MICROseconds. Copying either constant
// to the other is a silent 1000x error that still produces plausible values.

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// --- uname ----------------------------------------------------------------

func TestUnameEmitsAllSixLabels(t *testing.T) {
	c, err := newUnameCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	c.(*unameCollector).unameInfo = func() (unameFields, error) {
		return unameFields{
			SysName: "Linux", Release: "6.12.0", Version: "#1 SMP",
			Machine: "x86_64", NodeName: "ip-10-0-0-1", DomainName: "(none)",
		}, nil
	}

	labels := singleMetricLabels(t, c, "node_uname_info")
	assert.Equal(t, map[string]string{
		"sysname": "Linux", "release": "6.12.0", "version": "#1 SMP",
		"machine": "x86_64", "nodename": "ip-10-0-0-1", "domainname": "(none)",
	}, labels, "all six labels must be populated from the right fields")
}

func TestUnameLabelValuesAreNotSwapped(t *testing.T) {
	// Six string labels emitted positionally: a transposed pair would produce a
	// metric with the right name and label KEYS and wrong values, which no
	// structural comparison catches. Each value is made distinguishable.
	c, err := newUnameCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	c.(*unameCollector).unameInfo = func() (unameFields, error) {
		return unameFields{
			SysName: "SYSNAME", Release: "RELEASE", Version: "VERSION",
			Machine: "MACHINE", NodeName: "NODENAME", DomainName: "DOMAINNAME",
		}, nil
	}

	for key, want := range map[string]string{
		"sysname": "SYSNAME", "release": "RELEASE", "version": "VERSION",
		"machine": "MACHINE", "nodename": "NODENAME", "domainname": "DOMAINNAME",
	} {
		assert.Equal(t, want, singleMetricLabels(t, c, "node_uname_info")[key],
			"label %q holds the wrong field", key)
	}
}

func TestUnameLabelNamesMatchUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/uname.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	// Anchored on the variable declaration and the []string block, because the
	// label list sits on its own lines AFTER the help string -- my first attempt
	// assumed `"uname", "info",` was followed directly by `[]string{` and matched
	// nothing. Caught by require.NotNil rather than by silently asserting over an
	// empty list, which is the whole reason that guard is there.
	m := regexp.MustCompile(`(?s)var unameDesc = prometheus\.NewDesc\(.*?\[\]string\{(.*?)\},`).
		FindSubmatch(data)
	require.NotNil(t, m, "failed to extract upstream's label list; the regexp may be stale")

	upstream := regexp.MustCompile(`"([a-z]+)"`).FindAllSubmatch(m[1], -1)
	require.Len(t, upstream, 6, "expected 6 upstream labels, got %d", len(upstream))

	for _, want := range upstream {
		assert.Contains(t, unameDesc.String(), string(want[1]),
			"upstream label %q is missing", want[1])
	}
}

func TestUnameReadsRealSyscall(t *testing.T) {
	// The production path. Values are checked for shape rather than content, since
	// asserting this machine's kernel version would be a test of the machine.
	got, err := readUname()
	require.NoError(t, err)
	assert.Equal(t, "Linux", got.SysName)
	assert.NotEmpty(t, got.Release)
	assert.NotEmpty(t, got.Machine)
	assert.NotEmpty(t, got.NodeName)
}

func TestUnameStringsHaveNoTrailingNULs(t *testing.T) {
	// struct utsname fields are fixed-size NUL-padded char arrays.
	// unix.ByteSliceToString stops at the first NUL; string(buf[:]) would embed the
	// padding, which Prometheus ACCEPTS as a label value and every dashboard then
	// silently fails to match.
	got, err := readUname()
	require.NoError(t, err)
	for name, value := range map[string]string{
		"sysname": got.SysName, "release": got.Release, "version": got.Version,
		"machine": got.Machine, "nodename": got.NodeName, "domainname": got.DomainName,
	} {
		assert.NotContains(t, value, "\x00", "%s contains embedded NULs", name)
	}
}

func TestUnameSyscallErrorBranchIsReachable(t *testing.T) {
	// uname(2) takes no arguments that could be invalid, so it cannot fail on a
	// working host and this branch would otherwise ship untested. Exercised through
	// the seam so the error is actually wrapped rather than dropped.
	_, err := readUnameWith(func(*unix.Utsname) error { return assert.AnError })
	require.Error(t, err)
	assert.ErrorIs(t, err, assert.AnError)
	assert.Contains(t, err.Error(), "uname syscall failed")
}

func TestUnameSyscallSucceedsThroughTheSeam(t *testing.T) {
	// The real syscall via the seam, confirming the seam itself does not alter the
	// result.
	direct, err := readUname()
	require.NoError(t, err)
	viaSeam, err := readUnameWith(unix.Uname)
	require.NoError(t, err)
	assert.Equal(t, direct, viaSeam)
}

func TestUnameSyscallFailureIsReported(t *testing.T) {
	c, err := newUnameCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	c.(*unameCollector).unameInfo = func() (unameFields, error) {
		return unameFields{}, assert.AnError
	}

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read uname")
}

func TestUnameRegisteredConstructorWiresTheSyscall(t *testing.T) {
	c, err := newUnameCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	require.NotNil(t, c.(*unameCollector).unameInfo)
}

// --- entropy --------------------------------------------------------------

func TestEntropyEmitsBothMetricsWithEmptySubsystem(t *testing.T) {
	// node_entropy_available_bits, NOT node_entropy_entropy_available_bits.
	c := newEntropyFixtureCollector(t)
	avail, pool := 3500, 4096
	c.kernelRandom = func() (procfs.KernelRandom, error) {
		return procfs.KernelRandom{
			EntropyAvaliable: uint64Ptr(uint64(avail)),
			PoolSize:         uint64Ptr(uint64(pool)),
		}, nil
	}

	got := gatherUnlabelled(t, c)
	assert.Equal(t, 3500.0, got["node_entropy_available_bits"])
	assert.Equal(t, 4096.0, got["node_entropy_pool_size_bits"])
	assert.Len(t, got, 2)
	for name := range got {
		assert.NotContains(t, name, "entropy_entropy", "%q has a doubled subsystem", name)
	}
}

func TestEntropyMissingAvailableIsAnError(t *testing.T) {
	// Upstream returns an ERROR here rather than ErrNoData, and that is preserved: a
	// kernel exposing /proc/sys/kernel/random but not entropy_avail is genuinely
	// unexpected, unlike one lacking the directory entirely.
	c := newEntropyFixtureCollector(t)
	c.kernelRandom = func() (procfs.KernelRandom, error) {
		return procfs.KernelRandom{PoolSize: uint64Ptr(4096)}, nil
	}

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err), "upstream reports this as an error, not no-data")
	assert.Contains(t, err.Error(), "entropy_avail")
}

func TestEntropyMissingPoolSizeIsAnError(t *testing.T) {
	c := newEntropyFixtureCollector(t)
	c.kernelRandom = func() (procfs.KernelRandom, error) {
		return procfs.KernelRandom{EntropyAvaliable: uint64Ptr(3500)}, nil
	}

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "poolsize")
}

func TestEntropyReadFailureIsWrapped(t *testing.T) {
	c := newEntropyFixtureCollector(t)
	c.kernelRandom = func() (procfs.KernelRandom, error) {
		return procfs.KernelRandom{}, assert.AnError
	}

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get kernel random stats")
}

func TestEntropyLiveOnThisHost(t *testing.T) {
	c, err := newEntropyCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	got := gatherUnlabelled(t, c)
	require.Len(t, got, 2)
	assert.Positive(t, got["node_entropy_pool_size_bits"], "the pool size must be positive")
	assert.LessOrEqual(t, got["node_entropy_available_bits"], got["node_entropy_pool_size_bits"],
		"available entropy cannot exceed the pool size")
}

func TestEntropyConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newEntropyCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

// --- filefd ---------------------------------------------------------------

func TestFileFDEmitsAllocatedAndMaximumOnly(t *testing.T) {
	// THE MIDDLE FIELD IS SKIPPED. /proc/sys/fs/file-nr has three TAB-separated
	// values and the second, "free file handles", has been hardcoded to 0 since
	// Linux 2.6 -- the kernel no longer tracks it. Verified on this host:
	// "11872\t0\t9223372036854775807". Emitting it would publish a permanent zero
	// that looks like a measurement.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file-nr"),
		[]byte("11872\t0\t9223372036854775807\n"), 0o644))

	stats, err := parseFileFDStats(filepath.Join(dir, "file-nr"))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"allocated": "11872",
		"maximum":   "9223372036854775807",
	}, stats, "only fields 1 and 3; field 2 is a kernel-hardcoded zero")
	assert.NotContains(t, stats, "free")
}

func TestFileFDMaximumComesFromTheThirdFieldNotTheSecond(t *testing.T) {
	// The likely off-by-one: taking parts[1] would report the maximum as 0, making
	// every fd-exhaustion dashboard read as permanently exhausted (allocated/0) or
	// permanently fine, depending on the query.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file-nr"),
		[]byte("100\t0\t65536\n"), 0o644))

	stats, err := parseFileFDStats(filepath.Join(dir, "file-nr"))
	require.NoError(t, err)
	assert.Equal(t, "65536", stats["maximum"])
	assert.NotEqual(t, "0", stats["maximum"], "the maximum must not be the skipped middle field")
}

func TestFileFDRequiresThreeTabSeparatedFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file-nr")

	for _, content := range []string{
		"100\t0\n",      // two fields
		"100\n",         // one field
		"",              // empty
		"100 0 65536\n", // SPACE separated, not tab -- must not silently parse
	} {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		_, err := parseFileFDStats(path)
		require.Error(t, err, "content %q must not parse", content)
		assert.Contains(t, err.Error(), "unexpected number of file stats")
	}
}

func TestFileFDMissingFileIsReported(t *testing.T) {
	_, err := parseFileFDStats(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err))
}

func TestFileFDCollectorEmitsBothGauges(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sys", "fs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sys", "fs", "file-nr"),
		[]byte("2048\t0\t65536\n"), 0o644))

	c, err := newFileFDCollector(quietLogger(), Paths{ProcFS: root}.withDefaults())
	require.NoError(t, err)

	got := gatherUnlabelled(t, c)
	// allocated vs maximum is the fd exhaustion signal. A node running hundreds of
	// pods, each with sockets and log pipes, can approach fs.file-max -- and when it
	// does everything fails at once in ways that look unrelated to fds.
	assert.Equal(t, 2048.0, got["node_filefd_allocated"])
	assert.Equal(t, 65536.0, got["node_filefd_maximum"])
	assert.Len(t, got, 2)
}

func TestFileFDUnparseableValueIsReported(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sys", "fs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sys", "fs", "file-nr"),
		[]byte("not-a-number\t0\t65536\n"), 0o644))

	c, err := newFileFDCollector(quietLogger(), Paths{ProcFS: root}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid value")
}

func TestFileFDMissingProcfsFileIsWrapped(t *testing.T) {
	c, err := newFileFDCollector(quietLogger(), Paths{ProcFS: t.TempDir()}.withDefaults())
	require.NoError(t, err, "construction must not read file-nr")

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get file-nr")
}

func TestFileFDLiveOnThisHost(t *testing.T) {
	c, err := newFileFDCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	got := gatherUnlabelled(t, c)
	require.Len(t, got, 2)
	assert.Positive(t, got["node_filefd_maximum"])
	assert.LessOrEqual(t, got["node_filefd_allocated"], got["node_filefd_maximum"],
		"allocated cannot exceed the maximum")
}

// --- schedstat ------------------------------------------------------------

func TestSchedstatUsesNanosecondsNotMicroseconds(t *testing.T) {
	// THE CROSS-COLLECTOR HAZARD. schedstat is nanoseconds; pressure, in this same
	// package, is microseconds. Copying either constant to the other is a silent
	// 1000x error and both produce plausible values.
	assert.Equal(t, 1e9, float64(schedstatNanosecondsPerSecond))
	assert.NotEqual(t, psiMicrosecondsPerSecond, float64(schedstatNanosecondsPerSecond),
		"schedstat and pressure must not share a divisor")
}

func TestSchedstatUnitMatchesUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/schedstat_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	m := regexp.MustCompile(`nsPerSec\s*=\s*(\S+)`).FindSubmatch(data)
	require.NotNil(t, m, "failed to extract upstream's divisor; the regexp may be stale")
	assert.Equal(t, "1e9", string(m[1]), "upstream uses 1e9; ours must match")
}

func TestSchedstatConvertsNanosecondsAndLeavesTimeslicesRaw(t *testing.T) {
	// Two of the three values are durations and one is a count. Applying the
	// conversion to timeslices would divide a plain count by a billion, producing a
	// near-zero that reads as an idle CPU.
	c := newSchedstatFixtureCollector(t)
	c.schedstat = func() (*procfs.Schedstat, error) {
		return &procfs.Schedstat{CPUs: []*procfs.SchedstatCPU{{
			CPUNum:             "0",
			RunningNanoseconds: 2_500_000_000,
			WaitingNanoseconds: 1_000_000_000,
			RunTimeslices:      42,
		}}}, nil
	}

	got := gatherByCPU(t, c)
	assert.Equal(t, 2.5, got["node_schedstat_running_seconds_total"]["0"])
	assert.Equal(t, 1.0, got["node_schedstat_waiting_seconds_total"]["0"])
	assert.Equal(t, 42.0, got["node_schedstat_timeslices_total"]["0"],
		"timeslices is a count, not a duration; it must not be divided")
}

func TestSchedstatCPULabelComesFromTheFileNotTheIndex(t *testing.T) {
	// /proc/schedstat names CPUs and omits offline ones. Using the loop index would
	// silently renumber the remaining CPUs -- so on a node with cpu2 offline, cpu3's
	// stats would be reported as cpu2's.
	c := newSchedstatFixtureCollector(t)
	c.schedstat = func() (*procfs.Schedstat, error) {
		return &procfs.Schedstat{CPUs: []*procfs.SchedstatCPU{
			{CPUNum: "0", RunningNanoseconds: 1e9},
			{CPUNum: "3", RunningNanoseconds: 2e9}, // cpu1 and cpu2 offline
		}}, nil
	}

	got := gatherByCPU(t, c)
	assert.Equal(t, 1.0, got["node_schedstat_running_seconds_total"]["0"])
	assert.Equal(t, 2.0, got["node_schedstat_running_seconds_total"]["3"],
		"the second entry is cpu3, not cpu1")
	assert.NotContains(t, got["node_schedstat_running_seconds_total"], "1",
		"an offline CPU must not be invented from the loop index")
}

func TestSchedstatMissingFileIsNoData(t *testing.T) {
	// CONFIG_SCHEDSTATS not enabled: a supported configuration, so not a failure.
	c := newSchedstatFixtureCollector(t)
	c.schedstat = func() (*procfs.Schedstat, error) { return nil, os.ErrNotExist }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err), "expected ErrNoData, got %v", err)
}

func TestSchedstatRealErrorIsAFailure(t *testing.T) {
	c := newSchedstatFixtureCollector(t)
	c.schedstat = func() (*procfs.Schedstat, error) { return nil, assert.AnError }

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err), "a real error must not be reported as no-data")
}

func TestSchedstatMetricsAreCounters(t *testing.T) {
	c := newSchedstatFixtureCollector(t)
	c.schedstat = func() (*procfs.Schedstat, error) {
		return &procfs.Schedstat{CPUs: []*procfs.SchedstatCPU{{CPUNum: "0", RunningNanoseconds: 1e9}}}, nil
	}

	ch := make(chan prometheus.Metric, 32)
	require.NoError(t, c.Update(ch))
	close(ch)

	n := 0
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		assert.NotNil(t, pb.Counter, "%s must be a counter", metricName(t, m))
		n++
	}
	assert.Equal(t, 3, n, "three metrics per CPU")
}

func TestSchedstatLiveOnThisHost(t *testing.T) {
	c, err := newSchedstatCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 4096)
	err = c.Update(ch)
	close(ch)
	if err != nil && IsNoDataError(err) {
		t.Skip("CONFIG_SCHEDSTATS not enabled on this host")
	}
	require.NoError(t, err)
	assert.NotEmpty(t, ch, "a host with schedstat must report at least one CPU")
}

func TestSchedstatConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newSchedstatCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

func TestSchedstatRegisteredConstructorWiresTheRealReader(t *testing.T) {
	c, err := newSchedstatCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	require.NotNil(t, c.(*schedstatCollector).schedstat)
}

// --- helpers --------------------------------------------------------------

func uint64Ptr(v uint64) *uint64 { return &v }

func newEntropyFixtureCollector(t *testing.T) *entropyCollector {
	t.Helper()
	c, err := newEntropyCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	return c.(*entropyCollector)
}

func newSchedstatFixtureCollector(t *testing.T) *schedstatCollector {
	t.Helper()
	c, err := newSchedstatCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)
	return c.(*schedstatCollector)
}

// gatherUnlabelled returns metric name -> gauge value for collectors whose metrics
// carry no labels.
func gatherUnlabelled(t *testing.T, c Collector) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 256)
	require.NoError(t, c.Update(ch))
	close(ch)

	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		out[metricName(t, m)] = pb.GetGauge().GetValue()
	}
	return out
}

// gatherByCPU returns metric name -> cpu label -> counter value.
func gatherByCPU(t *testing.T, c Collector) map[string]map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 4096)
	require.NoError(t, c.Update(ch))
	close(ch)

	out := map[string]map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))

		name := metricName(t, m)
		cpu := ""
		for _, l := range pb.GetLabel() {
			if l.GetName() == "cpu" {
				cpu = l.GetValue()
			}
		}
		if out[name] == nil {
			out[name] = map[string]float64{}
		}
		out[name][cpu] = pb.GetCounter().GetValue()
	}
	return out
}

// singleMetricLabels returns the labels of the one metric with the given name.
func singleMetricLabels(t *testing.T, c Collector, want string) map[string]string {
	t.Helper()

	ch := make(chan prometheus.Metric, 64)
	require.NoError(t, c.Update(ch))
	close(ch)

	for m := range ch {
		if metricName(t, m) != want {
			continue
		}
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		labels := map[string]string{}
		for _, l := range pb.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		return labels
	}
	t.Fatalf("no metric named %q was emitted", want)
	return nil
}
