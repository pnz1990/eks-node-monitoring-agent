package hostmetrics

// Tests for the loadavg collector.
//
// The canonical input is upstream's own fixture, copied verbatim to
// testdata/proc/loadavg, because it is the ground truth for parsing behaviour and
// re-deriving it would be both wasteful and less trustworthy. Everything beyond
// that is adversarial input upstream does not test.

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestParseLoadavgUpstreamFixture(t *testing.T) {
	// Verbatim from node_exporter/collector/fixtures/proc/loadavg.
	loads, err := parseLoadavg("0.21 0.37 0.39 1/719 19737", "testdata/proc/loadavg")
	require.NoError(t, err)
	assert.Equal(t, []float64{0.21, 0.37, 0.39}, loads)
}

func TestParseLoadavgEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []float64
		wantErr string
	}{
		{
			name:  "trailing newline",
			input: "0.00 0.01 0.05 1/234 5678\n",
			want:  []float64{0, 0.01, 0.05},
		},
		{
			name:  "extra fields are ignored",
			input: "1.5 2.5 3.5 1/234 5678 extra junk here",
			want:  []float64{1.5, 2.5, 3.5},
		},
		{
			name:  "exactly three fields",
			input: "1 2 3",
			want:  []float64{1, 2, 3},
		},
		{
			name:  "high precision preserved",
			input: "0.123456789 0.987654321 1.111111111 1/2 3",
			want:  []float64{0.123456789, 0.987654321, 1.111111111},
		},
		{
			name:  "large values",
			input: "1024.00 2048.50 4096.25 1/2 3",
			want:  []float64{1024, 2048.5, 4096.25},
		},
		{
			name:    "empty file",
			input:   "",
			wantErr: "unexpected content",
		},
		{
			name:    "too few fields",
			input:   "0.1 0.2",
			wantErr: "unexpected content",
		},
		{
			// The shape of upstream #1710: a kernel reporting a non-numeric
			// placeholder where a number is expected.
			name:    "unknown placeholder",
			input:   "<unknown> <unknown> <unknown> 1/2 3",
			wantErr: "could not parse load",
		},
		{
			name:    "non-numeric third field",
			input:   "0.1 0.2 banana 1/2 3",
			wantErr: "could not parse load",
		},
		{
			name:    "binary garbage",
			input:   "\x00\x01\x02 \x03 \x04",
			wantErr: "could not parse load",
		},
		{
			name:    "whitespace only",
			input:   "   \t  \n  ",
			wantErr: "unexpected content",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLoadavg(tc.input, "/proc/loadavg")
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				// The path must appear so the operator knows what to inspect.
				assert.Contains(t, err.Error(), "/proc/loadavg")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestReadLoadavgMissingFile(t *testing.T) {
	_, err := readLoadavg(filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err), "a missing procfs file must surface as a not-exist error")
}

func TestLoadavgCollectorUpdate(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "proc"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "proc", "loadavg"),
		[]byte("0.21 0.37 0.39 1/719 19737\n"), 0o644))

	c, err := newLoadavgCollector(quietLogger(), Paths{ProcFS: filepath.Join(root, "proc")}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 8)
	require.NoError(t, c.Update(ch))
	close(ch)

	// Three metrics, in the order node_load1, node_load5, node_load15, matching
	// upstream. Order matters because the descriptors are indexed positionally.
	var got []string
	for m := range ch {
		got = append(got, m.Desc().String())
	}
	require.Len(t, got, 3)
	assert.Contains(t, got[0], "node_load1")
	assert.Contains(t, got[1], "node_load5")
	assert.Contains(t, got[2], "node_load15")
}

func TestLoadavgCollectorUpdateError(t *testing.T) {
	c, err := newLoadavgCollector(quietLogger(), Paths{ProcFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "couldn't get load")
}
