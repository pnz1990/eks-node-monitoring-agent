# Do the metrics forks compromise NMA's integration with Karpenter?

**Verdict: No. Neither fork changes NMA's observable behaviour toward Karpenter.**
**But testing against Karpenter found three defects that six nodes of managed-node-group testing
never surfaced — one of them a blocker in shared code, now fixed.**

| | |
|---|---|
| **Status** | K0–K6 complete. K7 (monitoring) ran throughout. |
| **Date** | 2026-07-29 |
| **Goal** | `GOAL-KARPENTER-INTEGRATION.md` · **Log:** `JOURNAL-KARPENTER.md` |
| **Cluster** | `nma-pne-parity-test`, EKS 1.36, AL2023, kernel 6.18.38, us-west-2 |
| **Karpenter** | v1.14.0, `NodeRepair=true` (verified from the Deployment env, not assumed) |
| **Topology** | 3 NodePools × 5 nodes, one variant each, one Karpenter controller, same churn |
| **Variants** | stock `main` (`sha256:7f4c51ca`) · dependency fork · native fork |

---

## 1. Why this needed testing at all

NMA publishes `NodeConditions`. On EKS Auto Mode, **Karpenter** consumes them and replaces
unhealthy nodes. Both metrics forks add a second HTTP listener — and the native fork adds 8,523
lines of collector code — **into the same process that owns that signalling path**.

All prior validation ran on a managed node group. Karpenter had never been in the loop, so the
integration that matters most had never been exercised.

**The dangerous outcomes are asymmetric.** A *spurious* Fatal makes Karpenter forcefully replace a
healthy node; a *suppressed* Fatal leaves a broken node unrepaired. Either is worse than any
metric-parity defect found in the earlier work, so this was weighted toward detecting them rather
than toward breadth.

### The contract, read from both sides

Karpenter's Node Auto Repair policy names **exactly** the five NMA condition types, with toleration
durations:

| Condition | Status | Toleration |
|---|---|---|
| `AcceleratedHardwareReady` | False | **10 min** |
| `StorageReady` · `NetworkingReady` · `KernelReady` · `ContainerRuntimeReady` | False | **30 min** |

So this is a **documented** contract, not an inferred one. NMA defines 74 reasons across 5 condition
types, 21 of them `Fatal` (`pkg/reasons/reasons.yaml`).

---

## 2. Result

### 2.1 The contract is intact on all three variants

| Claim | Result |
|---|---|
| **A1** condition type sets identical | **PASS** — 8 types, all three pairs `IDENTICAL` |
| **A2** no Fatal on a healthy node | **PASS** — empty Fatal set on all three at rest |
| **A3** injected Fatal detected | **PASS** — on all three |
| **A4** detection latency within 1 monitor period | **PASS** — `main` 15s, `dep` 15s, `nodep` 15s |
| **A5** no spurious repair | **PASS** — every termination maps to an injected fault |
| **A6** Karpenter launches/registers/drains/consolidates/repairs | **PASS** — all three |
| **A7** no orphaned `NodeDiagnostic` | **PASS** — 0 across 21 live nodes |
| **B1** agent schedules on every Karpenter node | **PASS** — 5/5 both forks |
| **B3** collector failures independent of conditions | **PASS** — `dep` 10 failing collectors, `nodep` 1, all conditions True |
| **B4** metric parity on Karpenter nodes | **PASS** — 304 names each, **empty diff** |
| **B5/B6** no errors, panics, restarts | **PASS** — 0/0/0 after fixes |

**A4 is the sharpest result:** identical detection latency *to the second* across all three
variants. Neither fork delays the signal Karpenter acts on — which was hazard H3's concern.

### 2.2 The repair loop, confirmed end to end

The first test in this whole effort of repair **execution** rather than condition **publication**:

```
condition False/IPAMDNotReady since   22:36:22Z
node + nodeclaim deleted              23:08:24Z
unhealthy duration                    32.0 minutes
Karpenter toleration                  30 minutes
```

Karpenter acted 2 minutes after the node became eligible. The pod events carry the forceful
signature the docs describe: *"bypasses the PDB of the pod and the do-not-disrupt annotation."*

**Repaired on all three variants**, one node per pool, within 35 seconds of each other:

```
23:07:49Z  nodep  ip-192-168-66-73    <- injected
23:08:04Z  dep    ip-192-168-32-223   <- injected
23:08:24Z  main   ip-192-168-14-177   <- injected
```

**Three terminations, three injected faults, zero unexplained.** Across ~54 minutes, 21 nodes,
1,200 pod-churn completions and three pools, **no healthy node was replaced.**

> **One honest limit:** `main`'s 32.0 min is exact, from `lastTransitionTime`. `dep` and `nodep` are
> **~21–31 min estimates** — those node objects were deleted and their transition times went with
> them. A harness gap: capture the transition time *before* a node can disappear.

### 2.3 H2 — the highest-suspicion hazard did not fire

The goal file's primary worry was veth churn under consolidation triggering Fatal
`InterfaceNotUp`/`InterfaceNotRunning` — the same hazard family as upstream #1915, which the metrics
work *did* reproduce. Here the consequence would be a node replacement rather than a missing metric.

After **1,200 pod-churn completions** creating and destroying veths continuously across all three
pools: **zero** nodes reported `InterfaceNotUp`, `InterfaceNotRunning` or
`MissingLoopbackInterface`. The two-adjacent-period guard (`interfaceHasConsistentIssue`) held under
Karpenter-rate churn.

**Recorded as a negative result, which is worth as much as a positive one here.** It does not prove
the hazard is impossible — one churn profile over ~40 minutes — but it is the specific scenario that
motivated the concern, and it did not reproduce.

### 2.4 E4/E5 — the empty interface cache is safe

The goal file's other high-value pair: a **freshly launched** Karpenter node starts with an empty
interface cache, and `interfaceHasConsistentIssue` needs a previous observation. A bug in the
`if !ok { continue }` path would mean a spurious Fatal on *every new node* — Karpenter replacing
healthy nodes in a loop.

- **E5:** three brand-new nodes (the K5 replacements), one per pool, past two monitor periods → no
  interface Fatal on any.
- **E4:** agent evicted and rescheduled, rebuilding its cache from empty → no interface Fatal.

Both pass on all three variants. Checking the *reason* rather than merely "is it False" is what
allowed the one False condition to be identified as an injected fault rather than a spurious one.

---

## 3. What Karpenter exposed that the MNG did not

**This is the strongest argument for having done this work**, and it was not in the goal file's
predictions.

### 3.1 F-K4-1 — a metrics-port conflict PANICKED the whole agent · **BLOCKER · FIXED**

```
panic: failed to listen on :9102: bind: address already in use
main.main() cmd/eks-node-monitoring-agent/main.go:99
```

Measured on Karpenter nodes: **5/5 CrashLoopBackOff.**

`mgr.Add(metricsServer)` puts the listener under the controller-runtime manager, which treats a
Runnable error as fatal, and `utilruntime.Must(run())` converts it to a panic. So a port conflict
**took down NodeCondition reporting** — and Karpenter would see a node with no health signal at all.

**This is the exact inverse of the resilience boundary's purpose.** That boundary exists so a
*collector* defect can never kill condition reporting, and it works. A *startup* bind failure
bypassed it entirely.

**Belongs to both forks** — the panic is in shared `main.go`/`pkg/metrics`. Mitigated in practice by
`metrics.enabled` defaulting to `false`.

**Fixed**, and the fix splits two failures that were conflated, because collapsing them would trade
one bad outcome for another:

- **port in use** → *environmental*. Log at ERROR with a remediation hint, disable the endpoint,
  keep the monitors running.
- **malformed address** → *operator config error*. Can never succeed, so silently disabling would
  let a `values.yaml` typo produce an agent that looks healthy and serves nothing. Still fails
  loudly, now at **construction**.

Verified live against the exact failing scenario: **5/5 CrashLoopBackOff → 5/5 Running, 0 restarts,
0 panics**, with `"reported node conditions"` present and `NetworkingReady=True`. Four tests, each
with a negative control; full module 34/34 under `-race`.

### 3.2 F-K4-5 — the `:9100`-vs-PNE case does *not* collide, and that is worse

An agent configured on `:9100` alongside a running node-exporter: **5/5 Running, both bound
successfully.**

```
pne   --web.listen-address=[$(HOST_IP)]:9100   <- specific address
agent address="[::]:9100"                      <- all addresses
```

Linux permits both when the specific bind exists first — **but traffic goes to the specific one.**
Probing `:9100` returned a metric set with `node_collector_panics_total` count **0**, an agent-only
family, so **PNE is answering and the agent's endpoint is silently unreachable.**

**Worse than a crash, because nothing reports it:** the agent logs "serving node_exporter compatible
metrics", the DaemonSet is Ready, and the scrape returns 200 with plausible data — from the wrong
process. A migration that left PNE installed would appear to work while never serving the agent's
metrics.

**Unfixed and recommended for the owners:** the agent should either bind a specific address or
detect that it is not the process answering its own port.

### 3.3 F-K3-1 — a latent probe misconfiguration, exposed by fresh nodes

The `nma-nodep` test DaemonSet passed `--probe-address=:8012` while its livenessProbe targeted
`:8002`. Result on Karpenter: **17 restarts** vs 0 for the other two.

**Classification: HARNESS**, not a fork defect — the binary was healthy throughout and said so in
its logs. But the *reason it surfaced* is the point: on the MNG the pod was never restarted after
the mismatch was introduced, and the metrics harness only sampled `restartCount` at moments when it
happened to be 0. **Karpenter launching fresh nodes made every pod start from scratch and hit the
probe immediately.** Karpenter did not cause the defect; it exposed one that was latent.

A liveness probe aimed at a port nothing listens on is **a crash loop with a healthy process inside
it**, and in a real deployment that is indistinguishable from an agent defect.

### 3.4 F-K6-1 — a log-triggered Fatal has no clearing path · **PRE-EXISTING**

An injected `IPAMDNotReady` **did not clear** after the fault was removed — held for 10+ minutes,
well past two monitor periods.

- `IPAMDNotReady` is raised by `handleIPAMDLogs(line)`, a **log-observer** callback
  (`monitors/networking/monitor.go:294-308`).
- Conditions are set `True` **only at startup** — `main.go:273` states outright that *"NodeExporter
  unconditionally sets all provided conditions to ConditionTrue."*
- **No periodic re-check resets it.** Unlike `InterfaceNotUp`, which is re-evaluated every 5 minutes
  against live interface state, a log-triggered reason is **one-way**: once observed it stays False
  for the agent process's lifetime.

Confirmed causally: restarting the agent cleared it **immediately**, where 10+ minutes without the
fault did not.

**A design property rather than an obvious defect** — for a genuine IPAM-D failure, sticky is
arguably right and replacement is the intended remedy. But: **a transient IPAM-D blip permanently
marks the node unhealthy, and Karpenter forcefully replaces it 30 minutes later.** One log line,
even from a self-recovered condition, is enough to destroy a node.

**Identical on all three variants** — stock `main` behaviour, untouched by either fork. The K2 phase
ordering (baseline `main` *first*) is what allows that to be stated with confidence rather than
guessed.

**Raised as a question for the NMA owners:** is a permanently sticky Fatal from a single log line
intended, now that Karpenter will forcefully replace the node?

---

## 4. Corrections to my own claims

Recorded because a report that only lists successes is not a record.

| Claim I made | What was actually true |
|---|---|
| The sustained-fault Job caused the repair | **Wrong.** `BackoffLimitExceeded, failed=1` — its pod ran *on* the node being repaired and died with it. The earlier one-shot injection held the condition. The repair result stands (it rests on the measured duration and Karpenter's log), the attribution did not. |
| "A `nodep` node was terminated where I injected nothing" | **Wrong.** K3's A4 test injected into `items[0]` of *each* pool; those are exactly the three repaired nodes. I had forgotten that injection. |
| The condition "self-clears" intermittently (15 min vs 32 min) | **Wrong mechanism.** It clears only on **agent restart**. In K1 the pod was restarting from F-K3-1, which reset it. Fixing that bug stopped the "self-clearing". |
| H1 would manifest as "agent Pending, node unmonitored" | **Did not occur.** 21 pods *are* port-blocked, but all are node-exporters. Two *different* failures occurred instead (§3.1, §3.2). |

And four harness bugs, all the same family — **a check that reports a clean result precisely when it
did not run**: an embedded-Python `SyntaxError` that wrote an empty conditions file and produced
five false FAILs; `AcceleratedHardwareReady` treated as required when it is legitimately absent
without a GPU; a verdict claiming "all 5 types present" while one was absent; and a termination
watcher whose empty log *looked* broken but was correct — started 5 minutes after the events it was
meant to catch.

> **An empty result from a correct check and from a broken check look identical.** The only way to
> tell them apart is to test the check itself, which is why every harness here has a negative
> control.

---

## 5. Confidence and limits

**Well established:** all three variants publish an identical condition type set, detect an injected
Fatal at identical latency, survive 1,200 pod-churn completions and consolidation without spurious
Fatals or restarts, and drive Karpenter's repair loop to a forced replacement. Every termination
observed is attributable to an injected fault.

**Not established:**

- **Only 5 of 21 Fatal reasons are reachable here.** 16 need accelerated hardware. The Fatal set was
  shown *empty at rest*, not enumerated.
- **`IPAMDNotReady` is the only injected reason.** It is log-scanned; `InterfaceNotUp` (the
  two-period, live-state path) was exercised only *negatively* — by churn failing to trigger it.
- **One churn profile, ~40 minutes, one instance type, one AZ set.** No multi-day soak, no arm64,
  no spot interruption (E3), no drift-driven fleet replacement (E9).
- **`dep`/`nodep` repair durations are estimates**, not measurements (§2.2).
- **E1/E2/E6–E12 not individually driven.** Several were incidentally exercised by the K5 churn but
  are not separately attributable, and are not claimed.
- **Node Auto Repair is alpha** (Karpenter v1.1.0+). Behaviour may change.

**Cost:** 21 nodes (6 MNG + 15 Karpenter). The MNG was deliberately **not** deleted, so the metrics
evidence stays reproducible.

---

## 6. Recommendation

**Neither fork compromises the NMA↔Karpenter integration, and the metrics recommendation in
`node-metrics-experiment.md` §9 stands unchanged.**

Two items belong in whichever fork ships:

1. **F-K4-1's fix is mandatory, not optional.** It is in shared code, it is a blocker, and it is
   already implemented and verified on the dependency branch. **It must be carried to the native
   branch** along with the Q9 handler fixes.
2. **F-K4-5 needs a decision.** The silent-unreachable-endpoint case is the worst failure found here
   precisely because nothing reports it. Recommend binding a specific address, or detecting that
   another process owns the configured port.

And one for the NMA owners, independent of this choice: **F-K6-1**, the sticky log-triggered Fatal,
now has a sharper consequence than it did before Karpenter was in the loop.
