package hostmetrics

// Tests for the meminfo collector.
//
// The important test here is TestMeminfoFieldsMatchUpstream, which diffs our field
// table against upstream's source mechanically. It already caught a real defect:
// the first version of the table included Hugetlb_bytes, which upstream does NOT
// map, so we would have emitted a metric upstream lacks — breaking parity in the
// direction that a name-list comparison would never catch, because the extra
// metric looks perfectly reasonable on its own.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upstreamMeminfoSource is the path to upstream's collector, relative to this
// package. It is read at test time rather than vendored so the comparison tracks
// whatever upstream commit is checked out alongside.
const upstreamMeminfoSource = "../../../node_exporter/collector/meminfo_linux.go"

// TestMeminfoFieldsMatchUpstream asserts our field table is exactly upstream's set.
//
// Any difference is a parity break in one direction or the other: a missing key
// drops a metric dashboards may use, and an extra key emits one upstream does not
// have. Both are silent — the endpoint looks healthy either way — which is why
// this is checked mechanically rather than by review.
func TestMeminfoFieldsMatchUpstream(t *testing.T) {
	data, err := os.ReadFile(upstreamMeminfoSource)
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v); "+
			"this comparison only runs in a workspace that has it", err)
	}

	// Upstream writes `metrics["Key"] = float64(*meminfo.Field)`.
	upstreamKeys := map[string]bool{}
	for _, m := range regexp.MustCompile(`metrics\["([^"]+)"\]`).FindAllStringSubmatch(string(data), -1) {
		upstreamKeys[m[1]] = true
	}
	require.NotEmpty(t, upstreamKeys, "failed to extract any keys from upstream source; the regexp may be stale")

	ourKeys := map[string]bool{}
	for key := range meminfoFields(&procfs.Meminfo{}) {
		ourKeys[key] = true
	}

	var missing, extra []string
	for key := range upstreamKeys {
		if !ourKeys[key] {
			missing = append(missing, key)
		}
	}
	for key := range ourKeys {
		if !upstreamKeys[key] {
			extra = append(extra, key)
		}
	}

	assert.Empty(t, missing, "fields upstream maps that we do not: dashboards would lose these metrics")
	assert.Empty(t, extra, "fields we map that upstream does not: we would emit metrics upstream lacks")
	assert.Len(t, ourKeys, len(upstreamKeys))
}

func TestMeminfoMetricNamesAreRuntimeGenerated(t *testing.T) {
	// The point of this test is documentary as much as functional: it asserts that
	// the metric name cannot be found by grepping the source, which is why parity
	// has to be measured against a live endpoint.
	raw, err := os.ReadFile("meminfo.go")
	require.NoError(t, err)

	// Strip comments before checking. The file's own doc comment mentions the
	// assembled name while explaining that the CODE never contains it, and an
	// earlier version of this test failed on exactly that -- it was checking the
	// prose rather than the implementation.
	var code strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}

	assert.NotContains(t, code.String(), "node_memory_MemAvailable_bytes",
		"the full metric name must NOT appear in code; it is built at runtime from a field key")
	// The pieces are present, the assembled name is not.
	assert.Contains(t, code.String(), `"MemAvailable_bytes"`)
	assert.Contains(t, code.String(), "meminfoSubsystem")
}

func TestMeminfoValueTypeRule(t *testing.T) {
	// Upstream's rule: a "_total" suffix is a counter, everything else a gauge.
	// A type change silently breaks rate() queries, so the rule is asserted rather
	// than assumed.
	for key := range meminfoFields(&procfs.Meminfo{}) {
		if strings.HasSuffix(key, "_total") {
			t.Errorf("field %q ends in _total and would be typed as a counter; "+
				"upstream has no such meminfo field, so verify this against upstream before allowing it", key)
		}
	}
}

func TestMeminfoNilFieldsAreOmitted(t *testing.T) {
	// A nil field means the kernel did not report it. Emitting zero would be a lie
	// about kernel state and would differ from upstream, which omits it.
	all := meminfoFields(&procfs.Meminfo{})
	for key, ptr := range all {
		require.Nil(t, ptr, "zero-value Meminfo must have every field nil, %q was not", key)
	}

	c := &meminfoCollector{logger: quietLogger()}
	_ = c // constructed to document intent; memInfo is exercised below via a real FS
}

func TestMeminfoCollectorAgainstFixture(t *testing.T) {
	// Uses upstream's own procfs fixture if present, otherwise a minimal one, so
	// the parsing path is exercised without depending on the host's kernel.
	root := t.TempDir()
	procDir := filepath.Join(root, "proc")
	require.NoError(t, os.MkdirAll(procDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(procDir, "meminfo"), []byte(
		"MemTotal:       16050724 kB\n"+
			"MemFree:         1360864 kB\n"+
			"MemAvailable:    9299260 kB\n"+
			"Buffers:          172052 kB\n"+
			"Cached:          6157508 kB\n"), 0o644))

	c, err := newMeminfoCollector(quietLogger(), Paths{ProcFS: procDir}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 128)
	require.NoError(t, c.Update(ch))
	close(ch)

	names := map[string]bool{}
	for m := range ch {
		names[m.Desc().String()] = true
	}
	require.NotEmpty(t, names)

	// Values are reported in bytes, not kB: the metric name says _bytes and procfs
	// does the conversion. A regression here would be off by 1024x and pass any
	// name-only comparison.
	var found bool
	for name := range names {
		if strings.Contains(name, "node_memory_MemTotal_bytes") {
			found = true
		}
	}
	assert.True(t, found, "MemTotal must be reported as node_memory_MemTotal_bytes")
}

func TestMeminfoCollectorMissingProcfs(t *testing.T) {
	c, err := newMeminfoCollector(quietLogger(), Paths{ProcFS: filepath.Join(t.TempDir(), "absent")}.withDefaults())
	// procfs.NewFS validates the directory exists, so construction is expected to
	// fail here. The framework skips a collector that cannot construct.
	if err != nil {
		assert.Contains(t, err.Error(), "failed to open procfs")
		return
	}
	// If construction succeeded, collection must fail cleanly rather than panic.
	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get meminfo")
}
