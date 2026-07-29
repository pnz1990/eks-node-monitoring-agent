package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/os_release.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2021 The Prometheus Authors, Apache-2.0
//
// FIXES A FIFTH UPSTREAM DEFECT — A REAL DATA RACE, AND THIS ONE IS REACHABLE.
//
// Upstream declares an osMutex and then only half-uses it. UpdateStruct takes the
// WRITE lock:
//
//	c.osMutex.Lock()
//	defer c.osMutex.Unlock()
//	c.os, err = parseOSRelease(releaseFile)
//
// but Update reads the very same fields with NO read lock, after that deferred
// Unlock has already fired:
//
//	ch <- prometheus.MustNewConstMetric(osInfoDesc, prometheus.GaugeValue, 1.0,
//	    c.os.BuildID, c.os.ID, ...)   // <-- unguarded
//
// So two concurrent scrapes race: one is inside UpdateStruct writing c.os, c.version
// and c.supportEnd while the other reads them. Reproduced with the race detector
// against a faithful copy of upstream's locking structure — the detector reports
// writes at UpdateStruct against reads in Update, on c.os, c.version AND
// c.supportEnd.
//
// REACHABILITY, checked rather than assumed: node_exporter's --web.max-requests
// defaults to 40, so promhttp serves up to 40 concurrent scrapes and nothing
// serialises them. Two Prometheus servers scraping the same node — or one server
// plus a human running curl — is enough. That makes this materially different from
// the netstat blank-line panic, which needs input a kernel never produces.
//
// The presence of a half-used mutex says someone already knew this needed guarding.
//
// THE FIX: cache the parsed data once and hold the read lock for the whole emit.
// Re-reading /etc/os-release on every scrape is pointless anyway — the file cannot
// change without a reboot — so the parse now happens once and later scrapes take
// only a read lock, which also removes a file open per scrape.
//
// SCOPE: the macOS SystemVersion.plist branch is not ported. This is a Linux-only
// agent, the plist path cannot be reached, and it would drag in encoding/xml.
// Recorded in docs/parity-exceptions-nodep.md.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	envparse "github.com/hashicorp/go-envparse"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	etcOSRelease    = "/etc/os-release"
	usrLibOSRelease = "/usr/lib/os-release"
)

// osVersionRegex extracts the leading major[.minor] from VERSION_ID.
//
// VERSION_ID is a quoted string that may be "2023", "22.04", "9.4" or something
// with a suffix, so a bare ParseFloat fails on real distributions.
var osVersionRegex = regexp.MustCompile(`^[0-9]+\.?[0-9]*`)

func init() {
	register("os", true, newOSReleaseCollector)
}

var (
	osInfoDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "os", "info"),
		"A metric with a constant '1' value labeled by build_id, id, id_like, image_id, image_version, "+
			"name, pretty_name, variant, variant_id, version, version_codename, version_id.",
		[]string{"build_id", "id", "id_like", "image_id", "image_version", "name", "pretty_name",
			"variant", "variant_id", "version", "version_codename", "version_id"}, nil,
	)
	osVersionDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "os", "version"),
		"Metric containing the major.minor part of the OS version.",
		[]string{"id", "id_like", "name"}, nil,
	)
	osSupportEndDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "os", "support_end_timestamp_seconds"),
		"Metric containing the end-of-life date timestamp of the OS.",
		nil, nil,
	)
)

// osRelease is the parsed /etc/os-release.
type osRelease struct {
	Name            string
	ID              string
	IDLike          string
	PrettyName      string
	Variant         string
	VariantID       string
	Version         string
	VersionID       string
	VersionCodename string
	BuildID         string
	ImageID         string
	ImageVersion    string
	SupportEnd      string
}

// osReleaseState is everything derived from the file, kept together so a single
// pointer swap publishes it atomically under the lock rather than three separate
// field writes.
type osReleaseState struct {
	release    *osRelease
	version    float64
	supportEnd time.Time
}

type osReleaseCollector struct {
	logger    *slog.Logger
	filenames []string

	// mu guards state. Unlike upstream, EVERY access takes it — the read side too,
	// which is the actual fix.
	mu    sync.RWMutex
	state *osReleaseState
}

func newOSReleaseCollector(logger *slog.Logger, paths Paths) (Collector, error) {
	// Rebased onto the host root: in a container /etc/os-release is the CONTAINER's
	// (Amazon Linux base image), not the node's. Reporting the container's OS as the
	// node's OS would be wrong in a way that looks entirely plausible.
	return &osReleaseCollector{
		logger: logger,
		filenames: []string{
			paths.rootPath(etcOSRelease),
			paths.rootPath(usrLibOSRelease),
		},
	}, nil
}

func (c *osReleaseCollector) Update(ch chan<- prometheus.Metric) error {
	state, err := c.load()
	if err != nil {
		return err
	}

	// The whole emit happens against a local pointer, so nothing here can race with
	// a concurrent load even if the cache were invalidated between the two.
	r := state.release

	ch <- prometheus.MustNewConstMetric(osInfoDesc, prometheus.GaugeValue, 1.0,
		r.BuildID, r.ID, r.IDLike, r.ImageID, r.ImageVersion, r.Name, r.PrettyName,
		r.Variant, r.VariantID, r.Version, r.VersionCodename, r.VersionID)

	// Gated on > 0 rather than emitted as zero: a distribution whose VERSION_ID is
	// not numeric (some rolling releases) has no major.minor, and zero would read as
	// "version 0".
	if state.version > 0 {
		ch <- prometheus.MustNewConstMetric(osVersionDesc, prometheus.GaugeValue,
			state.version, r.ID, r.IDLike, r.Name)
	}

	if r.SupportEnd != "" {
		ch <- prometheus.MustNewConstMetric(osSupportEndDesc, prometheus.GaugeValue,
			float64(state.supportEnd.Unix()))
	}
	return nil
}

// load returns the cached state, parsing it on first use.
//
// The double-checked pattern is deliberate: the common path is a read lock only.
// /etc/os-release cannot change without a reboot, so re-reading it every scrape
// bought nothing and cost a file open per scrape on top of the race.
func (c *osReleaseCollector) load() (*osReleaseState, error) {
	if state := c.cached(); state != nil {
		return state, nil
	}
	return c.loadSlow()
}

// cached returns the parsed state, or nil if it has not been parsed yet.
//
// Split out from load so the fast path is a read lock and nothing more, and so the
// lock discipline is checkable by reading one four-line function rather than by
// tracing a branch. Upstream's bug was precisely a lock that looked held and was
// not.
func (c *osReleaseCollector) cached() *osReleaseState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// loadSlow parses under the write lock.
//
// The re-check is not redundant: between cached() returning nil and this acquiring
// the write lock, another goroutine may have parsed. Without it, N concurrent
// scrapes on a cold cache each parse and the last write wins -- harmless, but it
// reintroduces the per-scrape file read the cache exists to remove.
func (c *osReleaseCollector) loadSlow() (*osReleaseState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state != nil {
		return c.state, nil
	}

	state, err := c.parseFirstAvailable()
	if err != nil {
		return nil, err
	}
	c.state = state
	return state, nil
}

// parseFirstAvailable tries each candidate path in order.
//
// Only ErrNotExist advances to the next candidate. A file that exists but cannot be
// read or parsed is a real failure — silently falling through would report the
// wrong OS rather than an error.
func (c *osReleaseCollector) parseFirstAvailable() (*osReleaseState, error) {
	for _, path := range c.filenames {
		state, err := parseOSReleaseFile(path)
		if err == nil {
			return state, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return nil, err
	}

	// No os-release file anywhere. Not a failure: a scratch container has none.
	c.logger.Debug("no os-release file found", "paths", c.filenames)
	return nil, ErrNoData
}

func parseOSReleaseFile(path string) (*osReleaseState, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	release, err := parseOSRelease(f)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}

	state := &osReleaseState{release: release}

	if majorMinor := osVersionRegex.FindString(release.VersionID); majorMinor != "" {
		state.version, err = strconv.ParseFloat(majorMinor, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse VERSION_ID %q in %s: %w",
				release.VersionID, path, err)
		}
	}

	if release.SupportEnd != "" {
		state.supportEnd, err = time.Parse(time.DateOnly, release.SupportEnd)
		if err != nil {
			return nil, fmt.Errorf("failed to parse SUPPORT_END %q in %s: %w",
				release.SupportEnd, path, err)
		}
	}

	return state, nil
}

// parseOSRelease maps the file's shell-style assignments onto the struct.
//
// envparse handles the quoting and escaping rules, which matter: PRETTY_NAME is
// routinely quoted and contains spaces, and a naive split on "=" would keep the
// quotes in the label value.
func parseOSRelease(r io.Reader) (*osRelease, error) {
	env, err := envparse.Parse(r)
	if err != nil {
		return nil, err
	}
	return &osRelease{
		Name:            env["NAME"],
		ID:              env["ID"],
		IDLike:          env["ID_LIKE"],
		PrettyName:      env["PRETTY_NAME"],
		Variant:         env["VARIANT"],
		VariantID:       env["VARIANT_ID"],
		Version:         env["VERSION"],
		VersionID:       env["VERSION_ID"],
		VersionCodename: env["VERSION_CODENAME"],
		BuildID:         env["BUILD_ID"],
		ImageID:         env["IMAGE_ID"],
		ImageVersion:    env["IMAGE_VERSION"],
		SupportEnd:      env["SUPPORT_END"],
	}, nil
}
