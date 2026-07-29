package hostmetrics

// Tests for the os collector.
//
// The centrepiece is TestOSReleaseConcurrentScrapesAreRaceFree, the regression test
// for the FIFTH upstream defect found on this branch and the second real data race.
// Upstream declares an osMutex, takes the WRITE lock in UpdateStruct, and then reads
// the same fields in Update with no read lock at all.
//
// Unlike the netstat blank-line panic, this one is reachable in production:
// node_exporter's --web.max-requests defaults to 40, so promhttp serves concurrent
// scrapes and nothing serialises them. Two Prometheus servers, or one server plus a
// human with curl, is enough.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A real Amazon Linux 2023 /etc/os-release, which is what EKS nodes run.
const amazonLinuxOSRelease = `NAME="Amazon Linux"
VERSION="2023"
ID="amzn"
ID_LIKE="fedora"
VERSION_ID="2023"
PLATFORM_ID="platform:al2023"
PRETTY_NAME="Amazon Linux 2023.6.20241010"
ANSI_COLOR="0;33"
CPE_NAME="cpe:2.3:o:amazon:amazon_linux:2023"
HOME_URL="https://aws.amazon.com/linux/amazon-linux-2023/"
DOCUMENTATION_URL="https://docs.aws.amazon.com/linux/"
SUPPORT_URL="https://aws.amazon.com/premiumsupport/"
BUG_REPORT_URL="https://github.com/amazonlinux/amazon-linux-2023"
VENDOR_NAME="AWS"
VENDOR_URL="https://aws.amazon.com/"
SUPPORT_END="2028-03-15"
`

// --- THE RACE FIX ---------------------------------------------------------

// TestOSReleaseConcurrentScrapesAreRaceFree is the regression test for upstream's
// half-used mutex.
//
// Upstream's UpdateStruct takes the write lock and its deferred Unlock fires before
// Update reads c.os, c.version and c.supportEnd -- so a concurrent scrape reads
// those fields while another writes them. Reproduced against a faithful copy of
// upstream's structure with -race: the detector reports writes at UpdateStruct
// against reads in Update, on all three fields.
//
// Run under -race. With upstream's locking this reports a race; with the read lock
// held across the whole emit it does not.
func TestOSReleaseConcurrentScrapesAreRaceFree(t *testing.T) {
	c := newOSReleaseFixtureCollector(t, amazonLinuxOSRelease)

	// Eight concurrent scrapes, which promhttp permits: --web.max-requests defaults
	// to 40 upstream and nothing serialises collectors.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ch := make(chan prometheus.Metric, 32)
				assert.NoError(t, c.Update(ch))
			}
		}()
	}
	wg.Wait()
}

func TestOSReleaseParsesOnceAndCaches(t *testing.T) {
	// The cache is not an optimisation for its own sake: /etc/os-release cannot
	// change without a reboot, so re-reading it every scrape cost a file open per
	// scrape on top of the race. Asserted by deleting the file after the first
	// scrape -- later scrapes must still succeed.
	dir := t.TempDir()
	path := filepath.Join(dir, "etc", "os-release")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(amazonLinuxOSRelease), 0o644))

	c, err := newOSReleaseCollector(quietLogger(), Paths{RootFS: dir}.withDefaults())
	require.NoError(t, err)

	first := gatherOSRelease(t, c)
	require.Contains(t, first, "node_os_info")

	require.NoError(t, os.Remove(path))

	second := gatherOSRelease(t, c)
	assert.Equal(t, first, second,
		"the parse is cached; a scrape after the file disappears must still report")
}

func TestOSReleaseCacheIsPopulatedOnlyOnce(t *testing.T) {
	// Double-checked locking: the second caller must reuse the cached pointer rather
	// than reparsing. Asserted on pointer identity, since a second parse would
	// produce an equal-but-distinct value and an equality check would pass either way.
	c := newOSReleaseFixtureCollector(t, amazonLinuxOSRelease)

	first, err := c.load()
	require.NoError(t, err)
	second, err := c.load()
	require.NoError(t, err)

	assert.Same(t, first, second, "load must return the cached pointer, not a fresh parse")
}

func TestOSReleaseConcurrentLoadReturnsTheSameState(t *testing.T) {
	// Exercises the re-check inside the write lock: without it, N goroutines racing
	// on a cold cache would each parse and the last write would win, which is
	// harmless but wasteful -- and the re-check branch would be uncovered.
	c := newOSReleaseFixtureCollector(t, amazonLinuxOSRelease)

	var (
		mu     sync.Mutex
		states []*osReleaseState
		wg     sync.WaitGroup
	)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, err := c.load()
			assert.NoError(t, err)
			mu.Lock()
			states = append(states, state)
			mu.Unlock()
		}()
	}
	wg.Wait()

	require.Len(t, states, 16)
	for _, state := range states {
		assert.Same(t, states[0], state, "every caller must observe the same cached state")
	}
}

func TestOSReleaseWriteLockRecheckReturnsTheExistingState(t *testing.T) {
	// The double-checked re-check inside the write lock.
	//
	// My first attempt at this test raced two goroutines with a sleep to force the
	// interleaving. It was both flaky AND still did not cover the branch -- the
	// window is nanoseconds. Restructuring load() into cached() + loadSlow() made the
	// re-check directly callable, which is a better outcome than a timing-dependent
	// test: the lock discipline is now checkable by reading one short function, and
	// upstream's bug was precisely a lock that looked held and was not.
	c := newOSReleaseFixtureCollector(t, amazonLinuxOSRelease)

	seeded, err := c.parseFirstAvailable()
	require.NoError(t, err)
	c.state = seeded

	// loadSlow with the cache already populated is exactly the state a goroutine
	// finds after losing the race for the write lock.
	got, err := c.loadSlow()
	require.NoError(t, err)
	assert.Same(t, seeded, got, "the re-check must return the cached state, not reparse")
}

func TestOSReleaseCachedTakesOnlyAReadLock(t *testing.T) {
	// cached() must not block another reader. If it took the write lock, concurrent
	// scrapes would serialise on every scrape rather than only on the first.
	c := newOSReleaseFixtureCollector(t, amazonLinuxOSRelease)
	_, err := c.load()
	require.NoError(t, err)

	// Holding a read lock here would deadlock if cached() took the write lock.
	c.mu.RLock()
	defer c.mu.RUnlock()

	done := make(chan *osReleaseState, 1)
	go func() { done <- c.cached() }()

	select {
	case state := <-done:
		assert.NotNil(t, state, "a concurrent reader must observe the cached state")
	case <-time.After(2 * time.Second):
		t.Fatal("cached() blocked while a read lock was held; it must not take the write lock")
	}
}

func TestOSReleaseSlowPathErrorIsReturned(t *testing.T) {
	// loadSlow's error return, with no file present.
	c, err := newOSReleaseCollector(quietLogger(), Paths{RootFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	state, lErr := c.(*osReleaseCollector).loadSlow()
	require.Error(t, lErr)
	assert.Nil(t, state)
	assert.True(t, IsNoDataError(lErr))
}

// --- parsing --------------------------------------------------------------

func TestOSReleaseParsesAmazonLinux(t *testing.T) {
	// The distribution EKS actually runs. Quoted values must arrive unquoted: a
	// PRETTY_NAME label of `"Amazon Linux 2023.6.20241010"` with literal quotes is a
	// different label value than the one every dashboard matches.
	release, err := parseOSRelease(strings.NewReader(amazonLinuxOSRelease))
	require.NoError(t, err)

	assert.Equal(t, "Amazon Linux", release.Name)
	assert.Equal(t, "amzn", release.ID)
	assert.Equal(t, "fedora", release.IDLike)
	assert.Equal(t, "2023", release.VersionID)
	assert.Equal(t, "Amazon Linux 2023.6.20241010", release.PrettyName)
	assert.Equal(t, "2028-03-15", release.SupportEnd)
	for _, value := range []string{release.Name, release.PrettyName, release.VersionID} {
		assert.NotContains(t, value, `"`, "quotes must be stripped by the parser")
	}
}

func TestOSReleaseVersionExtractsMajorMinor(t *testing.T) {
	// VERSION_ID is not always a bare float: "22.04", "2023", "9.4" and suffixed
	// forms all occur, so a plain ParseFloat fails on real distributions.
	for versionID, want := range map[string]float64{
		"2023":    2023,
		"22.04":   22.04,
		"9.4":     9.4,
		"8":       8,
		"12.1.2":  12.1, // only major.minor is taken
		"":        0,
		"rolling": 0, // non-numeric yields no version metric at all
		"v2":      0, // leading non-digit does not match
	} {
		state := mustParseOSReleaseString(t, "VERSION_ID=\""+versionID+"\"\n")
		assert.Equal(t, want, state.version, "VERSION_ID=%q", versionID)
	}
}

func TestOSReleaseVersionMetricOmittedWhenNotNumeric(t *testing.T) {
	// A rolling release has no major.minor. Emitting 0 would read as "version 0"
	// rather than "no version reported".
	c := newOSReleaseFixtureCollector(t, "NAME=\"Rolling\"\nID=rolling\nVERSION_ID=\"rolling\"\n")

	got := gatherOSRelease(t, c)
	assert.Contains(t, got, "node_os_info")
	assert.NotContains(t, got, "node_os_version",
		"a non-numeric VERSION_ID must not produce version 0")
}

func TestOSReleaseSupportEndParsedAsTimestamp(t *testing.T) {
	c := newOSReleaseFixtureCollector(t, amazonLinuxOSRelease)

	got := gatherOSRelease(t, c)
	require.Contains(t, got, "node_os_support_end_timestamp_seconds")

	// Computed rather than hardcoded. My first attempt wrote 1836604800 from
	// memory, which is exactly 86400 short -- one day off, and a hardcoded epoch is
	// the kind of constant that is easy to get wrong and impossible to eyeball.
	want := time.Date(2028, 3, 15, 0, 0, 0, 0, time.UTC).Unix()
	assert.Equal(t, float64(want), got["node_os_support_end_timestamp_seconds"])

	// SUPPORT_END is a bare date with no timezone, so time.DateOnly parses it as
	// UTC midnight. A parser using the local zone would shift the timestamp by the
	// host's offset and the metric would differ between nodes in different regions.
	assert.Equal(t, int64(1836691200), want, "2028-03-15T00:00:00Z")
}

func TestOSReleaseSupportEndOmittedWhenAbsent(t *testing.T) {
	// Most distributions do not set SUPPORT_END. A zero timestamp would place the
	// end-of-life at 1970, which any alert on "support ending soon" would fire on.
	c := newOSReleaseFixtureCollector(t, "NAME=\"Test\"\nID=test\nVERSION_ID=\"1.0\"\n")

	got := gatherOSRelease(t, c)
	assert.NotContains(t, got, "node_os_support_end_timestamp_seconds",
		"an absent SUPPORT_END must not report the epoch")
}

func TestOSReleaseMalformedSupportEndIsAnError(t *testing.T) {
	// A present-but-unparseable date is a real problem, not something to skip: it
	// means the file is not what we think it is.
	c := newOSReleaseFixtureCollector(t,
		"NAME=\"Test\"\nID=test\nVERSION_ID=\"1.0\"\nSUPPORT_END=\"not-a-date\"\n")

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SUPPORT_END")
	assert.False(t, IsNoDataError(err))
}

func TestOSReleaseMalformedFileIsAnError(t *testing.T) {
	// envparse rejects genuinely malformed lines. Each of these is a REAL failing
	// input, found by running envparse directly rather than by inventing something
	// and hoping it fails:
	//
	//   "=novalue"        -> empty key
	//   `NAME="unterm`    -> unmatched quote
	//   `NAME="\xzz"`     -> invalid escape sequence
	//   "9INVALID=x"      -> key must start with [A-Za-z_]
	//   "NAME"            -> missing =
	//
	// A malformed os-release means the file is not what we think it is, so it is an
	// error rather than something to skip past.
	for name, content := range map[string]string{
		"empty key":         "=novalue\n",
		"unmatched quote":   "NAME=\"unterminated\n",
		"invalid escape":    "NAME=\"bad\\xzz escape\"\n",
		"bad key character": "9INVALID=x\n",
		"missing equals":    "NAME\n",
	} {
		t.Run(name, func(t *testing.T) {
			c := newOSReleaseFixtureCollector(t, content)
			err := c.Update(make(chan prometheus.Metric, 8))
			require.Error(t, err)
			assert.False(t, IsNoDataError(err),
				"a malformed file is a failure, not missing data")
			assert.Contains(t, err.Error(), "failed to parse")
		})
	}
}

func TestOSReleaseParseErrorIsPropagatedNotSwallowed(t *testing.T) {
	// Upstream's parseOSRelease returns the struct AND the error together
	// (`return &osRelease{...}, err`), so a caller that ignored the error would get a
	// half-populated struct that looks valid. Ours returns nil on error, which makes
	// that mistake impossible rather than merely discouraged.
	release, err := parseOSRelease(strings.NewReader("=novalue\n"))
	require.Error(t, err)
	assert.Nil(t, release, "no partial struct may escape alongside an error")
}

func TestOSReleaseUnparseableVersionIDIsAnError(t *testing.T) {
	// A VERSION_ID that MATCHES the digit regex but overflows float64. Contrived, but
	// it is the only way to reach the branch, and the branch has to exist because the
	// regexp guarantees only "starts with digits", not "parses as a float".
	huge := strings.Repeat("9", 400)
	c := newOSReleaseFixtureCollector(t, "VERSION_ID=\""+huge+"\"\n")

	err := c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "VERSION_ID")
	assert.False(t, IsNoDataError(err))
}

// --- info labels ----------------------------------------------------------

func TestOSReleaseInfoLabelsAreInUpstreamOrder(t *testing.T) {
	// Twelve string labels emitted positionally. A transposed pair yields a metric
	// with the right name and label KEYS and wrong values, which no structural
	// comparison catches -- so each label is checked against a distinguishable value.
	c := newOSReleaseFixtureCollector(t, strings.Join([]string{
		`BUILD_ID="BUILDID"`,
		`ID="IDVAL"`,
		`ID_LIKE="IDLIKE"`,
		`IMAGE_ID="IMAGEID"`,
		`IMAGE_VERSION="IMAGEVERSION"`,
		`NAME="NAMEVAL"`,
		`PRETTY_NAME="PRETTYNAME"`,
		`VARIANT="VARIANT"`,
		`VARIANT_ID="VARIANTID"`,
		`VERSION="VERSIONVAL"`,
		`VERSION_CODENAME="CODENAME"`,
		`VERSION_ID="9.9"`,
	}, "\n")+"\n")

	labels := osInfoLabels(t, c)
	assert.Equal(t, map[string]string{
		"build_id": "BUILDID", "id": "IDVAL", "id_like": "IDLIKE",
		"image_id": "IMAGEID", "image_version": "IMAGEVERSION", "name": "NAMEVAL",
		"pretty_name": "PRETTYNAME", "variant": "VARIANT", "variant_id": "VARIANTID",
		"version": "VERSIONVAL", "version_codename": "CODENAME", "version_id": "9.9",
	}, labels)
}

func TestOSReleaseInfoLabelNamesMatchUpstream(t *testing.T) {
	data, err := os.ReadFile("../../../node_exporter/collector/os_release.go")
	if err != nil {
		t.Skipf("upstream source not checked out alongside (%v)", err)
	}
	// Anchored on the osInfoDesc declaration and its []string block rather than on
	// proximity, which has cost me five failing tests on this branch.
	m := regexp.MustCompile(`(?s)osInfoDesc = prometheus\.NewDesc\(.*?\[\]string\{(.*?)\}`).
		FindSubmatch(data)
	require.NotNil(t, m, "failed to extract upstream's label list; the regexp may be stale")

	upstream := regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(m[1], -1)
	require.Len(t, upstream, 12, "expected 12 upstream labels, got %d", len(upstream))
	for _, want := range upstream {
		assert.Contains(t, osInfoDesc.String(), string(want[1]),
			"upstream label %q is missing", want[1])
	}
}

// --- file selection -------------------------------------------------------

func TestOSReleaseFallsBackToUsrLib(t *testing.T) {
	// /etc/os-release is a symlink to /usr/lib/os-release on some images and absent
	// on others, so the fallback is the normal path there rather than an edge case.
	dir := t.TempDir()
	path := filepath.Join(dir, "usr", "lib", "os-release")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("NAME=\"FromUsrLib\"\nID=usrlib\n"), 0o644))

	c, err := newOSReleaseCollector(quietLogger(), Paths{RootFS: dir}.withDefaults())
	require.NoError(t, err)

	assert.Equal(t, "FromUsrLib", osInfoLabels(t, c)["name"])
}

func TestOSReleasePrefersEtcOverUsrLib(t *testing.T) {
	dir := t.TempDir()
	for path, content := range map[string]string{
		filepath.Join(dir, "etc", "os-release"):        "NAME=\"FromEtc\"\nID=etc\n",
		filepath.Join(dir, "usr", "lib", "os-release"): "NAME=\"FromUsrLib\"\nID=usrlib\n",
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	c, err := newOSReleaseCollector(quietLogger(), Paths{RootFS: dir}.withDefaults())
	require.NoError(t, err)
	assert.Equal(t, "FromEtc", osInfoLabels(t, c)["name"], "/etc takes precedence")
}

func TestOSReleaseNoFileIsNoData(t *testing.T) {
	// A scratch container has no os-release. Not a failure.
	c, err := newOSReleaseCollector(quietLogger(), Paths{RootFS: t.TempDir()}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.True(t, IsNoDataError(err), "expected ErrNoData, got %v", err)
}

func TestOSReleaseUnreadableFileIsAnErrorNotAFallthrough(t *testing.T) {
	// A file that EXISTS but cannot be read must not silently fall through to the
	// next candidate -- that would report /usr/lib's contents while /etc's are the
	// truth, i.e. the wrong OS rather than an error.
	if os.Geteuid() == 0 {
		t.Skip("running as root, mode 000 is still readable")
	}
	dir := t.TempDir()
	etc := filepath.Join(dir, "etc", "os-release")
	require.NoError(t, os.MkdirAll(filepath.Dir(etc), 0o755))
	require.NoError(t, os.WriteFile(etc, []byte(amazonLinuxOSRelease), 0o000))
	t.Cleanup(func() { _ = os.Chmod(etc, 0o644) })

	usrLib := filepath.Join(dir, "usr", "lib", "os-release")
	require.NoError(t, os.MkdirAll(filepath.Dir(usrLib), 0o755))
	require.NoError(t, os.WriteFile(usrLib, []byte("NAME=\"WrongAnswer\"\n"), 0o644))

	c, err := newOSReleaseCollector(quietLogger(), Paths{RootFS: dir}.withDefaults())
	require.NoError(t, err)

	err = c.Update(make(chan prometheus.Metric, 8))
	require.Error(t, err)
	assert.False(t, IsNoDataError(err))
	assert.NotContains(t, err.Error(), "WrongAnswer")
}

func TestOSReleaseReadsFromTheHostRootNotTheContainer(t *testing.T) {
	// In a container /etc/os-release is the CONTAINER's, not the node's. Reporting
	// the container base image as the node OS would be wrong in a way that looks
	// entirely plausible, so the paths must be rebased onto RootFS.
	c, err := newOSReleaseCollector(quietLogger(), ForHostRoot("/host"))
	require.NoError(t, err)

	orc := c.(*osReleaseCollector)
	require.Len(t, orc.filenames, 2)
	assert.Equal(t, "/host/etc/os-release", orc.filenames[0])
	assert.Equal(t, "/host/usr/lib/os-release", orc.filenames[1])
}

func TestOSReleaseLiveOnThisHost(t *testing.T) {
	c, err := newOSReleaseCollector(quietLogger(), Paths{}.withDefaults())
	require.NoError(t, err)

	ch := make(chan prometheus.Metric, 32)
	err = c.Update(ch)
	close(ch)
	if err != nil && IsNoDataError(err) {
		t.Skip("no os-release on this host")
	}
	require.NoError(t, err)
	assert.NotEmpty(t, ch)
}

// --- helpers --------------------------------------------------------------

func newOSReleaseFixtureCollector(t *testing.T, content string) *osReleaseCollector {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "etc", "os-release")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	c, err := newOSReleaseCollector(quietLogger(), Paths{RootFS: dir}.withDefaults())
	require.NoError(t, err)
	return c.(*osReleaseCollector)
}

func mustParseOSReleaseString(t *testing.T, content string) *osReleaseState {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "os-release")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	state, err := parseOSReleaseFile(path)
	require.NoError(t, err)
	return state
}

func gatherOSRelease(t *testing.T, c Collector) map[string]float64 {
	t.Helper()

	ch := make(chan prometheus.Metric, 32)
	require.NoError(t, c.Update(ch))
	close(ch)

	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		out[metricName(t, m)] = pb.GetGauge().GetValue()
	}
	return out
}

func osInfoLabels(t *testing.T, c Collector) map[string]string {
	t.Helper()
	return singleMetricLabels(t, c, "node_os_info")
}
