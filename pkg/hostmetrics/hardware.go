package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/thermal_zone_linux.go
//                 node_exporter/collector/cpufreq_linux.go
//                 node_exporter/collector/watchdog.go
//                 node_exporter/collector/edac_linux.go
//                 node_exporter/collector/kernel_hung_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015-2023 The Prometheus Authors, Apache-2.0
//
// THE HARDWARE GROUP, AND WHY "EMITS NOTHING" IS THE CORRECT BEHAVIOUR HERE.
//
// Almost none of this hardware exists on an EC2 instance. There are no thermal
// sensors exposed to the guest, no cpufreq governors (the hypervisor owns
// frequency), no EDAC memory controllers, no watchdog device. Measured against the
// live cluster's prometheus-node-exporter scrape:
//
//	collector          success   series
//	thermal_zone         1          0
//	cpufreq              1          0
//	edac                 1          0
//	watchdog             1          0
//	powersupplyclass     1          0
//	infiniband           1          0
//	btrfs                1          0
//	mdadm                1          0
//	dmi                  1          1
//	selinux              1          3
//	kernel_hung          1          0
//	hwmon                0          -    <-- the ONE that legitimately fails
//
// SO THE PARITY TARGET IS success=1 WITH ZERO SERIES, NOT ErrNoData. That
// distinction is the whole difficulty of this group: ErrNoData reports
// node_scrape_collector_success=0, which would differ from upstream on every EKS
// node and would fire any alert watching for collector failures. I got this wrong
// once already on the dependency branch — asserted zero collector failures, when
// upstream fails the same set on EKS — so it is asserted explicitly here.
//
// The mechanism that makes upstream behave this way is worth naming: procfs's
// sysfs helpers use filepath.Glob, which returns an EMPTY SLICE rather than an
// error when nothing matches. So "no thermal zones" is indistinguishable from "an
// empty list of thermal zones", and the loop body simply never runs. Reproducing
// that means NOT adding a helpful "nothing found, returning ErrNoData" check.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"github.com/prometheus/procfs/sysfs"
)

// ---------------------------------------------------------------------------
// thermal_zone
// ---------------------------------------------------------------------------

const (
	thermalZoneSubsystem   = "thermal_zone"
	coolingDeviceSubsystem = "cooling_device"
)

func init() {
	register("thermal_zone", true, newThermalZoneCollector)
}

type thermalZoneCollector struct {
	fs     sysfs.FS
	logger *slog.Logger

	zoneTemp              *prometheus.Desc
	coolingDeviceCurState *prometheus.Desc
	coolingDeviceMaxState *prometheus.Desc

	// Injected so the ErrNoData branch is reachable: on this host the glob succeeds
	// and simply matches nothing.
	thermalZoneStats   func() ([]sysfs.ClassThermalZoneStats, error)
	coolingDeviceStats func() ([]sysfs.ClassCoolingDeviceStats, error)
}

func newThermalZoneCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := sysfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}

	c := &thermalZoneCollector{
		fs:     fs,
		logger: logger,
		zoneTemp: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, thermalZoneSubsystem, "temp"),
			"Zone temperature in Celsius",
			[]string{"zone", "type"}, nil,
		),
		coolingDeviceCurState: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, coolingDeviceSubsystem, "cur_state"),
			"Current throttle state of the cooling device",
			[]string{"name", "type"}, nil,
		),
		coolingDeviceMaxState: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, coolingDeviceSubsystem, "max_state"),
			"Maximum throttle state of the cooling device",
			[]string{"name", "type"}, nil,
		),
	}
	c.thermalZoneStats = c.fs.ClassThermalZoneStats
	c.coolingDeviceStats = c.fs.ClassCoolingDeviceStats
	return c, nil
}

func (c *thermalZoneCollector) Update(ch chan<- prometheus.Metric) error {
	zones, err := c.thermalZoneStats()
	if err != nil {
		// Upstream tolerates four distinct error kinds here. EINVAL in particular is
		// what a sysfs "temp" file returns when the sensor is present but not
		// readable — a real condition on some hardware, and not a scrape failure.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) ||
			errors.Is(err, os.ErrInvalid) || errors.Is(err, syscall.EINVAL) {
			c.logger.Debug("could not read thermal zone stats", "err", err)
			return ErrNoData
		}
		return err
	}

	for _, stats := range zones {
		// millidegrees Celsius -> Celsius.
		ch <- prometheus.MustNewConstMetric(c.zoneTemp, prometheus.GaugeValue,
			float64(stats.Temp)/1000.0, stats.Name, stats.Type)
	}

	devices, err := c.coolingDeviceStats()
	if err != nil {
		return err
	}

	for _, stats := range devices {
		ch <- prometheus.MustNewConstMetric(c.coolingDeviceCurState, prometheus.GaugeValue,
			float64(stats.CurState), stats.Name, stats.Type)
		ch <- prometheus.MustNewConstMetric(c.coolingDeviceMaxState, prometheus.GaugeValue,
			float64(stats.MaxState), stats.Name, stats.Type)
	}
	return nil
}

// ---------------------------------------------------------------------------
// cpufreq
// ---------------------------------------------------------------------------

const cpufreqSubsystem = "cpu"

func init() {
	register("cpufreq", true, newCPUFreqCollector)
}

var (
	// NOTE the subsystem is "cpu", not "cpufreq": these are node_cpu_frequency_*,
	// sharing a namespace with the cpu collector's node_cpu_seconds_total. Using
	// "cpufreq" would produce names nothing matches.
	//
	// Help strings are upstream's VERBATIM, including the capitalisation of "CPU".
	// My first pass wrote "cpu thread" where upstream writes "CPU thread"; help text
	// is part of the exposition output, so that is a diff against the reference
	// endpoint. Caught by mechanically comparing against upstream's source, not by
	// reading.
	cpuFreqHertzDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, cpufreqSubsystem, "frequency_hertz"),
		"Current CPU thread frequency in hertz.",
		[]string{"cpu"}, nil,
	)
	// frequency_avg_hertz was MISSING from my first pass entirely -- upstream's
	// descriptors live in cpufreq_common.go, not cpufreq_linux.go, and I had only
	// read the latter. A dropped metric is invisible to any test that checks the
	// metrics which ARE present.
	cpuFreqAvgDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, cpufreqSubsystem, "frequency_avg_hertz"),
		"Average CPU thread frequency in hertz.",
		[]string{"cpu"}, nil,
	)
	cpuFreqMinDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, cpufreqSubsystem, "frequency_min_hertz"),
		"Minimum CPU thread frequency in hertz.",
		[]string{"cpu"}, nil,
	)
	cpuFreqMaxDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, cpufreqSubsystem, "frequency_max_hertz"),
		"Maximum CPU thread frequency in hertz.",
		[]string{"cpu"}, nil,
	)
	cpuFreqScalingFreqDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, cpufreqSubsystem, "scaling_frequency_hertz"),
		"Current scaled CPU thread frequency in hertz.",
		[]string{"cpu"}, nil,
	)
	cpuFreqScalingFreqMinDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, cpufreqSubsystem, "scaling_frequency_min_hertz"),
		"Minimum scaled CPU thread frequency in hertz.",
		[]string{"cpu"}, nil,
	)
	cpuFreqScalingFreqMaxDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, cpufreqSubsystem, "scaling_frequency_max_hertz"),
		"Maximum scaled CPU thread frequency in hertz.",
		[]string{"cpu"}, nil,
	)
	cpuFreqScalingGovernorDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, cpufreqSubsystem, "scaling_governor"),
		"Current enabled CPU frequency governor.",
		[]string{"cpu", "governor"}, nil,
	)
)

type cpuFreqCollector struct {
	fs     sysfs.FS
	logger *slog.Logger

	systemCPUFreqStats func() ([]sysfs.SystemCPUCpufreqStats, error)
}

func newCPUFreqCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := sysfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}
	c := &cpuFreqCollector{fs: fs, logger: logger}
	c.systemCPUFreqStats = c.fs.SystemCpufreq
	return c, nil
}

func (c *cpuFreqCollector) Update(ch chan<- prometheus.Metric) error {
	stats, err := c.systemCPUFreqStats()
	if err != nil {
		return err
	}

	for _, s := range stats {
		// Every field is a pointer: nil means the kernel does not expose that
		// attribute for this CPU. Emitting zero would report a stopped CPU.
		//
		// The kernel reports kHz and the metric is hertz, hence *1000. Getting this
		// wrong yields a frequency of ~2400 Hz instead of 2.4 GHz -- plausible-looking
		// only if you do not think about it.
		pushCPUFreq(ch, cpuFreqHertzDesc, s.CpuinfoCurrentFrequency, s.Name)
		pushCPUFreq(ch, cpuFreqAvgDesc, s.CpuinfoAverageFrequency, s.Name)
		pushCPUFreq(ch, cpuFreqMinDesc, s.CpuinfoMinimumFrequency, s.Name)
		pushCPUFreq(ch, cpuFreqMaxDesc, s.CpuinfoMaximumFrequency, s.Name)
		pushCPUFreq(ch, cpuFreqScalingFreqDesc, s.ScalingCurrentFrequency, s.Name)
		pushCPUFreq(ch, cpuFreqScalingFreqMinDesc, s.ScalingMinimumFrequency, s.Name)
		pushCPUFreq(ch, cpuFreqScalingFreqMaxDesc, s.ScalingMaximumFrequency, s.Name)

		if s.Governor == "" {
			continue
		}
		// One series per AVAILABLE governor, with 1 for the active one and 0 for the
		// rest. That shape lets a query see which governors exist, not just which is
		// active -- so emitting only the active one would lose information.
		for governor := range strings.SplitSeq(s.AvailableGovernors, " ") {
			state := 0.0
			if governor == s.Governor {
				state = 1.0
			}
			ch <- prometheus.MustNewConstMetric(cpuFreqScalingGovernorDesc,
				prometheus.GaugeValue, state, s.Name, governor)
		}
	}
	return nil
}

// pushCPUFreq emits a kHz value as hertz, skipping absent attributes.
func pushCPUFreq(ch chan<- prometheus.Metric, desc *prometheus.Desc, kHz *uint64, cpu string) {
	if kHz == nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, float64(*kHz)*1000.0, cpu)
}

// ---------------------------------------------------------------------------
// edac
// ---------------------------------------------------------------------------
//
// Unlike thermal_zone and cpufreq, procfs has no sysfs helper for EDAC, so upstream
// walks the directory tree itself with filepath.Glob. Ported the same way.
//
// THREE THINGS I GOT WRONG ON A FIRST PASS and only caught by reading upstream's
// source rather than inferring from the metric names:
//
//  1. The channel metrics carry FOUR labels (controller, csrow, channel,
//     dimm_label), not two. A two-label version would be a different metric that
//     no existing query matches.
//  2. ce_noinfo_count and ue_noinfo_count are emitted as csrow metrics with
//     csrow="unknown" -- errors the controller could not attribute to a specific
//     row. Omitting them would silently lose real error counts.
//  3. Channel UE count failure is logged and SKIPPED, while every other read
//     failure aborts the collector. That asymmetry is upstream's and is preserved.

const edacSubsystem = "edac"

var (
	edacMemControllerRE = regexp.MustCompile(`.*devices/system/edac/mc/mc([0-9]*)`)
	edacMemCsrowRE      = regexp.MustCompile(`.*devices/system/edac/mc/mc[0-9]*/csrow([0-9]*)`)
	edacMemChannelRE    = regexp.MustCompile(`ch([0-9]+)_ce_count`)
)

func init() {
	register("edac", true, newEDACCollector)
}

var (
	edacCECountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, edacSubsystem, "correctable_errors_total"),
		"Total correctable memory errors.",
		[]string{"controller"}, nil,
	)
	edacUECountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, edacSubsystem, "uncorrectable_errors_total"),
		"Total uncorrectable memory errors.",
		[]string{"controller"}, nil,
	)
	edacCsRowCECountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, edacSubsystem, "csrow_correctable_errors_total"),
		"Total correctable memory errors for this csrow.",
		[]string{"controller", "csrow"}, nil,
	)
	edacCsRowUECountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, edacSubsystem, "csrow_uncorrectable_errors_total"),
		"Total uncorrectable memory errors for this csrow.",
		[]string{"controller", "csrow"}, nil,
	)
	edacChannelCECountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, edacSubsystem, "channel_correctable_errors_total"),
		"Total correctable memory errors for this channel.",
		[]string{"controller", "csrow", "channel", "dimm_label"}, nil,
	)
	edacChannelUECountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, edacSubsystem, "channel_uncorrectable_errors_total"),
		"Total uncorrectable memory errors for this channel.",
		[]string{"controller", "csrow", "channel", "dimm_label"}, nil,
	)
)

type edacCollector struct {
	logger *slog.Logger
	paths  Paths

	// glob is injectable for two reasons, and both are about reachability rather
	// than convenience:
	//
	//  - filepath.Glob errors ONLY on ErrBadPattern. The patterns here are
	//    compile-time constants, but they are joined onto a caller-supplied sysfs
	//    root, so a root containing "[" makes the pattern invalid. Reachable, and
	//    genuinely worth handling rather than ignoring.
	//  - the regexp-mismatch branches cannot be reached through the real glob at
	//    all: every path it returns necessarily contains "devices/system/edac/mc/mc",
	//    which is exactly what the regexp requires. Upstream has the same dead
	//    branch. Keeping the check and covering it through the seam is better than
	//    deleting a guard that would matter if either pattern ever changed.
	glob func(pattern string) ([]string, error)
}

func newEDACCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	// No sysfs probe at construction: upstream does not do one, and the glob below
	// returns an empty list on a machine with no EDAC rather than failing.
	return &edacCollector{logger: logger, paths: paths, glob: filepath.Glob}, nil
}

func (c *edacCollector) Update(ch chan<- prometheus.Metric) error {
	// Glob returns an empty slice, not an error, when nothing matches -- which is
	// every EC2 instance. That is what makes this collector report success=1 with
	// zero series rather than ErrNoData.
	controllers, err := c.glob(c.paths.sysPath("devices/system/edac/mc/mc[0-9]*"))
	if err != nil {
		return err
	}

	for _, controller := range controllers {
		match := edacMemControllerRE.FindStringSubmatch(controller)
		if match == nil {
			return fmt.Errorf("controller string didn't match regexp: %s", controller)
		}
		controllerNumber := match[1]

		for _, m := range []struct {
			file   string
			desc   *prometheus.Desc
			labels []string
		}{
			{"ce_count", edacCECountDesc, []string{controllerNumber}},
			// The *_noinfo_count files are errors the controller could not attribute
			// to a specific row, reported under csrow="unknown". Dropping them would
			// lose real error counts on hardware that has them.
			{"ce_noinfo_count", edacCsRowCECountDesc, []string{controllerNumber, "unknown"}},
			{"ue_count", edacUECountDesc, []string{controllerNumber}},
			{"ue_noinfo_count", edacCsRowUECountDesc, []string{controllerNumber, "unknown"}},
		} {
			value, err := readUintFromFile(filepath.Join(controller, m.file))
			if err != nil {
				return fmt.Errorf("couldn't get %s for controller %s: %w", m.file, controllerNumber, err)
			}
			ch <- prometheus.MustNewConstMetric(m.desc, prometheus.CounterValue,
				float64(value), m.labels...)
		}

		if err := c.collectCSRows(ch, controller, controllerNumber); err != nil {
			return err
		}
	}
	return nil
}

// collectCSRows walks the csrow directories under one memory controller.
func (c *edacCollector) collectCSRows(ch chan<- prometheus.Metric, controller, controllerNumber string) error {
	csrows, err := c.glob(controller + "/csrow[0-9]*")
	if err != nil {
		return err
	}

	for _, csrow := range csrows {
		match := edacMemCsrowRE.FindStringSubmatch(csrow)
		if match == nil {
			return fmt.Errorf("csrow string didn't match regexp: %s", csrow)
		}
		csrowNumber := match[1]

		for file, desc := range map[string]*prometheus.Desc{
			"ce_count": edacCsRowCECountDesc,
			"ue_count": edacCsRowUECountDesc,
		} {
			value, err := readUintFromFile(filepath.Join(csrow, file))
			if err != nil {
				return fmt.Errorf("couldn't get %s for controller/csrow %s/%s: %w",
					file, controllerNumber, csrowNumber, err)
			}
			ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue,
				float64(value), controllerNumber, csrowNumber)
		}

		if err := c.collectChannels(ch, csrow, controllerNumber, csrowNumber); err != nil {
			return err
		}
	}
	return nil
}

// collectChannels emits the per-channel error counts for one csrow.
func (c *edacCollector) collectChannels(ch chan<- prometheus.Metric, csrow, controllerNumber, csrowNumber string) error {
	channelFiles, err := c.glob(csrow + "/ch*_ce_count")
	if err != nil {
		return err
	}

	for _, chFile := range channelFiles {
		match := edacMemChannelRE.FindStringSubmatch(filepath.Base(chFile))
		if match == nil {
			// A file matching the glob but not the regexp is skipped rather than
			// failing, matching upstream.
			continue
		}
		channelNumber := match[1]
		label := edacDimmLabel(csrow, channelNumber)

		value, err := readUintFromFile(chFile)
		if err != nil {
			return fmt.Errorf("couldn't get ce_count for controller/csrow/channel %s/%s/%s: %w",
				controllerNumber, csrowNumber, channelNumber, err)
		}
		ch <- prometheus.MustNewConstMetric(edacChannelCECountDesc, prometheus.CounterValue,
			float64(value), controllerNumber, csrowNumber, channelNumber, label)

		// ASYMMETRY, upstream's and preserved: a missing ue_count for a channel is
		// logged and skipped, while every other read failure above aborts. Some
		// hardware exposes ch*_ce_count without ch*_ue_count, so failing here would
		// lose the whole collector on those machines.
		value, err = readUintFromFile(filepath.Join(csrow, "ch"+channelNumber+"_ue_count"))
		if err != nil {
			c.logger.Debug("couldn't get ue_count for channel",
				"controller", controllerNumber, "csrow", csrowNumber,
				"channel", channelNumber, "err", err)
			continue
		}
		ch <- prometheus.MustNewConstMetric(edacChannelUECountDesc, prometheus.CounterValue,
			float64(value), controllerNumber, csrowNumber, channelNumber, label)
	}
	return nil
}

// edacDimmLabel reads the human-assigned DIMM label for a channel.
//
// The substitutions are upstream's verbatim and are not cosmetic: "#" is stripped
// and "csrow"/"channel" get an underscore prefix, so a BIOS label like
// "CPU#1_csrow0" becomes "CPU1__csrow0". Changing them changes the label value on
// real hardware.
func edacDimmLabel(csrow, channel string) string {
	raw, err := os.ReadFile(filepath.Join(csrow, "ch"+channel+"_dimm_label"))
	if err != nil {
		// Most hardware does not set a label. "unknown" rather than empty, so the
		// series is still selectable.
		return "unknown"
	}
	label := strings.TrimSpace(string(raw))
	label = strings.ReplaceAll(label, "#", "")
	label = strings.ReplaceAll(label, "csrow", "_csrow")
	label = strings.ReplaceAll(label, "channel", "_channel")
	return label
}

// ---------------------------------------------------------------------------
// kernel_hung
// ---------------------------------------------------------------------------

func init() {
	register("kernel_hung", true, newKernelHungCollector)
}

// kernelHungTasksDesc counts tasks the kernel's hung-task detector has flagged.
//
// This is one of the more useful metrics in the group for a Kubernetes node: a task
// blocked in uninterruptible sleep for over 120s is usually a stuck I/O path, and
// it is exactly what a hung NFS or EBS volume looks like from the kernel's side.
var kernelHungTasksDesc = prometheus.NewDesc(
	prometheus.BuildFQName(namespace, "kernel_hung", "tasks_total"),
	"Total number of tasks that have been detected as hung since the system booted.",
	nil, nil,
)

type kernelHungCollector struct {
	fs     procfs.FS
	logger *slog.Logger

	kernelHung func() (procfs.KernelHung, error)
}

func newKernelHungCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := procfs.NewFS(paths.ProcFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open procfs at %s: %w", paths.ProcFS, err)
	}
	c := &kernelHungCollector{fs: fs, logger: logger}
	c.kernelHung = c.fs.KernelHung
	return c, nil
}

func (c *kernelHungCollector) Update(ch chan<- prometheus.Metric) error {
	hung, err := c.kernelHung()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// hung_task_detect_count needs kernel 6.7. Absent is a supported
			// configuration, not a failure.
			c.logger.Debug("hung_task_detect_count does not exist")
			return ErrNoData
		}
		return err
	}

	// Upstream dereferences HungTaskDetectCount unconditionally, which would panic if
	// procfs ever returned a nil pointer with a nil error. Guarded: a nil-pointer
	// panic in a collector is far worse than a missing metric, and the resilience
	// layer should not have to be the only thing standing between this and a crash.
	if hung.HungTaskDetectCount == nil {
		c.logger.Debug("hung_task_detect_count was not reported")
		return ErrNoData
	}

	ch <- prometheus.MustNewConstMetric(kernelHungTasksDesc, prometheus.CounterValue,
		float64(*hung.HungTaskDetectCount))
	return nil
}
