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
