package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/time.go, time_linux.go
//                 node_exporter/collector/timex.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015, 2019 The Prometheus Authors, Apache-2.0
//
// Two clock collectors, grouped because they answer the same operational question:
// is this node's clock trustworthy? On EKS that is not academic — certificate
// validation, IAM request signing (SigV4 rejects a 5-minute skew), and log
// correlation across nodes all fail in confusing ways when a clock drifts, and none
// of those failures name the clock as the cause.
//
// timex is where the difficulty is, and it is entirely about DIVISORS. Three
// different ones apply to different fields of the same struct:
//
//   - offset and jitter use a divisor that DEPENDS ON A STATUS BIT. If STA_NANO is
//     set the kernel reports nanoseconds; otherwise microseconds. Hardcoding either
//     is a 1000x error on half the machines in existence, and the metric still
//     exists with a plausible small value.
//   - maxerror, esterror and tick are ALWAYS microseconds, regardless of STA_NANO.
//   - freq, ppsfreq and stabil are in a 16-bit-fraction PPM format, so the divisor
//     is 1e6 * 65536. And freq additionally has 1 ADDED to it, because the metric is
//     a frequency ratio around 1.0 rather than an offset around 0.
//
// Getting any of these wrong produces a number that looks like a healthy clock.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs/sysfs"
	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// time
// ---------------------------------------------------------------------------

func init() {
	register("time", true, newTimeCollector)
}

type timeCollector struct {
	logger *slog.Logger
	paths  Paths

	now                   typedDesc
	zone                  typedDesc
	clocksourcesAvailable typedDesc
	clocksourceCurrent    typedDesc

	// now/clockSources are injectable so the emitted value is assertable and the
	// sysfs failure path is reachable.
	nowFunc      func() time.Time
	clockSources func() ([]sysfs.ClockSource, error)
}

func newTimeCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	const subsystem = "time"

	c := &timeCollector{
		logger: logger,
		paths:  paths,
		now: typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "seconds"),
			"System time in seconds since epoch (1970).",
			nil, nil,
		), prometheus.GaugeValue},
		zone: typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "zone_offset_seconds"),
			"System time zone offset in seconds.",
			[]string{"time_zone"}, nil,
		), prometheus.GaugeValue},
		clocksourcesAvailable: typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "clocksource_available_info"),
			"Available clocksources read from '/sys/devices/system/clocksource'.",
			[]string{"device", "clocksource"}, nil,
		), prometheus.GaugeValue},
		clocksourceCurrent: typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "clocksource_current_info"),
			"Current clocksource read from '/sys/devices/system/clocksource'.",
			[]string{"device", "clocksource"}, nil,
		), prometheus.GaugeValue},
		nowFunc: time.Now,
	}

	// Unlike upstream, sysfs is opened ONCE at construction rather than on every
	// scrape. Upstream's time_linux.go calls sysfs.NewFS(*sysPath) inside update(),
	// which re-stats the mount point every scrape for no benefit.
	fs, err := sysfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}
	c.clockSources = fs.ClockSources

	return c, nil
}

func (c *timeCollector) Update(ch chan<- prometheus.Metric) error {
	now := c.nowFunc()

	// UnixNano/1e9 rather than Unix(): the metric is a float and sub-second
	// resolution is the point, since this is used to measure skew between nodes.
	ch <- c.now.mustNewConstMetric(float64(now.UnixNano()) / 1e9)

	zone, zoneOffset := now.Zone()
	ch <- c.zone.mustNewConstMetric(float64(zoneOffset), zone)

	sources, err := c.clockSources()
	if err != nil {
		return fmt.Errorf("couldn't get clocksources: %w", err)
	}

	for i, source := range sources {
		// The device label is the INDEX, not a name: /sys/devices/system/clocksource
		// contains clocksource0, clocksource1, ... and upstream labels them by
		// position. Preserved, since changing it changes the label value.
		device := strconv.Itoa(i)
		for _, available := range source.Available {
			ch <- c.clocksourcesAvailable.mustNewConstMetric(1.0, device, available)
		}
		ch <- c.clocksourceCurrent.mustNewConstMetric(1.0, device, source.Current)
	}
	return nil
}

// ---------------------------------------------------------------------------
// timex
// ---------------------------------------------------------------------------

const (
	// timeError is TIME_ERROR: the clock is not synchronised to a reliable server.
	timeError = 5

	// staNano is STA_NANO in timex.Status: 0 means the kernel reports offset and
	// jitter in microseconds, 1 means nanoseconds. THE DIVISOR DEPENDS ON THIS BIT,
	// so it cannot be hardcoded.
	staNano = 0x2000

	timexNanoSeconds  = 1000000000
	timexMicroSeconds = 1000000

	// ppm16frac is the scale for the kernel's 16-bit-fraction PPM fields
	// (freq, ppsfreq, stabil). See NOTES in adjtimex(2).
	ppm16frac = 1000000.0 * 65536.0
)

func init() {
	register("timex", true, newTimexCollector)
}

type timexCollector struct {
	logger *slog.Logger

	offset, freq, maxerror, esterror, status, constant, tick,
	ppsfreq, jitter, shift, stabil, jitcnt, calcnt, errcnt, stbcnt,
	tai, syncStatus typedDesc

	// adjtimex is injectable because the syscall reflects real kernel clock state:
	// the STA_NANO branch, the TIME_ERROR branch and the EPERM branch cannot all be
	// produced on one host.
	adjtimex func(*unix.Timex) (int, error)
}

func newTimexCollector(logger *slog.Logger, _ Paths) (Collector, error) {
	const subsystem = "timex"

	gauge := func(name, help string) typedDesc {
		return typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, name), help, nil, nil,
		), prometheus.GaugeValue}
	}
	counter := func(name, help string) typedDesc {
		return typedDesc{prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, name), help, nil, nil,
		), prometheus.CounterValue}
	}

	return &timexCollector{
		logger:     logger,
		adjtimex:   unix.Adjtimex,
		offset:     gauge("offset_seconds", "Time offset in between local system and reference clock."),
		freq:       gauge("frequency_adjustment_ratio", "Local clock frequency adjustment."),
		maxerror:   gauge("maxerror_seconds", "Maximum error in seconds."),
		esterror:   gauge("estimated_error_seconds", "Estimated error in seconds."),
		status:     gauge("status", "Value of the status array bits."),
		constant:   gauge("loop_time_constant", "Phase-locked loop time constant."),
		tick:       gauge("tick_seconds", "Seconds between clock ticks."),
		ppsfreq:    gauge("pps_frequency_hertz", "Pulse per second frequency."),
		jitter:     gauge("pps_jitter_seconds", "Pulse per second jitter."),
		shift:      gauge("pps_shift_seconds", "Pulse per second interval duration."),
		stabil:     gauge("pps_stability_hertz", "Pulse per second stability, average of recent frequency changes."),
		tai:        gauge("tai_offset_seconds", "International Atomic Time (TAI) offset."),
		syncStatus: gauge("sync_status", "Is clock synchronized to a reliable server (1 = yes, 0 = no)."),
		// The four *cnt fields are cumulative event counts, so counters.
		jitcnt: counter("pps_jitter_total", "Pulse per second count of jitter limit exceeded events."),
		calcnt: counter("pps_calibration_total", "Pulse per second count of calibration intervals."),
		errcnt: counter("pps_error_total", "Pulse per second count of calibration errors."),
		stbcnt: counter("pps_stability_exceeded_total", "Pulse per second count of stability limit exceeded events."),
	}, nil
}

func (c *timexCollector) Update(ch chan<- prometheus.Metric) error {
	var timex unix.Timex

	status, err := c.adjtimex(&timex)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			// adjtimex(2) with a zeroed modes field is a read, but some seccomp
			// profiles deny it outright. Not a failure: the agent simply cannot see
			// the clock state.
			c.logger.Debug("not collecting timex metrics", "err", err)
			return ErrNoData
		}
		return fmt.Errorf("failed to retrieve adjtimex stats: %w", err)
	}

	// sync_status is derived from the RETURN VALUE, not from timex.Status. They are
	// different things: the return value is the clock state enum (TIME_OK,
	// TIME_INS, ... TIME_ERROR) while Status is the bitmask.
	syncStatus := 1.0
	if status == timeError {
		syncStatus = 0
	}

	// THE CONDITIONAL DIVISOR. Applies to offset and jitter ONLY.
	divisor := float64(timexMicroSeconds)
	if timex.Status&staNano != 0 {
		divisor = timexNanoSeconds
	}

	ch <- c.syncStatus.mustNewConstMetric(syncStatus)
	ch <- c.offset.mustNewConstMetric(float64(timex.Offset) / divisor)
	// freq is a RATIO around 1.0, hence the +1. Without it the metric reads as a
	// frequency of ~0, i.e. a stopped clock.
	ch <- c.freq.mustNewConstMetric(1 + float64(timex.Freq)/ppm16frac)
	// maxerror/esterror/tick are ALWAYS microseconds, even when STA_NANO is set.
	ch <- c.maxerror.mustNewConstMetric(float64(timex.Maxerror) / timexMicroSeconds)
	ch <- c.esterror.mustNewConstMetric(float64(timex.Esterror) / timexMicroSeconds)
	ch <- c.status.mustNewConstMetric(float64(timex.Status))
	ch <- c.constant.mustNewConstMetric(float64(timex.Constant))
	ch <- c.tick.mustNewConstMetric(float64(timex.Tick) / timexMicroSeconds)
	ch <- c.ppsfreq.mustNewConstMetric(float64(timex.Ppsfreq) / ppm16frac)
	ch <- c.jitter.mustNewConstMetric(float64(timex.Jitter) / divisor)
	// shift is an exponent, not a duration: no conversion despite the _seconds name.
	ch <- c.shift.mustNewConstMetric(float64(timex.Shift))
	ch <- c.stabil.mustNewConstMetric(float64(timex.Stabil) / ppm16frac)
	ch <- c.jitcnt.mustNewConstMetric(float64(timex.Jitcnt))
	ch <- c.calcnt.mustNewConstMetric(float64(timex.Calcnt))
	ch <- c.errcnt.mustNewConstMetric(float64(timex.Errcnt))
	ch <- c.stbcnt.mustNewConstMetric(float64(timex.Stbcnt))
	// tai is a whole-second offset: no conversion.
	ch <- c.tai.mustNewConstMetric(float64(timex.Tai))

	return nil
}
