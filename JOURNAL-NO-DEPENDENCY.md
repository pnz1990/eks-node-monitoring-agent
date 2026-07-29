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
