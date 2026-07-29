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
```

Two expected differences, both explained and encoded in the harness: pne's
`promhttp_metric_handler_requests_*` (from `InstrumentMetricHandler`, which the agent does
not call) and the EKS filesystem mount exclusion (pne 25 series, both agents 4).

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

### Performance: an unexpected result

Over the **39 collectors both variants run** — the only apples-to-apples comparison, since
totals would credit the native variant for running 10 fewer:

```
                   baseline    under pressure
nma-dep            0.9114s        0.3288s
nma-nodep          0.0181s        0.0210s
pne                0.0123s        0.0215s
```

The native variant is **~15× faster than the dependency variant** and comparable to pne.
This was the opposite of what I was testing for, and it needed explaining rather than
celebrating. The distribution shows a **floor**, not slow work:

```
              min          median       max
nma-dep    0.000480s    0.013858s   0.077918s
nma-nodep  0.000011s    0.000122s   0.007148s
pne        0.000008s    0.000054s   0.004424s
```

Collectors doing wildly different amounts of work all land within a whisker of each other
on `nma-dep`. Partial cause: its relay channel is **unbuffered** where the native one is
buffered at 1024, so every metric costs a goroutine handoff. Benchmarked in isolation that
accounts for **~4×, not ~250×** — so it is a real contributor and not the whole story. Ruled
out: CPU limits (identical) and contention (the gap is *larger* at baseline).

**This is a finding about the dependency branch, not an argument for the native one.** It is
almost certainly fixable there, and tracked as Q8. At 0.9s against a 15s scrape interval it
breaks nothing today. It should not be weighed as a durable advantage until the remaining
factor is identified.

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

**#3 is the one that matters.** Upstream returns on the first device read error, so one
interface disappearing mid-scrape suppresses metrics for *every* interface. On a static host
that window is almost never hit — which is why it has been open since 2020. On EKS the VPC
CNI creates and destroys veth/eni interfaces on **every pod schedule**, so it is hit
routinely. Same code, different failure rate.

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

3. **The performance gap is not yet an argument.** ~15× is large, but it is a finding about
   the dependency branch's relay buffer plus an unidentified remainder — likely fixable
   there. Recommending on performance before Q8 is resolved would be recommending on a
   number I cannot fully explain.

4. **Ownership cost is recurring and real.** Every upstream release becomes a per-collector
   diff. Worth paying to fix a bug upstream will not take; not worth paying pre-emptively.

**What would change this recommendation:** upstream declining or sitting on #1915. That
single fix is the difference between "a dependency with a known, contained bug" and "a
dependency with a known bug that hits us on every pod schedule" — and in the latter case
owning the code is clearly correct.

### Sequencing

1. Raise #1915 upstream with the regression test from `pkg/hostmetrics/netclass_test.go`.
2. Split the dependency branch into a clean PR (Q6: ~1–2h; of 10,825 insertions only ~3,566
   are shippable — `evidence/` contains account IDs and Grafana credentials).
3. Resolve Q8 (the latency floor) — it affects whichever branch ships.
4. Hold the native branch. Merge if upstream declines #1915, or when the ownership cost is
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

**Scale tested:** 2 nodes, 70 pods under pressure. The dependency branch was previously
measured at ~2,888 pods across 6 nodes; the native branch has not been tested at that scale,
and that gap is a cost decision rather than a technical one (Q3).

**Unexplained:** the remaining factor in the latency floor beyond the ~4× the channel
accounts for (Q8).
