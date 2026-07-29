# Journal — Karpenter integration validation

Running log for `GOAL-KARPENTER-INTEGRATION.md`. Append-only; findings recorded when found, so a
context loss does not lose them.

---

## K0 — the contract, read from Karpenter's own docs rather than assumed

**Date:** 2026-07-29 · **Status:** complete · **Outcome: the goal file's timing was wrong and is corrected**

### K0.1 The repair policy — NMA's conditions are a *first-class* Karpenter input

Karpenter's `Node Auto Repair` monitors these node status conditions
(`karpenter/docs/concepts/disruption.md`):

**Node Monitoring Agent conditions** — these are *ours*, by name:

| Type | Status | Toleration duration |
|---|---|---|
| `AcceleratedHardwareReady` | False | **10 minutes** |
| `StorageReady` | False | **30 minutes** |
| `NetworkingReady` | False | **30 minutes** |
| `KernelReady` | False | **30 minutes** |
| `ContainerRuntimeReady` | False | **30 minutes** |

**Kubelet conditions:** `Ready=False` → 30 min, `Ready=Unknown` → 30 min.

**This is the single most important fact for this goal.** The five NMA condition types in
`pkg/conditions/conditions.go` are exactly the five Karpenter acts on — the contract is not
incidental, it is named in Karpenter's repair policy. So the §0 claim ("neither fork changes NMA's
observable behaviour toward Karpenter") is testing a *real, documented* integration, not an
inferred one.

### K0.2 Three mechanics that change the test design

**1. Node Auto Repair is ALPHA and OFF by default.** Feature state: Karpenter v1.1.0, alpha,
requires the feature gate **`NodeRepair=true`**. It is not on unless explicitly enabled.

**2. There is a 30-minute toleration before Karpenter acts** (10 min for
`AcceleratedHardwareReady`). **This is a mitigating factor the goal file did not account for:** a
spurious Fatal that *clears within 30 minutes* does **not** cause a node replacement. That
materially lowers the severity of hazard H2 — but does **not** eliminate it, because
`InterfaceNotUp` is emitted when the fault is seen in two adjacent 5-minute periods and would then
*persist* while the condition remains false. A flapping veth could hold the condition false well
past 30 minutes.

**3. Repair bypasses drain.** "Karpenter will **forcefully terminate** the node and its
corresponding NodeClaim, **bypassing the standard drain and grace period procedures**." So a false
Fatal is not a graceful replacement — it is a forced termination.

**4. A 20% safety valve.** Karpenter "will not perform repairs if more than 20% of nodes in a
NodePool are unhealthy". **This has a direct consequence for the test topology:** with 3 NodePools
of 2 nodes each, *one* unhealthy node is 50% of its pool, which is **above** the threshold — so
repair would be **suppressed** and an injected Fatal would produce no replacement. That is not a
pass; it is a test that cannot observe the thing it is testing.

> **Correction to the goal file, §4.3:** each NodePool used for repair-loop testing needs
> **≥5 nodes** so that one unhealthy node is ≤20%. Pools used only for condition-publication tests
> (Tier A1–A4) can stay small.

### K0.3 This cluster: the full loop is NOT currently testable

Measured, not assumed:

```
Karpenter deployment      : ABSENT
Karpenter CRDs            : ABSENT (no nodepools/nodeclaims/ec2nodeclasses)
cluster.computeConfig     : null      -> NOT Auto Mode
nodegroup.nodeRepairConfig: null      -> node auto repair NOT enabled on the MNG
OIDC issuer               : present   -> IRSA will work
addons                    : coredns, kube-proxy, vpc-cni  (no pod-identity-agent)
```

So the goal file's §10 assumption was right to flag this: **the `Fatal → replacement` link is
currently not wired at all.** Everything validated so far measured *condition publication*, never
*repair execution*.

**Consequence, stated plainly rather than glossed:** until Karpenter is installed *with*
`NodeRepair=true`, Tier A3/A5/A6 test whether NMA publishes and clears conditions correctly — which
is the input to repair — but **not** whether Karpenter acts on them. Both are worth testing; they
are different claims and the evidence must say which one it is.

### K0.4 An inconsistency worth noting

The eksctl MNG `nodeRepairConfig` documentation uses **different condition names** than Karpenter's
policy — `NetworkNotReady` and `AcceleratedInstanceNotReady`, with reasons like `InterfaceNotUp`
and `NvidiaXID13Error`. NMA publishes `NetworkingReady` / `AcceleratedHardwareReady`.

Two different repair implementations (MNG node repair vs Karpenter Node Auto Repair) with two
different condition vocabularies consuming the same agent. Recorded because it means **a test
against MNG node repair would not be a test against Karpenter node repair**, and results from one
cannot be claimed for the other.

### K0.5 Decisions taken from K0

1. **Install Karpenter with `NodeRepair=true`** — otherwise the headline claim is untestable.
2. **Repair-loop pools sized ≥5 nodes** (K0.2 item 4).
3. **Deadlines corrected** — see the table below; the goal file's 6/12/18-minute deadlines are for
   *condition appearance*, and a further **30 minutes** is needed for *repair action*.
4. **`IPAMDNotReady` remains the primary probe** — Fatal, one log line, no accelerator, and it maps
   to `NetworkingReady` which carries a 30-minute toleration.

| What is being timed | Deadline | Source |
|---|---|---|
| condition appears | 6 min | `interfaceMonitorPeriod` + margin |
| `InterfaceNotUp` appears | 12 min | 2 adjacent periods |
| `IPAMDNotRunning` appears | 18 min | `ipamdNotRunningConsistencyDuration` 15m |
| **Karpenter terminates the node** | **30 min + node-launch time** | Karpenter toleration duration |
| condition clears after fault removal | 2 periods | `interfaceCacheTTL` |

A single end-to-end repair test therefore takes **~40 minutes**, and that is a floor set by
Karpenter, not by the harness.

---

## K1 — Karpenter installed, NodePools live, Tier-D controls all pass

**Date:** 2026-07-29 · **Status:** complete

### K1.1 What was built

```
Karpenter        v1.14.0, kube-system, 1 replica, 0 errors
feature gate     FEATURE_GATES=...,NodeRepair=true,...   (read from the Deployment env, not assumed)
EC2NodeClass     nma-test        Ready=True (all 7 sub-conditions True)
NodePools        nma-main / nma-dep / nma-nodep   Ready=True
IAM              KarpenterController-nma-pne-parity-test (IRSA) + reused MNG node role
discovery tags   3 public subnets + cluster SG tagged karpenter.sh/discovery
```

Manifests committed at `hack/karpenter/{nodepools,capacity}.yaml`, harness at
`hack/karpenter/inject.sh`. **No existing resource was deleted** — the MNG and its 6 nodes are
untouched, so the metrics evidence stays reproducible.

### K1.2 Five setup defects, each found by a check that failed loudly

Recorded because they are the reproducibility notes someone re-running this will need:

1. **OIDC provider was never registered in IAM.** The trust policy referenced
   `oidc.eks.us-west-2.amazonaws.com/id/81F0AB31…`, but IAM only had a *different cluster's*
   provider (`…F26250E3…`). Karpenter crash-looped with `InvalidIdentityToken`. Created the
   provider with the correct thumbprint.
2. **`iam:ListInstanceProfiles` missing** → instanceprofile GC reconcile errors every few seconds.
3. **`ec2:CreateLaunchTemplate` missing** → `ValidationSucceeded=False`.
4. **`ec2:CreateFleet` blocked by over-scoped conditions** → same. The dry-run auth check cannot
   satisfy resource-scoped conditions, so a widened statement was added *for this test cluster*.
5. **Validation is cached.** After fixing (3) and (4) the nodeclass stayed `False` until the
   controller was restarted. Worth knowing: **an IAM fix is not picked up without a bounce**, and
   a reader could easily conclude the policy was still wrong.

### K1.3 A defect in my own topology, caught before it corrupted every result

Both existing DaemonSets carry `tolerations: [{operator: Exists}]` and no pool selector — so they
scheduled onto **every** pool, and **both run the same `q9-v1` image**. Measured directly:

```
pool=main:  ip-…-14-177  agents=2      <- two agents, same image, on the "baseline" pool
            ip-…-15-166  agents=2
            (all 5 nodes)
```

**That would have made all three pools identical**, and every A1–A7 comparison would have been the
same binary against itself — the exact "comparing the same code against itself" trap the metrics
harness has a positive control for. Fixed by pinning each DaemonSet to its own pool via
`nodeSelector: {nma-variant: …}` plus a matching single toleration, and by building a **genuine
stock-`main` image** (`stock-main`, digest `sha256:7f4c51ca…`, verified present in ECR rather than
trusted from the build exit code).

Stock main needed its own ConfigMap: the shared one contains a `metrics:` block that `main` cannot
parse, and its `hostPort` was dropped since `main` has no metrics endpoint.

### K1.4 Pool sizing works as designed

`podAntiAffinity` on `kubernetes.io/hostname` with 5 replicas **forces 5 nodes** rather than letting
Karpenter bin-pack onto fewer. Confirmed: 5 `NodeClaims`, all `Ready`, in **~90 s**. A CPU request
alone would have produced 2 large nodes and silently left the pool above the 20 % unhealthy
threshold (K0.2).

### K1.5 Tier D — all five controls pass

| ID | Control | Required | Observed |
|---|---|---|---|
| **D1** | inject `IPAMDNotReady` on stock `main` | condition appears | **DETECTED in 16 s** (`NetworkingReady=False/IPAMDNotReady`) |
| **D2** | agent removed, then inject | condition must **not** appear | **not present after 180 s** (11× the D1 latency) |
| **D3** | assert a condition never injected | must not be found | `StorageReady=True/DiskIsReady` |
| **D4** | broken kubeconfig | must **error loudly** | `FAIL: node … not found (or credentials are dead)` |
| **D5** | node with no agent | must report **missing** | `FAIL: no NMA agent Running … nothing would detect the fault` |

**D2 is the one that makes D1 meaningful.** Without it, "the condition appeared" could have been
ambient — something else setting `NetworkingReady`. It did not appear with the agent gone, so
detection is attributable to NMA.

**Observed detection latency 16 s**, far inside the 6-minute deadline. That is expected for a
log-scanned reason: `IPAMDNotReady` does not need the two-adjacent-period agreement that
`InterfaceNotUp` does.

### K1.6 FINDING F-K1-1 — the injected Fatal SELF-CLEARED, so no repair would fire

Not something the goal file anticipated. After D1 detected `NetworkingReady=False/IPAMDNotReady`,
the condition returned to `True/NetworkingIsReady` **on its own**, with no fault removal:

```
after D1  : NetworkingReady=False/IPAMDNotReady   (16 s)
~15 min later: NetworkingReady=True/NetworkingIsReady   on all 5 main-pool nodes
```

**Why this matters for the headline claim.** Karpenter's toleration for `NetworkingReady=False` is
**30 minutes**. A condition that clears well inside that window **never triggers a repair**. So:

- **the single-log-line injection is sufficient to test condition PUBLICATION (Tier A1–A4)**
- **it is NOT sufficient to test repair EXECUTION (A3/A5/A6 end-to-end)** — the fault must be made
  *persistent* so the condition stays false for >30 minutes

This is exactly the trap K0.2 warned about in the other direction: a test could inject, observe
"detected", observe "no replacement", and report the repair loop as safe — when in fact the
condition had cleared and Karpenter was never given anything to act on.

**Action for K2:** the repair-path test needs a **sustained** fault — a looping writer that appends
the line every monitor period — and must assert the condition is *continuously* false for >30 min
before claiming anything about replacement. Recorded as a required harness change rather than a
finding about either fork.

---

## K3 — three-way comparison on Karpenter nodes: A1–A4 pass, one real defect found

**Date:** 2026-07-29 · **Status:** A1–A4 complete; A5–A7 pending churn (K5)

### K3.1 Topology actually verified, not assumed

15 Karpenter nodes, 5 per pool, **exactly one agent per node and the correct variant per pool**:

```
pool=main   5 nodes -> nma-main-agent            (stock main, sha256:7f4c51ca)
pool=dep    5 nodes -> eks-node-monitoring-agent (dependency fork, q9-v1)
pool=nodep  5 nodes -> nma-nodep                 (native fork,     q9-v1)
```

### K3.2 FINDING F-K3-1 — 17 restarts on the native fork. **My deployment, not the code.**

The first K3 run failed on `nodep` with **17 agent restarts** while `main` and `dep` had **0**.
Uniform across all 5 pods (4 each), `exit=143` (SIGTERM), so systematic rather than random.

Cause, from the kubelet event rather than guessed:

```
Liveness probe failed: Get "http://…:8002/healthz": connect: connection refused
Killing: Container nma-nodep failed liveness probe, will be restarted
```

And from the agent's own log — it was starting **fine**:

```
"starting server" name="health probe" addr="[::]:8012"
```

**The DaemonSet passes `--probe-address=:8012` but its livenessProbe targets `:8002`.** A
pre-existing misconfiguration in *my* `nma-nodep` test DaemonSet, carried from the three-way
metrics work.

**Why it never surfaced before:** on the MNG the pod was never restarted after the mismatch was
introduced, and the metrics harness only ever checked `restartCount` at a moment when it happened
to be 0. Karpenter launching **fresh nodes** made every pod start from scratch and immediately hit
the probe. So Karpenter did not cause the defect — it **exposed** one that was latent.

Fixed by pointing the probe at `:8012`. **17 restarts → 0**, verified.

**Classification: HARNESS, not a fork defect.** It would be wrong to record this as "the native
fork is unstable under Karpenter" — the binary was healthy the whole time and said so in its logs.
But it is worth keeping visible, because a liveness probe aimed at a port nothing listens on is a
**crash loop with a healthy process inside it**, and on a real deployment that would look exactly
like an agent defect.

### K3.3 A1–A4 results

**A1 — condition TYPE sets identical.** 8 distinct types on every variant, all three pairs
`IDENTICAL`:

```
ContainerRuntimeReady  DiskPressure  KernelReady  MemoryPressure
NetworkingReady        PIDPressure   Ready        StorageReady
```

`AcceleratedHardwareReady` absent on all three (no accelerator), and **absent consistently** —
which is the A1 claim, not a gap.

**A2 — Fatal reason set.** No variant emitted any Fatal reason on a healthy node, so the observed
Fatal set is empty and identical across all three. *Stated precisely: this confirms no variant
emits a SPURIOUS Fatal at rest; it does not enumerate all 21 Fatal reasons, 16 of which need
accelerated hardware.*

**A3 — injected Fatal appears on all three.** `IPAMDNotReady` detected on every variant.

**A4 — latency within 1 monitor period.** Identical to the second:

```
main   DETECTED after 15s
dep    DETECTED after 15s
nodep  DETECTED after 15s
```

Deadline was 360 s (`interfaceMonitorPeriod` + margin). **Neither fork delays condition
reporting** — the hazard H3 concern (a *delayed* condition rather than an absent one) is not
observed at rest. Churn is still to come in K5.

**A6 (partial) — Karpenter's view.** All 15 `NodeClaims`
`Launched/Registered/Initialized/Consolidatable/Ready=True`. Karpenter successfully launched,
registered and initialised nodes with all three variants installed.

**A7 — 0 orphaned `NodeDiagnostic`s** across 21 live nodes on every run.

### K3.4 What K3 does NOT establish

- **Repair execution.** Every condition here cleared on its own well inside Karpenter's
  30-minute toleration (F-K1-1), so no repair was ever triggered. A5/A6-full need the sustained
  injector.
- **Behaviour under churn.** These are at-rest measurements. Consolidation churn is K5, and that is
  where hazard H2 (veth churn vs Fatal `InterfaceNotUp`) actually lives.
- **The Fatal reason set is not enumerated** — only shown to be empty at rest on all three.
