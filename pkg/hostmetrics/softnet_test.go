package hostmetrics

// Tests for the softnet collector.
//
// A near-verbatim port with no filtering or unit conversion, so the tests focus on
// the two things that could still be wrong: the value-to-column mapping (a swap
// between dropped and times_squeezed would mean reporting packet loss as CPU
// scheduling pressure) and backlog_len being the only gauge.

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSoftnetEmitsSevenMetricsPerCPU(t *testing.T) {
	// The upstream fixture has 4 CPU rows, so 7 metrics x 4 CPUs = 28 series. A
	// dropped metric family or a skipped row both show up here.
	got := gatherSoftnet(t)

	assert.Len(t, got, 7, "seven metric families: %v", keysOf(boolKeys(got)))
	for name, byCPU := range got {
		assert.Len(t, byCPU, 4, "%s must be reported for all 4 CPUs in the fixture", name)
	}
}

func TestSoftnetValuesMapToTheRightColumns(t *testing.T) {
	// The fixture's columns are hex. Row 1 (CPU 1) is the only one with non-zero
	// values in more than one column, which makes it the row that catches a swap:
	//   000dfb82 00000029 0000000a ...
	//   processed=0xdfb82=916354  dropped=0x29=41  squeezed=0xa=10
	//
	// Reporting dropped as times_squeezed would mean showing packet loss as CPU
	// scheduling pressure, and both metrics would still exist with plausible values.
	got := gatherSoftnet(t)

	assert.Equal(t, 916354.0, got["node_softnet_processed_total"]["1"])
	assert.Equal(t, 41.0, got["node_softnet_dropped_total"]["1"],
		"dropped must come from column 2")
	assert.Equal(t, 10.0, got["node_softnet_times_squeezed_total"]["1"],
		"times_squeezed must come from column 3, not column 2")

	// CPU 0 has processed and squeezed set but dropped zero, which is the inverse
	// pattern and fails a swap in the other direction.
	assert.Equal(t, 299641.0, got["node_softnet_processed_total"]["0"])
	assert.Equal(t, 0.0, got["node_softnet_dropped_total"]["0"])
	assert.Equal(t, 1.0, got["node_softnet_times_squeezed_total"]["0"])
}

func TestSoftnetCPULabelIsTheRowIndex(t *testing.T) {
	// The label is the row index, which is the CPU number. Off-by-one here would
	// attribute every CPU's stats to its neighbour.
	got := gatherSoftnet(t)
	for _, cpu := range []string{"0", "1", "2", "3"} {
		assert.Contains(t, got["node_softnet_processed_total"], cpu)
	}
	assert.NotContains(t, got["node_softnet_processed_total"], "4",
		"the fixture has 4 CPUs, indices 0-3")
}

func TestSoftnetBacklogLenIsTheOnlyGauge(t *testing.T) {
	// backlog_len is a queue depth: it goes up and down. As a counter, rate() would
	// read every decrease as a reset. Everything else is cumulative.
	c := newSoftnetFixtureCollector(t)

	ch := make(chan prometheus.Metric, 512)
	require.NoError(t, c.Update(ch))
	close(ch)

	seen := map[string]bool{}
	for m := range ch {
		name := metricName(t, m)
		seen[name] = true

		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		if name == "node_softnet_backlog_len" {
			assert.NotNil(t, pb.Gauge, "backlog_len must be a gauge")
			assert.Nil(t, pb.Counter)
			continue
		}
		assert.NotNil(t, pb.Counter, "%s must be a counter", name)
		assert.Nil(t, pb.Gauge, "%s must not be a gauge", name)
	}
	require.True(t, seen["node_softnet_backlog_len"], "backlog_len must be emitted")
}

func TestSoftnetHelpStringsMatchUpstream(t *testing.T) {
	// Help text is part of the exposition output, so a reworded string is a diff
	// against the reference endpoint. Kept verbatim including upstream's grammar.
	data, err := os.ReadFile("../../../node_exporter/collector/softnet_linux.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	src := string(data)

	c := newSoftnetFixtureCollector(t)
	ch := make(chan prometheus.Metric, 512)
	require.NoError(t, c.Update(ch))
	close(ch)

	checked := 0
	for m := range ch {
		help := descHelp(t, m)
		if help == "" {
			continue
		}
		assert.Contains(t, src, help,
			"help string %q does not appear in upstream's source", help)
		checked++
	}
	assert.Positive(t, checked, "no help strings were checked")
}

func TestSoftnetMissingProcfsFile(t *testing.T) {
	root := t.TempDir()
	c, err := newSoftnetCollector(quietLogger(), Paths{ProcFS: root}.withDefaults())
	require.NoError(t, err, "construction must not read softnet_stat")

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not get softnet statistics")
}

func TestSoftnetConstructionFailsOnMissingProcfs(t *testing.T) {
	_, err := newSoftnetCollector(quietLogger(),
		Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open procfs")
}

// --- helpers --------------------------------------------------------------

func newSoftnetFixtureCollector(t *testing.T) Collector {
	t.Helper()
	root := t.TempDir()
	netDir := filepath.Join(root, "net")
	require.NoError(t, os.MkdirAll(netDir, 0o755))

	data, err := os.ReadFile("testdata/proc/net/softnet_stat")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(netDir, "softnet_stat"), data, 0o644))

	c, err := newSoftnetCollector(quietLogger(), Paths{ProcFS: root}.withDefaults())
	require.NoError(t, err)
	return c
}

// gatherSoftnet returns metric name -> cpu label -> value.
func gatherSoftnet(t *testing.T) map[string]map[string]float64 {
	t.Helper()

	c := newSoftnetFixtureCollector(t)
	ch := make(chan prometheus.Metric, 1024)
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
		if pb.Gauge != nil {
			out[name][cpu] = pb.GetGauge().GetValue()
			continue
		}
		out[name][cpu] = pb.GetCounter().GetValue()
	}
	return out
}

// descHelp extracts the help string from a metric's descriptor.
//
// Parsed from Desc().String() because prometheus.Desc exposes no help accessor.
// Anchored on `help: "` so it cannot accidentally capture the fqName.
func descHelp(t *testing.T, m prometheus.Metric) string {
	t.Helper()
	match := regexp.MustCompile(`help: "([^"]*)"`).FindStringSubmatch(m.Desc().String())
	if match == nil {
		return ""
	}
	return match[1]
}

// boolKeys adapts a value map to the shape keysOf expects.
func boolKeys[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
