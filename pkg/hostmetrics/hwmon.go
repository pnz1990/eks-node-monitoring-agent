package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/hwmon_linux.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// THE ONE COLLECTOR WHOSE PARITY REQUIREMENT IS THAT IT MUST FAIL.
//
// Every other collector in the hardware group reports success=1 with zero series on
// EKS. hwmon reports success=0, because /sys/class/hwmon does not exist on an EC2
// guest at all and upstream returns ErrNoData rather than nil. Measured on the live
// cluster:
//
//	node_scrape_collector_success{collector="hwmon"} 0     <-- the only zero
//
// So "make it succeed" would be a parity BREAK here, not a fix. That inversion is why
// this collector is worth porting carefully despite emitting nothing: a plausible
// tidy-up ("why does this one fail? let's return nil like the others") would silently
// diverge from the reference endpoint on every node.
//
// WHY sysReadFile INSTEAD OF os.ReadFile, verbatim from upstream's reasoning: some
// hwmon drivers are broken and return EAGAIN on read, which makes Go's os.ReadFile
// POLL FOREVER. A single raw unix.Read either gets data or fails immediately. That is
// a hang in a scrape path, so it is reproduced exactly rather than simplified -- and
// it is the kind of detail that only shows up on the one machine that has the bad
// driver.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"
)

var (
	// hwmonInvalidMetricChars matches anything not valid in a metric name component.
	hwmonInvalidMetricChars = regexp.MustCompile("[^a-z0-9:_]")

	// hwmonFilenameFormat splits a sensor file into <type><num>_<property>.
	hwmonFilenameFormat = regexp.MustCompile(`^(?P<type>[^0-9]+)(?P<id>[0-9]*)?(_(?P<property>.+))?$`)

	// hwmonSensorTypes is upstream's allowlist. A file whose type is not here is
	// ignored entirely -- so this list is the difference between reporting a sensor
	// and silently dropping it.
	hwmonSensorTypes = []string{
		"vrm", "beep_enable", "update_interval", "in", "cpu", "fan",
		"pwm", "temp", "curr", "power", "energy", "humidity",
		"intrusion", "freq",
	}
)

var (
	hwmonLabelNames         = []string{"chip", "sensor"}
	hwmonChipNameLabelNames = []string{"chip", "chip_name"}
)

func init() {
	register("hwmon", true, newHwMonCollector)
}

type hwMonCollector struct {
	logger *slog.Logger
	paths  Paths

	deviceFilter deviceFilter
	sensorFilter deviceFilter
}

func newHwMonCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	return newHwMonCollectorWithFilters(logger, paths, "", "", "", "")
}

// newHwMonCollectorWithFilters is split out so the invalid-pattern branches are
// reachable from a test.
func newHwMonCollectorWithFilters(logger *slog.Logger, paths Paths,
	chipExclude, chipInclude, sensorExclude, sensorInclude string) (Collector, error) {

	deviceFilter, err := newDeviceFilter(chipExclude, chipInclude)
	if err != nil {
		return nil, fmt.Errorf("failed to build hwmon chip filter: %w", err)
	}
	sensorFilter, err := newDeviceFilter(sensorExclude, sensorInclude)
	if err != nil {
		return nil, fmt.Errorf("failed to build hwmon sensor filter: %w", err)
	}

	return &hwMonCollector{
		logger:       logger,
		paths:        paths,
		deviceFilter: deviceFilter,
		sensorFilter: sensorFilter,
	}, nil
}

func (c *hwMonCollector) Update(ch chan<- prometheus.Metric) error {
	hwmonRoot := c.paths.sysPath("class", "hwmon")

	entries, err := os.ReadDir(hwmonRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// THE EKS PATH. ErrNoData, which reports collector_success=0 -- and that is
			// the parity target, not something to improve.
			c.logger.Debug("hwmon collector metrics are not available for this system")
			return ErrNoData
		}
		return err
	}

	// Sorted so the emission order is deterministic across scrapes.
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	// Chip names can COLLIDE: multiple hwmon nodes can share one parent device (asus-
	// nb-wmi exposes one for fan control and another for WMI sensors), and two nodes
	// resolving to the same name would emit duplicate label sets, which Prometheus
	// rejects with "collected before with the same name and label values" -- failing
	// the ENTIRE scrape, not just this collector. So collisions are detected and
	// disambiguated before anything is emitted.
	type hwmonDir struct {
		dir  string
		name string
	}
	dirs := make([]hwmonDir, 0, len(names))
	nameCounts := make(map[string]int, len(names))

	for _, name := range names {
		dir := filepath.Join(hwmonRoot, name)
		hwmonName, err := c.hwmonName(dir)
		if err != nil {
			c.logger.Debug("could not derive hwmon name", "dir", dir, "err", err)
			continue
		}
		dirs = append(dirs, hwmonDir{dir: dir, name: hwmonName})
		nameCounts[hwmonName]++
	}

	seen := make(map[string]int, len(dirs))
	for _, d := range dirs {
		hwmonName := d.name
		if nameCounts[hwmonName] > 1 {
			// Disambiguate by appending the occurrence index. Upstream appends the
			// hwmonX directory name; either is stable, and the index keeps the label
			// shorter. Recorded in docs/parity-exceptions-nodep.md as the one place a
			// COLLIDING chip label differs -- it cannot occur on EKS, where the
			// directory does not exist at all.
			hwmonName = fmt.Sprintf("%s_%d", hwmonName, seen[d.name])
		}
		seen[d.name]++

		if c.deviceFilter.ignored(hwmonName) {
			continue
		}
		if err := c.updateHwmon(ch, d.dir, hwmonName); err != nil {
			return err
		}
	}

	return nil
}

// updateHwmon emits every sensor for one hwmon chip.
func (c *hwMonCollector) updateHwmon(ch chan<- prometheus.Metric, dir, hwmonName string) error {
	data := make(map[string]map[string]string)
	if err := collectHwmonSensorData(dir, data); err != nil {
		return err
	}

	// Some chips expose their sensors under a "device" subdirectory as well. Both are
	// merged into the same map, so a sensor present in both is read once.
	if _, err := os.Stat(filepath.Join(dir, "device")); err == nil {
		if err := collectHwmonSensorData(filepath.Join(dir, "device"), data); err != nil {
			return err
		}
	}

	if chipName, err := c.humanReadableChipName(dir); err == nil {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				"node_hwmon_chip_names",
				"Annotation metric for human-readable chip names",
				hwmonChipNameLabelNames, nil,
			),
			prometheus.GaugeValue, 1.0, hwmonName, chipName)
	}

	sensors := make([]string, 0, len(data))
	for sensor := range data {
		sensors = append(sensors, sensor)
	}
	sort.Strings(sensors)

	for _, sensor := range sensors {
		// The sensor filter matches "chip;sensor", so an operator can exclude one
		// sensor on one chip. The semicolon is part of the contract.
		if c.sensorFilter.ignored(hwmonName + ";" + sensor) {
			c.logger.Debug("ignoring sensor", "sensor", sensor)
			continue
		}
		c.emitSensor(ch, hwmonName, sensor, data[sensor])
	}
	return nil
}

// emitSensor emits the metrics for one sensor.
func (c *hwMonCollector) emitSensor(ch chan<- prometheus.Metric, hwmonName, sensor string, sensorData map[string]string) {
	_, sensorType, _, _ := explodeHwmonSensorFilename(sensor)
	labels := []string{hwmonName, sensor}

	if labelText, ok := sensorData["label"]; ok {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("node_hwmon_sensor_label", "Label for given chip and sensor",
				[]string{"chip", "sensor", "label"}, nil),
			prometheus.GaugeValue, 1.0,
			hwmonName, sensor, strings.ToValidUTF8(labelText, "�"))
	}

	// Two sensor types are handled whole rather than per element.
	switch sensorType {
	case "beep_enable":
		value := 0.0
		if sensorData[""] == "1" {
			value = 1.0
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("node_hwmon_beep_enabled", "Hardware beep enabled",
				hwmonLabelNames, nil),
			prometheus.GaugeValue, value, labels...)
		return
	case "vrm":
		parsed, err := strconv.ParseFloat(sensorData[""], 64)
		if err != nil {
			return
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("node_hwmon_voltage_regulator_version",
				"Hardware voltage regulator version", hwmonLabelNames, nil),
			prometheus.GaugeValue, parsed, labels...)
		return
	}

	elements := make([]string, 0, len(sensorData))
	for element := range sensorData {
		elements = append(elements, element)
	}
	sort.Strings(elements)

	for _, element := range elements {
		if element == "label" {
			continue
		}
		parsed, err := strconv.ParseFloat(sensorData[element], 64)
		if err != nil {
			// A non-numeric element (temp1_type is a string on some chips) is skipped
			// silently, matching upstream.
			continue
		}
		c.emitElement(ch, sensorType, element, parsed, sensorData, labels)
	}
}

// hwmonElementRule describes how one (sensorType, element) pair becomes a metric.
//
// Held as a table because upstream's version is a 12-branch if/else chain in which the
// ORDER of the branches is load-bearing -- "power"+"accuracy" must be tested before
// the general "power" case, and "temp"+"type" must be excluded before the general
// "temp" case. A table makes the precedence explicit instead of implicit in statement
// order.
type hwmonElementRule struct {
	// sensorType this rule applies to.
	sensorType string
	// elements this rule applies to; empty means "any element not claimed by a more
	// specific rule".
	elements []string
	// excludeElements are elements this rule must NOT claim.
	excludeElements []string
	// suffix appended to the metric name.
	suffix string
	// multiplier applied to the raw value.
	multiplier float64
	help       func(element string) string
	valueType  prometheus.ValueType
}

func hwmonElementRules() []hwmonElementRule {
	gauge := prometheus.GaugeValue
	return []hwmonElementRule{
		// MOST SPECIFIC FIRST. These three must precede the general "power" rule.
		{sensorType: "power", elements: []string{"accuracy"}, multiplier: 1.0 / 1000000.0,
			help:      func(string) string { return "Hardware monitor power meter accuracy, as a ratio" },
			valueType: gauge},
		{sensorType: "power",
			elements: []string{"average_interval", "average_interval_min", "average_interval_max"},
			suffix:   "_seconds", multiplier: 0.001,
			help:      func(e string) string { return "Hardware monitor power usage update interval (" + e + ")" },
			valueType: gauge},
		{sensorType: "power", suffix: "_watt", multiplier: 1.0 / 1000000.0,
			help:      func(e string) string { return "Hardware monitor for power usage in watts (" + e + ")" },
			valueType: gauge},

		// Voltage: millivolts -> volts.
		{sensorType: "in", suffix: "_volts", multiplier: 0.001,
			help:      func(e string) string { return "Hardware monitor for voltage (" + e + ")" },
			valueType: gauge},
		{sensorType: "cpu", suffix: "_volts", multiplier: 0.001,
			help:      func(e string) string { return "Hardware monitor for voltage (" + e + ")" },
			valueType: gauge},

		// Temperature: MILLI-degrees here, unlike power_supply's DECI-degrees.
		// "type" is excluded because it is a string, not a temperature.
		{sensorType: "temp", excludeElements: []string{"type"}, suffix: "_celsius", multiplier: 0.001,
			help:      func(e string) string { return "Hardware monitor for temperature (" + e + ")" },
			valueType: gauge},

		{sensorType: "curr", suffix: "_amps", multiplier: 0.001,
			help:      func(e string) string { return "Hardware monitor for current (" + e + ")" },
			valueType: gauge},

		// The only COUNTER in the set: joules accumulate.
		{sensorType: "energy", suffix: "_joule_total", multiplier: 1.0 / 1000000.0,
			help:      func(e string) string { return "Hardware monitor for joules used so far (" + e + ")" },
			valueType: prometheus.CounterValue},

		{sensorType: "humidity", multiplier: 1.0 / 1000000.0,
			help: func(e string) string {
				return "Hardware monitor for humidity, as a ratio (multiply with 100.0 to get the humidity as a percentage) (" + e + ")"
			},
			valueType: gauge},

		// Fan RPM is already in RPM: NO conversion, unlike everything around it.
		{sensorType: "fan", elements: []string{"input", "min", "max", "target"},
			suffix: "_rpm", multiplier: 1,
			help:      func(e string) string { return "Hardware monitor for fan revolutions per minute (" + e + ")" },
			valueType: gauge},
	}
}

// emitElement emits one sensor element, applying the first matching rule.
func (c *hwMonCollector) emitElement(ch chan<- prometheus.Metric, sensorType, element string,
	parsed float64, sensorData map[string]string, labels []string) {

	name := "node_hwmon_" + sensorType
	switch {
	case element == "input":
		// "input" IS the value, so it does not become part of the name -- UNLESS the
		// sensor also has a bare "" element, in which case both exist and must be
		// distinguished.
		if _, ok := sensorData[""]; ok {
			name += "_input"
		}
	case element != "":
		name += "_" + cleanHwmonMetricName(element)
	}

	// fault, alarm and beep are status flags with no unit, handled before the
	// unit-bearing rules.
	switch element {
	case "fault", "alarm":
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(name, "Hardware sensor "+element+" status ("+sensorType+")",
				hwmonLabelNames, nil),
			prometheus.GaugeValue, parsed, labels...)
		return
	case "beep":
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(name+"_enabled", "Hardware monitor sensor has beeping enabled",
				hwmonLabelNames, nil),
			prometheus.GaugeValue, parsed, labels...)
		return
	}

	// freq is special: it is emitted ONLY when the sensor has a label, and the label
	// REPLACES the sensor label rather than adding to it.
	if sensorType == "freq" && element == "input" {
		label, ok := sensorData["label"]
		if !ok {
			return
		}
		freqLabels := append(append([]string{}, labels[:len(labels)-1]...), cleanHwmonMetricName(label))
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(name+"_freq_mhz", "Hardware monitor for GPU frequency in MHz",
				hwmonLabelNames, nil),
			prometheus.GaugeValue, parsed/1000000.0, freqLabels...)
		return
	}

	for _, rule := range hwmonElementRules() {
		if rule.sensorType != sensorType {
			continue
		}
		if len(rule.elements) > 0 && !slices.Contains(rule.elements, element) {
			continue
		}
		if slices.Contains(rule.excludeElements, element) {
			continue
		}

		// temp's bare element is reported as "input" in the help text.
		helpElement := element
		if sensorType == "temp" && element == "" {
			helpElement = "input"
		}

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(name+rule.suffix, rule.help(helpElement), hwmonLabelNames, nil),
			rule.valueType, parsed*rule.multiplier, labels...)
		return
	}

	// Fallback: dump the value as-is. Reached by pwm, intrusion, update_interval and
	// any unit-less element of a typed sensor.
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(name, "Hardware monitor "+sensorType+" element "+element,
			hwmonLabelNames, nil),
		prometheus.GaugeValue, parsed, labels...)
}

// collectHwmonSensorData reads every recognised sensor file in a directory.
func collectHwmonSensorData(dir string, data map[string]map[string]string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, file := range files {
		ok, sensorType, sensorNum, sensorProperty := explodeHwmonSensorFilename(file.Name())
		if !ok {
			continue
		}
		// A type not in the allowlist is dropped entirely.
		if !slices.Contains(hwmonSensorTypes, sensorType) {
			continue
		}
		addHwmonValueFile(data, sensorType+strconv.Itoa(sensorNum), sensorProperty,
			filepath.Join(dir, file.Name()))
	}
	return nil
}

// addHwmonValueFile reads one sensor file into the data map.
//
// A read failure is silently skipped: hwmon files can disappear or return errors for
// individual sensors, and losing one reading must not cost the chip's other sensors.
func addHwmonValueFile(data map[string]map[string]string, sensor, property, file string) {
	raw, err := hwmonReadFile(file)
	if err != nil {
		return
	}
	if data[sensor] == nil {
		data[sensor] = make(map[string]string)
	}
	data[sensor][property] = strings.Trim(string(raw), "\n")
}

// hwmonReadFile reads a sysfs file with a single raw read.
//
// NOT os.ReadFile, and this is upstream's reasoning kept verbatim because it describes
// a hang rather than an inefficiency: some hwmon drivers are broken and return EAGAIN,
// which makes os.ReadFile POLL FOREVER. A single unix.Read either gets data or fails
// immediately. 128 bytes is upstream's buffer -- sensor values are short.
func hwmonReadFile(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	b := make([]byte, 128)
	n, err := unix.Read(int(f.Fd()), b)
	if err != nil {
		return nil, err
	}
	// NOTE: upstream also checks `n < 0`. That is unreachable -- unix.Read converts a
	// negative syscall return into a non-nil error, so n<0 with err==nil cannot occur.
	// Dropped for the same reason as the loop guard above.
	return b[:n], nil
}

// explodeHwmonSensorFilename splits a sensor filename into <type><num>_<property>.
func explodeHwmonSensorFilename(filename string) (ok bool, sensorType string, sensorNum int, sensorProperty string) {
	matches := hwmonFilenameFormat.FindStringSubmatch(filename)
	if len(matches) == 0 {
		return false, "", 0, ""
	}

	// NOTE: upstream guards this loop with `if i >= len(matches)`. That guard is
	// UNREACHABLE -- FindStringSubmatch always returns exactly 1+NumSubexp entries and
	// SubexpNames has the same length, so the indices cannot diverge. Verified against
	// the actual pattern (4 groups, so both are length 5). Dropped rather than kept as
	// permanently-uncovered code, since a guard that cannot fire is not protection, it
	// is noise that hides the guards that can.
	for i, name := range hwmonFilenameFormat.SubexpNames() {
		switch name {
		case "type":
			sensorType = matches[i]
		case "property":
			sensorProperty = matches[i]
		case "id":
			if matches[i] == "" {
				continue
			}
			// THIS guard, unlike the two dropped above, IS reachable: the id group is
			// [0-9]* with no length bound, so a filename like
			// "temp99999999999999999999999_input" parses as digits and overflows int.
			// Verified -- it returns ok=false, and the sensor is skipped rather than
			// recorded under a wrapped number.
			num, err := strconv.Atoi(matches[i])
			if err != nil {
				return false, sensorType, sensorNum, sensorProperty
			}
			sensorNum = num
		}
	}
	return true, sensorType, sensorNum, sensorProperty
}

// cleanHwmonMetricName lowercases and replaces characters invalid in a metric name.
func cleanHwmonMetricName(name string) string {
	lower := strings.ToLower(name)
	replaced := hwmonInvalidMetricChars.ReplaceAllLiteralString(lower, "_")
	return strings.Trim(replaced, "_")
}

// hwmonName derives a stable name for a chip.
//
// Sensor NUMBERING depends on module load order and is therefore unstable across
// reboots, so the name must come from the device path instead. Three preferences, in
// upstream's order.
func (c *hwMonCollector) hwmonName(dir string) (string, error) {
	// Preference 1: the device path, which is stable.
	if devicePath, err := filepath.EvalSymlinks(filepath.Join(dir, "device")); err == nil {
		prefix, devName := filepath.Split(devicePath)
		_, devType := filepath.Split(strings.TrimRight(prefix, "/"))

		cleanName := cleanHwmonMetricName(devName)
		cleanType := cleanHwmonMetricName(devType)
		if cleanType != "" && cleanName != "" {
			return cleanType + "_" + cleanName, nil
		}
		if cleanName != "" {
			return cleanName, nil
		}
	}

	// Preference 2: the human-readable "name" file.
	if raw, err := os.ReadFile(filepath.Join(dir, "name")); err == nil && len(raw) > 0 {
		if cleanName := cleanHwmonMetricName(string(raw)); cleanName != "" {
			return cleanName, nil
		}
	}

	// Preference 3: the hwmonX directory name. Unstable across reboots, which is why
	// it is last.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	_, name := filepath.Split(realDir)
	if cleanName := cleanHwmonMetricName(name); cleanName != "" {
		return cleanName, nil
	}

	return "", fmt.Errorf("could not derive a monitoring name for %s", dir)
}

// humanReadableChipName reads the chip's "name" file.
//
// Different precedence from hwmonName on purpose: duplicates are ALLOWED here, because
// this feeds an annotation metric rather than a series identity.
func (c *hwMonCollector) humanReadableChipName(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "name"))
	if err != nil {
		return "", err
	}
	if cleanName := cleanHwmonMetricName(string(raw)); cleanName != "" {
		return cleanName, nil
	}
	return "", fmt.Errorf("could not derive a human-readable chip type for %s", dir)
}
