# Design: node_exporter compatible metrics in the EKS Node Monitoring Agent

**Status:** implemented, pressure-tested, not yet submitted upstream
**Author:** rrroizma
**Last updated:** 2026-07-29

Companion documents:
- `GOAL.md` / `GOAL-PRODUCTION-READINESS.md` — the falsifiable plans this executed against
- `JOURNAL.md` — chronological record of findings, including refutations and measurement errors
- `evidence/` — raw measurements backing every number here
- `docs/parity-exceptions.md` — the deviation register

---

## 1. Problem

EKS ships `prometheus-node-exporter` (PNE) as a managed add-on. It exists solely to expose host
metrics, and to do so it needs privileged host access on every node: a DaemonSet with `hostNetwork`,
the host root filesystem mounted, and `system-node-critical` priority.

The node monitoring agent (NMA) **already has all of that**, for its health-monitoring mission. So the
cluster runs two privileged DaemonSets reading the same `/proc` and `/sys` for different reasons.

The proposal: serve node_exporter-compatible metrics from NMA, making the separate add-on redundant.

## 2. Goals and non-goals

**Goals**
- Metric output indistinguishable from upstream node_exporter, so dashboards, recording rules and
  alerts keep working with no change.
- Zero impact on customers who do not want it.
- A collector defect must never be able to degrade NMA's health-monitoring mission.

**Non-goals**
- Replacing the CloudWatch/EMF path NMA already uses for its own condition metrics.
- Serving metrics on Auto Mode nodes via the DaemonSet. Auto Mode gets NMA through the Tachyon AMI, so
  coverage there follows an AMI release, not this change.
- Improving on upstream's *metrics*. The value is consolidation and resilience, not new signals.

## 3. Key design decisions and their tradeoffs

### 3.1 In-process, not a sidecar

**Decision:** import `prometheus/node_exporter/collector` into the agent binary.

| | In-process (chosen) | Sidecar |
|---|---|---|
| Parity | must be proven | free — it *is* node_exporter |
| Footprint | one process | two containers, two privileged surfaces |
| Blast radius | **collector defect can kill the agent** | isolated by process boundary |
| Upstream tracking | dependency bump | image bump |

**Why in-process:** a sidecar delivers the business goal (retire the add-on) but keeps two privileged
containers, which is most of what the consolidation was meant to remove. The decisive risk — blast
radius — is addressable in code (§3.4), whereas the sidecar's duplicated privilege is not.

**Tradeoff accepted:** we own a resilience boundary upstream does not have, and we must re-prove parity
on every dependency bump. `hack/parity-test.sh` exists precisely to make that cheap.

**Fallback if this had failed:** the sidecar remained a first-class option until the kingpin/pflag
coexistence spike passed. It did (§3.2), so in-process stayed.

### 3.2 kingpin flags resolved once, on the package-level CommandLine

Upstream registers every `--collector.<name>` flag onto kingpin's **package-level** `CommandLine`
during `init()`. The agent uses `pflag`. The values backing `NewNodeCollector` are only populated once
that CommandLine has been parsed.

**Decision:** parse `kingpin.CommandLine` exactly once, behind a `sync.Once`, with `Terminate(nil)` so
a bad flag returns an error instead of calling `os.Exit` and taking the agent down.

**Tradeoff:** the two flag libraries coexist in one process, which is unusual. Verified in a spike
before committing to the design: both parse without collision, and the agent's own CLI is unaffected.
The `sync.Once` means flags are immutable after startup — acceptable, since the config is read at
startup anyway.

### 3.3 A separate listener on :9100, not the existing controller-runtime endpoint

The agent already serves `problem_condition_count` and `fatal_condition_gauge` on `:8003`.

**Decision:** own a second `http.Server` for host metrics.

**Why:** `:8003` has an existing contract, and 9100 is the node_exporter convention that existing
scrape configuration expects. Merging them would break both.

**Tradeoff:** two listeners in one process. Mitigated by binding 9100 only when the feature is enabled
(§3.5) and by giving the endpoint its own `ReadHeaderTimeout`, since it is reachable on the host
network.

### 3.4 A resilience boundary upstream does not have

**This is the core design decision, and the clearest way the fork is better than PNE rather than
merely equal.**

Upstream `NodeCollector.Collect` fans every collector into its own goroutine and calls `Update()` bare
(`collector/collector.go:145-157`). There is **no `recover()` in that file and no timeout.**

That is defensible for node_exporter: a crashed metrics exporter loses metrics. It is **not** defensible
here, because the same process publishes the `NodeCondition`s that EKS node auto repair acts on. A
collector bug would escalate from "lost metrics" to "unmonitored node."

The risk is not theoretical. Open upstream issues include three crash reports (#1007 panic, #3346
SIGSEGV on linux/amd64, #1987 crash) and three hang/timeout reports (#1841, #1353, plus #2585/#3649
which are *requests for timeouts upstream still does not have*).

**Decision:** replace `NodeCollector` with `resilientCollector`, providing per-collector `recover()` and
a 5s per-collector timeout, plus `node_collector_panics_total` / `node_collector_timeouts_total` so a
contained fault is visible rather than silent.

**Why the recover must wrap `Update` specifically** — proven, not assumed:

| Panic location | Outcome |
|---|---|
| On the request goroutine | contained by `promhttp` → HTTP 500, process survives |
| In a goroutine spawned by `Collect` (**upstream's path**) | **fatal**, nothing can recover it |

A guard in the HTTP handler cannot work, because the handler is not on the panicking stack.

**Tradeoffs accepted:**
- We diverge from upstream's collection loop, so upstream changes to `execute()` need manual review.
  The meta-metric names and semantics (`node_scrape_collector_{success,duration_seconds}`, `ErrNoData`
  handling) are reproduced exactly so the contract is unchanged.
- A timed-out collector cannot be *cancelled* — `Update` takes no context — only abandoned. Its
  goroutine is left to finish into a drain.
- Partial data is preferred over no data: metrics emitted before a panic or timeout are kept.

**Three bugs found in the guard while building it**, all in channel lifetime rather than logic. Worth
recording because they show the shape of the risk:
1. An abandoned collector writing after `Collect` returned would send on a closed channel and panic
   where nothing can recover — *the guard would have become a new crash source*. Fixed with a relay
   channel plus a drain.
2. Closing the forwarder on the success path silently dropped healthy metrics, because `Update` sends
   to `done` before the deferred `close(relay)` runs.
3. A `return` inside the recover skipped `close(relay)`, hanging the forwarder on the panic path.

A panic guard that mishandles channel lifetime is worse than no guard. The tests that caught these —
sibling-metric survival, blocked-send detach — are the ones that must stay green.

### 3.5 Opt-in, default off

**Decision:** the endpoint is disabled unless explicitly enabled, inverting NMA's convention where a
nil `enabled` means true.

**Why:** enabling it exposes an additional listener from a `privileged` + `hostNetwork` + `hostPID`
process. Compared with the PNE add-on — which runs unprivileged with a read-only root filesystem and an
optional `kube-rbac-proxy` — that is a posture change, and it must be a customer's explicit choice
rather than something that appears on upgrade.

**Tradeoff:** consolidation only happens for customers who opt in, so the add-on cannot be retired
unilaterally. Accepted: a silent posture change on upgrade would be worse.

**Enforced, not just documented:** a test asserts nothing is bound on 9100 when disabled. A feature
that is off but still listening is not off.

### 3.6 Strict parity beats redundant mitigation

Two EKS-appropriate collector defaults were candidates for mitigating the netclass churn hazard. Both
were **measured and rejected**:

| Candidate | Parity result |
|---|---|
| `--collector.netclass.ignored-devices=<pod ifaces>` | **297** names vs upstream 298 — loses `node_network_speed_bytes` |
| `--collector.netclass.netlink` | **299** names vs upstream 298 — adds `node_network_altnames_info` |
| neither | **298 / 298, empty diff** |

On an EKS node the only interfaces exposing a readable link speed are the pod-side `eni*` halves; the
primary `ens5`/`ens6` do not. So excluding pod interfaces removes that family entirely.

**Decision:** ship neither. The per-collector timeout (§3.4) already contains the failure they would
prevent, so they would buy redundant protection at the cost of a measurable parity regression. Both are
documented as opt-in flags for churn-heavy clusters.

An invariant test allowlists every EKS default, so adding one forces a parity re-measurement.

### 3.7 One deliberate deviation: per-pod ephemeral mounts

**The single default we do ship**, and the only intentional behavioural deviation.

The filesystem collector reads **PID 1's** mount table (`filesystem_linux.go:185`). The agent runs with
`hostPID: true`, so PID 1 is host init and its namespace contains every per-pod mount. Upstream never
sees these, because PNE does not use `hostPID`.

Measured on an idle 20-pod node: **17** filesystem series versus upstream's **4**. The 13 extra were
containerd sandbox `shm` mounts and kubelet projected volumes, whose paths embed unique pod UIDs and
sandbox IDs — unbounded, high-churn cardinality that grows with pod density and persists for the
retention window on every pod create/delete.

**Decision:** extend upstream's `defMountPointsExcluded` with `run/containerd/.+/sandboxes/.+` and
`var/lib/kubelet/pods/.+`.

**Why this is consistent with upstream intent rather than a divergence from it:** upstream already
excludes `var/lib/docker/.+` and `var/lib/containers/storage/.+` for exactly this reason, and simply
has no entry for containerd-on-Kubernetes.

**Tradeoff:** the agent will not report free space *inside* a pod's ephemeral volumes. That is a pod
concern, not a node one, and kubelet already surfaces it.

**Verified:** parity unchanged at 298/298 (the exclusion removes series within a family, not families),
and on the live cluster both exporters now report **24/24** filesystem series with **zero** pod mounts —
even under 2,888 pods of churn.

The regression test asserts **both** directions. The must-keep half matters more: an over-broad regexp
swallowing `/var/lib/kubelet` would hide a real disk-full condition.

## 4. What pressure testing showed

6 nodes (4× t3.large amd64, 2× m7g.large arm64), EKS 1.36, both exporters co-resident and scraped by
one Prometheus, under ~2,888 churning pods plus full CPU saturation.

| Measure | NMA | PNE | Reading |
|---|---|---|---|
| Total series | 4,726 | 4,737 | parity holds under churn |
| Filesystem series | 24 | 24 | §3.7 fix holds under churn |
| **Max collector duration** | **0.138s** | 0.681s | **NMA ~5× faster** |
| Scrape success (`up`) | 1.0 | 1.0 | neither dropped a scrape |
| Goroutines | 115 (peak 117) | 8 (peak 9) | flat — no leak |
| Open fds | 28 (peak 29) | 9 | flat — no leak |
| Resident memory | **65MB** (peak 81MB) | 23MB | see below |
| Panics / timeouts contained | 0 / 0 | n/a | guard never needed to fire |

**Memory is the honest finding.** NMA sits at ~65MB against PNE's 23MB, and rose to 81MB under load.
Investigated whether it was a leak:
- derivative decayed 10147 → 3971 → 1258 B/s as load drained
- memory fell from the 81MB peak back to ~65MB
- goroutines and fds stayed flat throughout
- all six nodes converged on ~65MB, arm64 and amd64 alike

So it is Go heap working set under load, **not an unbounded leak**. But 65MB steady-state against the
chart's 200Mi limit is a third of the budget before the health monitors' own usage, and this was
measured with `includeExporterMetrics` on. The resource envelope needs revisiting before this ships
widely, and on Auto Mode it interacts with `EKSTachyonAMIOverhead`, which feeds Karpenter bin-packing
(see `PARITY-PLAN.md` F5).

**Prediction PR7 outcome:** NMA did *not* degrade worse than PNE under identical pressure — it was
faster on collection latency and equal on scrape success. The in-process design is not a liability
under load. Memory is the one axis where it costs more, and that cost is bounded and explainable.

## 5. Rejected alternatives

| Alternative | Why rejected |
|---|---|
| Sidecar PNE in the NMA chart | Keeps two privileged containers, which is most of what consolidation was meant to remove. Retained as fallback until the flag-coexistence spike passed. |
| Fork node_exporter collectors to fix upstream bugs | Trades permanent maintenance cost for fixes that mostly do not apply to EKS (10 of 12 open bugs are N/A or refuted here). Contain at the boundary instead; send real fixes upstream. |
| Serve host metrics on the existing `:8003` | Breaks that endpoint's contract and the 9100 convention simultaneously. |
| Default the endpoint on | Silent posture change on upgrade for a privileged listener. |
| Ship EKS collector defaults for netclass | Measured parity regression for protection the timeout already provides (§3.6). |

## 6. Known limitations

1. **Auto Mode coverage lags.** Auto nodes get NMA from the Tachyon AMI, owned by DCP, so coverage
   follows an AMI build→activate cycle (~1 week plus regional waves), not this merge.
2. **Port 9100 contention.** PNE and NMA cannot both bind it on a `hostNetwork` node. Migration is a
   cutover; side-by-side requires moving one of them (this testing used 9101).
3. **`node_procs_running` carries an observer-effect bias.** It counts runnable threads and NMA is a
   larger process, so it reads higher than PNE (30m avg 6.33 vs 1.11) while `node_load1` agrees exactly.
   Alerts thresholded on its absolute value need retuning; documented in `docs/parity-exceptions.md`.
4. **Memory footprint** ~3× PNE (§4). Bounded and non-leaking, but consumes a third of the current
   chart limit.
5. **Not tested:** memory-pressure/near-OOM, disk-I/O saturation, GPU/Neuron nodes, Bottlerocket, and a
   multi-hour soak. These are named blind spots, not assumed passes.
6. **Upstream submission not started.** `CONTRIBUTING.md` requires an issue before the PR; that is a
   public post to an AWS repo and needs a maintainer's go-ahead.
