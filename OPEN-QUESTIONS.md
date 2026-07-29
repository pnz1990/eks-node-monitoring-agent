# Open questions for rrroizma

Durable list of decisions that need a human answer. **Do not block on these** — record, pick a
defensible default, proceed, and note the default taken. Read this file on resumption.

Status key: `OPEN` needs an answer · `DEFAULTED` proceeding on an assumption that may be revisited ·
`ANSWERED` resolved, kept for the record.

---

## Q1 — What does "no dependency on PNE" actually mean?
**Status:** DEFAULTED to A
**Raised:** 2026-07-29 (N0), posted to Slack

Three readings, measured:

| | Removes | Keeps | Vendored lines |
|---|---|---|---|
| **A** | `node_exporter` | `procfs`, `client_golang` | ~10.6k |
| B | `node_exporter` + `procfs` | `client_golang` | ~32k |
| C | clean-room, no copied code | `client_golang` | from scratch |

**Proceeding on A.** Since raising it, a concrete argument for A emerged: `/proc/meminfo` has 55 keys on
the test host, `procfs` models a subset, and upstream hand-maps 51 of those, emitting 49. **Upstream
silently drops kernel fields `procfs` does not model.** Reusing `procfs` reproduces that exactly,
including the blind spot — which is what parity requires. A hand-written parser would emit *more*
metrics than upstream and break parity in the other direction. So keeping `procfs` is load-bearing for
parity, not merely convenient.

**If the intent was B**, the scope roughly triples and the parity argument above has to be solved
another way.

## Q2 — The 10 collectors that emit nothing on EKS
**Status:** ANSWERED 2026-07-29 — "do the 39 applicable for now"

Of 49 enabled collectors, 39 produce data and 10 emit nothing on any EKS node (`bcachefs`, `bonding`,
`fibrechannel`, `hwmon`, `ipvs`, `nfs`, `nfsd`, `rapl`, `tapestats`, `zfs`). PNE emits nothing for them
either, so it is absent hardware rather than a defect.

**Porting the 39.** Consequence to revisit: the `node_scrape_collector_success` series set will differ
from PNE, since PNE emits `success=0` for the 10 and we will emit nothing at all. The three-way harness
will correctly flag that as divergence. It will be recorded in
`docs/parity-exceptions-nodep.md` with the exact diff, so the cost of dropping them is visible rather
than argued.

## Q3 — Upstream contribution: bundle the resilience layer or split it?
**Status:** OPEN (carried from the dependency branch)

The resilience layer (per-collector `recover()` + timeout) is the strongest part of the dependency
branch, but it diverges from upstream's collection loop and invites "why not fix this upstream instead?"

- **Bundle** — one PR, complete story, bigger review surface
- **Split** — land the endpoint first, propose the timeout/recover to `prometheus/node_exporter`
  separately, where #2585/#3649 show demand already exists

Leaning bundle: the endpoint without the guard is hard to defend in a process that reports
NodeConditions.

## Q4 — Upstream issue filing
**Status:** OPEN, blocking the contribution PR

`CONTRIBUTING.md` requires an issue *before* a PR. That is a public post to an AWS-owned repository, so
it needs an explicit go-ahead rather than being done unilaterally.

## Q5 — The 65MB resource envelope
**Status:** OPEN

`nma-dep` sits at ~65MB steady versus PNE's ~23MB. Confirmed bounded, not a leak (derivative decayed
across load → drain → idle and went negative at one point; memory band 64.0–65.7MB with goroutines and
fds flat). Still a third of the chart's 200Mi limit, and on Auto Mode it feeds
`EKSTachyonAMIOverhead` → Karpenter bin-packing → customer allocatable.

Needs a position from the NMA owners, and from ENO/Atlas for Auto Mode. `nma-nodep` may land lower,
which would be a data point for this decision — measure before escalating.

## Q6 — The dependency branch is not PR-ready
**Status:** OPEN, pure execution, ~1–2h

`feat/prometheus-node-exporter-parity` is 10,825 insertions of which only ~3,566 belong upstream. 23
files / 7,259 lines are process artifacts: `evidence/` (contains the account ID and Grafana
credentials), dashboards hardcoded to our job labels, `JOURNAL.md`, pressure manifests with our
nodegroup selectors, design doc referencing internal systems.

Needs a clean upstream branch by cherry-picking the product commits. Ready to execute on request.

## Q7 — Upstream bugs found while porting: file them?
**Status:** OPEN, accumulating

Porting reads upstream's implementation line by line, which surfaces defects that using it as a
dependency never would. Found so far:

1. **`vmstat` panics on a malformed line.** `collector/vmstat_linux.go` does
   `parts := strings.Fields(line); strconv.ParseFloat(parts[1], 64)` with no length check, so a
   single-token or empty line in `/proc/vmstat` indexes out of range and panics. Verified by reading
   the source. Our port skips the line instead.

2. **`filesystem` has an unsynchronized data race on its result slice.**
   `collector/filesystem_linux.go` `GetStats()` declares `stats := []filesystemStats{}` on the calling
   goroutine, then appends to it from **two** goroutines with no mutex:
   - the spawned goroutine appends a `deviceError` entry for each mount already known to be stuck
   - the caller appends every entry drained from `statChan`

   These overlap: the spawned goroutine is still iterating mount points while the caller drains the
   channel. A racing `append` can lose entries or tear the slice header.

   Only triggers when a stuck mount is already recorded, which is why it has survived — the path is
   dormant on a healthy host. Found by reading the code, not by running it; `go test -race` would not
   catch it without a hung mount.

   Our port must fix this. Filing upstream is more valuable than the fix itself.

Each of these is a candidate upstream issue or PR. They are *more* valuable to upstream than our
endpoint work, and cheap to contribute individually. But filing is a public post to
`prometheus/node_exporter`, so it needs the same go-ahead as Q4.

**Recommendation:** batch them and file after the port is further along, so the list is complete rather
than trickled. Confirming ND4 (the prediction that porting reveals ≥1 upstream bug) — already true at
5 of 39 collectors.

## Q8 — the dependency branch's per-collector latency floor
**Status:** ANSWERED 2026-07-29 — **RETRACTED. F-N7-1 was a measurement artefact.**
**Commit:** `2dee53a`

The finding was: nma-dep ~15x slower than native over the 39 shared collectors, median ~250x,
with ~4x attributed to an unbuffered relay channel and the rest unexplained.

**There is nothing to fix. The harness was measuring wrong.**

`node_scrape_collector_duration_seconds` measures **wall** time, and every collector runs
**concurrently**. With fewer usable cores than collectors, a collector's timer keeps running
while its goroutine is descheduled, so each one reports roughly the *whole batch's* window
rather than its own work. `stress.sh` **summed** those values, multiplying the real cost by up
to the collector count.

Evidence (GOMAXPROCS swept, same host, same collectors):

```
GOMAXPROCS=1   dep sum=0.5552s  min=0.010862s  med=0.011322s  max=0.011806s
GOMAXPROCS=8   dep sum=0.0620s  min=0.000028s  med=0.001314s  max=0.005501s
```

At GOMAXPROCS=1 the spread across 49 collectors is **1.09x**. `netclass` walks every interface;
`loadavg` reads one short file. They cannot honestly take the same time. That is one shared
window reported 49 times.

Confirmed three ways:
- the same 49 collectors run **serially** through the same resilient wrapper cost **0.0166s**
  total, against the 0.9114s reported — a **~55x overstatement**
- **wall time is nearly identical**: 0.0121s dep vs 0.0110s native at GOMAXPROCS=1; **1.22x**
  by median in the new test; **0.66x** native/upstream on the live 6-node cluster
- buffering the relay changed nothing (0.5552s → 0.5243s), so the ~4x attributed to it does
  not survive a controlled test either. **The buffer change was reverted** — it does not do
  what it was credited with.

**Two hypotheses tested and rejected** (recorded because the negative results are what make
the conclusion trustworthy):

1. **CPU throttling.** Both agents *are* genuinely throttled — measured from the cgroup:
   nma-dep 5124 throttle periods / 337.9s throttled, nma-nodep 2007 / 140.1s, pne unlimited
   and never throttled, all at `cpu=250m` which is the **chart default**. But raising the limit
   8x (250m → 2 cores) moved the reported median from 7.72ms to 8.06ms — i.e. not at all.
   **Real finding, wrong cause.** See Q10.
2. **The relay channel**, as above.

**Guarded by 4 tests** in `pkg/metrics/concurrentduration_test.go`, which assert the
*conclusion* rather than machine-dependent timings.
`TestPerCollectorDurationsSpreadWithWork` is the negative control: with cores available the
spread is 264.5x, so the GOMAXPROCS=1 flattening is evidence of an artefact rather than how the
metric always behaves.

**Consequence for the recommendation:** the design doc declined to recommend on performance
because the number was unexplained. It is now explained, and it is **not a difference between
the branches**. Performance should be dropped from the decision entirely.

## Q9 — should the agent adopt `promhttp.InstrumentMetricHandler`?
**Status:** ANSWERED 2026-07-29 — **adopted, and it exposed two real defects.**
**Commit:** `bd9e7cb`

Adopted. All three variants now export the same `promhttp_*` set, so the last remaining
metric-name difference against pne is closed and the exclusion has been **removed** from
`compare.sh` T1 rather than left in place.

Doing it surfaced two defects that no existing test could catch, both found by diffing **live**
endpoints:

1. **A duplicate error counter failing every scrape.** `newHandler` built a handler with
   `Registry: registry`, then *reassigned* `promHandler` with `Registry: exporterRegistry`.
   `HandlerOpts.Registry` registers `promhttp_metric_handler_errors_total` onto whatever it is
   given, so the discarded first handler had already put that counter in the **main** registry
   and the replacement put it in the exporter registry too. Gathering
   `Gatherers{exporterRegistry, registry}` then collected the same family twice and failed with
   *"was collected before with the same name and label values"*.

   Measured live, **+1 per scrape** confirmed over three consecutive scrapes:
   nma-dep **3182** gathering errors (13h uptime), nma-nodep **9**, pne **0**. Every scrape of
   both agents was serving a **partial** response. Now 0, verified live.

   Fixed by using upstream's `if/else` so only one handler is ever built. The
   assign-then-reassign version reads as equivalent to upstream's and is not.

   **Scope:** fires only with `includeExporterMetrics: true`, which is **not** the chart default
   (`values.yaml` sets `false`). Default deployments were unaffected; the three-way test cluster
   enables it, which is why both agents showed it and pne did not.

2. **A silent error counter**, which is why (1) went unnoticed for 13 hours.
   `ErrorHandling: ContinueOnError` with `ErrorLog` nil counts a gather error and logs
   **nothing** — 3182 errors, zero log lines. Upstream sets `ErrorLog` on both paths
   (`node_exporter.go:157,172`); the port dropped it.

**The lesson worth keeping:** T1 compares names, T2 success values, T3 series counts — and this
bug was a *value*. `compare.sh` deliberately does not compare values (two scrapes seconds apart
differ on every counter), which is still the right call, but it is a **real blind spot** and
this bug lived in it for 13 hours. Noted in the script header.

## Q10 — the 250m CPU limit throttles the agent  *(OPEN, found while closing Q8)*

Not the cause of Q8, but a real finding in its own right and it needs an owner's decision.

Measured from the cgroup on a live node, both agents at the **chart default**
`limits.cpu: 250m` (`charts/eks-node-monitoring-agent/values.yaml:83`):

```
                cpu.max            usage_usec   nr_throttled   throttled_usec
nma-dep      25000/100000 (0.25)   486,916,501       5124        337,897,035
nma-nodep    25000/100000 (0.25)   198,990,014       2007        140,147,269
pne          (no limit)                     —            0                  0
```

nma-dep spent **337.9s throttled across 5124 periods**; average stall **65.9ms**. pne ships
with no CPU limit and is never throttled.

Compounding it: **`GOMAXPROCS` is unmanaged** (no `automaxprocs`, no env var), so the Go runtime
sizes itself from the *node's* core count — observed `GOMAXPROCS=2` against a 0.25-core quota.
The runtime schedules more parallelism than the cgroup will grant.

**Why this is not urgent:** it does not currently break anything. Wall-time scrapes are
0.0107–0.0157s against a 15s interval, and no scrape has ever timed out. Throttling costs
latency inside a budget with three orders of magnitude of headroom.

**Why it still needs a decision:** it is the shipped default for every EKS node, it is invisible
(nothing alerts on cgroup throttling), and on Auto Mode the agent's envelope feeds
`EKSTachyonAMIOverhead` → Karpenter bin-packing → customer allocatable. Related to Q5.

**Options:** (a) raise the limit; (b) set `GOMAXPROCS` to match the quota, so the runtime stops
over-scheduling — cheapest and most targeted; (c) drop the CPU limit as pne does; (d) accept it
and document. **Leaning (b).** Needs the NMA owners.
