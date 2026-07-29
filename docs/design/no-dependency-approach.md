# Node metrics without a `prometheus/node_exporter` dependency

**Status:** complete, awaiting a decision
**Branches:** `feat/prometheus-node-exporter-parity` (dependency) · `feat/metrics-no-upstream-dependency` (native)
**Evidence:** `evidence/three-way-validation.md`, `evidence/three-way-stress.md`, `JOURNAL-NO-DEPENDENCY.md`

---

## 1. The question

The dependency branch reaches node_exporter parity by importing
`github.com/prometheus/node_exporter/collector` and serving its collectors from the agent.
That works — 298/298 metric names against v1.12.1, validated live on EKS.

This branch answers a different question: **what does it cost to own the collectors
instead?** Not "can it be done" — that was never in doubt — but what the two approaches
actually trade against each other once both exist and can be measured side by side.

Both now do. This document is the comparison and a recommendation.

---

## 2. What was built

39 of upstream's 49 Linux collectors, ported into `pkg/hostmetrics` with no import of
`prometheus/node_exporter`, plus a `prometheus.Collector` adapter carrying the same
resilience boundary as the dependency branch.

The 10 not ported are out of scope for EKS and were approved as such: `bcache`, `bcachefs`,
`bonding`, `fibrechannel`, `ipvs`, `nfs`, `nfsd`, `rapl`, `tapestats`, `zfs`. None emits
anything on an EC2 instance; all 10 report `collector_success=0` on both other variants.

### What "no dependency" does and does not mean

`prometheus/procfs` is **kept**, and that is load-bearing rather than a compromise.

Upstream's collectors are largely thin wrappers over `procfs`/`sysfs`: `procfs` does the
actual `/proc` and `/sys` parsing. Reimplementing it would mean reimplementing the parsers,
and — measured during the port — upstream *silently drops kernel fields that `procfs` does
not model. In `meminfo`, 55 keys in `/proc/meminfo` become 51 mapped fields become 49
emitted metrics. Matching that behaviour requires the same parsing layer. Dropping `procfs`
would not be a smaller dependency footprint; it would be a different metric set.

So the boundary drawn here is: **own the collectors, keep the parsers.** That is where the
EKS-specific behaviour lives, and it is what the dependency actually costs.

---

## 3. Measured comparison

All figures from the live cluster (`nma-pne-parity-test`, EKS 1.36, 2× t3.large, kernel
6.18.38), with all three variants on the same nodes scraped in the same pass.

### Correctness: identical

```
T1 metric names        pne vs nma-dep     0 differences    <- positive control
                       pne vs nma-nodep   0 differences
                       nma-dep vs nodep   0 differences
T2 collector success   49 shared / 0 disagree  (pne vs nma-dep)
                       39 shared / 0 disagree  (both, vs nma-nodep)
T3 series counts       0 families differ, all three pairs
                       346/346 families identical across all three
```

Re-run at **6 nodes** on 2026-07-29 with the same result, and with **one fewer exception**:
`promhttp_metric_handler_requests_*` is no longer excluded, because the agent now calls
`InstrumentMetricHandler` (Q9). Parity is therefore *stronger* than in the original run — that
family is now compared like any other and matches.

One expected difference remains: the EKS filesystem mount exclusion (pne 13 series on the
6-node cluster, both agents 4), which is the pod-mount cardinality exclusion working as
designed.

**A caveat this tier cannot cover, stated because it cost 13 hours of a silent defect.** T1/T2/T3
compare names, per-collector success, and series counts. They do **not** compare values, and
that is still the right call — two scrapes seconds apart differ on every counter. But Q9 found
`promhttp_metric_handler_errors_total{cause="gathering"}` at **3182 on nma-dep, 9 on nma-nodep,
0 on pne**, incrementing **once per scrape**: every scrape of both agents was serving a partial
response, and nothing in this harness could see it because the *name* was present and the
*count* was right. The blind spot is real; the mitigation is that a value-tier comparison needs
a Prometheus `rate()` window (§7).

### Cost and footprint

| | dependency | native |
|---|---|---|
| non-test lines in the package | 840 | 8,523 |
| test lines | — | 11,993 |
| transitive non-stdlib deps | 121 | 84 |
| `node_exporter` packages in the graph | 2 | 0 |
| collectors registered | 49 | 39 |

The native branch is **~10× the code to own**, and that is the central cost. It also removes
**37 transitive dependencies**, which is the central benefit.

### Performance: there is no difference — and the earlier claim was my measurement error

**An earlier revision of this document reported the native variant as ~15× faster, median
~250×. That was wrong, and it was wrong because of how I measured, not because of anything
in either implementation.** The correction is kept in the open rather than quietly edited,
because the retraction is the useful part.

`node_scrape_collector_duration_seconds` measures **wall** time, and every collector runs
**concurrently**. With fewer usable cores than collectors, each collector's timer keeps
running while its goroutine is descheduled — so all of them report approximately the *whole
batch's* window instead of their own work. `stress.sh` **summed** those per-collector values
and called the total "collection cost", multiplying the real figure by up to the collector
count.

The giveaway was in the distribution I had already published and misread:

```
              min          median       max      spread
nma-dep    0.000480s    0.013858s   0.077918s    162×
GOMAXPROCS=1 (measured while closing Q8):
nma-dep    0.010862s    0.011322s   0.011806s    1.09×   <- 49 collectors, one window
```

`netclass` walks every network interface; `loadavg` reads one short file. A 1.09× spread
across 49 such collectors is not 49 similar measurements — it is one measurement reported 49
times.

Corrected, on wall time, live on the 6-node cluster under pressure:

```
             wall (baseline)   wall (pressure)   sum(cc) — the misleading number
pne             0.0136s           0.0132s              0.0149s
nma-dep         0.0107s           0.0157s              0.3390s
nma-nodep       0.0125s           0.0103s              0.0127s
```

**All three are within ~1.5× of each other.** Native/upstream wall ratio: **0.66×**, with
native running 10 fewer collectors — so the difference is scope, not speed. In-process, by
median over five passes, upstream is **1.22×** native.

Confirmed three ways: the same 49 collectors run **serially** cost **0.0166s** total against
the 0.9114s once reported (~55× overstatement); wall times are near-identical; and buffering
the relay channel — the mechanism I had credited with ~4× — changed nothing (0.5552s →
0.5243s), so **that change was reverted**.

Also measured and *rejected* as the cause: **CPU throttling.** Both agents genuinely are
throttled (nma-dep 5124 periods / 337.9s at the chart-default `cpu: 250m`; pne has no limit
and is never throttled), but raising the limit 8× moved the median from 7.72ms to 8.06ms.
Real finding, wrong cause — tracked separately as **Q10**, since it is a fact about the
shipped default rather than about either branch.

**Consequence for this document's recommendation: performance is not a differentiator and
should carry no weight in the decision.** It previously appeared as reason 3 for holding the
native branch, on the grounds that a number I could not explain should not be leaned on.
That was the right instinct for the wrong reason — the number was not real.

Guarded by 4 tests in `pkg/metrics/concurrentduration_test.go`, which assert the conclusion
rather than machine-specific timings, plus a negative control showing a 264.5× spread when
cores *are* available.

---

## 4. What owning the collectors actually bought

Five upstream defects were found by porting, with reachability assessed individually rather
than lumped together:

| # | Defect | Reachable on EKS? | Native branch |
|---|---|---|---|
| 1 | `filesystem` data race — two unsynchronised writers | yes | fixed |
| 2 | `vmstat` panic on a malformed line | no — needs malformed `/proc` | fixed |
| 3 | `netclass` #1915/#1841 all-or-nothing device read | **yes, routinely** | **fixed** |
| 4 | `netstat` panic on an empty line | no — kernel never emits one | fixed |
| 5 | `os_release` half-used mutex, 3 racing fields | **yes** | fixed |

Two are genuine field bugs; three are robustness gaps found by adversarial testing.
Conflating them would overstate the case, so they are tracked separately.

**#3 is the one that matters, and it is no longer a theoretical argument.** Upstream returns
on the first device read error, so one interface disappearing mid-scrape suppresses metrics
for *every* interface. On a static host that window is almost never hit — which is why it has
been open since 2020. On EKS the VPC CNI creates and destroys veth/eni interfaces on **every
pod schedule**, so it is hit routinely. Same code, different failure rate.

**Reproduced in the field, 2026-07-29** (`evidence/q3-scale-test.md`). Under sustained pod
churn at 2,434 pods on a 6-node cluster, pne's `netclass` collector reported
`success=0` while **both agents reported `success=1` on the same node in the same scrape
pass**:

```
node_scrape_collector_success{collector="netclass"}
  sample 6 / 2,434 pods    pne=0    nma-dep=1    nma-nodep=1
```

pne's failing-collector count went from 10 (the absent-hardware set) to 11, the addition being
`netclass`. `netdev` was unaffected in the same samples, which corroborates rather than
contradicts: its default backend is netlink, so it never performs the per-device sysfs reads
that #1915 concerns.

**This is worth stating carefully, because an earlier run at comparable scale did not
reproduce it** and that negative was recorded plainly as *"PR1 — did not reproduce"*. The
prediction was correct; the earlier miss was a matter of churn *rate*, not of the reasoning
being wrong. Two runs, one negative and one positive, is what the claim now rests on — so:
reachable, demonstrated once in 12 pressure samples, rate not established.

**On the dependency branch, #3 can only be contained** (the resilience boundary catches the
consequences); the collector still returns nothing. On the native branch it is **fixed at
the cause**: the unreadable device is skipped and the rest are reported. That difference —
containment versus a fix — is the strongest argument for owning the code.

It also *retired* a workaround: excluding pod-side interfaces to dodge the churn hazard was
measured to **cost** `node_network_speed_bytes` (297 names vs 298). Fixing the read made the
exclusion unnecessary.

### The other side of ownership

Upstream fixes must now be **ported, not received**. Concretely: v1.12.1 → v1.13 will need a
per-collector diff. The mechanical upstream-comparison tests exist precisely to make that
tractable — they extract upstream's tables from source and compare — but it is recurring
work that the dependency branch gets for free.

**And the porting itself is error-prone in ways unit tests do not catch.** The clearest
example: I built `netdev` on `/proc/net/dev` and lost
`node_network_receive_nohandler_total` — 7 series on the live node — because upstream's
default backend is **netlink**, which exposes `RXNoHandler` where procfs has no column.
**All 565 unit tests passed**, because they compare the port against *its own* table rather
than against the endpoint upstream serves. Only the end-to-end diff caught it.

That is the honest shape of this cost: not "porting is hard" but "porting produces defects
that look correct from inside the port."

---

## 5. Deliberate parity exceptions

| Exception | Impact | Reason |
|---|---|---|
| `btrfs` ioctl device stats | 3 families absent on a btrfs host: `device_unused_bytes`, `device_errors_total`, the `btrfs_dev_uuid` label | needs `CAP_SYS_ADMIN`, which the DaemonSet lacks — so it would fail and fall back to procfs anyway; pulls in a cgo-adjacent dep; no EKS node has btrfs. ~80 lines to close. |
| macOS `SystemVersion.plist` in `os` | none | Linux-only agent; unreachable |
| `netdev` detailed metrics / `address-info` | none | both default off; deliberately incompatible names |
| `hwmon` colliding chip label | none on EKS | disambiguated by index rather than by `hwmonX`; cannot occur where `/sys/class/hwmon` is absent |

The 10 unported collectors are scope, recorded in §2 rather than here.

---

## 6. Recommendation

**Ship the dependency branch first. Keep the native branch as the successor, and merge it
once upstream #1915 has been raised there.**

Reasoning:

1. **The dependency branch is closer to shippable.** 840 lines against 8,523, with a
   completed 298/298 parity run. Reviewing 840 lines of integration is a different
   proposition from reviewing 8,523 lines of ported collectors, and the contribution
   guidance favours the smaller change.

2. **The native branch's decisive advantage is one fix, and that fix belongs upstream.**
   #1915 affects every node_exporter user on Kubernetes, not just this agent. The right
   first move is a PR to `prometheus/node_exporter` — the port here is the reference
   implementation and the regression test for it. If upstream takes it, the dependency
   branch inherits the fix and the strongest argument for owning 8,523 lines goes away.

3. ~~**The performance gap is not yet an argument.**~~ **RETRACTED — there is no performance
   gap.** The ~15×/~250× figures were an artefact of summing concurrent wall-clock durations
   (§3). On wall time the three are within ~1.5× of each other. **Performance should carry no
   weight in this decision in either direction.** The instinct not to lean on an unexplained
   number was right; the number itself was not real.

4. **Ownership cost is recurring and real.** Every upstream release becomes a per-collector
   diff. Worth paying to fix a bug upstream will not take; not worth paying pre-emptively.

**What would change this recommendation:** upstream declining or sitting on #1915. That
single fix is the difference between "a dependency with a known, contained bug" and "a
dependency with a known bug that hits us on every pod schedule" — and in the latter case
owning the code is clearly correct.

### What the 2026-07-29 runs changed, and what they did not

**The recommendation stands, but reason 2 is now stronger and reason 3 is gone.**

- **#1915 is demonstrated, not argued** (§4). pne's `netclass` failed under churn where both
  agents held. This does not change *which branch to ship first* — it raises the priority of
  **raising the issue upstream**, because there is now a reproduction to attach to it, and it
  is the step everything else in the sequencing depends on.
- **Performance is off the table** (reason 3, retracted above).
- **Q3 is answered:** the native branch holds at 2,938 pods — 0 panics, 0 timeouts, 0
  restarts. The scale gap in §7 is closed.
- **Two defects were found in the agent, not upstream** (Q9): a duplicate error-counter
  registration failing *every* scrape, and a silent gather error that hid it for 13 hours.
  Both are fixed on this branch and both apply to `pkg/metrics`, i.e. **to the dependency
  branch too** — they must be carried across whichever ships.
- **A new finding that belongs to neither branch:** the mount exclusion is an *availability*
  property, not just cardinality hygiene. pne restarted 8 times under churn — killed by its
  own liveness probe, not by the OOM killer — while its `node_filesystem_readonly` cardinality
  tracked pod count and both agents stayed flat at 4 series. Partly attributable to the
  exclusion (a measured 4.7× endpoint speed-up) and partly to the agents probing a separate
  `/healthz` port; the two are separable and both are recorded in
  `evidence/q3-scale-test.md`.

**What none of this resolves:** the ownership cost (reason 4) is unchanged, and it remains the
substance of the decision. 8,523 lines to own against 840 lines of integration is the trade,
and it is not a measurement question.

### Sequencing

1. **Raise #1915 upstream** with the regression test from `pkg/hostmetrics/netclass_test.go`
   **and the field reproduction from `evidence/q3-scale-test.md`.** Now the highest-value step
   and better supported than when this was written: the issue has been open since 2020 largely
   because it is hard to trigger on a static host, and there is now a same-node, same-scrape
   demonstration that it fires on EKS under pod churn. **Needs a go-ahead — it is a public post
   to a repository we do not own (Q4).**
2. **Carry the Q9 fixes into whichever branch ships.** They are in `pkg/metrics`, so they apply
   to the dependency branch identically: the duplicate error-counter registration was failing
   *every* scrape with `includeExporterMetrics: true`, and the missing `ErrorLog` is why nobody
   noticed. Not optional cleanup — the endpoint was serving partial responses.
3. Split the dependency branch into a clean PR (Q6: ~1–2h; of 10,825 insertions only ~3,566
   are shippable — `evidence/` contains account IDs and Grafana credentials).
4. ~~Resolve Q8 (the latency floor)~~ — **done, and it was a harness bug, not a code one.** No
   action remains for either branch.
5. **Decide Q10** (the `cpu: 250m` default and unmanaged `GOMAXPROCS`). Independent of this
   choice, affects every EKS node, and cheap to fix.
6. Hold the native branch. Merge if upstream declines #1915, or when the ownership cost is
   justified by a second such fix.

### One thing to decide either way

`metrics.implementation: upstream|native` exists so one binary can serve both, which is
what made the controlled comparison possible. **It should not survive as a public option** —
two collector implementations behind a config flag is two code paths to support and a
question customers should not have to answer. Whichever wins becomes the only one.

---

## 7. Confidence and limits

**What is well established:** the two implementations produce identical metric names,
identical per-collector success values, and identical series counts on live EKS nodes, at
baseline and under pressure, with a harness that self-tests its own ability to detect each
class of difference.

**What is not:** absolute metric *values* have not been compared across implementations.
Two scrapes seconds apart legitimately differ on every counter, so that needs a Prometheus
`rate()` window — the V3/V4 tier from the dependency branch, driven from Grafana. Structural
and success-value agreement is strong evidence but not proof that every number matches.

**Scale tested:** ~~2 nodes, 70 pods~~ — **closed 2026-07-29.** 6 nodes, peak **2,938
concurrent pods**, 3,000 churn completions, 16 samples over ~38 min (12 loaded, 4 at rest, which show full recovery), all three variants
co-resident and scraped in the same pass. The native branch held: 0 panics, 0 timeouts, 0
restarts, constant collector-failure count. `evidence/q3-scale-test.md`.

**Unexplained:** ~~the remaining factor in the latency floor~~ — **nothing.** Q8 is closed:
there was no latency floor, only a harness that summed concurrent wall-clock durations. Both
rejected hypotheses (CPU throttling, the relay channel) are recorded with their negative
results in `OPEN-QUESTIONS.md`.

**Still open, and honest about it:**

- **Values are not compared across variants.** Unchanged, and still the right call — but Q9
  showed the cost: `promhttp_metric_handler_errors_total{cause="gathering"}` sat at **3,182 on
  nma-dep, 9 on nma-nodep, 0 on pne**, incrementing once per scrape, so *every* scrape of both
  agents served a partial response for 13 hours. The name was present and the series count was
  right, so T1/T2/T3 could not see it. A `rate()`-window tier would have.
- **#1915's failure *rate* is unknown.** Reproduced once in 12 pressure samples (and 0 of 4 at rest), and *not* reproduced in
  an earlier run at comparable scale. Reachability is established; frequency is not.
- **`nma-dep` containing #1915 is not the same as fixing it.** Its `netclass` did not fail in
  this run, which is evidence and not a guarantee.
- **Q10 (new):** both agents are CPU-throttled at the chart-default `cpu: 250m` — nma-dep 5,124
  throttle periods / 337.9s throttled — with `GOMAXPROCS` unmanaged and observed at 2 against a
  0.25-core quota. Harmless today against a 15s interval; it is a fact about the shipped
  default and needs an owner's decision.
