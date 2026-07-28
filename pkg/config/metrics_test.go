package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/aws/eks-node-monitoring-agent/pkg/config"
)

// boolPtr is defined in monitor_test.go within this same test package.

func TestMetricsSettingsIsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		settings *config.MetricsSettings
		expected bool
	}{
		{
			// The metrics endpoint must never start implicitly on upgrade, so a
			// nil settings block means disabled.
			name:     "nil settings is disabled",
			settings: nil,
			expected: false,
		},
		{
			name:     "unset enabled is disabled",
			settings: &config.MetricsSettings{},
			expected: false,
		},
		{
			name:     "explicitly disabled",
			settings: &config.MetricsSettings{Enabled: boolPtr(false)},
			expected: false,
		},
		{
			name:     "explicitly enabled",
			settings: &config.MetricsSettings{Enabled: boolPtr(true)},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.settings.IsEnabled())
		})
	}
}

func TestIsMetricsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		cfg      *config.MonitorConfig
		expected bool
	}{
		{
			name:     "nil config is disabled",
			cfg:      nil,
			expected: false,
		},
		{
			name:     "config without metrics block is disabled",
			cfg:      &config.MonitorConfig{},
			expected: false,
		},
		{
			name:     "metrics block disabled",
			cfg:      &config.MonitorConfig{Metrics: &config.MetricsSettings{Enabled: boolPtr(false)}},
			expected: false,
		},
		{
			name:     "metrics block enabled",
			cfg:      &config.MonitorConfig{Metrics: &config.MetricsSettings{Enabled: boolPtr(true)}},
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.cfg.IsMetricsEnabled())
		})
	}
}

func TestGetMetricsSettings(t *testing.T) {
	t.Run("nil config returns zero value", func(t *testing.T) {
		var cfg *config.MonitorConfig
		assert.Equal(t, config.MetricsSettings{}, cfg.GetMetricsSettings())
	})

	t.Run("absent metrics block returns zero value", func(t *testing.T) {
		cfg := &config.MonitorConfig{}
		assert.Equal(t, config.MetricsSettings{}, cfg.GetMetricsSettings())
	})

	t.Run("populated metrics block is returned", func(t *testing.T) {
		cfg := &config.MonitorConfig{Metrics: &config.MetricsSettings{
			Enabled:    boolPtr(true),
			Address:    ":9100",
			Collectors: []string{"cpu", "meminfo"},
			ExtraArgs:  []string{"--no-collector.zfs"},
		}}
		got := cfg.GetMetricsSettings()
		assert.True(t, got.IsEnabled())
		assert.Equal(t, ":9100", got.Address)
		assert.Equal(t, []string{"cpu", "meminfo"}, got.Collectors)
		assert.Equal(t, []string{"--no-collector.zfs"}, got.ExtraArgs)
	})
}

func TestMetricsConfigYAMLRoundTrip(t *testing.T) {
	raw := []byte(`
metrics:
  enabled: true
  address: ":9100"
  collectors:
    - cpu
    - meminfo
  extraArgs:
    - "--no-collector.zfs"
  includeExporterMetrics: true
monitors:
  kernel-monitor:
    enabled: true
`)

	var cfg config.MonitorConfig
	require.NoError(t, yaml.Unmarshal(raw, &cfg))

	assert.True(t, cfg.IsMetricsEnabled())
	settings := cfg.GetMetricsSettings()
	assert.Equal(t, ":9100", settings.Address)
	assert.Equal(t, []string{"cpu", "meminfo"}, settings.Collectors)
	assert.Equal(t, []string{"--no-collector.zfs"}, settings.ExtraArgs)
	require.NotNil(t, settings.IncludeExporterMetrics)
	assert.True(t, *settings.IncludeExporterMetrics)
	// Existing monitor configuration must keep working alongside the new block.
	assert.True(t, cfg.IsMonitorEnabled("kernel-monitor"))
}

func TestMetricsAbsentFromConfigLeavesEndpointOff(t *testing.T) {
	// A config that only configures monitors must not enable the endpoint: this
	// is the upgrade-safety property the opt-in default exists to guarantee.
	raw := []byte("monitors:\n  networking:\n    enabled: false\n")

	var cfg config.MonitorConfig
	require.NoError(t, yaml.Unmarshal(raw, &cfg))
	assert.False(t, cfg.IsMetricsEnabled())
}
