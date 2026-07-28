// Package metrics serves host-level Prometheus metrics with feature parity to
// the upstream prometheus/node_exporter project, allowing the agent to replace a
// separately installed node_exporter deployment.
//
// NOTE ON NAMING: this package is unrelated to pkg/manager.NodeExporter, which
// exports Kubernetes NodeConditions and has nothing to do with Prometheus. To
// avoid that ambiguity nothing here is named "NodeExporter"; the upstream
// project is always referred to as "node_exporter" or "upstream".
package metrics

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/node_exporter/collector"
)

// kingpinOnce guards resolution of the upstream collector flags. The upstream
// collector package registers every --collector.<name> flag onto kingpin's
// package-level CommandLine during init(), and the values backing
// collector.NewNodeCollector are only populated once that CommandLine has been
// parsed. The agent itself uses pflag, so nothing else in this process parses
// kingpin; we must do it explicitly exactly once.
var kingpinOnce struct {
	sync.Once
	err error
}

// ResolveUpstreamFlags parses the upstream node_exporter flag definitions that
// were registered on kingpin's package-level CommandLine at init() time.
//
// It must be called before NewRegistry. Calling it more than once is safe; only
// the first call has any effect, and every caller observes the same result.
//
// args are upstream-style collector arguments (for example
// "--no-collector.zfs" or "--collector.textfile.directory=/var/lib/node_exporter").
// They are parsed by kingpin, not by the agent's pflag set, so they never
// collide with the agent's own command line.
func ResolveUpstreamFlags(args []string) error {
	kingpinOnce.Do(func() {
		kingpinOnce.err = parseKingpin(kingpin.CommandLine, args)
	})
	return kingpinOnce.err
}

// parseKingpin is split out from ResolveUpstreamFlags so the parse behaviour,
// including its error path, can be unit tested without the sync.Once latch.
//
// app is a parameter rather than kingpin.CommandLine directly so tests can drive
// a throwaway application; production callers always pass the package-level
// CommandLine that the upstream collectors registered onto.
func parseKingpin(app *kingpin.Application, args []string) error {
	// Terminate(nil) stops kingpin from calling os.Exit on a parse error, which
	// would take the whole agent down; the error is surfaced instead.
	app.Terminate(nil)
	if _, err := app.Parse(args); err != nil {
		return fmt.Errorf("failed to parse node_exporter collector flags %v: %w", args, err)
	}
	return nil
}

// NewCollector builds the upstream node_exporter collector set.
//
// filters optionally restricts the set to specific collector names; when empty
// every collector enabled by the resolved flag state is used, which is what
// gives parity with an unconfigured upstream node_exporter.
func NewCollector(logger *slog.Logger, filters ...string) (*collector.NodeCollector, error) {
	nc, err := collector.NewNodeCollector(logger, filters...)
	if err != nil {
		return nil, fmt.Errorf("failed to create node collector: %w", err)
	}
	return nc, nil
}

// EnabledCollectorNames returns the sorted names of the collectors in nc. It is
// used for startup logging and by tests asserting the enabled set.
func EnabledCollectorNames(nc *collector.NodeCollector) []string {
	if nc == nil {
		return nil
	}
	names := make([]string, 0, len(nc.Collectors))
	for name := range nc.Collectors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// HostPathArgs returns the upstream --path.* arguments needed so the collectors
// read the host filesystem rather than the container's own. The agent mounts the
// host root at HOST_ROOT (/host in the shipped DaemonSet), so every procfs,
// sysfs and udev path must be rebased onto it.
//
// When hostRoot is empty or "/" the upstream defaults already point at the right
// place and no arguments are returned.
func HostPathArgs(hostRoot string) []string {
	if hostRoot == "" || hostRoot == "/" {
		return nil
	}
	trimmed := strings.TrimSuffix(hostRoot, "/")
	return []string{
		"--path.procfs=" + trimmed + "/proc",
		"--path.sysfs=" + trimmed + "/sys",
		"--path.rootfs=" + trimmed,
		"--path.udev.data=" + trimmed + "/run/udev/data",
	}
}
