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

// eksCollectorDefaults are upstream collector flags applied before the operator's
// own, so an explicit flag always wins (kingpin is last-wins).
//
// It is deliberately EMPTY. Two candidate defaults were measured against the
// parity harness and both changed the metric surface, so neither is applied:
//
//	--collector.netclass.ignored-devices=<pod interfaces>
//	    Mitigates #1915/#1841 by not reading pod-side veth/eni interfaces, which
//	    is where the churn and the per-device sysfs cost come from. But on an EKS
//	    node the ONLY interfaces reporting a link speed are those pod-side
//	    halves (verified on a live node: eni* report speed=10000 while the
//	    primary ens5/ens6 report an unreadable speed), so excluding them removes
//	    node_network_speed_bytes. Measured: 297 metric names vs upstream's 298.
//
//	--collector.netclass.netlink
//	    Replaces the per-device sysfs walk with a single netlink query, removing
//	    the listing-then-read race without excluding anything. But it ADDS
//	    node_network_altnames_info. Measured: 299 names vs upstream's 298.
//
// Since the per-collector timeout and panic recovery in resilience.go already
// contain the failure these would prevent, strict parity is the better default
// and the mitigations are left to operators. Both are documented with copyable
// flags in docs/prometheus-node-exporter-parity.md.
var eksCollectorDefaults = []string{
	// Exclude per-pod ephemeral mounts from the filesystem collector.
	//
	// The collector reads PID 1's mount table (filesystem_linux.go:185). Because
	// the agent runs with hostPID: true, PID 1 is the host's init and its mount
	// namespace contains every per-pod mount on the node — which upstream
	// node_exporter never sees, since it does not use hostPID. Measured on an
	// idle 20-pod node: 17 filesystems reported versus upstream's 4, the extra 13
	// being containerd sandbox shm and kubelet projected volumes.
	//
	// These mounts are per-pod, so the series count grows with pod density, and
	// their paths embed unique pod UIDs and sandbox IDs. That is unbounded
	// high-churn cardinality: every pod create/delete permanently adds a series
	// for the retention window. It also makes aggregations such as
	// sum(node_filesystem_size_bytes) disagree with upstream.
	//
	// This extends upstream's own intent rather than diverging from it: its
	// default already excludes var/lib/docker/.+ and
	// var/lib/containers/storage/.+ for the same reason, and simply has no entry
	// for containerd-on-Kubernetes.
	"--collector.filesystem.mount-points-exclude=" + eksExcludedMountPoints,
}

// eksExcludedMountPoints extends upstream's defMountPointsExcluded with the
// per-pod paths a Kubernetes node creates. The leading alternatives are upstream's
// defaults, repeated verbatim because supplying this flag replaces them rather
// than appending.
const eksExcludedMountPoints = `^/(dev|proc|run/credentials/.+|sys|var/lib/docker/.+|var/lib/containers/storage/.+` +
	// containerd pod sandbox shm mounts: /run/containerd/.../sandboxes/<id>/shm
	`|run/containerd/.+/sandboxes/.+` +
	// kubelet per-pod volumes, including projected service-account tokens:
	// /var/lib/kubelet/pods/<pod-uid>/volumes/...
	`|var/lib/kubelet/pods/.+` +
	`)($|/)`

// Operators on churn-heavy clusters may want to exclude pod-side interfaces from
// the netclass collector, trading node_network_speed_bytes for a smaller and more
// stable device set. This is NOT a default (see eksCollectorDefaults); the regexp
// is recorded here and in docs/prometheus-node-exporter-parity.md so it can be
// passed via extraArgs:
//
//	--collector.netclass.ignored-devices=^(veth.*|eni[0-9a-f]+|lo|docker[0-9]+|br-[0-9a-f]+|cali[0-9a-f]+|cni[0-9]+|tunl[0-9]+|nodelocaldns)$

// applyEKSDefaults prepends the EKS defaults to args so an operator's explicit
// flags, which come later, take precedence.
func applyEKSDefaults(args []string) []string {
	return append(append([]string{}, eksCollectorDefaults...), args...)
}

// ApplyEKSDefaultsForTest exposes applyEKSDefaults to the external test package so
// the parity-preserving invariant can be asserted without making the defaults
// themselves part of the public API.
func ApplyEKSDefaultsForTest(args []string) []string { return applyEKSDefaults(args) }

// EKSExcludedMountPointsForTest exposes eksExcludedMountPoints to the external
// test package so the exclusion regexp can be asserted against real mount paths.
func EKSExcludedMountPointsForTest() string { return eksExcludedMountPoints }
