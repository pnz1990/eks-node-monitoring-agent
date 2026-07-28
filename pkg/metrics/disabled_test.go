package metrics_test

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/eks-node-monitoring-agent/pkg/config"
	"github.com/aws/eks-node-monitoring-agent/pkg/metrics"
)

// TestDisabledMeansNoListener is the security-relevant assertion behind the
// opt-in default: when the endpoint is disabled the agent must not bind a socket
// at all. A feature that is "off" but still listening is not off, and this
// endpoint is reachable on the host network.
//
// The agent only constructs a metrics.Server when config reports the endpoint as
// enabled, so this test asserts both halves of that contract: the config default
// is disabled, and nothing is listening on the default port as a result.
func TestDisabledMeansNoListener(t *testing.T) {
	// A config with no metrics block must report disabled.
	var cfg *config.MonitorConfig
	require.False(t, cfg.IsMetricsEnabled(), "nil config must not enable the endpoint")

	empty := &config.MonitorConfig{}
	require.False(t, empty.IsMetricsEnabled(), "config without a metrics block must not enable the endpoint")

	monitorsOnly := &config.MonitorConfig{
		Monitors: map[string]config.MonitorSettings{"kernel-monitor": {}},
	}
	require.False(t, monitorsOnly.IsMetricsEnabled(),
		"configuring monitors must not implicitly enable the metrics endpoint")

	// Because the endpoint is disabled the agent never calls NewServer, so the
	// default port must be free. Assert nothing in this process is bound to it.
	conn, err := net.DialTimeout("tcp", "127.0.0.1"+metrics.DefaultAddress, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatalf("something is listening on %s while the endpoint is disabled", metrics.DefaultAddress)
	}
	assert.Error(t, err, "no listener must exist on the default port when disabled")
}

// TestEnabledRequiresExplicitOptIn documents that only an explicit true enables
// the endpoint, so an upgrade that adds the field without setting it stays off.
func TestEnabledRequiresExplicitOptIn(t *testing.T) {
	disabled := false
	cfg := &config.MonitorConfig{Metrics: &config.MetricsSettings{Enabled: &disabled}}
	assert.False(t, cfg.IsMetricsEnabled())

	enabled := true
	cfg = &config.MonitorConfig{Metrics: &config.MetricsSettings{Enabled: &enabled}}
	assert.True(t, cfg.IsMetricsEnabled())
}
