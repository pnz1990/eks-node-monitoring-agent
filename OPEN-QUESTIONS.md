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

## Q8 — the dependency branch's per-collector latency floor  *(OPEN, found in N7)*

Measured over the 39 collectors BOTH variants run, on the same nodes in the same scrape pass:

```
              baseline    under pressure
nma-dep        0.9114s        0.3288s
nma-nodep      0.0181s        0.0210s
pne            0.0123s        0.0215s
```

The dependency branch is ~15x slower than the native one and ~70x pne for the same data. The
distribution shows a FLOOR rather than slow work — its median is ~250x the native variant's,
and collectors doing wildly different amounts of work land within a whisker of each other.

**Partial cause:** its relay channel is unbuffered (`make(chan prometheus.Metric)`) where the
native one is buffered at 1024, so every metric costs a goroutine handoff. Benchmarked in
isolation, that accounts for **~4x, not ~250x**.

**Ruled out:** CPU limits (both agents 250m/200Mi), CPU contention (the gap is larger at
baseline with no saturation running).

**Unexplained:** the remaining factor.

**Why not fixed here:** changing that branch's relay buffer would invalidate the 298/298
parity evidence it carries and the controlled comparison this branch exists for.

**What is needed:** fix the buffer on the dependency branch, re-measure, and identify the
remainder. It affects whichever branch ships, and it should be resolved before either is
recommended on performance grounds. At 0.9s against a 15s scrape interval it breaks nothing
today.

## Q9 — should the agent adopt `promhttp.InstrumentMetricHandler`?  *(OPEN, found in N6)*

pne exports `promhttp_metric_handler_requests_total` and `_requests_in_flight`; the agent
exports neither, because it calls `promhttp.HandlerFor` without wrapping it in
`InstrumentMetricHandler`. Both do export `promhttp_metric_handler_errors_total`, which
`HandlerFor` provides directly.

These describe the scrape endpoint rather than the node, so it is not a host-metrics parity
gap — but it IS the only remaining metric-name difference between the agent and pne, and a
dashboard that graphs scrape request rates would find it missing.

Cheap to add (one wrapper call) and affects both branches identically. Not done unilaterally
because it adds 2 series per node to every scrape, and that is a fleet-wide cardinality
decision rather than a code decision.
