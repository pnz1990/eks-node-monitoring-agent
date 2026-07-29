// Package hostmetrics collects host-level metrics from procfs and sysfs and
// exposes them in a format compatible with prometheus/node_exporter.
//
// PROVENANCE. This package is a derived work. The metric names, help strings,
// label sets and collection semantics are reproduced from
// github.com/prometheus/node_exporter (Apache-2.0) at commit b401dcfc, so that
// dashboards, recording rules and alerts written against node_exporter continue
// to work unchanged. The implementation is ours; the contract is theirs. Per-file
// provenance is recorded in docs/attribution.md.
//
// WHY THIS EXISTS. The agent previously imported node_exporter/collector
// directly. That worked and reached full parity, but coupled our metric output to
// an upstream release cycle, dragged kingpin's global flag registration into a
// pflag process, and left upstream collector defects as something to contain
// rather than fix. This package removes the dependency while keeping the output
// identical. The trade is that we now own the collectors: upstream fixes no
// longer arrive for free.
//
// SCOPE. Only the collectors that do something on an EKS node are implemented.
// Collectors for absent hardware, other operating systems, and upstream's
// default-disabled set are deliberately not ported; see
// docs/parity-exceptions-nodep.md for the list. This is narrower than the
// dependency-based implementation, which had every upstream collector by
// construction.
package hostmetrics

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// namespace prefixes every metric, matching node_exporter.
const namespace = "node"

// ErrNoData signals that a collector found nothing to report but did not fail.
//
// Upstream distinguishes this from a real error: it logs at debug rather than
// error, but still reports success=0. Reproduced exactly, because
// node_scrape_collector_success is part of the endpoint contract and is commonly
// alerted on.
var ErrNoData = errors.New("collector returned no data")

// IsNoDataError reports whether err is ErrNoData.
func IsNoDataError(err error) bool {
	return errors.Is(err, ErrNoData)
}

// Collector gathers one subsystem's metrics. It matches upstream's interface
// shape so ported collector bodies need no structural change, which keeps them
// diffable against their originals.
type Collector interface {
	// Update collects metrics and sends them to ch. Returning ErrNoData means
	// "nothing to report here", which is not a failure.
	Update(ch chan<- prometheus.Metric) error
}

// factory builds a collector. Separate from Collector so construction can fail
// (a missing sysfs path, an unparseable file) without the caller having to
// distinguish construction from collection.
type factory func(logger *slog.Logger, paths Paths) (Collector, error)

// registration is one collector's entry in the registry.
type registration struct {
	name           string
	defaultEnabled bool
	build          factory
}

var (
	registryMu sync.RWMutex
	registry   = map[string]registration{}
)

// register adds a collector to the package registry.
//
// Unlike upstream, this does NOT create a command-line flag as a side effect.
// Upstream's registerCollector calls kingpin.Flag() during init(), which is why
// importing it forced a second flag library into this process. Enablement here is
// resolved from configuration at construction time instead.
func register(name string, defaultEnabled bool, build factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		// A duplicate name would silently shadow a collector, so fail loudly at
		// init rather than serve a subtly wrong metric set.
		panic(fmt.Sprintf("hostmetrics: collector %q registered twice", name))
	}
	registry[name] = registration{name: name, defaultEnabled: defaultEnabled, build: build}
}

// RegisteredNames returns every registered collector name, sorted.
func RegisteredNames() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DefaultEnabledNames returns the collectors enabled unless configuration says
// otherwise, sorted. This set defines parity with an unconfigured node_exporter.
func DefaultEnabledNames() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for name, r := range registry {
		if r.defaultEnabled {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Config selects which collectors run and where they read from.
type Config struct {
	// Paths locates procfs, sysfs and rootfs. Zero value uses the host defaults.
	Paths Paths
	// Include, when non-empty, restricts collection to exactly these collectors,
	// ignoring their default state. Used for testing and for operators who want a
	// minimal set.
	Include []string
	// Enable force-enables collectors that default to disabled.
	Enable []string
	// Disable force-disables collectors that default to enabled. Applied after
	// Enable, so Disable wins a conflict — the safer resolution, since disabling
	// is the more conservative outcome.
	Disable []string
}

// resolve computes the enabled collector set from cfg.
//
// Order is Include (exclusive) → defaults + Enable → minus Disable. It returns an
// error for unknown names rather than ignoring them: a typo in a collector name
// should fail at startup, not silently produce a smaller metric set that someone
// discovers when a dashboard is empty.
func (cfg Config) resolve() ([]string, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	known := func(name string) bool {
		_, ok := registry[name]
		return ok
	}
	for _, group := range [][]string{cfg.Include, cfg.Enable, cfg.Disable} {
		for _, name := range group {
			if !known(name) {
				return nil, fmt.Errorf("unknown collector %q (registered: %v)", name, namesLocked())
			}
		}
	}

	selected := map[string]bool{}
	if len(cfg.Include) > 0 {
		for _, name := range cfg.Include {
			selected[name] = true
		}
	} else {
		for name, r := range registry {
			if r.defaultEnabled {
				selected[name] = true
			}
		}
		for _, name := range cfg.Enable {
			selected[name] = true
		}
	}
	for _, name := range cfg.Disable {
		delete(selected, name)
	}

	out := make([]string, 0, len(selected))
	for name := range selected {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// namesLocked returns registered names. Caller must hold registryMu.
func namesLocked() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Set is a constructed group of collectors ready to collect.
type Set struct {
	collectors map[string]Collector
	logger     *slog.Logger
}

// New builds the collector set described by cfg.
//
// A collector whose factory fails is reported and skipped rather than failing the
// whole set. On a heterogeneous fleet some collectors cannot construct — no sysfs
// entry for a device class, no permission — and refusing to serve any metrics
// because one subsystem is absent would be worse than serving the rest. Upstream
// fails the entire set in this case.
func New(logger *slog.Logger, cfg Config) (*Set, error) {
	names, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	paths := cfg.Paths.withDefaults()

	registryMu.RLock()
	defer registryMu.RUnlock()

	collectors := make(map[string]Collector, len(names))
	for _, name := range names {
		c, err := registry[name].build(logger.With("collector", name), paths)
		if err != nil {
			logger.Warn("collector unavailable on this host, skipping",
				"collector", name, "err", err)
			continue
		}
		collectors[name] = c
	}
	if len(collectors) == 0 && len(names) > 0 {
		return nil, fmt.Errorf("no collectors could be constructed from %v", names)
	}
	return &Set{collectors: collectors, logger: logger}, nil
}

// Collectors exposes the constructed collectors, keyed by name.
func (s *Set) Collectors() map[string]Collector {
	return s.collectors
}

// Names returns the constructed collector names, sorted.
func (s *Set) Names() []string {
	out := make([]string, 0, len(s.collectors))
	for name := range s.collectors {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
