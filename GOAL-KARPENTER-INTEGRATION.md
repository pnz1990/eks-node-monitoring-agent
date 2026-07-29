# GOAL — validate both forks against Karpenter without compromising NMA's original function

**Status:** K0 complete — **two assumptions in this file were wrong and are corrected below.**
See `JOURNAL-KARPENTER.md` §K0 for the source reading.
**Owner decision needed:** none to begin; see §10 for the cost items

> ### K0 CORRECTIONS (2026-07-29), applied in place
>
> **1. NMA's conditions are a *named, first-class* Karpenter input.** Karpenter's Node Auto Repair
> policy lists exactly the five NMA condition types, with toleration durations:
> `AcceleratedHardwareReady` **10 min**; `StorageReady`, `NetworkingReady`, `KernelReady`,
> `ContainerRuntimeReady` **30 min**. The contract is documented, not inferred.
>
> **2. There is a 30-minute toleration before Karpenter acts.** This **lowers H2's severity**: a
> spurious Fatal that clears inside 30 minutes causes **no** replacement. It does not eliminate it —
> a flapping veth holds the condition false indefinitely. Every "spurious Fatal" finding must now
> also state **how long it persisted**, because under 30 min is a logging problem and over 30 min is
> an availability incident.
>
> **3. Repair bypasses drain** — "forcefully terminate ... bypassing the standard drain and grace
> period procedures". A false Fatal is a forced termination, not a graceful replacement.
>
> **4. The 20% safety valve breaks the original topology.** Karpenter skips repair when >20% of a
> NodePool is unhealthy. With 2-node pools, one unhealthy node is 50% → **repair is suppressed and
> the test observes nothing while appearing to pass.** Repair-loop pools must be **≥5 nodes**;
> §4.3 is corrected.
>
> **5. Node Auto Repair is alpha and off by default** (Karpenter v1.1.0+, gate `NodeRepair=true`),
> and **this cluster has no Karpenter, no Auto Mode, and `nodeRepairConfig: null`** — so the
> `Fatal → replacement` link is currently **not wired at all**. §10's flagged assumption is
> confirmed: everything validated to date measured *condition publication*, never *repair
> execution*.
**Branches under test:** `feat/prometheus-node-exporter-parity` (**dep**) · `feat/metrics-no-upstream-dependency` (**nodep**)
**Baseline to protect:** stock NMA (`main`), unmodified

---

## 0. The actual goal, stated so it can fail

NMA exists to publish `NodeConditions` that **EKS node auto repair** acts on. On EKS Auto Mode
that repair is executed by **Karpenter**, which drains and replaces the node. Both forks add a
second HTTP listener and (on `nodep`) 8,523 lines of new collector code **into the same process**
that owns that signalling path.

All validation so far ran on a **managed node group**. Karpenter has never been in the loop.

> **The claim this goal must falsify or confirm:**
> **Neither fork changes NMA's observable behaviour toward Karpenter** — not the conditions it
> publishes, not their timing, not their accuracy, and not the node's ability to be launched,
> scheduled onto, drained, consolidated, or replaced.

**A false negative here is the dangerous outcome.** If a fork causes a *spurious* Fatal condition,
Karpenter replaces a healthy node. At fleet scale that is an availability incident caused by a
monitoring agent. If a fork *suppresses* a real Fatal condition, a broken node is never repaired.
Both are worse than any metric-parity defect found so far, so this goal is weighted toward
detecting them rather than toward breadth.

### Non-goals

- Re-validating metric parity. Done: 304/304 names, 346/346 families, `docs/design/node-metrics-experiment.md`.
- Choosing between dep and nodep. That recommendation stands; this goal can *invalidate* it but is
  not framed to decide it.
- Testing Karpenter itself. Karpenter is the environment, not the subject.

---

## 1. What is being protected, concretely

The contract NMA→Karpenter, from the code rather than from memory:

| | Value | Source |
|---|---|---|
| Condition types | `KernelReady`, `ContainerRuntimeReady`, `NetworkingReady`, `AcceleratedHardwareReady`, `StorageReady` (+ `Ready`, `DiskPressure`, `MemoryPressure` defined) | `pkg/conditions/conditions.go` |
| Total reasons | **74** | `pkg/reasons/reasons.yaml` |
| **Fatal** reasons (the ones that trigger repair) | **21** | ibid., `DefaultSeverity: Fatal` |
| Write path | `Status().Patch` on `nodes/status` and the `NodeDiagnostic` CRD (`eks.amazonaws.com/v1alpha1`) | `pkg/controllers/nodediagnostic.go`, `charts/.../clusterrole.yaml` |
| Severity → action | `Fatal` = "permanent issue, only repairable through an external action from a repair agent" | `api/monitor/interface.go:56` |

The 21 Fatal reasons, in full, because "the Fatal set is unchanged" is a claim that needs a
list to check against:

```
KernelReady/ForkFailedOutOfPIDs
ContainerRuntimeReady/PodStuckTerminating
NetworkingReady/MissingLoopbackInterface  IPAMDNotReady  IPAMDNotRunning
                 InterfaceNotRunning  InterfaceNotUp  NPANotRunning
AcceleratedHardwareReady/DCGMDiagnosticFailure  DCGMError  DCGMHealthCodeFatal
                 NvidiaDeviceCountMismatch  NvidiaDoubleBitError  NvidiaNVLinkError
                 NvidiaXIDError  FabricManagerNotRunning  NvidiaFabricError
                 NeuronDMAError  NeuronHBMUncorrectableError
                 NeuronNCUncorrectableError  NeuronSRAMUncorrectableError
```

### Timing constants that make tests deterministic

From `monitors/networking/monitor.go`:

```
interfaceMonitorPeriod             = 5 * time.Minute
interfaceCacheTTL                  = 10 * time.Minute   (2 x period)
ipamdNotRunningConsistencyDuration = 15 * time.Minute
criIPCacheTTL                      = 2 * time.Hour
```

`InterfaceNotUp`/`InterfaceNotRunning` require **the same fault in two adjacent monitor periods**
(`interfaceHasConsistentIssue`). **Every wait in this goal is expressed as a multiple of these
constants, never as a guessed sleep.** A test that waits 30s for a 5-minute monitor is a test that
always passes.

---

## 2. The specific hazards this goal exists to find

Not a generic "test with Karpenter" — five named mechanisms, each with a reason to suspect it.

### H1 — `hostPort: 9100` can make a node unschedulable for NMA itself

`charts/.../daemonset.yaml` sets `hostNetwork: true`, `hostPID: true`, and
`hostPort: {{ .Values.nodeAgent.metrics.port | default 9100 }}`.

**Already observed on the MNG cluster:** an unrelated `prometheus-node-exporter` release holding
`hostPort 9100` left PNE pods **Pending** on 2 of 6 nodes with
`didn't have free ports for the requested pod ports`.

On Karpenter this is worse than an inconvenience. If NMA cannot schedule on a freshly launched
node, that node has **no health monitoring at all** — and Karpenter will keep launching more. A
customer who installs the PNE addon *and* enables `metrics.enabled` gets exactly this collision.

**Note the mitigation already in place:** `metrics.enabled` defaults to **`false`**, so stock
installs do not bind the port. This hazard is about the *forked, enabled* configuration.

### H2 — veth churn under consolidation vs Fatal `InterfaceNotUp`/`InterfaceNotRunning`

`checkInterfaces` enumerates `net.Interfaces()` and emits **Fatal** when an interface lacks
`FlagUp`/`FlagRunning` in two adjacent periods. Karpenter consolidation deliberately churns pods
and therefore veth interfaces, far more aggressively than an MNG at steady state.

**This is the same hazard family as upstream #1915**, which we *reproduced* on the metrics side.
Here the consequence is not a missing metric — it is a **node replacement**.

The two-period guard is the existing defence and it looks sound. **This goal tests whether it
holds under Karpenter-rate churn**, and whether either fork perturbs it (e.g. by adding load that
delays a monitor period past the cache TTL).

### H3 — the forks share a process with the monitors

`nodep` adds 39 collectors on a 15s scrape; `dep` adds 49. Both run inside the process that owns
condition reporting. Plausible interference: goroutine pressure, `GOMAXPROCS`/CPU-quota
contention (**already measured: 5,124 throttle periods / 337.9s throttled at the chart default
`cpu: 250m`** — see Q10), file-descriptor pressure, or memory pushing the 200Mi limit (`dep` sits
at ~65MB steady vs PNE's ~23MB — Q5).

**The failure mode to look for is a *delayed* condition, not an absent one.** A condition that
arrives 2 monitor periods late is a repair that happens late, and nothing currently alerts on it.

### H4 — drain, termination, and the `NodeDiagnostic` CRD

Karpenter drains and deletes nodes routinely (consolidation, drift, expiry, spot interruption).
Questions with real answers: does the agent shut down cleanly when evicted? Does it leave a stale
`NodeDiagnostic` for a node that no longer exists? Does the second listener delay
`SIGTERM` handling past the grace period? Does a condition written *during* drain race the node's
deletion?

### H5 — node launch: does the agent report before Karpenter gives up?

Karpenter has its own readiness expectations for new nodes. If either fork slows startup —
`nodep` builds 39 collectors at construction; `dep` runs upstream's kingpin flag resolution — a
node could be marked ready late, or churn.

---

## 3. Falsifiable claims

Each claim states what would **disprove** it. A claim that cannot fail is not on this list.

### Tier A — the contract (any failure blocks both forks)

| ID | Claim | Disproved by |
|---|---|---|
| **A1** | The set of condition **types** published on a Karpenter node is identical across `main`, `dep`, `nodep` | any type present on one and not another |
| **A2** | The **Fatal reason set** is identical across all three — all 21, no additions | a reason emitted by a fork and not by `main`, or vice versa |
| **A3** | An injected Fatal condition appears on the node for **all three** variants | absent on any variant within the deadline in §4.2 |
| **A4** | Time-to-condition for an injected fault is within **1 monitor period** across variants | a fork is slower by ≥1 full period (5 min) |
| **A5** | **Zero spurious Fatal conditions** on a healthy node under Karpenter churn, all variants | any Fatal whose reason does not correspond to an injected fault |
| **A6** | Karpenter successfully **launches, schedules, drains, consolidates and terminates** nodes with each variant installed | any node stuck `NotReady`, undrainable, or a consolidation that never completes |
| **A7** | No stale `NodeDiagnostic` remains for a deleted node | a `NodeDiagnostic` outliving its node by >2 min |

### Tier B — the forks specifically

| ID | Claim | Disproved by |
|---|---|---|
| **B1** | With `metrics.enabled: true`, the agent schedules on **every** Karpenter node | any NMA pod `Pending` on `FreePort` |
| **B2** | With PNE **also installed**, the hostPort collision is either absent or **documented with a mitigation** | collision occurs and no mitigation is stated |
| **B3** | `node_scrape_collector_success` and condition reporting are **independent**: a failing collector never affects a condition | a collector failure correlated with a missing/late condition |
| **B4** | Metric parity **holds on Karpenter nodes** as on MNG (304/304 names) | any name difference on a Karpenter-launched node |
| **B5** | Neither fork logs ERROR/FATAL or panics during a full launch→drain→terminate cycle | any such line |
| **B6** | Agent restarts during Karpenter churn = **0** for both forks | ≥1 restart not caused by an injected fault |

### Tier C — resource interaction (informational, but measured)

| ID | Claim | Disproved by |
|---|---|---|
| **C1** | CPU throttling does not delay a condition past 1 monitor period | correlation between `nr_throttled` and condition latency |
| **C2** | Memory stays under the 200Mi limit through a full cycle for both forks | OOMKill, or RSS >180Mi |
| **C3** | Goroutine and fd counts are **bounded** across ≥20 consolidation events | monotonic growth over the run |

### Tier D — negative controls (**these must FAIL, or the harness proves nothing**)

| ID | Control | Required outcome |
|---|---|---|
| **D1** | Inject `IPAMDNotReady` on stock `main` | condition **appears** — proves injection works before testing forks |
| **D2** | Stop the agent, then inject | condition does **not** appear — proves detection isn't ambient |
| **D3** | Assert a condition that was never injected | check **fails** — proves the assertion can fail |
| **D4** | Corrupt the kubeconfig, then run the harness | harness **errors loudly**, does not report "0 spurious conditions" |
| **D5** | Point the harness at a node with no agent | reports **missing**, not healthy |

**D4 exists because this failure has happened repeatedly in this project** — expired credentials
producing "0 restarts", a `set -e` grep killing a script silently, a pressure run with no pressure
that *passed*. Every check in this goal must fail loudly on a failed query rather than report a
zero.

---

## 4. Method — deterministic and reproducible

### 4.1 Fault injection: log lines, not real breakage

The e2e suite already establishes the mechanism (`e2e/suites/monitors/node_exporter.go`): a
privileged pod writes a known line to a host path. This is **deterministic, reversible, and needs
no special hardware**:

```
NetworkingReady / IPAMDNotReady   (FATAL)
  echo 'Unable to reach API Server' >> /host/var/log/aws-routed-eni/ipamd.log
  detection: monitors/networking/monitor.go:302

KernelReady / SoftLockup          (Warning — for the non-Fatal path)
  echo 'watchdog: BUG: soft lockup - CPU#6 stuck for 23s! [VM Thread:4054]' > /host/dev/kmsg
```

**`IPAMDNotReady` is the primary probe:** it is Fatal, it is one line, it needs no GPU, and it
exercises the exact path Karpenter acts on.

**The 16 GPU/Neuron Fatal reasons are out of scope** — they need accelerated hardware. Stated as a
coverage limit rather than silently skipped: **5 of 21 Fatal reasons are testable without
accelerators** (`ForkFailedOutOfPIDs`, `PodStuckTerminating`, and 6 networking ones, of which
`IPAMDNotReady` and `MissingLoopbackInterface` are cleanly injectable).

### 4.2 Deadlines derived from the code, never guessed

| What is timed | Deadline | Source |
|---|---|---|
| `IPAMDNotReady` condition appears | **6 min** | log scan is prompt; 1 period + margin |
| `InterfaceNotUp/NotRunning` appears | **12 min** | needs 2 adjacent periods (2 × 5 min) + margin |
| `IPAMDNotRunning` appears | **18 min** | `ipamdNotRunningConsistencyDuration` = 15 min + margin |
| **Karpenter terminates the node** | **30 min + launch time** | Karpenter toleration for `NetworkingReady=False` |
| condition clears | **2 periods** | after removing the fault (`interfaceCacheTTL`) |

**A single end-to-end repair test takes ~40 minutes, and that floor is set by Karpenter's
toleration duration, not by the harness.** Budget accordingly; do not shorten it by "just checking
the condition", which tests a different claim (K0.3).

**Any harness wait shorter than these is a bug in the harness.** Record the *observed* latency,
not just pass/fail, so A4 can be evaluated.

### 4.3 Karpenter topology

Replace the MNG with a Karpenter `NodePool` + `EC2NodeClass`:

- consolidation **enabled** (`WhenEmptyOrUnderutilized`) — this is the churn driver for H2
- short `consolidateAfter` to force frequent, observable consolidation
- `expireAfter` set to force drift-driven replacement (H4)
- instance types constrained for reproducibility (one family, one size)
- **the existing MNG kept at minimum size** for the Karpenter controller itself, and labelled so
  no test workload lands there

**One variant per node pool**, not per cluster: three pools (`main`, `dep`, `nodep`) with distinct
labels and taints, so the three run under *the same* Karpenter controller and the same churn — the
same "same-node, same-pass" discipline that made the metrics comparison trustworthy.

> **CORRECTED per K0.2: pool sizing is load-bearing.** Karpenter suppresses repair when >20% of a
> NodePool is unhealthy. A 2-node pool with one unhealthy node is at 50% → **repair never fires and
> the test silently observes nothing.** Repair-loop pools (Tier A3/A5/A6 with `NodeRepair=true`)
> therefore need **≥5 nodes each**; condition-publication pools (A1/A2/A4, B*) may stay at 2.
>
> This is itself a Tier-D-style trap: a 2-node pool would have produced "no replacement occurred"
> and could have been misread as "no spurious repair", when the truth is the harness could not have
> seen one either way.

> **Why not one cluster per variant:** it would triple cost and make "the same churn" unprovable.
> **Why not all three on one node:** `hostPort 9100` collides (H1), which is itself under test.
> Node pools give shared conditions with isolated ports.

### 4.4 Determinism requirements

- every wait a multiple of a **named constant** from §1
- every assertion states the **observed value**, not just a verdict
- credentials validated **before** every measurement round; refresh, and **abort loudly** if the
  refresh fails
- every kubectl query checked **non-empty** before its result is used
- pressure verified by the workload's **own status** (`.status.active`/`.status.succeeded`), never
  by cluster-wide pod count — the pod-count guard has already produced a false pass here
- Karpenter's own decisions read from **its events and `NodeClaim` status**, not inferred from node
  count
- record the Karpenter version, AMI ID, kernel, and agent image digest in the evidence file, since
  a result that cannot be tied to a build is not reproducible

---

## 5. Phases

Each phase ends in a committed artifact. **The order is chosen so that a Tier-A failure stops the
run before money is spent on stress.**

| | Phase | Exit criterion |
|---|---|---|
| **K0** | Read Karpenter's node-repair/health interaction; record the *actual* contract, correcting §1 if the code disagrees | contract documented from source, not assumption |
| **K1** | Karpenter installed; 3 NodePools; MNG minimised; **D1–D5 negative controls pass** | injection proven to work *and* proven able to fail, on `main` first |
| **K2** | Tier A on `main` only — the baseline the forks must match | baseline conditions, reasons, and latencies recorded |
| **K3** | Tier A on `dep` and `nodep`; three-way diff | A1–A7 verdicts with observed values |
| **K4** | Tier B: hostPort (H1/B1/B2), collector-vs-condition independence (B3), parity on Karpenter nodes (B4) | B1–B6 verdicts |
| **K5** | Stress: consolidation churn + pod churn + CPU saturation, ≥20 consolidation events, all three pools | A5/A6/B6/C1–C3 under churn |
| **K6** | Edge cases (§6), each individually | each case a recorded verdict, including "not reached" |
| **K7** | Continuous log + metric monitoring across K2–K6; fix what is found, **on both forks** | issues found, fixed, re-verified |
| **K8** | `docs/design/karpenter-integration.md` + `evidence/karpenter-*.md`; update the §9 recommendation in `node-metrics-experiment.md` if this changes it | verdict on the §0 claim |

---

## 6. Edge and corner cases to drive deliberately

Enumerated because "look for edge cases" is not testable. Each is a scenario with an expected
outcome.

| # | Case | Why it could break | Expected |
|---|---|---|---|
| E1 | Condition written **during** drain | race between patch and node deletion | no error storm; no stale `NodeDiagnostic` |
| E2 | Node consolidated **while a Fatal is active** | two systems acting on one node | Karpenter's action wins; no stuck node |
| E3 | Spot interruption mid-scrape | abrupt termination | clean shutdown, no panic |
| E4 | Agent evicted, rescheduled on the same node | cache state lost | conditions re-derived; **no spurious Fatal from an empty interface cache** |
| E5 | Interface cache empty on a **brand-new** node | `interfaceHasConsistentIssue` needs a previous observation | **no** Fatal on first period (the `!ok → continue` path) |
| E6 | Two consolidations inside one monitor period | churn faster than the monitor | no false `InterfaceNotUp` |
| E7 | `hostPort 9100` already taken on a new node | H1 | agent schedules, or fails **visibly** |
| E8 | Node at max pods during consolidation | scheduling pressure | agent not evicted (priority/`system-node-critical`) |
| E9 | Karpenter drift replaces **all** nodes at once | fleet-wide simultaneous restart | no condition gap >1 period |
| E10 | `NodeClaim` fails to launch | agent never starts | no orphan `NodeDiagnostic` |
| E11 | Clock skew / large `timex` offset | affects both metric and condition timestamps | conditions still ordered correctly |
| E12 | Metrics endpoint scraped at high rate during consolidation | `--web.max-requests` = 40; the `os_release` race we fixed lives here | no condition delay; no 503 storm |

**E5 and E4 are the highest-value cases.** Both concern the interface cache being empty, which is
exactly the state a Karpenter-launched node starts in — and a bug there produces a **spurious
Fatal on a healthy new node**, i.e. Karpenter replacing nodes in a loop. The code path
(`obj, ok, _ := m.interfaceCache.GetByKey(...)` → `if !ok { continue }`) looks correct; this goal
must **prove** it under real churn rather than reason about it.

---

## 7. What continuous monitoring means here

Not "watch the logs" — a defined signal set sampled on a fixed cadence throughout K2–K6, with the
credential and non-empty guards from §4.4:

| Signal | Source | Alarm condition |
|---|---|---|
| ERROR/FATAL/panic lines | agent logs, per pod, full history | any |
| container restarts | `.status.containerStatuses[].restartCount` | any increase |
| `node_collector_panics_total`, `_timeouts_total` | metrics endpoint | non-zero |
| `promhttp_metric_handler_errors_total{cause="gathering"}` | metrics endpoint | **any increase** — this is the Q9 defect; it was 3,182 and silent |
| `node_scrape_collector_success` | metrics endpoint | new zero not in the absent-hardware set |
| condition flap rate | node status transitions | same condition toggling >1× per 2 periods |
| goroutines, fds, RSS | `go_*`/`process_*` | monotonic growth |
| cgroup `nr_throttled` | `/sys/fs/cgroup/cpu.stat` | correlate with condition latency (C1) |
| Karpenter decisions | Karpenter events, `NodeClaim` status | a replacement with no corresponding Fatal |

**The last row is the one that matters most:** a node replacement for which no Fatal condition
exists means either Karpenter acted on something else, or a condition appeared and vanished before
sampling. Both need explaining, not filing.

---

## 8. Deliverables

| Artifact | Contents |
|---|---|
| `docs/design/karpenter-integration.md` | design, hazards, results, verdict on §0 |
| `evidence/karpenter-baseline.md` | K2 — stock `main` behaviour |
| `evidence/karpenter-three-way.md` | K3/K4 — A and B verdicts with observed values |
| `evidence/karpenter-stress.md` | K5 — churn results, ≥20 consolidations |
| `evidence/karpenter-edge-cases.md` | K6 — E1–E12, each with a verdict incl. "not reached" |
| `hack/karpenter/` | NodePool/EC2NodeClass manifests, injection pods, the harness, its self-tests |
| `JOURNAL-KARPENTER.md` | running log, so context loss does not lose findings |
| updated `OPEN-QUESTIONS.md` | anything needing a human decision |

**Both forks in every result table.** A finding on one is not a finding on the other, and the
whole point is that both must be safe.

---

## 9. How this goal reports failure

Findings are classified, because "compromises Karpenter" spans two very different severities:

- **BLOCKER** — a fork causes a spurious Fatal, suppresses a real one, or prevents a Karpenter
  operation. Stops the recommendation in `node-metrics-experiment.md` §9 outright.
- **DEGRADATION** — measurably slower or noisier, contract intact. Documented with numbers.
- **PRE-EXISTING** — reproduces on stock `main` too. Reported as an NMA finding, explicitly **not**
  attributed to the fork. *This distinction must be established by running `main` first (K2),
  which is why the phase order matters.*
- **HARNESS** — a bug in the test tooling. Recorded, because this project's harnesses have been
  buggier than the code under test.

**A "no issues found" result is only publishable if the Tier-D negative controls passed in the same
run.** Otherwise the correct report is "the harness could not have detected an issue".

---

## 10. Cost, risk, and what needs a decision

**Cost:** 3 NodePools with consolidation churn plus ≥20 consolidation events means significant
instance launch/terminate volume on top of the current 6 nodes. Constrained to one small instance
family, but this is a real spend and the churn is the *point*, so it cannot be reduced without
weakening K5.

**Risk to the existing setup:** replacing the MNG with NodePools changes the cluster the metrics
evidence was gathered on. **Mitigation: keep the MNG at minimum size rather than deleting it**, so
the metrics baseline stays reproducible. No existing resource is deleted to satisfy this goal.

**Needs a decision before K5:** the churn volume above. K0–K4 are cheap and can proceed
immediately.

**Assumption to verify in K0, not assumed here:** that this cluster's node auto repair is actually
wired to Karpenter. It is a self-managed EKS cluster with an MNG, not Auto Mode. If repair is
**not** active, then A3/A5/A6 test *condition publication* rather than *repair execution*, and the
`Fatal → replacement` link is inferred rather than observed. **If so, say that plainly in the
evidence rather than implying the full loop was tested** — and treat enabling Auto Mode or node
auto repair as a decision for the owner.
