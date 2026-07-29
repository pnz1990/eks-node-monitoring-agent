package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/powersupplyclass.go
//                 node_exporter/collector/powersupplyclass_linux.go
//                 node_exporter/collector/watchdog.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2021-2023 The Prometheus Authors, Apache-2.0
//
// Neither collector emits anything on EC2 (no batteries, no watchdog device exposed
// to the guest), and both report success=1 with zero series on the live cluster. So
// the parity requirement here is the same as the rest of the hardware group: NOT
// ErrNoData.
//
// THE TRAP IN powersupplyclass IS THREE DIFFERENT SCALE FACTORS applied to fields of
// the same struct, and this package now contains FOUR temperature/unit conventions
// that are easy to cross-contaminate:
//
//	powersupply temps   -> DECI-degrees   (/10)
//	thermal_zone temps  -> MILLI-degrees  (/1000)
//	powersupply volts   -> MICRO-volts    (/1e6)
//	pressure            -> MICROseconds   (/1e6)
//	schedstat           -> NANOseconds    (/1e9)
//
// Two temperature collectors in one package, 100x apart. Every divisor is therefore
// named in a table rather than written inline, and the table is diffed against
// upstream's source.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs/sysfs"
)

const powerSupplySubsystem = "power_supply"

func init() {
	register("powersupplyclass", true, newPowerSupplyClassCollector)
}

// defPowerSupplyIgnored is upstream's --collector.powersupply.ignored-supplies
// default: "^$" matches only the empty string, i.e. nothing.
const defPowerSupplyIgnored = "^$"

// powerSupplyNumeric pairs a metric name with its divisor and accessor.
//
// The DIVISOR IS PART OF THE TABLE deliberately. Written inline at 49 call sites it
// would be a matter of luck whether each one is right, and a wrong divisor produces a
// metric with the correct name, type and labels reporting a number off by 10, 1e6, or
// 1e5 -- all of which look like plausible hardware readings.
type powerSupplyNumeric struct {
	name    string
	divisor float64
	value   func(*sysfs.PowerSupply) *int64
}

// powerSupplyNumerics is upstream's three field maps, flattened with their divisors
// made explicit. Generated from upstream's source, not transcribed.
func powerSupplyNumerics() []powerSupplyNumeric {
	return []powerSupplyNumeric{
		// GROUP 1: no scaling. Counts, booleans and second-valued fields.
		{"authentic", 1, func(p *sysfs.PowerSupply) *int64 { return p.Authentic }},
		{"calibrate", 1, func(p *sysfs.PowerSupply) *int64 { return p.Calibrate }},
		{"capacity", 1, func(p *sysfs.PowerSupply) *int64 { return p.Capacity }},
		{"capacity_alert_max", 1, func(p *sysfs.PowerSupply) *int64 { return p.CapacityAlertMax }},
		{"capacity_alert_min", 1, func(p *sysfs.PowerSupply) *int64 { return p.CapacityAlertMin }},
		{"cyclecount", 1, func(p *sysfs.PowerSupply) *int64 { return p.CycleCount }},
		{"online", 1, func(p *sysfs.PowerSupply) *int64 { return p.Online }},
		{"present", 1, func(p *sysfs.PowerSupply) *int64 { return p.Present }},
		{"time_to_empty_seconds", 1, func(p *sysfs.PowerSupply) *int64 { return p.TimeToEmptyNow }},
		{"time_to_full_seconds", 1, func(p *sysfs.PowerSupply) *int64 { return p.TimeToFullNow }},
		// GROUP 2: divided by 1e6. The kernel reports these in MICRO-units
		// (microamps, microvolts, microwatt-hours), and the metric names are in base
		// units. A missed division here reports 12000000 volts.
		{"current_boot", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.CurrentBoot }},
		{"current_max", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.CurrentMax }},
		{"current_ampere", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.CurrentNow }},
		{"energy_empty", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.EnergyEmpty }},
		{"energy_empty_design", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.EnergyEmptyDesign }},
		{"energy_full", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.EnergyFull }},
		{"energy_full_design", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.EnergyFullDesign }},
		{"energy_watthour", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.EnergyNow }},
		{"voltage_boot", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.VoltageBoot }},
		{"voltage_max", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.VoltageMax }},
		{"voltage_max_design", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.VoltageMaxDesign }},
		{"voltage_min", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.VoltageMin }},
		{"voltage_min_design", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.VoltageMinDesign }},
		{"voltage_volt", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.VoltageNow }},
		{"voltage_ocv", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.VoltageOCV }},
		{"charge_control_limit", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ChargeControlLimit }},
		{"charge_control_limit_max", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ChargeControlLimitMax }},
		{"charge_counter", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ChargeCounter }},
		{"charge_empty", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ChargeEmpty }},
		{"charge_empty_design", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ChargeEmptyDesign }},
		{"charge_full", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ChargeFull }},
		{"charge_full_design", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ChargeFullDesign }},
		{"charge_ampere", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ChargeNow }},
		{"charge_term_current", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ChargeTermCurrent }},
		{"constant_charge_current", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ConstantChargeCurrent }},
		{"constant_charge_current_max", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ConstantChargeCurrentMax }},
		{"constant_charge_voltage", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ConstantChargeVoltage }},
		{"constant_charge_voltage_max", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.ConstantChargeVoltageMax }},
		{"precharge_current", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.PrechargeCurrent }},
		{"input_current_limit", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.InputCurrentLimit }},
		{"power_watt", 1e6, func(p *sysfs.PowerSupply) *int64 { return p.PowerNow }},
		// GROUP 3: divided by 10. Temperatures are in DECI-degrees Celsius, NOT
		// milli-degrees like thermal_zone. Two different temperature scales in one
		// package, 100x apart.
		{"temp_celsius", 10.0, func(p *sysfs.PowerSupply) *int64 { return p.Temp }},
		{"temp_alert_max_celsius", 10.0, func(p *sysfs.PowerSupply) *int64 { return p.TempAlertMax }},
		{"temp_alert_min_celsius", 10.0, func(p *sysfs.PowerSupply) *int64 { return p.TempAlertMin }},
		{"temp_ambient_celsius", 10.0, func(p *sysfs.PowerSupply) *int64 { return p.TempAmbient }},
		{"temp_ambient_max_celsius", 10.0, func(p *sysfs.PowerSupply) *int64 { return p.TempAmbientMax }},
		{"temp_ambient_min_celsius", 10.0, func(p *sysfs.PowerSupply) *int64 { return p.TempAmbientMin }},
		{"temp_max_celsius", 10.0, func(p *sysfs.PowerSupply) *int64 { return p.TempMax }},
		{"temp_min_celsius", 10.0, func(p *sysfs.PowerSupply) *int64 { return p.TempMin }}}
}

// powerSupplyLabel pairs an info-metric label with its accessor.
type powerSupplyLabel struct {
	name  string
	value func(*sysfs.PowerSupply) string
}

// powerSupplyLabels are the string fields that become node_power_supply_info labels.
//
// Like dmi, the label set is HOST-DEPENDENT: an empty value is omitted entirely
// rather than emitted blank, so node_power_supply_info has different labels on
// different hardware. Preserved.
func powerSupplyLabels() []powerSupplyLabel {
	return []powerSupplyLabel{
		{"power_supply", func(p *sysfs.PowerSupply) string { return p.Name }},
		{"capacity_level", func(p *sysfs.PowerSupply) string { return p.CapacityLevel }},
		{"charge_type", func(p *sysfs.PowerSupply) string { return p.ChargeType }},
		{"health", func(p *sysfs.PowerSupply) string { return p.Health }},
		{"manufacturer", func(p *sysfs.PowerSupply) string { return p.Manufacturer }},
		{"model_name", func(p *sysfs.PowerSupply) string { return p.ModelName }},
		{"serial_number", func(p *sysfs.PowerSupply) string { return p.SerialNumber }},
		{"status", func(p *sysfs.PowerSupply) string { return p.Status }},
		{"technology", func(p *sysfs.PowerSupply) string { return p.Technology }},
		{"type", func(p *sysfs.PowerSupply) string { return p.Type }},
		{"usb_type", func(p *sysfs.PowerSupply) string { return p.UsbType }},
		{"scope", func(p *sysfs.PowerSupply) string { return p.Scope }}}
}

type powerSupplyClassCollector struct {
	fs     sysfs.FS
	logger *slog.Logger

	ignoredPattern *regexp.Regexp

	// descs are cached: upstream builds a fresh prometheus.NewDesc for EVERY field of
	// EVERY power supply on EVERY scrape, which is 49 allocations per supply per
	// scrape for descriptors that never change. Harmless on a laptop with one
	// battery; pointless everywhere.
	descs map[string]*prometheus.Desc

	powerSupplyClass func() (sysfs.PowerSupplyClass, error)
}

func newPowerSupplyClassCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	return newPowerSupplyClassCollectorWithFilter(logger, paths, defPowerSupplyIgnored)
}

// newPowerSupplyClassCollectorWithFilter is split out so the invalid-pattern branch
// is reachable from a test.
func newPowerSupplyClassCollectorWithFilter(logger *slog.Logger, paths Paths, ignored string) (Collector, error) {
	fs, err := sysfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}

	pattern, err := regexp.Compile(ignored)
	if err != nil {
		return nil, fmt.Errorf("invalid ignored-supplies pattern %q: %w", ignored, err)
	}

	descs := make(map[string]*prometheus.Desc, len(powerSupplyNumerics()))
	for _, m := range powerSupplyNumerics() {
		descs[m.name] = prometheus.NewDesc(
			prometheus.BuildFQName(namespace, powerSupplySubsystem, m.name),
			fmt.Sprintf("%s value of /sys/class/power_supply/<power_supply>.", m.name),
			[]string{"power_supply"}, nil,
		)
	}

	c := &powerSupplyClassCollector{
		fs:             fs,
		logger:         logger,
		ignoredPattern: pattern,
		descs:          descs,
	}
	c.powerSupplyClass = c.fs.PowerSupplyClass
	return c, nil
}

func (c *powerSupplyClassCollector) Update(ch chan<- prometheus.Metric) error {
	supplies, err := c.powerSupplyClass()
	if err != nil {
		// UPSTREAM DISTINGUISHES THESE and so must we: a MISSING
		// /sys/class/power_supply is ErrNoData, while an unreadable one is a failure.
		//
		// On EC2 the directory EXISTS AND IS EMPTY (verified on the live node), so
		// PowerSupplyClass returns an empty map with a nil error and this collector
		// reports success with zero series -- which is what the cluster golden shows
		// (collector_success=1). My first version wrapped every error as a failure,
		// which would have reported success=0 on any host genuinely lacking the
		// directory.
		if errors.Is(err, os.ErrNotExist) {
			c.logger.Debug("power_supply class not found, skipping")
			return ErrNoData
		}
		return fmt.Errorf("could not get power_supply class info: %w", err)
	}

	// Iterated in sorted order so the emitted sequence is deterministic. Upstream
	// ranges over the map directly.
	names := make([]string, 0, len(supplies))
	for name := range supplies {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if c.ignoredPattern.MatchString(name) {
			continue
		}
		supply := supplies[name]
		c.emitSupply(ch, &supply)
	}
	return nil
}

// emitSupply emits the numeric metrics and the info metric for one power supply.
func (c *powerSupplyClassCollector) emitSupply(ch chan<- prometheus.Metric, supply *sysfs.PowerSupply) {
	for _, m := range powerSupplyNumerics() {
		value := m.value(supply)
		// nil means the sysfs file was absent. Emitting zero would claim a reading the
		// kernel never made -- a battery reporting 0 volts rather than no voltage
		// sensor.
		if value == nil {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.descs[m.name], prometheus.GaugeValue,
			float64(*value)/m.divisor, supply.Name)
	}

	// The info metric's label set is built per supply from the non-empty string
	// fields, so like dmi it is host-dependent. The Desc must therefore be built here
	// rather than cached, because its label NAMES vary by supply.
	var keys, values []string
	for _, l := range powerSupplyLabels() {
		value := l.value(supply)
		if value == "" {
			continue
		}
		keys = append(keys, l.name)
		// sysfs strings are not guaranteed valid UTF-8 and the text format requires
		// it; an invalid byte would make the whole exposition unparseable.
		values = append(values, strings.ToValidUTF8(value, "�"))
	}

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			prometheus.BuildFQName(namespace, powerSupplySubsystem, "info"),
			"info of /sys/class/power_supply/<power_supply>.",
			keys, nil,
		),
		prometheus.GaugeValue, 1.0, values...)
}

// ---------------------------------------------------------------------------
// watchdog
// ---------------------------------------------------------------------------

func init() {
	register("watchdog", true, newWatchdogCollector)
}

// watchdogNumeric pairs a metric name with its accessor. No divisors here: the
// kernel reports whole seconds.
type watchdogNumeric struct {
	name  string
	value func(*sysfs.WatchdogStats) *int64
}

func watchdogNumerics() []watchdogNumeric {
	return []watchdogNumeric{
		{"bootstatus", func(w *sysfs.WatchdogStats) *int64 { return w.Bootstatus }},
		{"fw_version", func(w *sysfs.WatchdogStats) *int64 { return w.FwVersion }},
		{"nowayout", func(w *sysfs.WatchdogStats) *int64 { return w.Nowayout }},
		{"timeleft_seconds", func(w *sysfs.WatchdogStats) *int64 { return w.Timeleft }},
		{"timeout_seconds", func(w *sysfs.WatchdogStats) *int64 { return w.Timeout }},
		{"pretimeout_seconds", func(w *sysfs.WatchdogStats) *int64 { return w.Pretimeout }},
		{"access_cs0", func(w *sysfs.WatchdogStats) *int64 { return w.AccessCs0 }},
	}
}

type watchdogCollector struct {
	fs     sysfs.FS
	logger *slog.Logger

	descs    map[string]*prometheus.Desc
	infoDesc *prometheus.Desc

	watchdogClass func() (sysfs.WatchdogClass, error)
}

func newWatchdogCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	fs, err := sysfs.NewFS(paths.SysFS)
	if err != nil {
		return nil, fmt.Errorf("failed to open sysfs at %s: %w", paths.SysFS, err)
	}

	descs := make(map[string]*prometheus.Desc, len(watchdogNumerics()))
	for _, m := range watchdogNumerics() {
		descs[m.name] = prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "watchdog", m.name),
			fmt.Sprintf("Value of /sys/class/watchdog/<watchdog>/%s",
				strings.TrimSuffix(m.name, "_seconds")),
			[]string{"name"}, nil,
		)
	}

	c := &watchdogCollector{
		fs:     fs,
		logger: logger,
		descs:  descs,
		// The info metric's label set is FIXED here, unlike power_supply and dmi:
		// upstream passes empty strings for absent values rather than omitting the
		// label. So this Desc can be built once.
		infoDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "watchdog", "info"),
			"Info of /sys/class/watchdog/<watchdog>",
			[]string{"name", "options", "identity", "state", "status", "pretimeout_governor"}, nil,
		),
	}
	c.watchdogClass = c.fs.WatchdogClass
	return c, nil
}

func (c *watchdogCollector) Update(ch chan<- prometheus.Metric) error {
	class, err := c.watchdogClass()
	if err != nil {
		// Three tolerated error kinds, one fewer than thermal_zone (no EINVAL).
		// Preserved as upstream has it rather than unified.
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) ||
			errors.Is(err, os.ErrInvalid) {
			c.logger.Debug("could not read watchdog stats", "err", err)
			return ErrNoData
		}
		return err
	}

	names := make([]string, 0, len(class))
	for name := range class {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		wd := class[name]
		for _, m := range watchdogNumerics() {
			value := m.value(&wd)
			if value == nil {
				continue
			}
			ch <- prometheus.MustNewConstMetric(c.descs[m.name], prometheus.GaugeValue,
				float64(*value), wd.Name)
		}

		// Unlike power_supply, the info labels are always all present -- absent values
		// become empty strings rather than omitted labels. That is upstream's choice
		// and the two collectors genuinely differ.
		ch <- prometheus.MustNewConstMetric(c.infoDesc, prometheus.GaugeValue, 1.0,
			wd.Name,
			watchdogLabelValue(wd.Options),
			watchdogLabelValue(wd.Identity),
			watchdogLabelValue(wd.State),
			watchdogLabelValue(wd.Status),
			watchdogLabelValue(wd.PretimeoutGovernor))
	}
	return nil
}

// watchdogLabelValue renders a possibly-absent string as a label value.
func watchdogLabelValue(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return *ptr
}
