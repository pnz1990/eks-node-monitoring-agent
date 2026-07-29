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
