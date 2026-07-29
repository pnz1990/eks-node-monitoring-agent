package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/textfile.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2015 The Prometheus Authors, Apache-2.0
//
// The one collector that emits metrics it did not gather: it reads operator-supplied
// *.prom files and re-exports whatever is in them. On the live EKS node that is
// exactly ONE series -- node_textfile_scrape_error 0 -- because no directory is
// configured, which is the default.
//
// THAT SINGLE SERIES IS THE WHOLE PARITY REQUIREMENT HERE, and it is easy to get
// wrong in two directions:
//
//   - Returning ErrNoData when no directory is set would drop the series AND set
//     collector_success=0. Upstream emits scrape_error=0 and returns nil.
//   - Omitting the series when there is nothing to report would break the standard
//     alert on this collector, which is `node_textfile_scrape_error != 0`. An absent
//     series makes that alert silently un-evaluable rather than false.
//
// SCOPE: the FULL re-export path is ported (parsing, mtime, help-text conflict
// detection), because a customer who configures a textfile directory needs their
// metrics, not a stub. What is NOT ported is upstream's flag plumbing -- the
// directories are a struct field here, since this package has no flag layer.
//
// A NOTE ON WHY THIS COLLECTOR IS UNUSUAL: it is the only one whose output is
// attacker-influenced in the sense that a process able to write the directory controls
// metric names. Upstream accepts that by design; the mitigation is that the directory
// defaults to unset, so the capability does not exist unless an operator opts in.

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

func init() {
	register("textfile", true, newTextFileCollector)
}

var (
	textFileMtimeDesc = prometheus.NewDesc(
		"node_textfile_mtime_seconds",
		"Unixtime mtime of textfiles successfully read.",
		[]string{"file"}, nil,
	)
	// NOT namespaced through BuildFQName: upstream hardcodes both of these names
	// without the "node_" prefix being applied by BuildFQName, because they already
	// carry it. Using BuildFQName(namespace, "textfile", ...) would produce
	// node_textfile_textfile_scrape_error.
	textFileScrapeErrorDesc = prometheus.NewDesc(
		"node_textfile_scrape_error",
		"1 if there was an error opening or reading a file, 0 otherwise",
		nil, nil,
	)
)

type textFileCollector struct {
	logger *slog.Logger

	// paths are directories or globs to read. EMPTY BY DEFAULT, matching upstream:
	// the collector is enabled but reads nothing until an operator configures it.
	paths []string

	// now is injectable so the mtime metric is assertable.
	now func() time.Time
}

func newTextFileCollector(logger *slog.Logger, _ Paths) (Collector, error) {
	return &textFileCollector{logger: logger, now: time.Now}, nil
}

func (c *textFileCollector) Update(ch chan<- prometheus.Metric) error {
	errored := false

	families, mtimes := c.readAll(&errored)

	for _, mf := range families {
		c.emitFamily(ch, mf, &errored)
	}

	// mtime per file, in sorted order so the emission sequence is deterministic.
	// Upstream ranges over the map.
	paths := make([]string, 0, len(mtimes))
	for path := range mtimes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		ch <- prometheus.MustNewConstMetric(textFileMtimeDesc, prometheus.GaugeValue,
			float64(mtimes[path].UnixNano())/1e9, path)
	}

	// ALWAYS emitted, even with no directories configured and nothing read. This is
	// the one series the live EKS node reports, and the standard alert on this
	// collector tests it -- an absent series would make that alert un-evaluable
	// rather than false.
	errValue := 0.0
	if errored {
		errValue = 1.0
	}
	ch <- prometheus.MustNewConstMetric(textFileScrapeErrorDesc, prometheus.GaugeValue, errValue)

	return nil
}

// readAll reads every configured path, collecting metric families and mtimes.
//
// Sets *errored on any failure but keeps going: one unreadable file must not cost the
// metrics from the others, and the scrape_error series is how the failure is
// reported.
func (c *textFileCollector) readAll(errored *bool) ([]*dto.MetricFamily, map[string]time.Time) {
	var families []*dto.MetricFamily
	mtimes := make(map[string]time.Time)

	// A path may be a glob or a plain directory. Upstream tries Glob first and falls
	// back to treating it literally, which is what makes both forms work.
	var expanded []string
	for _, pattern := range c.paths {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			matches = []string{pattern}
		}
		expanded = append(expanded, matches...)
	}

	// Help texts seen per metric name, so a conflict between two files is detected
	// rather than silently resolved. First occurrence wins.
	helpTexts := make(map[string]string)
	sources := make(map[string][]string)

	for _, dir := range expanded {
		entries, err := os.ReadDir(dir)
		if err != nil && dir != "" {
			*errored = true
			c.logger.Error("failed to read textfile collector directory", "path", dir, "err", err)
			continue
		}

		// Sorted so which file "wins" a help-text conflict is deterministic. Upstream
		// relies on ReadDir's ordering, which is already sorted, but making it explicit
		// means a future change to the read cannot silently reorder it.
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".prom") {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)

		for _, name := range names {
			path := filepath.Join(dir, name)

			mtime, parsed, err := c.processFile(dir, name)

			// UPSTREAM PROCESSES THE FAMILIES BEFORE CHECKING err, AND THAT IS
			// LOAD-BEARING: expfmt returns the families it managed to parse ALONGSIDE
			// the error. Verified directly -- a file containing "good_metric 7" followed
			// by a malformed line yields 2 families and an error, and good_metric is one
			// of them.
			//
			// My first version checked err first and discarded them, which loses every
			// valid metric in a file with one bad line. The error is still recorded via
			// scrape_error, so nothing is hidden; the metrics are simply not thrown away.
			for _, mf := range sortedFamilies(parsed) {
				if prior, seen := helpTexts[mf.GetName()]; seen && mf.Help != nil && prior != mf.GetHelp() {
					// Two files disagree about the help text for one metric name. The
					// Prometheus text format allows only one, so this is a genuine
					// conflict and reporting it is more useful than picking silently.
					*errored = true
					c.logger.Error("inconsistent metric help text",
						"metric", mf.GetName(),
						"original_help_text", prior,
						"new_help_text", mf.GetHelp(),
						"file", strings.Join(sources[mf.GetName()], ", "))
					continue
				}
				if mf.Help != nil {
					helpTexts[mf.GetName()] = mf.GetHelp()
				}
				sources[mf.GetName()] = append(sources[mf.GetName()], path)
				families = append(families, mf)
			}

			if err != nil {
				*errored = true
				c.logger.Error("failed to collect textfile data", "file", name, "err", err)
				continue
			}

			mtimes[path] = *mtime
		}
	}

	// A family with no HELP line gets a synthetic one naming its source files, so the
	// exposition is valid and an operator can find where a metric came from.
	for _, mf := range families {
		if mf.Help != nil {
			continue
		}
		help := fmt.Sprintf("Metric read from %s", strings.Join(sources[mf.GetName()], ", "))
		mf.Help = &help
	}

	return families, mtimes
}

// emitFamily converts one parsed family into emitted metrics.
func (c *textFileCollector) emitFamily(ch chan<- prometheus.Metric, mf *dto.MetricFamily, errored *bool) {
	for _, m := range mf.Metric {
		names, values := labelPairsToNamesValues(m.Label)

		desc := prometheus.NewDesc(mf.GetName(), mf.GetHelp(), names, nil)

		value, valueType, ok := metricValueAndType(mf.GetType(), m)
		if !ok {
			// Histograms and summaries are not supported here, matching upstream's
			// handling of the simple types only. Flagged rather than dropped silently.
			*errored = true
			c.logger.Error("unsupported metric type in textfile",
				"metric", mf.GetName(), "type", mf.GetType().String())
			continue
		}

		metric, err := prometheus.NewConstMetric(desc, valueType, value, values...)
		if err != nil {
			// NOT MustNewConstMetric: the label set comes from an operator-supplied
			// file, so a malformed one is untrusted input rather than a programming
			// error. A panic here would take down the scrape over a bad text file.
			*errored = true
			c.logger.Error("failed to build metric from textfile",
				"metric", mf.GetName(), "err", err)
			continue
		}
		ch <- metric
	}
}

// metricValueAndType extracts the value and Prometheus type from a parsed metric.
func metricValueAndType(mfType dto.MetricType, m *dto.Metric) (float64, prometheus.ValueType, bool) {
	switch mfType {
	case dto.MetricType_COUNTER:
		return m.GetCounter().GetValue(), prometheus.CounterValue, true
	case dto.MetricType_GAUGE:
		return m.GetGauge().GetValue(), prometheus.GaugeValue, true
	case dto.MetricType_UNTYPED:
		return m.GetUntyped().GetValue(), prometheus.UntypedValue, true
	default:
		return 0, prometheus.UntypedValue, false
	}
}

// labelPairsToNamesValues splits parsed label pairs into parallel slices.
func labelPairsToNamesValues(pairs []*dto.LabelPair) ([]string, []string) {
	names := make([]string, 0, len(pairs))
	values := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		names = append(names, pair.GetName())
		values = append(values, pair.GetValue())
	}
	return names, values
}

// sortedFamilies returns the parsed families in name order, so emission is
// deterministic rather than dependent on map iteration.
func sortedFamilies(parsed map[string]*dto.MetricFamily) []*dto.MetricFamily {
	names := make([]string, 0, len(parsed))
	for name := range parsed {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]*dto.MetricFamily, 0, len(names))
	for _, name := range names {
		out = append(out, parsed[name])
	}
	return out
}

// processFile parses one *.prom file, returning its mtime on success.
func (c *textFileCollector) processFile(dir, name string) (*time.Time, map[string]*dto.MetricFamily, error) {
	path := filepath.Join(dir, name)

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	families, err := parseTextFile(f)
	if err != nil {
		// The families parsed so far are returned ALONGSIDE the error, matching
		// upstream and expfmt's own contract. A nil here would discard every valid
		// metric in a file with one bad line.
		return nil, families, fmt.Errorf("failed to parse textfile data from %q: %w", path, err)
	}

	// The mtime is read AFTER parsing, so a file rewritten mid-parse reports the
	// newer time -- which is the honest answer, since the parsed content may be from
	// either version and the newer mtime signals that it changed.
	//
	// A stat failure here returns the parsed families WITHOUT an mtime, so the metrics
	// survive and only node_textfile_mtime_seconds is lost. Reachable when the file is
	// unlinked between the open and the stat, which on a directory an operator writes
	// to is a real race rather than a theoretical one.
	return statTextFile(f, families)
}

// statTextFile reads the mtime of an already-parsed file.
//
// Split out so the stat-failure path is reachable from a test: on a healthy host an
// open file handle always stats successfully, and the branch would otherwise ship
// untested.
func statTextFile(f *os.File, families map[string]*dto.MetricFamily) (*time.Time, map[string]*dto.MetricFamily, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, families, err
	}
	mtime := stat.ModTime()
	return &mtime, families, nil
}

// parseTextFile parses the Prometheus text exposition format.
//
// The validation scheme MUST be passed explicitly. A zero-valued expfmt.TextParser
// panics with "Invalid name validation scheme requested: unset" in
// prometheus/common v0.70 -- so `var parser expfmt.TextParser` compiles, looks
// idiomatic, and crashes on the first file. Upstream uses LegacyValidation, which
// rejects UTF-8 metric names; matching it matters because a name accepted here and
// rejected by the reference endpoint is a parity difference in the operator's own
// metrics.
func parseTextFile(r io.Reader) (map[string]*dto.MetricFamily, error) {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	return parser.TextToMetricFamilies(r)
}
