package hostmetrics

// Tests for the collector framework: registry, config resolution, and set
// construction. These are the parts that decide *which* collectors run, so a bug
// here silently changes the metric surface rather than producing a visible error.

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test doubles ---------------------------------------------------------

type stubCollector struct {
	err error
}

func (s stubCollector) Update(ch chan<- prometheus.Metric) error { return s.err }

// withTestRegistry swaps in an isolated registry for the duration of a test, so
// tests do not depend on which real collectors happen to be registered.
func withTestRegistry(t *testing.T, entries map[string]registration) {
	t.Helper()
	registryMu.Lock()
	saved := registry
	registry = entries
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = saved
		registryMu.Unlock()
	})
}

func testRegistry() map[string]registration {
	return map[string]registration{
		"alpha": {name: "alpha", defaultEnabled: true, build: func(l *slog.Logger, p Paths) (Collector, error) {
			return stubCollector{}, nil
		}},
		"beta": {name: "beta", defaultEnabled: true, build: func(l *slog.Logger, p Paths) (Collector, error) {
			return stubCollector{}, nil
		}},
		"gamma": {name: "gamma", defaultEnabled: false, build: func(l *slog.Logger, p Paths) (Collector, error) {
			return stubCollector{}, nil
		}},
		"broken": {name: "broken", defaultEnabled: true, build: func(l *slog.Logger, p Paths) (Collector, error) {
			return nil, errors.New("no such device class on this host")
		}},
	}
}

// --- registry -------------------------------------------------------------

func TestRegisteredAndDefaultNames(t *testing.T) {
	withTestRegistry(t, testRegistry())

	assert.Equal(t, []string{"alpha", "beta", "broken", "gamma"}, RegisteredNames())
	// gamma defaults off, so it must not appear in the default set.
	assert.Equal(t, []string{"alpha", "beta", "broken"}, DefaultEnabledNames())
}

func TestRegisterDuplicatePanics(t *testing.T) {
	withTestRegistry(t, map[string]registration{})
	register("dup", true, func(l *slog.Logger, p Paths) (Collector, error) { return stubCollector{}, nil })

	// A duplicate name would silently shadow a collector, so this must fail loudly
	// at init rather than serve a subtly wrong metric set.
	assert.PanicsWithValue(t, `hostmetrics: collector "dup" registered twice`, func() {
		register("dup", true, func(l *slog.Logger, p Paths) (Collector, error) { return stubCollector{}, nil })
	})
}

// --- config resolution ----------------------------------------------------

func TestConfigResolve(t *testing.T) {
	withTestRegistry(t, testRegistry())

	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "defaults only",
			cfg:  Config{},
			want: []string{"alpha", "beta", "broken"},
		},
		{
			name: "include is exclusive and ignores default state",
			cfg:  Config{Include: []string{"gamma"}},
			want: []string{"gamma"},
		},
		{
			name: "enable adds a default-disabled collector",
			cfg:  Config{Enable: []string{"gamma"}},
			want: []string{"alpha", "beta", "broken", "gamma"},
		},
		{
			name: "disable removes a default-enabled collector",
			cfg:  Config{Disable: []string{"beta"}},
			want: []string{"alpha", "broken"},
		},
		{
			name: "disable wins over enable",
			cfg:  Config{Enable: []string{"gamma"}, Disable: []string{"gamma"}},
			want: []string{"alpha", "beta", "broken"},
		},
		{
			name: "disable applies within include",
			cfg:  Config{Include: []string{"alpha", "beta"}, Disable: []string{"beta"}},
			want: []string{"alpha"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cfg.resolve()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestConfigResolveUnknownName(t *testing.T) {
	withTestRegistry(t, testRegistry())

	// A typo must fail at startup rather than silently produce a smaller metric
	// set that someone discovers when a dashboard is empty.
	for _, cfg := range []Config{
		{Include: []string{"nope"}},
		{Enable: []string{"nope"}},
		{Disable: []string{"nope"}},
	} {
		_, err := cfg.resolve()
		require.Error(t, err)
		assert.Contains(t, err.Error(), `unknown collector "nope"`)
		// The error must list what IS available, or the operator has to guess.
		assert.Contains(t, err.Error(), "alpha")
	}
}

// --- set construction -----------------------------------------------------

func TestNewSkipsUnconstructableCollectors(t *testing.T) {
	withTestRegistry(t, testRegistry())

	// "broken" fails to construct. On a heterogeneous fleet some collectors cannot
	// construct (no sysfs entry for a device class, no permission), and refusing to
	// serve any metrics because one subsystem is absent would be worse than serving
	// the rest. Upstream fails the whole set here; this deliberately does not.
	set, err := New(quietLogger(), Config{})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "beta"}, set.Names())
	assert.Len(t, set.Collectors(), 2)
}

func TestNewFailsWhenNothingConstructs(t *testing.T) {
	withTestRegistry(t, map[string]registration{
		"broken": {name: "broken", defaultEnabled: true, build: func(l *slog.Logger, p Paths) (Collector, error) {
			return nil, errors.New("nope")
		}},
	})

	// Serving an endpoint with zero collectors would look healthy while reporting
	// nothing, which is worse than failing to start.
	_, err := New(quietLogger(), Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no collectors could be constructed")
}

func TestNewEmptySelectionIsNotAnError(t *testing.T) {
	withTestRegistry(t, testRegistry())

	// Every collector disabled is a legitimate configuration; it must not be
	// confused with "everything failed to construct".
	set, err := New(quietLogger(), Config{Disable: []string{"alpha", "beta", "broken"}})
	require.NoError(t, err)
	assert.Empty(t, set.Names())
}

func TestNewPropagatesUnknownName(t *testing.T) {
	withTestRegistry(t, testRegistry())
	_, err := New(quietLogger(), Config{Include: []string{"nope"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown collector")
}

func TestNewUsesConfiguredPaths(t *testing.T) {
	root := t.TempDir()
	captured := Paths{}
	withTestRegistry(t, map[string]registration{
		"probe": {name: "probe", defaultEnabled: true, build: func(l *slog.Logger, p Paths) (Collector, error) {
			captured = p
			return stubCollector{}, nil
		}},
	})

	_, err := New(quietLogger(), Config{Paths: ForHostRoot(root)})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "proc"), captured.ProcFS)
	assert.Equal(t, filepath.Join(root, "sys"), captured.SysFS)
}

// --- ErrNoData ------------------------------------------------------------

func TestIsNoDataError(t *testing.T) {
	assert.True(t, IsNoDataError(ErrNoData))
	// Must work through wrapping, since collectors wrap with context.
	assert.True(t, IsNoDataError(errWrap(ErrNoData)))
	assert.False(t, IsNoDataError(errors.New("something else")))
	assert.False(t, IsNoDataError(nil))
}

func errWrap(err error) error { return errors.Join(errors.New("context"), err) }
