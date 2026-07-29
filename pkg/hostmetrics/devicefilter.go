package hostmetrics

// PROVENANCE
//   derived from: node_exporter/collector/device_filter.go
//   upstream commit: b401dcfc667cee0a5d29232bab51a8ce1c58ec07
//   upstream copyright: 2019 The Prometheus Authors, Apache-2.0
//
// Shared by ten upstream collectors (diskstats, arp, filesystem, hwmon,
// interrupts, ethtool, netdev, infiniband, qdisc, slabinfo), so it lives in its
// own file here as it does upstream rather than being duplicated per collector.
//
// ONE DELIBERATE DIVERGENCE: upstream's newDeviceFilter uses regexp.MustCompile
// and therefore panics on an invalid pattern. That is tolerable upstream because
// the patterns arrive from kingpin, which parses before any collector is built, so
// a bad pattern panics during flag parsing with a stack trace at startup. Here the
// patterns can arrive from a Helm value, and a panic on first scrape inside a
// DaemonSet is a crash loop with no useful message. newDeviceFilter returns an
// error instead, and construction fails at startup with the offending pattern
// named. Behaviour for every valid pattern is identical.

import (
	"fmt"
	"regexp"
)

// deviceFilter decides whether a device should be reported.
//
// The two patterns are mutually exclusive by convention at the flag layer, but the
// type itself allows both, matching upstream: if both are set, a device must both
// not match ignore AND match accept.
type deviceFilter struct {
	ignorePattern *regexp.Regexp
	acceptPattern *regexp.Regexp
}

// newDeviceFilter compiles the exclude and include patterns.
//
// An empty pattern is not the same as a pattern matching nothing: an empty exclude
// means "exclude nothing", while an empty include means "include everything". If
// an empty include compiled to a regexp it would match every device as a substring
// and the distinction would be lost, which is why upstream leaves the field nil and
// this does too.
func newDeviceFilter(ignorePattern, acceptPattern string) (deviceFilter, error) {
	var f deviceFilter

	if ignorePattern != "" {
		re, err := regexp.Compile(ignorePattern)
		if err != nil {
			return deviceFilter{}, fmt.Errorf("invalid device exclude pattern %q: %w", ignorePattern, err)
		}
		f.ignorePattern = re
	}

	if acceptPattern != "" {
		re, err := regexp.Compile(acceptPattern)
		if err != nil {
			return deviceFilter{}, fmt.Errorf("invalid device include pattern %q: %w", acceptPattern, err)
		}
		f.acceptPattern = re
	}

	return f, nil
}

// ignored reports whether the device should be skipped.
func (f *deviceFilter) ignored(name string) bool {
	return (f.ignorePattern != nil && f.ignorePattern.MatchString(name)) ||
		(f.acceptPattern != nil && !f.acceptPattern.MatchString(name))
}
