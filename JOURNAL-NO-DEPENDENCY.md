# Journal — host metrics with no node_exporter dependency

Append-only. Newest at the bottom. **Read this first on any resumption.** Companion to
`GOAL-NO-DEPENDENCY.md`; the dependency-based branch's record is in `JOURNAL.md`.

Rules: never rewrite history — a wrong conclusion gets a *new* correcting entry. Record refutations and
measurement mistakes as prominently as successes. Paste real values, not summaries.

---

## [2026-07-29T02:15Z] N0 — branch created, scope measured, interpretation question raised

**Phase:** N0
**Status:** confirmed

**What I did:** Branched `feat/metrics-no-upstream-dependency` from
`feat/prometheus-node-exporter-parity` so the entire validation apparatus carries over. Measured the
actual porting surface before planning.

**What I observed:**

| Component | Non-test lines | Files |
|---|---|---|
| `node_exporter/collector`, all platforms | 26,075 | 152 |
| `node_exporter/collector`, linux only | 19,636 | 100 |
| **the 49 collectors enabled on a live EKS node** | **~10,653** | 59 |
| **`prometheus/procfs`** | **21,639** | 13 packages |

**The finding that shapes the whole branch:** *54 of 152 collector files import `prometheus/procfs`.*
node_exporter's collectors are largely a thin metric-descriptor layer over `procfs`, which does the
actual `/proc` and `/sys` parsing. So vendoring `collector/` removes the *smaller* half of the code and
leaves the harder half as an external dependency.

This means "no dependency" is ambiguous, with three readings:

| Interpretation | Removes | Vendored lines |
|---|---|---|
| A | `node_exporter` only | ~10.6k |
| B | `node_exporter` + `procfs` | ~32k |
| C | clean-room, no copied code | write from scratch |

**Planned A**, with the residual stated openly: `procfs` is a general-purpose Prometheus-org parsing
library, the same class of dependency as `client_golang`, which we keep regardless because Prometheus
metric types come from it. B doubles the surface for little strategic gain and puts us in the business
of re-implementing `/proc/mountinfo` parsing, which is exactly where silent bugs live. C forfeits the
battle-testedness that makes any of this tractable.

**Open question posted to Slack (blocking N3):** if the intent behind "no dependency on PNE" is
specifically *no code from a competing exporter in our supply chain*, A satisfies it. If the intent is
*zero Prometheus-org dependencies*, A does **not**, and the scope roughly triples. Confirm before
porting begins.

**Two consequences recorded now so they are not discovered late:**

1. **Parity gets weaker, not stronger.** The dependency branch achieves parity on *all* upstream
   collectors including default-disabled ones. This branch ports only the 49 that run on EKS, so a
   customer enabling `--collector.buddyinfo` loses it. The honest claim becomes "parity on the 49
   collectors that run on EKS," and every unimplemented collector must be recorded.
2. **This cuts against the original motivation.** Contributing upstream becomes *harder* after this —
   it is a fork of a fork. That has to be weighed against the gains (no version coupling, direct bug
   fixes, no kingpin global-flag hack, smaller audit surface).

**Next:** N1 — extend the parity harness to three-way *before* writing any collector, and prove it
correctly **fails** against a stubbed implementation. A harness that cannot fail the new code is
worthless, and this is the same negative-control discipline that caught the dashboard verdict bug on
the previous branch.

---
## [2026-07-29T03:00Z] N1 complete — three-way harness, with both controls proven

**Phase:** N1
**Status:** confirmed

**What I did:** Wrote `hack/parity-test-3way.sh`, reusing the contract-extraction awk and both guards
(stale-port refusal, `node_exporter_build_info` verification) from the two-way harness. Then proved it
works in both directions *before* writing any collector code.

**Positive control** — the known-parity pair:
```
>> pne        298 metric names, 298 series shapes
>> nma-dep    298 metric names, 298 series shapes
>> PARITY  pne == nma-dep
EXIT=0
```

**Negative control** — a deliberately deficient stub standing in for `nma-nodep`, serving only
`node_load1`:
```
>> nma-nodep  1 metric names, 1 series shapes
>> DIVERGED  pne != nma-nodep  (594 only in pne, 0 only in nma-nodep)
>> DIVERGED  nma-dep != nma-nodep  (594 only in nma-dep, 0 only in nma-nodep)
EXIT=1
```

The harness detects 594 missing contract entries and exits non-zero. It can fail the new
implementation, which is the whole point of building it first.

**A bug in my own harness, found by the positive control failing when it should have passed.**

The `verify_is_node_exporter` guard reported "does not look like node_exporter" against a perfectly
healthy exporter. Three debugging steps, the first two of which were wrong:

1. Guessed the locally-built binary emits no `build_info` because its version string is empty.
   *Refuted:* `node_exporter_build_info{...,version=""} 1` is present, 3 occurrences.
2. Guessed a startup race — endpoint answers 200 before the registry populates. Added a 10s retry.
   *Refuted:* still failed after 20 attempts.
3. Ran with `bash -x` and read the exporter's own log. The tell was there:
   `msg="error encoding and sending metric family: ... write: broken pipe"`.

**Root cause:** `curl -s ... | grep -q pattern`. `grep -q` exits on the *first match*, closing the pipe;
curl then dies of SIGPIPE, and `set -o pipefail` propagates that as pipeline failure. So the guard
failed **precisely when the pattern matched** — inverted logic, invisible without reading the
subprocess log.

My standalone reproduction attempt missed it because I tested the *no-match* case, where curl reads the
whole body and exits 0. The bug only manifests on a match.

Fixed by buffering the body into a variable and using a bash pattern test instead of a pipe.

**The same latent bug exists in the two-way harness.** `hack/parity-test.sh:88` has the identical
`curl -s ... | grep -q` construct and also sets `pipefail`. It has not fired there yet, which is luck
rather than correctness: whether curl has finished writing when grep exits depends on body size and
scheduling. Fixed in both harnesses rather than left as a latent inverted guard.

**Next:** N2 — the `pkg/hostmetrics/` framework with no collectors: registry, dispatch, `--path.*`,
`ErrNoData`, meta metrics, native pflag config. Reuse `resilience.go` and `server.go` unchanged.

---
## [2026-07-29T03:20Z] N2 — framework built, first collector ported, no kingpin

**Phase:** N2 (framework) + start of N3 (collectors)
**Status:** confirmed

**What I did:** Built `pkg/hostmetrics/` — registry, config resolution, path handling, `ErrNoData`,
`typedDesc` — then ported the first collector (`loadavg`) end to end to validate the whole pipeline
before scaling up.

**Design decisions taken here, with the reasoning:**

1. **`register()` does NOT create a flag.** Upstream's `registerCollector` calls `kingpin.Flag()` during
   `init()`, which is precisely why importing it dragged a second flag library into a pflag process.
   Enablement is resolved from `Config` at construction time instead. **The kingpin bridge and its
   `sync.Once` latch disappear entirely** — that was one of the stated gains of this branch and it is
   already realised.

2. **A collector that fails to construct is skipped, not fatal.** Upstream fails the entire set if any
   factory errors. On a heterogeneous fleet some collectors genuinely cannot construct (no sysfs entry
   for a device class, no permission), and refusing to serve *any* metrics because one subsystem is
   absent is worse than serving the rest. But zero constructed collectors *is* an error, because an
   endpoint serving nothing while looking healthy is worse than failing to start.

3. **Unknown collector names fail at startup.** A typo must not silently produce a smaller metric set
   that someone discovers when a dashboard is empty. The error lists the registered names so the
   operator does not have to guess.

4. **Duplicate registration panics at init.** A duplicate name would silently shadow a collector.

5. **`typedDesc.mustNewConstMetric` keeps upstream's panic-on-mismatch.** Acceptable *only* because the
   resilience layer recovers per-collector panics on the goroutine that raises them, so a label
   mismatch degrades one collector rather than killing the agent. Without that guard this would be
   unacceptable in library code, and the comment says so.

**Provenance recorded per file** — upstream source file, commit `b401dcfc`, upstream copyright retained
alongside ours, and a note of what differs. `loadavg` is the template for the remaining 48.

**Test approach validated on `loadavg`:** upstream's own fixture (`fixtures/proc/loadavg`, verbatim) as
ground truth, plus 11 adversarial cases upstream does not test — empty file, too few fields, the
`<unknown>` placeholder shape from #1710, non-numeric fields, binary garbage, whitespace-only, extra
fields, high precision, large values. Errors must name both the offending value *and* the file path, so
an operator knows what to inspect.

**Gates:**
```
pkg/hostmetrics coverage   100.0% of statements
go test -race              clean
staticcheck                clean
gofmt / go vet             clean
```

**Next:** N3 — port the remaining 48 collectors in dashboard-value order, one at a time, each with
fixtures copied, tests ported, and the three-way harness green for that collector before moving on. Not
in bulk: a 10.6k-line commit that fails parity is undebuggable.

---
## [2026-07-29T03:35Z] N3 in progress — meminfo ported; a mechanical check caught a real parity break

**Phase:** N3
**Status:** confirmed (2 of 49 collectors ported: loadavg, meminfo)

**What I did:** Ported `meminfo`, deliberately chosen second because it is the collector that proves why
parity must be measured rather than enumerated.

**Finding — upstream silently drops kernel fields, and parity requires reproducing that.** Measured on
the test host:
```
keys in /proc/meminfo            55
fields upstream hand-maps        51
node_memory_* actually emitted   49
```
`procfs.Meminfo()` only models the keys it knows about, and upstream hand-maps a subset of those. So
upstream drops kernel fields it does not model. **Reusing `procfs` reproduces that exactly, including
the blind spot — which is what parity requires.** Writing our own `/proc/meminfo` parser would emit
*more* metrics than upstream and break parity in the opposite direction. That is an argument for
interpretation A that I had not anticipated when planning: keeping `procfs` is not merely convenient,
it is load-bearing for parity.

**A real parity break in my own code, caught mechanically within minutes of writing it.**

After porting, I diffed my field table against upstream's source programmatically rather than by eye:
```
upstream keys: 51   ours: 52   IDENTICAL: False
  extra in ours (1): ['Hugetlb_bytes']
```
I had added `Hugetlb_bytes` because `procfs.Meminfo` exposes `HugetlbBytes` and it looked like an
obvious omission on upstream's part. It is not in upstream's map, so emitting it would have produced a
metric upstream lacks.

**This is the failure mode that a name-list comparison would never catch**, because an *extra*
plausible-looking metric does not trip any "missing metric" check, and on a live diff it appears as
`+SERIES node_memory_Hugetlb_bytes` which is easy to rationalise as an improvement. Removed; now 51/51.

**Encoded as a permanent test**, `TestMeminfoFieldsMatchUpstream`, which parses upstream's source at
test time and fails on any asymmetry in either direction. It skips cleanly when upstream is not checked
out alongside, so it does not break CI elsewhere.

**A second self-inflicted test bug**, worth recording as a pattern: `TestMeminfoMetricNamesAreRuntimeGenerated`
asserts the assembled name never appears in the source — and failed, because the file's own *doc
comment* explains that the name never appears in code. The test was checking prose rather than
implementation. Fixed by stripping comment lines first. Two for two now on tests that failed because I
asserted against the wrong artifact.

**Gates after closing the coverage gap:**
```
pkg/hostmetrics coverage   100.0% of statements
go test -race              clean
staticcheck                clean
full suite                 35 packages, 0 failures
```
The last uncovered branch was upstream's "_total suffix means counter" rule, which is unreachable with
the real field table because no upstream meminfo field has that suffix. Rather than delete
upstream-faithful logic or exclude it from coverage, the field table was made injectable so a test can
reach the branch. The branch is kept deliberately: a future procfs field named `*_total` would otherwise
be silently typed as a gauge, breaking `rate()` on it.

**Next:** close the coverage gap, then continue N3 with `cpu`, `filesystem`, `diskstats`, `netdev`,
`stat`, `vmstat` — the remaining high-value collectors. Run the three-way harness once enough are ported
to make the comparison meaningful.

---
## [2026-07-29T03:50Z] N3 scoping finding — 10 of 49 collectors emit nothing on EKS

**Phase:** N3
**Status:** confirmed — changes the scope estimate materially

**What I did:** Before porting `cpu` (495 lines, 8 sub-collectors, 10 metric families), checked what it
actually emits on a live EKS node. Then generalised the question to all 49.

**What I observed:**

`cpu` emits only **2 of its 10** metric families on EKS:
```
node_cpu_seconds_total          16 series
node_cpu_guest_seconds_total     4 series
```
The sysfs-based families — `core_throttles_total`, `package_throttles_total`, `frequency_hertz`,
`flag_info`, `bug_info`, `isolated`, `online`, `info` — produce nothing, because EC2 does not expose
that sysfs. **PNE emits exactly the same 2 families with `success=1`**, so this is absence of hardware,
not a defect in either implementation.

Generalising across all 49 enabled collectors:
```
collectors enabled       49
  succeeding             39
  failing (no hardware)  10  -> bcachefs bonding fibrechannel hwmon ipvs
                                nfs nfsd rapl tapestats zfs
node_* families emitted  304
```

**Implication for the plan.** `GOAL-NO-DEPENDENCY.md` §3 scoped N3 as "port the 49 collectors observed
enabled." That was measured from `node_scrape_collector_success` *existing*, which conflated "enabled"
with "producing data". Ten of those collectors emit **nothing** on any EKS node — porting them means
writing code whose output is provably empty on every target host, purely so a `success=0` series
appears.

This is a real decision, not a detail:

- **Port all 49** — the `success=0` series for absent hardware is itself part of the contract. An
  operator alerting on `node_scrape_collector_success == 0` would see a different series set otherwise.
  Costs ~10 collectors of effort for zero metric output.
- **Port the 39 that produce data, and emit `success=0` for the other 10 without implementing them** —
  same observable contract on EKS, far less code. But it is a lie in the metric: we would be reporting
  "this collector ran and failed" when it never ran.
- **Port the 39 and drop the other 10 entirely** — honest, smallest, but the `success` series set
  differs from PNE, which the three-way harness will correctly flag as divergence.

**Leaning toward porting all 39 data-producing collectors first, then deciding on the 10 with the
harness in hand** — the diff will show exactly what dropping them costs, which is better evidence than
my judgement now. Recorded so the decision is explicit rather than drifting.

**Also worth noting:** the 10 failing collectors are the same set identified on the dependency branch as
"absent hardware, identical failure sets between both exporters." Consistent finding, arrived at
independently.

**Next:** port `cpu` (the 2 families that produce data, with the sysfs paths implemented so they work on
hardware that has them), then `stat`, `vmstat`, `filesystem`, `diskstats`, `netdev`.

---
## [2026-07-29T04:05Z] N3 — cpu ported (3 of 39). The monotonicity cache is the real content.

**Phase:** N3
**Status:** confirmed

**What I did:** Ported `cpu`. Upstream is 495 lines with 8 sub-collectors and 10 metric families; on EKS
only 2 families produce data, so the scope is much smaller than the line count suggests. Verified
against a live node first: PNE emits the same 2 families with `success=1`, so the sysfs-backed families
(core/package throttles, frequency, flag/bug info, isolated, online, info) are absent hardware rather
than a gap.

**The part that mattered, and that a naive port would have dropped.**

Upstream carries a per-CPU cache in `updateCPUStats` that looks like an optimisation and is actually
correctness:

- Kernel CPU counters can jump **backwards** — on CPU hotplug, and on some hypervisors.
- A Prometheus counter that decreases makes `rate()` emit a spike or a gap.
- So the cache only ever moves a counter forward, and separately resets a CPU's stats entirely if idle
  regresses by `>= 3s`, on the assumption the CPU was hotplugged and its counters restarted.
- Offline CPUs are deleted, or their stale series would be reported forever.

**None of that is visible in a single scrape.** A port that dropped it would pass every unit test I
would naturally have written and produce wrong graphs in production. I only found it by reading the code
rather than the metric output — which is an argument for reading each collector's implementation rather
than inferring behaviour from its emitted series.

Ported faithfully, with one structural change: upstream writes the monotonicity check as ten
near-identical `if` blocks; I expressed it as a table over field pointers so the rule is stated once.
Behaviour is identical, and a table cannot develop a copy-paste inconsistency between fields the way ten
blocks can. **`TestCPUStatFieldsCoversEveryEmittedMode` guards the coupling** — every emitted counter
must also be covered by the monotonicity rule, or it could silently regress.

**13 tests, focused on the invisible behaviour:** counter never decreases, normal advance, hotplug
reset, small-jump-is-not-a-hotplug, the `>=` threshold boundary, offline CPU removal, new CPU addition,
field-coverage coupling, multi-CPU series counts, guest toggle, and concurrent `Update` (the cache is
shared mutable state and `Collect` runs collectors in parallel).

**Gates:** coverage 100.0% · race clean · staticcheck clean.

**Progress: 3 of 39** data-producing collectors (`loadavg`, `meminfo`, `cpu`).

**Next:** `stat`, `vmstat`, `filesystem`, `diskstats`, `netdev`, `netclass`. `filesystem` and `netclass`
are the two with known upstream defects (#1672, #1915/#1841), so those are where fixing rather than
containing becomes possible.

---
## [2026-07-29T04:20Z] N3 — vmstat and stat ported (5 of 39). Found a real upstream panic.

**Phase:** N3
**Status:** confirmed

**Two parity-critical details in `vmstat`:**

1. **The default field filter.** `/proc/vmstat` has ~200 fields; upstream emits only those matching
   `^(oom_kill|pgpg|pswp|pg.*fault).*`, which on a live EKS node is exactly 7 series. Emitting the
   unfiltered set would produce ~200 series — the same "extra plausible metrics" failure as the
   `Hugetlb_bytes` mistake, and equally invisible to a missing-metric check. Asserted against upstream's
   source in `TestVMStatDefaultPatternMatchesUpstream`.

2. **An upstream panic this port fixes.** `collector/vmstat_linux.go`:
   ```go
   parts := strings.Fields(scanner.Text())
   value, err := strconv.ParseFloat(parts[1], 64)   // no length check
   ```
   A single-token or empty line in `/proc/vmstat` indexes `parts[1]` out of range and **panics**.
   Verified by reading the source; `strings.Fields("solitary")` returns a one-element slice.

   This is one of the "fix directly rather than contain" gains the branch was supposed to deliver, and
   it is the first concrete instance. On the dependency branch this panic is *contained* by the
   resilience layer — the agent survives, but vmstat produces nothing for that scrape. Here it does not
   happen at all: a malformed line is skipped with a debug log and the other ~199 fields still report.

   **Worth filing upstream.** Added to the follow-ups in `OPEN-QUESTIONS.md`.

**A deliberate divergence, recorded:** upstream returns an error on an unparseable *value*, failing the
whole collector. Skipping the single bad field keeps the rest, which is the better trade for a 200-field
file. This changes `node_scrape_collector_success` behaviour under malformed input — ours reports 1
where upstream reports 0. Recorded in `docs/parity-exceptions-nodep.md`; the three-way harness will not
see it, since it needs malformed input to trigger.

**`stat`** was straightforward. One detail that would silently rename six metrics: these use an *empty*
subsystem, so they sit directly under `node_` — `node_intr_total`, not `node_stat_intr_total`. Also
`node_softirqs_total` is gated on `--collector.stat.softirq` which defaults OFF, so it is not ported;
asserted in a test so it cannot appear by accident.

**Two more unreachable branches made testable rather than excluded:** the invalid-regexp check (kept
because the pattern becomes operator-configurable the moment it is wired to the chart) and
`scanner.Err()` (reached with a line exceeding `bufio.MaxScanTokenSize`).

**Gates:** coverage 100.0% · race clean · staticcheck clean.

**Progress: 5 of 39** — `loadavg`, `meminfo`, `cpu`, `vmstat`, `stat`.

**Next:** `filesystem`, `diskstats`, `netdev`, `netclass`. `filesystem` and `netclass` carry the known
upstream defects (#1672, #1915/#1841), so those are where "fix rather than contain" gets tested properly.

---
## [2026-07-29T04:45Z] N3 — filesystem ported (6 of 39), carrying both fixes

**Phase:** N3
**Status:** confirmed

**What I did:** Ported `filesystem` — 549 lines upstream, the most substantial collector so far, and the
one carrying both of this branch's concrete improvements.

**FIX 1 — upstream's data race, now structurally impossible.** Upstream's `GetStats` appends to the
result slice from two goroutines: the producer appends a `deviceError` entry per already-stuck mount
while the consumer loop appends everything drained from `statChan`. Here the stuck entry is *sent
through the same channel*, so there is exactly one writer. Regression test
`TestStuckMountEntryTravelsThroughTheChannel` runs under `-race`; if the entry were appended directly it
would report a race.

**FIX 2 — the pod-mount cardinality exclusion**, carried over from the dependency branch. The mount
table read is PID 1's, and under `hostPID` that is host init's namespace containing every per-pod mount.
`TestFilesystemNoPerPodMountsOnThisHost` asserts against the *real* host table that no reported mount
point is a per-pod path.

**The must-keep half of the exclusion test is the important one.** An over-broad regexp swallowing
`/var` or `/var/lib/kubelet` would hide a real disk-full condition — a worse failure than the
cardinality it fixes. `TestEKSExclusionKeepsRealFilesystems` pins 11 real mount points as must-keep.
Also `TestEKSExclusionExtendsUpstreamRatherThanReplacing`: supplying the flag *replaces* upstream's
default, so every alternative upstream excludes must be repeated or we would silently start reporting
filesystems upstream never did.

**Two of my own errors, caught by the compiler and by reading:**
1. `fs.Proc(1)` returns `(Proc, error)`; I used it in single-value context. Compiler caught it.
2. My fallback condition was `if !errors.Is(err, errors.Unwrap(err)) || err != nil` — incoherent
   nonsense that would have evaluated as `err != nil` by accident. Rewrote it plainly. Worth recording
   because it compiled fine after the first fix and only reading it revealed it was gibberish.

Also replaced a hand-rolled insertion sort with `sort.Strings` — no reason to reimplement it.

**Stuck-mount machinery ported faithfully**, including `clearStuck`: a mount that starts responding
again is forgotten, so a transient hang does not permanently suppress a filesystem. Upstream does this
and it is easy to miss.

**Four more unreachable branches made testable rather than excluded:** both invalid-regexp checks (kept
because the patterns become operator-configurable once wired to the chart), the hidepid fallback, and
its double-failure. All via injected seams; `.covignore` remains untouched.

**Gates:** coverage 100.0% · race clean · staticcheck clean.

**Progress: 6 of 39** — `loadavg`, `meminfo`, `cpu`, `vmstat`, `stat`, `filesystem`.

**Next:** `diskstats`, `netdev`, `netclass`. `netclass` carries #1915/#1841 — the third place where
fixing beats containing.

---
## [2026-07-29T05:05Z] N3 — netdev ported (7 of 39). The legacy() transformation is a SUM, not a rename.

**Phase:** N3
**Status:** confirmed

**The subtle part, and the most dangerous so far.** Upstream's `legacy()` does not merely rename kernel
fields to stable metric names — for four of its rules it **sums several kernel counters into one
metric**:

```
node_network_receive_frame_total    = receive_frame_errors + receive_length_errors
                                      + receive_over_errors + receive_crc_errors
node_network_transmit_carrier_total = transmit_carrier_errors + transmit_aborted_errors
                                      + transmit_heartbeat_errors + transmit_window_errors
node_network_receive_drop_total     = receive_dropped + receive_missed_errors
```

**Omitting a contributor produces a metric with the right name, right type, right labels, and the wrong
value.** No name comparison, no label comparison, and no series-shape diff would catch it — including
the three-way harness, which compares the contract rather than values. Only reading upstream's source
reveals it.

Ported as a rule table and verified mechanically: extracted upstream's `legacy()` body with a regexp,
parsed out (from, to, contributors) for each rule, and compared as sets.
```
upstream rules: 11    our rules: 10    IDENTICAL (as sets): True
```
The count differs because **upstream lists the `multicast` rule twice** — the second is a no-op, since
the first `pop()` already removed the key. Harmless redundancy in their code, not a missing rule in
ours. Recorded so the discrepancy in counts is not mistaken for a gap later.

**A subtlety I reproduced deliberately rather than "improving":** if a *contributor* is present but its
*source* field is not, upstream's rule never fires and the contributor surfaces as its own metric. That
looks like a bug, but changing it would diverge from upstream's output, so it is preserved and asserted
in `TestLegacyContributorWithoutSourceIsStillRemoved`.

**One more of my own test bugs, same family as the previous two.** I asserted the pre-legacy name
`multicast_total` never appears — but that is a substring of the *correct* name
`node_network_receive_multicast_total`, so the test failed against working code. Fixed by matching full
metric names with the `node_network_` prefix. **Third time now that a test failed because I asserted
against a loosely-specified artifact.** The pattern is clear enough to watch for deliberately.

**Gates:** coverage 100.0% · race clean · staticcheck clean.

**Progress: 7 of 39** — `loadavg`, `meminfo`, `cpu`, `vmstat`, `stat`, `filesystem`, `netdev`.

**Next:** `diskstats`, then `netclass` (#1915/#1841 — the all-or-nothing device read).

---
## N3 — `netclass`: fixing upstream #1915 / #1841 at the cause

The third and most consequential "fix rather than contain" instance on this branch.

Upstream's `getNetClassInfo` enumerates `/sys/class/net`, then reads each device:

```go
for _, device := range netDevices {
    interfaceClass, err := c.fs.NetClassByIface(device)
    if err != nil {
        return netClass, err     // <-- ONE bad device discards EVERYTHING
    }
    netClass[device] = *interfaceClass
}
```

A device present at *listing* time and gone at *read* time makes the collector emit **nothing** — not
partial data. Every interface on the node loses its metrics because one veth vanished.

**Why this has been open since 2020 upstream and matters here.** On a static host, interfaces
essentially never disappear, so the listing-to-read window is almost never hit. On EKS, the VPC CNI
creates and destroys veth and eni interfaces on *every pod schedule*, so the window is hit routinely
rather than rarely. Same code, different failure rate — which is why it reads as a low-priority
upstream issue and a real one for this use case.

**The fix:** skip the unreadable device, log at debug, keep going. No readable devices at all returns
`ErrNoData` (genuinely nothing to report); an unreadable `class/net` returns an error (could not look).
Those are different conditions and `TestNetClassEnumerationFailureIsWrapped` asserts the second is
*not* `ErrNoData`, so a future refactor cannot quietly collapse "collection broke" into "no data".

**This also retires an EKS workaround.** On the dependency branch I considered excluding pod-side
interfaces via `--collector.netclass.ignored-devices` to dodge the churn hazard, and measured that it
*costs* `node_network_speed_bytes` — 297 metric names versus upstream's 298, because on that node
every remaining interface reported an invalid speed. With the read fixed at its cause, the exclusion is
unnecessary: the default stays upstream's `^$` (matches nothing) and the filter exists only as a seam.
Fixing the bug was strictly better than not looking.

**Regression test.** `TestNetClassSkipsUnreadableDevice` builds a fake `/sys/class/net` with three
devices, then makes one unreadable while its directory entry still lists — the exact
listing-then-read race. Upstream returns 0 of 3 here; we must return the 2 readable ones. Verified it
fails against upstream's logic before it passes against ours, so it is a real negative control and not
a test that cannot fail.

**Two things verified mechanically rather than by eye**, because both are invisible to the three-way
harness:

```
field set:        upstream 17, ours 17, IDENTICAL: True (no missing, no extra)
ignored-devices:  upstream default "^$" == ours, verbatim
```

A missing field silently drops a metric; an extra one emits a metric upstream lacks. Neither shows up
in a name-only review.

**`adminState` rewrite, proven equivalent rather than assumed.** Upstream writes
`*flags & int64(net.FlagUp) == 1`, which only works because `net.FlagUp` happens to be 1 — it is a
test of bit 0 dressed up as a flag comparison. Mine is `*flags&0x01 != 0`. Rather than reason about
it, I ran both forms over real interface flag values (0, 1, 2, 3, 0x1002, 0x1003) in a standalone
program: identical on every input. Also `nil` flags → `"unknown"`, not `"down"`: a kernel that did not
report flags has not told us the interface is down.

**A fourth instance of my own recurring test bug.** `TestNetClassDefaultFilterIsUpstreamVerbatim`
extracted `netclassIgnoredDevices[^"]*"([^"]*)"` from upstream's source — which captured the flag
*name*, `collector.netclass.ignored-devices`, not the default `^$`. Fixed by anchoring on the
`.Default("...")` call. **Fourth time a test of mine failed because I asserted against a
loosely-specified artifact** (file doc comment instead of code; no-match case of a match-only bug;
`multicast_total` as a substring; now a regexp that matched the wrong string literal). Every one was
caught by the test failing rather than by review — which is the argument for these mechanical
upstream-diff tests existing at all, but the pattern is frequent enough that I now write the extraction
regexp anchored to the syntax I actually want, not to proximity.

**Gates:** coverage 100.0% (all four initially-uncovered branches closed with injected seams, `.covignore`
untouched) · race clean · vet clean · staticcheck clean · 113 tests in the package.

**Progress: 8 of 39** — `loadavg`, `meminfo`, `cpu`, `vmstat`, `stat`, `filesystem`, `netdev`, `netclass`.

All three upstream open bugs that were *containable* on the dependency branch are now *fixed* at the
cause on this one: the filesystem data race, the vmstat malformed-line panic, and netclass #1915/#1841.

**Next:** `diskstats`, then the `/proc/net` group (`netstat`, `sockstat`, `softnet`, `udp_queues`, `arp`,
`conntrack`).

---
## N3 — `diskstats` + shared `deviceFilter`

The largest single collector so far (546 upstream lines across two files), and the first where **every**
failure mode is a wrong *value* rather than a wrong *shape*. The names come out right no matter what you
do; the numbers do not. Three traps, all invisible to the three-way harness because it compares the
contract:

**1. The discard-sectors asymmetry.** `ReadSectors` and `WriteSectors` are multiplied by 512 to become
`*_bytes_total`. `DiscardSectors` is **not** — it is emitted raw, because the metric is named
`discarded_sectors_total` and is denominated in sectors. All three are "sectors" in `/proc/diskstats`
and only two are converted. This is the single easiest field in the file to "fix" into a 512x bug, and
the resulting metric has the right name, type and labels.

**2. `statCount` truncation.** `/proc/diskstats` has 14 fields pre-4.18, 18 on 4.18+, 20 on 5.5+.
Upstream emits only as many metrics as the kernel actually reported. Zero-filling instead would assert
"this disk has never discarded" when the truth is "this kernel does not say" — and `rate()` graphs that
lie as a confident flat line. `IoStatsCount` includes major, minor and the device name, hence the `- 3`;
an off-by-one there shifts the boundary by a whole metric.

**3. Unit conversions.** Sectors are always UNIX 512-byte sectors *regardless of the device's real block
size*. An NVMe device with 4096-byte blocks still reports 512-byte sectors, so reading the actual block
size from sysfs and using it — which looks more correct — makes every byte counter 8x too large.

**Four negative controls, each mutating the source and confirming the specific test fails:**

```
mutation                                          → failing tests
DiscardSectors * unixSectorSize ("consistency")   → 1  (TestDiskstatsDiscardSectorsAreNotConverted...)
remove the truncation guard (zero-fill)           → 2  (ShortLinesTruncate..., TruncationBoundaryIsExact)
statCount - 2 instead of - 3 (off-by-one)         → 2
io_now typed as a counter                         → 1
swap two positional descs                         → 2  (DescOrderMatchesUpstream, SectorsConverted)
```

The zero-fill control is worth recording because its output shows exactly the fabricated data the guard
prevents — `map[...]{"dm-0":0, "nvme1n1":1600, "sdz":0}` for `flush_requests_total`, i.e. two devices
confidently reporting zero flushes on kernels that never mentioned flushes.

**A measurement error of my own, caught before it misled me.** My first pass at the zero-fill and
io_now controls reported **0 failing tests**, which reads as "the test doesn't bite". Both mutations had
actually failed to *compile* (`declared and not used: statCount` / `gauge`), so no test ran at all — and
my `grep -c '^--- FAIL'` counted zero. **A negative control that does not build is not a passing
control, it is no control**, and counting failures rather than reading output hid the difference. Redone
with the mutations kept compiling (`&& false`, `_ = gauge`) and both fail as intended. Same root cause
as the earlier SIGPIPE harness bug: a check that reports success when it did not actually run.

**Positional pairing is load-bearing twice over.** `descs` and `values` pair *by index*, and truncation
drops from the *end*. So a reordering both mislabels values and truncates the wrong fields.
`TestDiskstatsDescOrderMatchesUpstream` extracts upstream's `descs` slice in order — resolving the
inline `NewDesc` calls *and* the shared `*Desc` vars from `diskstats_common.go` — and compares the
sequence, not the set. `io_now` is the only gauge among the 17 (it is a queue depth that goes up and
down; typed as a counter, `rate()` treats every decrease as a reset), asserted individually.

**Live validation against the cluster golden corpus — exact match:**
```
ours (live host):  18 node_disk_* families, 1 series each
PNE (node45):      18 node_disk_* families, 1 series each     IDENTICAL
```
Also confirms udev **is** populated on EKS — the golden `node_disk_info` carries real `path`, `model`,
`serial` (`vol023eb1d2f25982ce5`) and `wwn` values — so the udev path is load-bearing, not dead code.

**`deviceFilter` extracted to its own file**, as upstream has it: shared by ten upstream collectors,
three of which (`diskstats`, `arp`, `infiniband`) are in the 39. **One deliberate divergence:** upstream
uses `regexp.MustCompile` and *panics* on an invalid pattern. That is tolerable upstream because kingpin
parses before any collector is built, so a bad pattern panics at startup with a stack trace. Here the
patterns can come from a Helm value, and a panic on first scrape inside a DaemonSet is a crash loop with
no useful message. Returns an error instead; behaviour is identical for every valid pattern.

**Two other small divergences, both recorded rather than silent:**
- exclude+include supplied together is a *construction error*, not a silent precedence rule. Upstream
  enforces mutual exclusion at the flag layer, which this package does not have. "Include wins" and
  "exclude wins" yield different metric sets and an operator cannot tell which they got.
- `readUdevProperties` returns `scanner.Err()`; upstream ignores it and returns the partial map. A
  truncated read yields partial labels, and a disk silently missing its serial is worse than a reported
  read failure. The caller logs at debug and keeps the counters either way, so it cannot fail the
  collector.

**No EKS-specific exclusion added here**, unlike filesystem — and that is a measured decision, not an
omission. A node reports exactly **one** block device (`nvme0n1`): EBS volumes are whole devices and
containers do not create block devices, so there is no churn cardinality to solve. Upstream's default
already excludes the loop/ram/partition devices that would cause one.

**Gates:** coverage 100.0% on first run (including `devicefilter.go`) · race clean · vet clean ·
staticcheck clean · 147 tests in the package (was 113) · `.covignore` untouched.

**Progress: 9 of 39** — `loadavg`, `meminfo`, `cpu`, `vmstat`, `stat`, `filesystem`, `netdev`,
`netclass`, `diskstats`.

**Next:** the `/proc/net` group — `netstat`, `sockstat`, `softnet`, `udp_queues`, `arp`, `conntrack`.

---
## N3 — the `/proc/net` group: `netstat`, `softnet`, `udp_queues`

### A FOURTH upstream crash, and an honest severity assessment

Upstream's `parseNetStats` computes the protocol name by stripping the trailing colon:

```go
nameParts := strings.Split(scanner.Text(), " ")
scanner.Scan()
valueParts := strings.Split(scanner.Text(), " ")
protocol := nameParts[0][:len(nameParts[0])-1]   // <-- [:-1] on an empty line
```

On an **empty line** `nameParts[0]` is `""` and this becomes `[:-1]`, which panics. Verified against a
verbatim copy of upstream's function in isolation:

```
well-formed            ok
trailing empty line    PANIC: slice bounds out of range [:-1]
leading empty line     PANIC
empty line in middle   PANIC
blank-only file        PANIC
odd number of lines    ok, err=field count mismatch
```

**Severity, stated honestly: this is NOT reachable on a healthy kernel.** I checked before claiming it:
`/proc/net/netstat` and `/proc/net/snmp` are kernel-generated in strict header/value pairs — on the live
node, 6 and 12 lines, **zero blank lines, both even**. So unlike netclass #1915, where EKS pod churn hits
the window routinely, there is no mechanism here that produces a blank line. **This is a robustness gap
found by adversarial testing, not a field bug**, and it is labelled that way in the code comment, the
test comment and this journal so nobody later cites it as an observed outage. Fixed anyway: the cost is
three lines, the collector parses a file whose format it does not control, and a panic takes the whole
scrape down rather than degrading one collector.

Negative control — removing the blank-line guard restores upstream's behaviour and **all five subcases
fail**:
```
--- FAIL: TestParseNetStatsRejectsEmptyLines/leading_empty_line
--- FAIL: .../empty_line_in_middle    --- FAIL: .../blank-only_file
--- FAIL: .../whitespace-only_line    --- FAIL: .../trailing_empty_line
```

**Two more hardening changes in the same parser**, both of which turn silent corruption into an error:
- a non-empty header not ending in `:` is now rejected. Upstream truncates the last character
  regardless, so a malformed line silently files every metric under protocol `TcpEx` instead of
  `TcpExt` — worse than an error, because it looks like data.
- a repeated protocol block now *accumulates*. Upstream reassigns the inner map, discarding the first
  block's fields.

**And one divergence on the same reasoning as netclass:** upstream returns an error from `Update` on an
unparseable value, which discards every protocol already collected. Here a bad value skips that one
field. Partial data beats no data.

### The netstat field filter is a cardinality decision, not a convenience

`/proc/net/{netstat,snmp,snmp6}` expose several hundred counters; upstream's default allowlist selects
~60. Both halves are asserted — the ~30 fields it must **keep** (what dashboards join on) and the bulk it
must **drop** — plus that the pattern is `^...$` anchored, because unanchored it would let hundreds
through and the collector would still "work". Live: **42 series**.

`UntypedValue` is preserved deliberately and the reason is now recorded: these fields mix cumulative
counters with instantaneous gauges (`Tcp_CurrEstab` is a current connection count), and upstream does not
distinguish them. Typing them all as counters would make `rate()` lie about the gauges.

### softnet: why the per-CPU cardinality is worth paying for

`node_softnet_dropped_total` and `times_squeezed_total` are the two counters that expose packet loss in
the kernel receive path — backlog overflow, or NAPI running out of budget before draining the queue. On a
node running hundreds of pods behind the VPC CNI that is a real and otherwise invisible failure mode.
Live: **224 series** (7 metrics × 32 CPUs).

The column mapping is asserted against decoded fixture values because a swap between `dropped` and
`times_squeezed` would report packet loss as CPU scheduling pressure, and both metrics would still exist
with plausible numbers. **CPU 1 is the only fixture row with more than one non-zero column**
(`processed=0xdfb82=916354, dropped=0x29=41, squeezed=0xa=10`), which makes it the row that catches a
swap; CPU 0 has the inverse pattern (`dropped=0, squeezed=1`) and catches a swap in the other direction.

**Caught a wrong constant in my own test before trusting it:** I wrote `299129` for CPU 0's `processed`
from memory; decoding the fixture gave `299641`. Verified every asserted hex value against the fixture
with `awk strtonum` rather than by eye.

`backlog_len` is the only gauge (a queue depth, up and down), asserted individually against the protobuf
type rather than inferred.

### udp_queues: four series, three-way error distinction

The whole subject is the error handling, and it is preserved exactly:
```
IPv6 file absent   -> report v4, say nothing about v6, NOT a failure
both files absent  -> ErrNoData
any other error    -> a real failure
```
Collapsing "IPv6 is disabled" into either a failure or a silent success is wrong in **opposite**
directions: the first alerts on a normal configuration, the second hides a genuinely broken procfs. On a
v4-only EKS cluster the "IPv6 absent" branch is the **common** path, not an edge case — which is why four
series get eight tests. The unreadable-vs-absent distinction gets its own test: absent means IPv6 is
disabled, unreadable means something is wrong.

### Live validation — every collector on a real host

```
cpu 320   netclass 150   softnet 224   netdev 128   filesystem 63
meminfo 49   netstat 42   diskstats 18   vmstat 7   stat 6   udp_queues 4   loadavg 3
SET(12)  total 1014 series  0 errors  0 nodata
```

**Gates:** coverage 100.0% · race clean · vet clean · staticcheck clean · 186 tests (was 147) ·
`.covignore` untouched.

**Progress: 12 of 39** — `loadavg`, `meminfo`, `cpu`, `vmstat`, `stat`, `filesystem`, `netdev`,
`netclass`, `diskstats`, `netstat`, `softnet`, `udp_queues`.

**Upstream defects found so far: 4** — filesystem data race, vmstat malformed-line panic, netclass
#1915/#1841 (the only one reachable in the field on EKS), netstat empty-line panic (robustness only).

**Next:** `sockstat`, `arp`, `conntrack` to finish the `/proc/net` group.

---
## N3 — finishing `/proc/net`: `sockstat`, `arp`, `conntrack` — plus the N4 gate

### `sockstat`: the page-size trap

`/proc/net/sockstat` reports the `mem` field in **pages**, and upstream additionally exposes it
multiplied by the page size as `node_sockstat_TCP_mem_bytes`. Hardcoding 4096 would be correct on x86_64
and **16x wrong on an arm64 kernel with 64K pages**. Graviton nodes are common on EKS, so this is not
theoretical. `os.Getpagesize()` is load-bearing.

The test asserts against `os.Getpagesize()` rather than against `4096` — deliberately. Writing 4096
would make the test *agree with the bug* on this host and fail legitimately on a 64K-page host. There is
also a separate assertion on the constructor, because a literal assigned to `c.pageSize` would pass the
arithmetic test on x86_64 and only fail on hardware nobody runs CI on.

One inconsistency preserved rather than "fixed": when both address families are absent, `sockstat`
returns `nil` while `udp_queues` returns `ErrNoData`. That is upstream's behaviour in both cases and the
two genuinely differ. Asserted in a test so the difference reads as deliberate rather than as a port
error.

### `arp`: a claim of mine that did not survive checking

I wrote a confident comment that `/proc/net/arp` truncates device names at 15 characters, so on EKS the
procfs backend could **merge two CNI interfaces into one label** and silently sum their counts — and
that this was why netlink is the default. It sounded right and it was wrong. Checked instead of shipped:

```
kernel IFNAMSIZ            = 16 (15 usable) -- a longer name CANNOT EXIST
procfs parseARPEntry       uses strings.Fields -> whitespace split, not fixed columns
longest name, live corpus  = 14 chars (eni51bec58f2f1)
```

There is no truncation and no collision hazard. Comment rewritten to state only what is verified, and
the corrected claim is now itself a test (`TestARPLongCNIDeviceNamesAreNotTruncated`) because it is what
justifies either backend being safe.

**What IS load-bearing, and now measured rather than asserted:** the `NUD_NOARP` filter. Netlink returns
those entries (permanent ones needing no ARP resolution) and `/proc/net/arp` omits them. Measured with a
standalone rtnetlink program on this host:

```
3 IPv4 neighbours returned, of which 1 is NUD_NOARP
```

So without the filter netlink reports 3 where procfs reports 2 — a 50% overcount for identical kernel
state. A metric whose value depends on which backend an operator selected is not alertable. Negative
control: removing the filter fails 2 tests.

**A fifth instance of my own recurring test bug — caught this time by coverage, not by a failure.** My
first `NUD_NOARP` test reimplemented the filter inline in the test body and compared it to itself. It
passed, and it would have passed against a completely broken collector. Coverage exposed it: the real
`arpEntriesViaNetlink` sat at 80%, which is what made me look. Rewritten to call the actual
`arpEntriesFromNeighbours`. **Fifth time now** — file doc comment instead of code; no-match case of a
match-only bug; `multicast_total` as a substring; a regexp matching the flag name; and now a test
mirroring the logic it was meant to check.

**A real defect found while making those branches reachable.** Splitting the socket wrapper out to test
the error paths surfaced that the query-failure path had to close the connection itself — nothing else
would, since the success path hands the caller a closer. Left unclosed, the collector leaks one netlink
socket **per scrape**; at a 15s interval that exhausts the fd limit within hours and would present as an
unrelated failure long after the cause. `TestARPNetlinkQueryFailureClosesTheConnection` pins it. The fake
connection also rejects any family other than `AF_INET`, so a change to `AF_UNSPEC` fails there rather
than silently doubling counts on a dual-stack node.

### `conntrack`: the most operationally important collector in this set

`node_nf_conntrack_entries` against `node_nf_conntrack_entries_limit` is **the** signal for conntrack
table exhaustion, which on a Kubernetes node presents as random connection failures and DNS timeouts
rather than as anything resembling a network problem. kube-proxy in iptables mode creates an entry per
connection, so a node running hundreds of pods approaches `nf_conntrack_max` under ordinary traffic. The
test asserts the two values come from the *right files* — swapping them inverts the ratio and makes an
exhausted table look empty.

Two things that would be invisible without explicit tests:
- **Empty subsystem.** These are `node_nf_conntrack_*`, not `node_conntrack_nf_conntrack_*`. Verified
  against the live corpus. Same trap as `stat`'s `node_intr_total`.
- **Per-CPU summation.** `/proc/net/stat/nf_conntrack` has one row per CPU — **32 rows on this host**.
  Reporting one row would silently report one CPU's share and understate drops. Each of the 8 fields is
  asserted against a distinct sum, plus a test that no two descriptors read the same struct field (which
  would leave both metrics present and plausible).

`readUintFromFile` returns the missing-file error **unwrapped**, and there is a test for that specific
property: `handleErr` keys on `errors.Is(err, os.ErrNotExist)`, so a future refactor using `%v` instead
of `%w` would turn "module not loaded" into a reported scrape failure. Also asserted that garbage
content is *not* mistakable for an absent file.

Everything is a gauge, including `stat_drop` and `stat_early_drop`, which **are** monotonic kernel
counters. That is arguably wrong upstream — `rate()` over them is unsupported by the type even though the
data would support it — but changing it breaks the contract. Preserved, and recorded in
`docs/parity-exceptions-nodep.md` rather than silently improved.

### N4 gate — and why the goal file's version of it was wrong

The goal file specified:
```bash
grep -rn "prometheus/node_exporter" --include="*.go" --include=go.mod . && exit 1
```
That gate **fails on this branch, correctly**. `pkg/metrics` — the dependency-based approach — is still
here *on purpose*, because N5/N6 deploy all three variants side by side and compare them. Deleting it to
make the grep pass would destroy the thing being measured.

So the claim is scoped to the package that makes it, in `hack/no-dependency-gate.sh`, checking the
**transitive import closure** rather than grepping source. That is stronger in both directions: a grep
cannot see an import arriving through an intermediate package, and it cannot tell an import from an
attribution comment.

```
pkg/hostmetrics transitive node_exporter packages : 0
pkg/hostmetrics declared imports                  : 0
pkg/hostmetrics source references                 : 2 (both prose attribution in a doc comment)
PASS
```

**The gate has a `--self-test` that runs it against `pkg/metrics`, which is known-dependent, and fails if
it reports clean.** A gate that cannot fail proves nothing:
```
pkg/metrics transitive node_exporter packages: 2
SELF-TEST PASSED: gate correctly detects a real dependency
```

`go mod tidy` moved `rtnetlink`, `mdlayher/netlink` and `procfs` from indirect to direct — **no new
modules added**, they were already in the graph.

### Live validation — 15 collectors on a real host

```
cpu 320  softnet 224  netclass 150  netdev 128  filesystem 63  meminfo 49
netstat 42  sockstat 20  diskstats 18  conntrack 10  vmstat 7  stat 6  udp_queues 4  loadavg 3  arp 2
TOTAL 15 collectors, 1046 series, 0 errors, 0 nodata
```
Two independent cross-checks against the cluster PNE golden corpus: conntrack **10/10 families**,
sockstat **20/20 series**.

**Gates:** coverage 100.0% · race clean · vet clean · staticcheck clean · no-dependency gate PASS with a
passing self-test · 250 tests (was 186) · `.covignore` untouched.

**Progress: 15 of 39.** The `/proc/net` group is complete.

**Next:** the small `/proc` collectors — `uname`, `os`, `time`, `timex`, `pressure`, `schedstat`,
`entropy`, `filefd`.

---
## N3 — `pressure` (PSI) and the four small ones: `uname`, `entropy`, `filefd`, `schedstat`

### The cross-collector unit hazard, and why I measured instead of trusting the docs

`pressure` divides by **1e6** (microseconds); `schedstat`, in the same package, divides by **1e9**
(nanoseconds). Copying either constant to the other is a silent 1000x error **in either direction**, and
both wrong answers look plausible — a 1000x-too-small pressure reading is indistinguishable from a
healthy node.

I did not take the unit from the kernel docs. `/proc/pressure/cpu`'s `some` total, bounded against
uptime:

```
uptime 1429441s, cpu some total = 32151574389
  as microseconds ->    32151.6s =   2.249% of uptime     plausible
  as nanoseconds  ->       32.2s =   0.002% of uptime
  as milliseconds -> 32151574.4s = 2249.2% of uptime      ARITHMETICALLY IMPOSSIBLE
```

Microseconds is the only interpretation that is both possible and plausible. `TestPSIUnitIsEmpirically-
Microseconds` reruns that bound on whatever host it executes on — **and includes a guard that the bound
is not trivially true**: it asserts that a 1000x-smaller divisor *would* exceed uptime, because otherwise
"total ≤ uptime" proves nothing.

**A measurement error of my own along the way:** my first attempt to extract the total used
`awk '/^some/{print $4}'`, which returned `1.79` — the `avg300` field, not `total`. I noticed because the
resulting percentages were absurd (0.0% of uptime for all three interpretations). Fixed with an anchored
`grep -oP 'total=\K[0-9]+'`. Same family as the four prior extraction bugs: **anchor on the syntax you
want, not on field position**.

Negative controls, each mutating one constant:
```
pressure  1e6 -> 1e9   -> 4 failing tests
schedstat 1e9 -> 1e6   -> 4 failing tests
filefd    parts[2] -> parts[1] (the skipped middle field) -> 4 failing tests
```

### PSI: the some/full asymmetry is NOT uniform, and that shapes the metric set

```
cpu     some, NO full   (a fully-stalled CPU is not a meaningful state)
irq     full, NO some   (by design; see linux include/linux/psi_types.h)
io      both
memory  both
```
So **4 resources yield 6 series, not 8**. Emitting `cpu_stalled` or `irq_waiting` would be inventing a
measurement the kernel does not make, and both are asserted absent.

**The partial-availability path is the live path here, not an edge case.** This host runs kernel 6.12 and
`/proc/pressure` contains `cpu`, `io`, `memory` — **no `irq`**, which needs 6.1 *plus* the config. Live
scrape confirms **5 series** rather than 6, exactly the branch
`TestPSIMissingIRQFileDoesNotSuppressOtherResources` covers. A collector that failed wholesale on the
missing irq file would lose PSI entirely on this kernel.

`ENOTSUP` (PSI compiled in, disabled at boot) is distinguished from `ENOENT` (no CONFIG_PSI) and
**stops immediately** rather than probing the remaining three resources, since it is system-wide. Asserted
by counting calls — continuing would be three wasted syscalls per scrape forever.

Why PSI matters more than anything else in this set for a Kubernetes node:
`node_pressure_memory_stalled_seconds_total` rising means processes made **no progress** waiting for
memory — the precursor to an OOM kill. A memory-utilisation gauge looks identical at 95% whether the node
is fine or thrashing.

### filefd: the middle field is a kernel lie

`/proc/sys/fs/file-nr` has three TAB-separated values and the second — "free file handles" — has been
**hardcoded to 0 since Linux 2.6**; the kernel stopped tracking it. Verified on this host:
`11872\t0\t9223372036854775807`. Emitting it would publish a permanent zero that reads as a measurement,
and an operator computing `allocated + free` would get a number wrong by construction. Upstream skips it
and so do we, with the reason written down rather than left as a bare index.

Taking `parts[1]` as the maximum is the plausible off-by-one, and it would make every fd-exhaustion
dashboard read as permanently exhausted. Explicit test, plus a negative control.

Also asserted: the split is on **TAB specifically**, not whitespace — a space-separated line must fail
the field-count check rather than silently parse as one field.

### uname: NUL padding, and a collector that Paths cannot fix

`struct utsname` fields are fixed-size **NUL-padded** char arrays. `unix.ByteSliceToString` stops at the
first NUL; `string(buf[:])` would embed the padding — which **Prometheus accepts as a label value** and
every dashboard then silently fails to match. Asserted against the real syscall output.

Recorded a scope note that applies to no other collector: `uname` is a *syscall*, so it ignores `Paths`
entirely and reports the **container's** UTS namespace. That is correct only because the shipped DaemonSet
sets `hostNetwork`/`hostPID` and therefore shares the host UTS namespace. A rebased procfs path would not
fix it if that ever changed.

Also added a seam for `unix.Uname`'s error branch: the syscall takes no arguments that could be invalid,
so it cannot fail on a working host and the branch would otherwise ship untested.

### schedstat: the CPU label must come from the file, not the loop index

`/proc/schedstat` names CPUs (`cpu0`, `cpu12`) and **omits offline ones**. Using the loop index would
silently renumber the survivors — on a node with cpu2 offline, cpu3's stats would be reported as cpu2's.
Tested with a gap in the CPU list. Live: **96 series** (3 × 32 CPUs).

`timeslices_total` is a plain count and must NOT be divided by 1e9; dividing it produces a near-zero that
reads as an idle CPU.

### A fifth extraction-regexp bug of my own — caught by a guard I had already added

`TestUnameLabelNamesMatchUpstream` matched nothing: I assumed `"uname", "info",` was followed directly by
`[]string{`, but upstream puts the label list on its own lines *after* the help string. It failed on
`require.NotNil` rather than silently asserting over an empty list — **which is exactly why that guard
exists**, and it is now paired with `require.Len(t, upstream, 6)` so an extraction returning a partial
list also fails.

### Live validation — 20 collectors

```
cpu 320  softnet 224  netclass 150  netdev 128  schedstat 96  filesystem 63  meminfo 49
netstat 42  sockstat 20  diskstats 18  conntrack 10  vmstat 7  stat 6  pressure 5
udp_queues 4  loadavg 3  arp 2  entropy 2  filefd 2  uname 1
TOTAL 20 collectors, 1152 series, 0 errors, 0 nodata
```

**Gates:** coverage 100.0% · race clean · vet clean · staticcheck clean · no-dependency gate PASS ·
301 tests (was 250) · `.covignore` untouched.

**Progress: 20 of 39.** Past halfway.

**Next:** `os`, `time`, `timex`, then the hardware group (`thermal_zone`, `powersupplyclass`, `cpufreq`,
`edac`, `nvme`, `mdadm`, `btrfs`, `xfs`, `watchdog`, `dmi`, `infiniband`, `selinux`, `kernel_hung`,
`dmmultipath`, `textfile`).

---
## N3 — `os`, `time`, `timex`: a FIFTH upstream defect, and this one is reachable

### The second real data race, in `os_release.go`

Upstream declares an `osMutex` and then **only half-uses it**. `UpdateStruct` takes the write lock:

```go
c.osMutex.Lock()
defer c.osMutex.Unlock()
c.os, err = parseOSRelease(releaseFile)
```

but `Update` reads those same fields with **no read lock**, after the deferred `Unlock` has already
fired:

```go
ch <- prometheus.MustNewConstMetric(osInfoDesc, prometheus.GaugeValue, 1.0,
    c.os.BuildID, c.os.ID, ...)   // unguarded
```

Reproduced with `-race` against a faithful copy of upstream's locking structure. The detector reports
writes at `UpdateStruct` against reads in `Update` on **`c.os`, `c.version` AND `c.supportEnd`** — three
separate races.

**Reachability, checked before claiming it.** This one is materially different from the netstat
blank-line panic. `node_exporter`'s `--web.max-requests` **defaults to 40**, so promhttp serves up to 40
concurrent scrapes and nothing serialises collectors. Two Prometheus servers scraping one node — or one
server plus a human running `curl` — is sufficient. No exotic input required, no unusual kernel.

**The presence of a half-used mutex is itself evidence someone already knew this needed guarding.**

**The fix, and why it is better than just adding `RLock`:** parse once, cache, and hold the read lock for
the whole emit. `/etc/os-release` cannot change without a reboot, so re-reading it every scrape bought
nothing and cost a file open per scrape *on top of* the race.

Negative control: reverting `load()` to upstream's structure makes my own regression test fail with
`WARNING: DATA RACE`. Restored, it passes — and passes three consecutive `-race` runs.

**One more divergence in the same file.** Upstream's `parseOSRelease` does
`return &osRelease{...}, err` — returning a **populated struct alongside an error**, so a caller who
ignored the error gets a half-parsed struct that looks valid. Ours returns `nil` on error, making that
mistake impossible rather than merely discouraged. Asserted.

**Also rebased onto the host root**, which upstream does too but which is worth stating: in a container
`/etc/os-release` is the *container's* (the agent's base image), not the node's. Reporting the base image
as the node OS would be wrong in a way that looks entirely plausible.

### `timex`: three different divisors on fields of one struct

This is the densest unit-conversion collector in the whole set:

```
offset, jitter           -> divisor DEPENDS ON THE STA_NANO STATUS BIT (1e9 or 1e6)
maxerror, esterror, tick -> ALWAYS microseconds, EVEN WHEN STA_NANO IS SET
freq, ppsfreq, stabil    -> 16-bit-fraction PPM: 1e6 * 65536
freq additionally        -> has 1 ADDED (it is a ratio around 1.0, not an offset around 0)
shift, tai, constant     -> NO conversion at all (shift is an exponent despite _seconds)
```

Hardcoding either side of the conditional divisor is a 1000x error **on half the machines in existence**,
and the metric still exists with a plausible small value. Omitting freq's `+1` reports a ratio of ~0 —
a stopped clock. Each is asserted against a computed expectation, including that `maxerror`/`esterror`/
`tick` produce **identical** values with STA_NANO set and clear.

**A distinction that is easy to miss:** `sync_status` comes from `adjtimex`'s **return value** (the clock
state enum, `TIME_ERROR=5`), not from `timex.Status` (a bitmask). They are different things. The test sets
`Status: 8193` while returning `TIME_ERROR` so that deriving `sync_status` from `Status` would fail. Also
asserted that states 0–4 (`TIME_OK`, `TIME_INS`, `TIME_DEL`, `TIME_OOP`, `TIME_WAIT`) all mean
*synchronised* — a leap-second insertion is not a sync failure.

`syscall.EPERM` vs `os.ErrPermission` gets its own test: `errors.Is` bridges them only because
`syscall.Errno` implements `Is`, and a refactor to `==` would turn a hardened seccomp sandbox into a
reported scrape failure.

**Why these two collectors matter on EKS:** clock skew breaks certificate validation, SigV4 request
signing (which rejects a 5-minute skew), and cross-node log correlation — and **none of those failures
name the clock as the cause**.

### `time`: one upstream inefficiency fixed

Upstream's `time_linux.go` calls `sysfs.NewFS(*sysPath)` **inside `update()`**, re-stating the mount point
on every scrape. Moved to construction, so a bad path fails at startup rather than every 15 seconds
forever. Also asserted that `now` and `zone_offset` are emitted *before* the sysfs read, so a clocksource
failure does not cost the two metrics actually used for skew detection.

### Two mistakes of my own this round

**A hardcoded epoch, off by exactly one day.** I wrote `1836604800` for `2028-03-15` from memory; the
correct value is `1836691200` — 86400 seconds out. The test now *computes* it with
`time.Date(2028, 3, 15, ...).UTC().Unix()` and additionally pins the literal, so the constant is checked
rather than trusted. A hardcoded epoch is impossible to eyeball.

**A flaky test that also did not work.** My first attempt to cover the double-checked re-check raced two
goroutines with a `time.Sleep` to force the interleaving. It was **both flaky and still uncovered** — the
window is nanoseconds. Rather than tune the sleep, I restructured `load()` into `cached()` + `loadSlow()`
so the re-check is directly callable. Strictly better than a timing-dependent test: the lock discipline
is now checkable by reading one four-line function, which matters here of all places, since **upstream's
bug was precisely a lock that looked held and was not**. Added a test that `cached()` cannot be blocked
while a read lock is held, so a regression to the write lock fails rather than silently serialising every
scrape.

**Malformed-input tests used REAL failing inputs**, found by running `envparse` directly rather than
inventing something and hoping: empty key, unmatched quote, invalid escape, bad key character, missing
`=`. And the unparseable-`VERSION_ID` branch needed a 400-digit number — contrived, but it is the only way
in, and the branch must exist because the regexp guarantees "starts with digits", not "parses as a float".

### Live validation — 23 collectors

```
cpu 320  softnet 224  netclass 150  netdev 128  schedstat 96  filesystem 63  meminfo 49
netstat 42  sockstat 20  diskstats 18  timex 17  conntrack 10  time 7  vmstat 7  stat 6
pressure 5  udp_queues 4  os 3  loadavg 3  arp 2  entropy 2  filefd 2  uname 1
TOTAL 23 collectors, 1179 series, 0 errors, 0 nodata
```

**Gates:** coverage 100.0% · race clean (3 consecutive runs, checking for flakes) · vet clean ·
staticcheck clean · no-dependency gate PASS with passing self-test · 351 tests (was 301) ·
`.covignore` untouched.

**Progress: 23 of 39.**

**Upstream defects found: 5.**
| # | defect | reachable on EKS? |
|---|---|---|
| 1 | filesystem data race (two unsynchronised writers) | yes |
| 2 | vmstat malformed-line panic | no (needs malformed /proc) |
| 3 | netclass #1915/#1841 all-or-nothing device read | **yes, routinely** (CNI churn) |
| 4 | netstat empty-line panic | no (kernel never emits one) |
| 5 | os_release half-used mutex, 3 racing fields | **yes** (max-requests defaults to 40) |

**Next:** the hardware group — `thermal_zone`, `powersupplyclass`, `cpufreq`, `edac`, `nvme`, `mdadm`,
`btrfs`, `xfs`, `watchdog`, `dmi`, `infiniband`, `selinux`, `kernel_hung`, `dmmultipath`, `textfile`.
Most emit nothing on EC2 (absent hardware), which is itself the parity requirement: upstream reports the
same collectors as failing on these nodes, so matching that is the target rather than making them succeed.

---
## N3 — the hardware group, part 1: `thermal_zone`, `cpufreq`, `edac`, `kernel_hung`

### The contract for this group is "success with ZERO SERIES", not ErrNoData

Almost none of this hardware exists on EC2. Measured against the live cluster's PNE scrape:

```
collector          success   series
thermal_zone         1          0
cpufreq              1          0
edac                 1          0
watchdog             1          0
powersupplyclass     1          0
infiniband           1          0
btrfs                1          0
mdadm                1          0
dmi                  1          1
selinux              1          3
kernel_hung          1          0
hwmon                0          -     <-- the ONE that legitimately fails
```

`ErrNoData` sets `node_scrape_collector_success=0`, so returning it where upstream returns `nil` would
differ from upstream **on every EKS node** and fire any alert watching collector failures. **I got exactly
this wrong once before on the dependency branch** — asserted zero collector failures when upstream fails
the same set on EKS — so it is now the first assertion in the test file rather than an assumption.

**The mechanism, which is worth naming because reproducing it means NOT being helpful:** procfs's sysfs
helpers use `filepath.Glob`, which returns an **empty slice** rather than an error when nothing matches.
So "no thermal zones" is indistinguishable from "an empty list of thermal zones" and the loop body simply
never runs. Adding a well-meaning `if len(x) == 0 { return ErrNoData }` would break parity.

### Three defects in my own port, all caught mechanically rather than by reading

I compared descriptor **name + help text + label set** against upstream's source programmatically. That
found:

1. **`node_cpu_frequency_avg_hertz` was missing entirely.** Upstream's cpufreq descriptors live in
   `cpufreq_common.go`, not `cpufreq_linux.go`, and I had read only the latter. **A dropped metric is
   invisible to any test that checks the metrics which ARE present** — this is the failure mode the whole
   mechanical-diff approach exists to catch.
2. **Help text differed:** I wrote "cpu thread" where upstream writes "CPU thread". Help text is part of
   the exposition output, so that is a real diff against the reference endpoint.
3. **EDAC channel metrics carry FOUR labels** (`controller`, `csrow`, `channel`, `dimm_label`), not two.
   A two-label version is a different metric that no existing query matches. I had also missed that
   `ce_noinfo_count`/`ue_noinfo_count` are emitted as csrow metrics with `csrow="unknown"` — errors the
   controller could not attribute to a row. Dropping them would lose real error counts.

After correcting: `cpufreq 8/8 identical (name+help+labels)`, `edac 6/6 identical`,
`thermal_zone 3/3 identical`.

### An upstream asymmetry preserved, and a nil-deref guarded

EDAC's channel `ue_count` read failure is **logged and skipped**, while every other read failure aborts
the collector. That looks like an oversight but is load-bearing: some hardware exposes `ch*_ce_count`
without `ch*_ue_count`, so failing would lose the whole collector there. Both halves are now pinned —
`TestEDACChannelMissingUECountIsSkippedNotFatal` and `TestEDACChannelCECountUnreadableIsFatal`.

`kernel_hung`: upstream dereferences `HungTaskDetectCount` **unconditionally**, so a nil pointer with a
nil error would panic. Guarded here. A nil-deref in a collector is far worse than a missing metric, and
the resilience layer should not be the only thing between this and a crash.

### The `dimm_label` substitutions are not cosmetic

`"#"` is stripped and `"csrow"`/`"channel"` get an underscore prefix, so a BIOS label `CPU#1_csrow0`
becomes `CPU1__csrow0`. Copied verbatim and asserted, because changing them changes the label value on
real hardware.

### Two fixture mistakes of my own — both my fixture, not the collector

Building the "absent hardware" fixture took two corrections, and the second was genuinely informative:

1. procfs's `SystemCpufreq` reads `devices/system/cpu/offline` **unconditionally**, and that file exists
   on every real host (verified here: mode 0444, empty). My first fixture omitted it, so cpufreq "failed"
   for a reason no real machine would hit.
2. It also needs at least one `cpu[0-9]*` directory. With **none**, procfs returns
   `could not find any cpufreq files`. With CPUs present and no `cpufreq` subdirectory it returns a
   **pre-sized slice of zero-valued entries and a nil error**, because it does
   `make([]SystemCPUCpufreqStats, len(cpus))` up front and fills only what it can read.

**That second point IS the mechanism behind success=1-with-no-series on EC2**, and it explains why every
field is a pointer: the zero-valued entries have `nil` everywhere and emit nothing. An "absent hardware"
fixture missing a file every real machine has would have tested nothing useful. Confirmed by probing the
live collector on this host — 32 CPUs, no cpufreq directories, `series=0 err=nil`, matching the golden.

### Reachability of the EDAC glob errors, checked rather than assumed

`filepath.Glob` errors **only** on `ErrBadPattern`. The patterns are compile-time constants — but they are
joined onto a **caller-supplied sysfs root**, so a root containing `[` makes the pattern invalid. Verified:
`filepath.Glob("/tmp/[")` returns `syntax error in pattern`. That makes the error branches genuinely
reachable and worth handling.

The **regexp-mismatch** branches, by contrast, cannot be reached through the real glob at all: every path
it returns necessarily contains `devices/system/edac/mc/mc`, which is exactly what the regexp requires.
Upstream has the same dead branch. Kept the guards (they matter if either pattern changes) and covered
them through the injected glob, rather than deleting a check to make a coverage number.

**One more of my own:** my first nested-glob test *rewrote* the pattern instead of returning the error,
which left the csrow branch uncovered. Only the coverage report showed it — the test passed.

### Live validation — 27 collectors

```
cpu 320  softnet 224  netclass 150  netdev 128  schedstat 96  thermal_zone 64
filesystem 63  meminfo 49  netstat 42  sockstat 20  diskstats 18  timex 17
conntrack 10  time 7  vmstat 7  stat 6  pressure 5  udp_queues 4  os 3
loadavg 3  arp 2  entropy 2  filefd 2  uname 1  cpufreq 0  edac 0  kernel_hung 0
TOTAL 27 collectors, 1243 series, 26 success + 1 nodata
```

`cpufreq` and `edac` report **success with 0 series** — exactly the EC2 contract.

**The one nodata is `kernel_hung`, and it is NOT a divergence.** `hung_task_detect_count` needs kernel
6.7. This dev host runs **6.12.94** and does not have the file; the EKS node runs **6.18.38** and does,
which is why the cluster golden shows `success=1` there. Correct behaviour on both, and worth checking
rather than assuming a mismatch.

**Gates:** coverage 100.0% · race clean · vet clean · staticcheck clean · no-dependency gate PASS ·
388 tests (was 351) · `.covignore` untouched.

**Progress: 27 of 39.**

**Next:** the rest of the hardware group — `powersupplyclass`, `watchdog`, `dmi`, `selinux`, `nvme`,
`mdadm`, `btrfs`, `xfs`, `infiniband`, `dmmultipath`, `textfile`, `hwmon`. Note `hwmon` is the one
collector that legitimately reports `success=0` on EKS, so matching that means it must FAIL, not succeed
emptily.

---
## N3 — hardware group part 2: `dmi`, `nvme`, `selinux` (the three that DO emit on EKS)

Unlike the rest of the hardware group, these produce real data on an EC2 instance, and all three now
match the cluster golden exactly: `nvme` **6/6**, `selinux` **3/3**, `dmi` **1/1**.

### `dmi` has a HOST-DEPENDENT label set, and that must not be "fixed"

Upstream builds the descriptor's label list **at construction** from whichever DMI fields the platform
exposes, omitting the nil ones. On the live EKS node that yields **16 of 20**:

```
present (16): bios_date bios_release bios_vendor bios_version board_asset_tag board_name
              board_vendor board_version chassis_asset_tag chassis_vendor chassis_version
              product_family product_name product_sku product_version system_vendor
absent  (4):  board_serial chassis_serial product_serial product_uuid
```

The four absentees are mode-0400 sysfs files — root-readable only, and the agent does not run as root for
these reads. So **`node_dmi_info` is literally a different metric on different hosts.** That is unusual
enough to look like a bug, and the tempting "fix" is to always emit all 20 with empty strings for the
missing ones. Doing that would change the series identity on every node and break any query joining on
it. Preserved exactly, with the EKS 16/20 case reproduced as a fixture.

**A distinction that is easy to collapse:** a field that *exists but is empty* (`board_name` on EC2) still
gets its label, with an empty value. Only a **nil pointer** — an unreadable file — omits the label.
Conflating "empty" with "absent" would silently drop labels on real hosts. Both asserted.

**One deliberate improvement over upstream:** upstream ranges over a Go **map** to build the label list,
so its `Desc` label order varies between process starts. Harmless for series identity (Prometheus sorts
labels) but it makes a golden corpus non-diffable. Sorted here, and asserted stable across repeated
construction.

**Error handling that must distinguish three cases, not two:**
- directory **absent** (ENOENT) → construct with an empty struct, `Update` returns `ErrNoData`. Most ARM
  boards have no DMI, and failing construction would stop the whole agent over absent firmware tables.
- directory **unreadable** (EACCES) or **not a directory** (ENOTDIR) → construction **fails**. Something
  is wrong, and silently reporting a node with no firmware information would hide it.
- **no fields at all** → `ErrNoData`, which is upstream's behaviour and is right: a `dmi_info` with zero
  labels is a bare `1` carrying no information.

Also: DMI strings come from firmware and are **not guaranteed valid UTF-8**, while the Prometheus text
format requires it. An invalid byte would make the whole exposition unparseable, not just this metric, so
upstream substitutes U+FFFD. Kept and asserted.

### `nvme` — EBS presents as NVMe, so this is live data on EKS

`node_nvme_info`'s six labels are emitted **positionally**, and `cntlid` is **last** in the descriptor
even though it sorts first alphabetically. A transposition yields a metric with the right name and label
keys and wrong values, so each label is asserted against a distinguishable value.

Verified mechanically against upstream: **6/6 identical on name + help text + label set.**

### `selinux` — the modes are a three-state enum

`config_mode` and `current_mode` are `-1` disabled / `0` permissive / `1` enforcing, which is why they are
gauges rather than booleans. When SELinux is **disabled** only `enabled=0` is emitted: emitting `0` for
the two modes would read as *"permissive"* — a specific claim rather than an absence. Asserted, including
that `-1` is not clamped.

Scope note recorded: `go-selinux` reads `/sys/fs/selinux` directly with no configurable root, so this
collector ignores `Paths`. Correct in the shipped DaemonSet (host `/sys` is mounted) but it is the
**second** collector after `uname` that a rebased path would not fix.

### A test-helper bug of mine that read as a collector failure

Four nvme assertions failed with `expected 1073741824, actual 0` — which looks like the collector
dropping values. It was my `gatherAllLabels` helper calling `pb.GetCounter().GetValue()` unconditionally;
that returns **0 for a gauge**, and every nvme metric is a gauge. Diagnosed by dumping the actual emitted
metrics rather than reading the collector, which showed all six values correct.

**A test helper that silently returns 0 for an entire metric type is worse than one that panics** — it
reads as a real failure and sends you looking in the wrong place. Fixed to check `pb.Counter != nil`
first, and the reason is written into the helper's doc comment so it does not regress.

### Live validation — 30 collectors

```
cpu 320  softnet 224  netclass 150  netdev 128  schedstat 96  thermal_zone 64  filesystem 63
meminfo 49  netstat 42  sockstat 20  diskstats 18  timex 17  conntrack 10  time 7  vmstat 7
nvme 6  stat 6  pressure 5  udp_queues 4  os 3  selinux 3  loadavg 3  arp 2  entropy 2
filefd 2  dmi 1  uname 1  cpufreq 0  edac 0  kernel_hung 0(nodata)
TOTAL 30 collectors, 1253 series, 29 success + 1 nodata
```

`go mod tidy` promoted `go-envparse` and `opencontainers/selinux` from indirect to direct — **no new
modules**; both were already in the graph via the dependency branch.

**Gates:** coverage 100.0% · race clean · vet clean · staticcheck clean · no-dependency gate PASS ·
413 tests (was 388) · `.covignore` untouched.

**Progress: 30 of 39.**

**Next:** `xfs` (40 metrics on the live node — the largest remaining), then `btrfs`, `mdadm`,
`powersupplyclass`, `watchdog`, `infiniband`, `dmmultipath`, `textfile`, `hwmon`. `hwmon` is the one
collector that legitimately reports `success=0` on EKS, so parity there means it must FAIL rather than
succeed emptily.

---
## N3 — `xfs`: 39 counters, and a table I refused to transcribe by hand

The largest flat table in the set: 39 counters per XFS filesystem. On the live EKS node this is **real,
load-bearing data** — 40 series, because the root volume is XFS on Amazon Linux 2023 — not another
absent-hardware collector.

### Why the table was GENERATED rather than transcribed

Every entry pairs a metric name with a struct field, across **11 nested structs with deliberately
repetitive names**: `AllocationBTree.Lookups` and `BlockMapBTree.Lookups`, `DirectoryOperation.Lookup`,
`InodeOperation.Found`. Transcribing 39 of those by hand is precisely the shape of task where one entry
ends up pointing at its neighbour — and the result is a metric with the **correct name, type and labels
reporting a plausible number from the wrong counter**. Invisible to any structural comparison, invisible
to the three-way harness, invisible to review.

So the table was extracted from upstream's source programmatically, and the test **re-extracts it and
compares all 39 pairs** rather than spot-checking values.

### Recovering "which field does this accessor read?" by reflection

Comparing names and help text is easy; comparing *which struct field each closure reads* is not, because
a `func(*xfs.Stats) float64` is opaque. Solved by probing: walk every numeric leaf of `xfs.Stats`, set
exactly one to a sentinel, and see which accessor returns it. That recovers the dotted field path from our
own table, which can then be compared against the path parsed out of upstream's source.

That makes a mis-wired accessor fail **even though the metric name is right** — which is the entire point.

**Negative controls, all three biting:**

```
point block_map_btree_lookups at s.AllocationBTree.Lookups   -> 3 tests fail, by name:
   "block_map_btree_lookups_total reads the WRONG FIELD:
    upstream uses s.BlockMapBTree.Lookups, we use s.AllocationBTree.Lookups"
   "block_map_btree_lookups_total and allocation_btree_lookups_total both read
    s.AllocationBTree.Lookups"
reword one help string                                       -> 1 test fails
drop one of the 39 entries                                   -> 3 tests fail
```

The duplicate-accessor test is the one I'd have missed by hand: **two entries reading the same field** is
the copy-paste failure this collector is most exposed to, and both metrics would exist with plausible
values while nothing complained. Asserted as `39 metrics must read 39 distinct fields`.

### Contract details

- **No `ErrNoData` when there are no XFS filesystems.** `SysStats` returns an empty slice and upstream
  returns `nil`, so a node without XFS reports success with zero series — same contract as the rest of
  the hardware group.
- **Every metric is a counter.** All 39 are monotonic since mount, so `rate()` is the useful query; as
  gauges they would be nearly useless. Asserted for all 39 rather than a sample.
- **Descriptors built once at construction.** The metric set is fixed at compile time, unlike `netdev`
  where the kernel supplies field names, so rebuilding 39 `Desc`s every 15 seconds would be pure waste.
  Asserted on pointer identity across a scrape.

**Gates:** coverage 100.0% · race clean · vet clean · staticcheck clean · no-dependency gate PASS ·
426 tests (was 413) · `.covignore` untouched.

**Progress: 31 of 39.**

**Next:** `btrfs`, `mdadm`, `powersupplyclass`, `watchdog`, `infiniband`, `dmmultipath`, `textfile`,
`hwmon`. `hwmon` is the one collector that legitimately reports `success=0` on EKS, so parity there means
it must FAIL rather than succeed emptily — the inverse of everything else in this group.

---
## N3 COMPLETE — 39 of 39 collectors

Final batch: `powersupplyclass`, `watchdog`, `mdadm`, `infiniband`, `dmmultipath`, `btrfs`, `textfile`,
`hwmon`.

### `hwmon`: the one collector whose parity requirement is that it must FAIL

Every other hardware collector reports `success=1` with zero series on EKS. `hwmon` reports `success=0`,
because `/sys/class/hwmon` does not exist on an EC2 guest and upstream returns `ErrNoData` rather than
`nil`. On the live cluster it is **the only zero**. So the plausible tidy-up — *"why does this one fail?
make it return nil like the others"* — would be a parity BREAK on every node. Pinned with a test that
says so.

Also preserved verbatim, with upstream's reasoning: `hwmon` reads sysfs with a raw `unix.Read` rather
than `os.ReadFile`, because **some hwmon drivers return EAGAIN and make `os.ReadFile` poll forever**.
That is a hang in a scrape path, not an inefficiency.

### Three upstream defensive guards, and only one of them can fire

I checked each rather than keeping all three as permanently-uncovered code:

```
i >= len(matches) in the SubexpNames loop  -> UNREACHABLE. FindStringSubmatch always returns
                                              exactly 1+NumSubexp entries and SubexpNames has the
                                              same length (verified: 4 groups, both length 5).
n < 0 after unix.Read                      -> UNREACHABLE. unix.Read converts a negative syscall
                                              return into a non-nil error.
strconv.Atoi on the id group               -> REACHABLE. The group is [0-9]* with NO length bound,
                                              so "temp99999999999999999999999_input" parses as
                                              digits and OVERFLOWS int. Verified: ok=false.
```

The two unreachable ones are dropped with the proof recorded in a comment; the reachable one is kept and
tested end to end (the overflowing sensor is skipped, not wrapped to `temp0`). **A guard that cannot fire
is not protection — it is noise that hides the guards that can.**

### `powersupplyclass`: three scale factors, and a bug of mine the golden caught

49 numeric fields across three scale groups (raw, `/1e6`, `/10`), generated from upstream's source and
diffed **with divisors**: `49/49 identical`, plus `12/12` info labels and `7/7` watchdog fields.

**My bug:** I wrapped every read error as a failure. Upstream distinguishes them — a **missing**
`/sys/class/power_supply` is `ErrNoData`, an unreadable one is a failure. On EC2 the directory **exists
and is empty**, which is why the cluster golden shows `success=1`. My version would have reported
`success=0` on any host genuinely lacking it. Both branches now have tests, because neither can be
collapsed into the other.

### The unit-convention matrix is now six deep, and two of them are 100x apart

```
pressure          MICROseconds   /1e6
schedstat         NANOseconds    /1e9
timex offset      MICRO or NANO  -- depends on the STA_NANO status BIT
timex freq        16-bit PPM     /(1e6 * 65536), and +1 because it is a ratio
diskstats         512-byte sectors  (NOT the device block size)
btrfs commits     MILLIseconds   /1000
powersupply temp  DECI-degrees   /10     <-- these two sit in adjacent files
thermal_zone temp MILLI-degrees  /1000   <-- and are 100x apart
hwmon temp        MILLI-degrees  /1000
hwmon fan         RPM            no conversion at all
infiniband lifespan MILLIseconds /1000, and the only conversion among 63 metrics
```

`TestUnitConventionsAreAllDistinctAndCorrect` asserts the whole matrix so the DIFFERENCES are the
subject, not each value in isolation.

### `mdadm`: an upstream label/key mismatch reproduced rather than "fixed"

`node_md_state` is emitted five times per device from five descriptors with different constant labels,
each read from a map keyed by the `ActivityState` string. **Two of those keys do not match their label:**

```
label "resync" <- stateVals["resyncing"]
label "check"  <- stateVals["checking"]
```

Verified mechanically against upstream's source. Making them "consistent" would leave
`node_md_state{state="resync"}` permanently **0 on a device that IS resyncing** — exactly when someone is
looking at it. Reproduced, and the mismatch is asserted so a future tidy-up fails loudly.

### `dmmultipath`: two words for one healthy state

`isPathActive` accepts **both** `"running"` (SCSI) and `"live"` (NVMe). Handling only `"running"` would
count every NVMe path as FAILED — a metric reporting total path failure on a healthy machine, which is
worse than no metric. Also `device_active` is **inverted** from the struct's `Suspended` field, and
`active + failed` always equals the path count, so a path in an unrecognised state counts as failed
rather than vanishing. An unknown state is not evidence of health.

### The single largest deliberate parity gap: btrfs ioctl device stats

Upstream reports per-device btrfs stats two ways. Only the **procfs** path is ported, so three families
are absent on a btrfs host: `device_unused_bytes`, `device_errors_total{type=...}`, and the
`btrfs_dev_uuid` label. Reasons, recorded rather than glossed:

- the ioctl path needs `CAP_SYS_ADMIN`, which the shipped DaemonSet does not have — so on the actual
  deployment target it would fail and fall back to procfs anyway
- it pulls in `github.com/dennwc/btrfs` for metrics unreachable in our deployment
- **zero** btrfs filesystems exist on any EKS node, so the gap is currently unobservable — which is an
  argument for deferring it, NOT for pretending it does not exist

Bounded at ~80 lines plus the dependency if a customer ever needs it.

### Three test-helper bugs of the same family, all mine

```
gatherAllLabels     read GetCounter() only -> every GAUGE silently read as 0
gatherLabelled      same
drainMixed          read gauge+counter only -> every UNTYPED metric silently read as 0
```

The third one presented as *"the textfile collector dropped the value"* and sent me reading the
collector; the collector was correct. All three now go through one `metricValueOf` helper that **fails
the test** on a type it does not understand rather than returning 0. **A test helper that returns a
plausible zero for an entire metric type is worse than one that panics** — it reads as a real failure and
sends you looking in the wrong place.

Also corrected four wrong expectations of my own: `cleanMetricName("__a  b__")` is `a__b` not `a_b` (Trim
strips only the ENDS), `explodeHwmonSensorFilename("")` returns `false` not `true` (the type group needs
a non-digit), the humidity metric is `node_hwmon_humidity` not `_humidity_input`, and
`450000/1e6` needs `InDelta` because it is `0.44999999999999996` in float64.

### `textfile`: partial families are load-bearing

Upstream processes the parsed families **before** checking the parse error, and that is deliberate:
`expfmt` returns the families it managed to parse **alongside** the error. Verified directly — a file
with `good_metric 7` followed by a malformed line yields 2 families and an error. My first version
checked the error first and discarded them, losing every valid metric in a file with one bad line.

Also: a zero-valued `expfmt.TextParser` **panics** with "Invalid name validation scheme requested: unset"
in `prometheus/common` v0.70. `var parser expfmt.TextParser` compiles, looks idiomatic, and crashes on
the first file. Upstream passes `model.LegacyValidation` explicitly.

### Live validation — all 39 collectors

```
cpu 320  softnet 224  netclass 150  netdev 128  schedstat 96  thermal_zone 64  filesystem 63
meminfo 49  netstat 42  xfs 39  sockstat 20  diskstats 18  timex 17  conntrack 10  time 7
vmstat 7  nvme 6  stat 6  pressure 5  udp_queues 4  os 3  selinux 3  loadavg 3  arp 2
entropy 2  filefd 2  dmi 1  textfile 1  uname 1
zero-series successes: btrfs cpufreq dmmultipath edac mdadm powersupplyclass watchdog
nodata (success=0):    hwmon infiniband kernel_hung

TOTAL 39 collectors, 1293 series, 0 failures
```

The three `nodata` results are all correct and none is a divergence: `hwmon` and `infiniband` have no
such hardware on any EC2 instance (and the cluster golden shows `hwmon` at `success=0` too), while
`kernel_hung` needs kernel 6.7 — this dev host is 6.12 without the file, and the EKS node is 6.18 with
it.

**Gates:** coverage **100.0%** · race clean · vet clean · staticcheck clean · no-dependency gate PASS
with passing self-test · **529 tests** · `.covignore` untouched.

**N3 IS COMPLETE: 39 of 39.**

**Upstream defects found: 5** (2 reachable in the field on EKS, 3 robustness gaps found by adversarial
testing). Deliberate parity exceptions: 4, all recorded in `docs/parity-exceptions-nodep.md`.

**Next:** N5 three-way deployment (pne 9100, nma-dep 9101, nma-nodep 9102), N6 three-way validation of
metrics AND logs, N7 stress/load across all three, N8 the design doc with a recommendation between the
two approaches.

---
## N4 COMPLETE — dependency gate verified, and the native endpoint wired end to end

### The end-to-end diff found a parity gap that 565 unit tests could not

Wiring both implementations behind one binary and diffing the two `/metrics` endpoints
immediately surfaced this:

```
families upstream=304  native=303
ONLY-UPSTREAM (1): node_network_receive_nohandler_total
```

**Root cause: `netdev`'s default backend is NETLINK, not procfs.** Upstream's
`--collector.netdev.netlink` defaults to `true`, and `rtnetlink.LinkStats64` carries
`RXNoHandler`, for which `/proc/net/dev` has no column at all. My port read procfs, so it
silently lost 7 series on the live node.

**Every per-collector test passed**, because they compare my port against **its own
table** rather than against the endpoint upstream actually serves. That is precisely the
gap the three-way comparison exists to close, and it is the strongest argument so far for
N5/N6 being worth the effort rather than a formality.

Fixing it exposed a second error of mine in the same change. I wrote that netlink field
names were already final and skipped `legacy()`. The re-diff:

```
ONLY-UPSTREAM (10): receive_drop receive_errs receive_fifo receive_frame
                    receive_multicast transmit_carrier transmit_colls
                    transmit_drop transmit_errs transmit_fifo
ONLY-NATIVE   (17): receive_dropped receive_errors receive_fifo_errors
                    receive_frame_errors multicast collisions ... (pre-legacy names)
```

That is exactly the signature of a skipped transformation. Upstream calls `legacy()` in
`Update`, **after** `getNetDevStats`, so it runs on whichever backend produced the map.

**After both fixes:**
```
families upstream=304  native=304
ONLY-UPSTREAM: 0     ONLY-NATIVE: 0
collectors upstream=49  native=39
  ONLY-UPSTREAM (10): bcache bcachefs bonding fibrechannel ipvs nfs nfsd rapl tapestats zfs
  SUCCESS-VALUE-DIFF: 0     <-- all 39 shared collectors agree on success/failure
```

The 10 are the out-of-scope collectors, and **zero** of the 39 shared ones disagree on
`node_scrape_collector_success`.

### One binary, two implementations — and why that matters for the comparison

Added `metrics.implementation: upstream|native`. Everything below the registry is
**shared**: the same `Options`, `Server`, `newHandler`, and the same listen/serve/shutdown
seams. Only the registry contents differ.

That is deliberate. Two separate binaries would confound the collector difference with
every other build difference — and `newHandler` turned out to take a collector parameter
it never read, so removing it made the independence provable rather than asserted.

Default is `upstream`, not `native`: it is the variant with a completed parity run behind
it (298/298 against v1.12.1), so an operator who enables metrics without naming an
implementation gets the proven one. An unrecognised value is **rejected** rather than
silently defaulted — someone who misspells "native" should learn that, not quietly get the
other implementation and wonder why their metrics differ.

### The resilience boundary, ported and now actually tested

`pkg/hostmetrics/prometheus.go` reproduces the contract exactly:
`node_scrape_collector_{duration_seconds,success}` plus the panic and timeout counters.
The equivalent code on the dependency branch cost **three bugs**, all in the timeout path,
so this file's tests are adversarial by design — a collector that panics, one that hangs,
one that emits then panics, and one that **keeps emitting 500 metrics after being
abandoned**. That last one is the regression test for the worst of the three: writing to a
channel the registry has already closed panics on a goroutine where nothing can recover
it, turning the timeout guard into a *new* crash source.

**Two measured properties of the counters, both of which I first got wrong in a comment:**

```
a CounterVec with no observed labels emits NOTHING
  -> node_collector_panics_total is ABSENT until the first panic
the increment lands during Collect, after the registry snapshotted its collector list
  -> it appears on the NEXT scrape, not this one (verified: gather 1 absent, gather 2 = 2)
```

Neither breaks containment — `success=0` is reported in the *same* scrape — but it does
change what an alert should watch:

```
ALERT ON:      node_scrape_collector_success == 0      same-scrape, always present
DIAGNOSE WITH: node_collector_panics_total             lagging, absent until first panic
```

Written down rather than left for whoever writes the alert to discover.

**A test-authoring mistake of mine, caught by the lag test itself.** My first version
asserted the counter *family* was absent on gather 1. It failed — because the counters are
**package-level**, so an earlier test in the same binary had already created the family.
The lag claim was right; the assertion was wrong. Now keyed on the collector's own label
with a unique name, and verified in isolation.

**And one real bug `go vet` caught:** `pb = *m` copies a `dto.Metric`, which embeds a
`sync.Mutex`. Copying a mutex is undefined behaviour; the copy served no purpose and the
pointer works directly.

### N4 gate

```
pkg/hostmetrics transitive node_exporter packages : 0
declared imports                                  : 0
source references                                 : 2 (both prose attribution)
--self-test vs known-dependent pkg/metrics        : detects 2, PASSES
```

### Live: both endpoints served locally

```
native    status=200  families=304  series=1374
upstream  status=200  families=304  series=1402
```
The 28-series difference is entirely the 10 out-of-scope collectors' meta metrics
(2 × 10 = 20) plus their own output.

**Gates:** coverage 100.0% · race clean · vet clean · staticcheck clean · nodep gate PASS
with passing self-test · 565 tests.

**N3 and N4 COMPLETE.** Next: N5 deploy all three to the cluster (pne 9100, nma-dep 9101,
nma-nodep 9102), N6 three-way validation of metrics AND logs, N7 stress/load, N8 design doc.

---
## N5 + N6 COMPLETE — three-way deployment and validation on the live cluster

### N5: all three variants running on the same nodes

```
kube-system   eks-node-monitoring-agent   2/2   :9101   implementation=upstream
kube-system   nma-nodep                   2/2   :9102   implementation=native
monitoring    pne-prometheus-node-exporter 2/2  :9100   (reference)
```

Image `nodep-v1` (`c53b6fb`), tagged immutably rather than deployed as `latest`: with three
variants under comparison, "which image is actually running" cannot be a question.

The two agents share the service account, host mounts and node set, differing only in image
tag, config, and the health/metrics ports (8012/8013 vs 8002/8003) so they can coexist on
`hostNetwork`. A `hostPort` I had missed rejected the first apply — it must equal
`containerPort` under `hostNetwork`.

### N6 metrics: all three agree

```
T1 STRUCTURAL   pne vs nma-dep    : only-pne=0  only-nma-dep=0     <- POSITIVE CONTROL
                pne vs nma-nodep  : only-pne=0  only-nma-nodep=0
                nma-dep vs nodep  : 0           0
T2 COLLECTOR    pne vs nma-dep    : 49 shared, 0 disagree
                pne vs nma-nodep  : 39 shared, 0 disagree
                nma-dep vs nodep  : 39 shared, 0 disagree
                hwmon             : 0 on all three  <- the inversion holds
T3 SERIES       all three pairs   : 0 families differ in count

SUMMARY  pne 773 series / 347 families / 49 collectors
         nma-dep 727 / 345 / 49
         nma-nodep 707 / 345 / 39
```

**Zero disagreements on metric names and on every shared collector's success value.**

Two differences are expected and are now encoded with their reasons rather than
rediscovered each run:

1. `promhttp_metric_handler_requests_total` / `_requests_in_flight` on pne only. These come
   from `promhttp.InstrumentMetricHandler`, which upstream's binary calls and this agent
   does not — they describe the scrape endpoint, not the node. Both agents do have
   `promhttp_metric_handler_errors_total`, which `HandlerFor` provides. Whether the agent
   should adopt `InstrumentMetricHandler` is a separate decision, noted for the design doc.
2. `node_filesystem_readonly` / `_device_error`: pne 25 series, both agents 4. **The EKS
   mount exclusion working**, and the log evidence confirms it rather than inferring it —
   1,102 and 168 `ignoring mount point` lines respectively. pne reports per-pod
   `volume-subpaths/config/grafana/N` and containerd sandbox `shm` mounts, each with a
   unique pod UID; the agents report `/ /boot/efi /run /tmp`.

**The harness self-tests on every run.** Three injected defects, one per tier:

```
T1 remove a metric family      -> detected 1
T2 flip hwmon success 0 -> 1   -> detected 1
T3 drop one CPU's series       -> detected 1
```

A comparison reporting "no differences" is worthless unless it can be shown to report them
when they exist, and a self-test behind a flag is a self-test nobody runs.

### N6 logs: the half a metrics comparison cannot cover

A collector can export a correct metric set while logging an error every scrape — metrics
pass, log volume doubles. So:

```
ERRORS/PANICS      pne 0   nma-dep 0   nma-nodep 0
RESTARTS           nma-dep 0   nma-nodep 0        (an escaped panic would restart)
WARN messages      identical sets
DEBUG volume       ratio nodep/dep = 0.07         (the native variant is QUIETER)
implementation     nma-nodep: native (2 pods)  nma-dep: upstream
```

**That last line is the one that makes everything above meaningful.** If nma-nodep were
silently running the upstream implementation, every comparison would be of the same code
against itself. It is checked explicitly, in both directions.

### Four harness bugs of mine in this phase, and one that was the worst possible kind

1. **Case-sensitive message match.** nma-dep logs `"Ignoring mount point"` (upstream's
   capital I), nma-nodep logs it lowercase. My grep reported 0 for nma-dep and then printed
   "the exclusion may not be running" — about a collector that was working correctly. A
   message comparison across two codebases has to be case-insensitive.

2. **`--tail` on a once-written line.** The startup line ages out of any tail window on a
   long-running pod.

3. **`kubectl logs -l` does not return full per-pod history.** It gave 20 lines where a
   single pod had 348. Now queried per pod by name.

4. **`set -e` + a grep that finds nothing.** nma-dep does not log an `implementation` field
   (it predates the switch), so its grep exited 1 and **killed the script at that line** —
   printing the section header and nothing else. The files were correct all along.

Bugs 2, 3 and 4 all produced the *same* false alarm: "the native variant may be running the
upstream implementation." That is the worst possible false alarm here, because it is the one
claim that would invalidate every other result — and I hit it three separate ways before the
data was actually wrong even once.

Bug 4 is the same shape as the SIGPIPE bug on the dependency branch: **a check that reports
nothing precisely when the interesting case occurs.** There, grep closing the pipe killed
curl; here, grep finding nothing killed the script. Worth naming as a recurring hazard in
shell harnesses under `set -e`.

Evidence captured in `evidence/three-way-validation.md`.

**Remaining: N7 stress/load across all three, N8 the design doc with a recommendation.**

---
