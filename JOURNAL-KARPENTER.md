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

---

## K4 — H1 is real, but its shape is NOT what the goal file predicted

**Date:** 2026-07-29 · **Status:** complete

### K4.1 B4 — metric parity holds on Karpenter nodes

Scraped both forks on Karpenter-launched nodes, same pass:

```
dep   (:9101)  304 node_* names
nodep (:9102)  304 node_* names
name diff      EMPTY both ways
```

Identical to the MNG result, so **nothing about Karpenter-launched nodes changes the metric
contract.** `promhttp_metric_handler_errors_total{cause="gathering"} = 0` on both, confirming the
Q9 fix holds here too.

### K4.2 B3 — collector failures and condition reporting are independent

```
dep    10 failing collectors   all conditions True
nodep   1 failing collector    all conditions True
```

A 10-vs-1 difference in collector failures with **zero** difference in condition health. That is
B3's claim demonstrated rather than assumed: the metrics path cannot degrade the Karpenter signal.

### K4.3 B1 — both agents schedule on every Karpenter node

`desired=5 ready=5` for both forks. They use `:9101`/`:9102`, so they do not contend for `:9100`.

**21 pods are port-blocked cluster-wide** (`didn't have free ports`), every one a *node-exporter*
competing for 9100 — never an NMA agent. So B1 passes, and the collision is real but currently
lands on the exporters rather than on the agent.

### K4.4 FINDING F-K4-1 — a port conflict PANICS the whole agent

The first H1 attempt deployed an extra agent with `hostPort: 9100` into the `nodep` pool. Result:
**5/5 CrashLoopBackOff**, and the reason is the important part:

```
panic: failed to listen on :9102: listen tcp :9102: bind: address already in use
main.main() /workspace/cmd/eks-node-monitoring-agent/main.go:99
```

Two things here:

1. **The port came from the ConfigMap, not the container port.** I set `containerPort/hostPort:
   9100` but the agent read `address: ":9102"` from the mounted config and collided with the
   *existing* `nma-nodep` agent. My test was wrong — but it produced a real finding anyway.

2. **A bind failure is a `panic` that kills the entire process.** `mgr.Add(metricsServer)` puts the
   metrics listener under the controller-runtime manager, and `utilruntime.Must(run())` at
   `main.go:99` turns any returned error into a panic. So **a metrics-port conflict takes down
   NodeCondition reporting and the Karpenter repair signal with it.**

   This is the *inverse* of the resilience boundary's whole purpose. That boundary was built so a
   collector defect could never take down condition reporting — and it works. But a **startup**
   bind failure on the metrics listener bypasses it entirely and kills the agent.

   **Severity: this is the strongest candidate BLOCKER found so far, and it belongs to BOTH forks**
   (the panic is in `main.go`/`pkg/metrics`, shared). On a customer node where anything already
   holds the configured port, enabling the metrics endpoint would turn a *monitoring* agent into a
   crash loop — and Karpenter would see a node with no health signal at all.

   Mitigating: `metrics.enabled` defaults to **false**, so this cannot fire on a default install.

### K4.5 The `:9100`-vs-PNE case does NOT collide — and the reason matters

Deployed an agent with `address: ":9100"` onto nodes where a node-exporter *was* running.
Result: **5/5 Running, 0 restarts.** Both bound successfully.

Cause, read from PNE's own DaemonSet rather than guessed:

```
pne args: --web.listen-address=[$(HOST_IP)]:9100     <- a SPECIFIC address
agent   : "serving ... metrics" address="[::]:9100"  <- ALL addresses
```

Linux permits both binds when the specific-IP socket is created first. **But traffic goes to the
specific bind:** probing `:9100` on that node returned a metric set with
`node_collector_panics_total` count **0** — an agent-only family — so **PNE is answering and the
agent's endpoint is silently unreachable.**

**This is a worse failure than a crash**, because nothing reports it: the agent logs "serving
node_exporter compatible metrics", the DaemonSet is Ready, and the scrape returns 200 with
plausible data — from the wrong process. A migration that left PNE installed would appear to work
while never actually serving the agent's metrics.

### K4.6 Consequence for the goal file

The goal predicted H1 as "the agent cannot schedule, so the node has no monitoring". Measured, it
is two *different* failures:

| Predicted | Measured |
|---|---|
| agent Pending on `FreePort` | **did not occur** — 21 port-blocked pods are all node-exporters |
| — | **F-K4-1:** bind conflict → `panic` → whole agent crash-loops (both forks) |
| — | **F-K4-5:** `[::]` vs `[HOST_IP]` → both bind, PNE wins traffic, agent silently unreachable |

Neither was anticipated. Both are recorded rather than reshaped to fit the prediction.

### K4.7 F-K4-1 FIXED and verified live

Fixed in `1abd337`, image `fk4-fix` (`sha256:44833c89`), then **verified against the exact scenario
that previously produced 5/5 CrashLoopBackOff**:

```
before fix : 5/5 CrashLoopBackOff, panic: failed to listen on :9102: bind: address already in use
after  fix : 5/5 Running, 0 restarts, 0 panic lines
             ERROR logged with cause AND remediation hint
             "reported node conditions" present; NetworkingReady=True/NetworkingIsReady
```

The last line is the point: **the agent keeps doing its actual job with an unusable metrics port.**

**The fix splits two failures that were conflated**, because collapsing them would trade one bad
outcome for another:

- **port in use** → environmental. Log at ERROR with a hint, disable the endpoint for the process
  lifetime, `return nil`, keep the monitors running.
- **malformed address** (`not-an-address`, port 99999) → operator config error. It can never
  succeed, so silently disabling would let a `values.yaml` typo produce an agent that looks healthy
  and serves nothing. Still fails loudly, now at **construction** via `validateAddress`.

Split by *when* the failure is detected rather than by inspecting error strings at runtime, which
would be brittle across Go versions and platforms.

**Two of my own test-setup errors on the way, both recorded because they cost time:**

1. First verification attempt used `containerPort/hostPort: 9100`, but the agent reads its address
   from the **mounted ConfigMap** — so it collided with the wrong port. The container port is
   irrelevant to what the process binds.
2. Second attempt reused the `nma-nodep` ConfigMap, which contains `implementation: native` — a
   field that exists **only on the native branch**, while `fk4-fix` was built from the dependency
   branch. Result: `panic: parsing monitor config: unknown field "implementation"`. A *different*
   panic that briefly looked like the fix had failed. **Worth noting as a real finding in itself:
   an unknown config field is a hard panic**, which means a config written for one branch crash-loops
   the other.

**Applies to both forks** — the panic was in shared `main.go`/`pkg/metrics` code. The fix is on the
dependency branch and must be carried to native.

---

## K5 (part 1) — THE REPAIR LOOP IS CONFIRMED END TO END

**Date:** 2026-07-29 · **Status:** repair execution confirmed on stock `main`

### K5.1 The headline result

**NMA published a Fatal condition → Karpenter tolerated it for its configured window → Karpenter
forcefully terminated the node and its NodeClaim → a replacement launched.** The full contract, on
a live cluster, measured rather than inferred.

```
condition False/IPAMDNotReady since  22:36:22Z
node + nodeclaim deleted             23:08:24Z
unhealthy duration                   32.0 minutes
Karpenter toleration (NetworkingReady=False)  30 minutes
```

**32.0 minutes against a 30-minute toleration.** Karpenter acted 2 minutes after the node became
eligible, which is exactly the documented behaviour.

Karpenter's own log:

```
"deleted node"     Node=ip-192-168-14-177   NodeClaim=nma-main-8sxrx
"deleted nodeclaim" NodeClaim=nma-main-8sxrx provider-id=aws:///us-west-2d/i-068cdab627ecdccc8
```

And the forceful signature K0.2 predicted, from the pod events:

```
Disrupted: Deleting the pod to accommodate the terminationTime ... The pod was granted 1 seconds
of grace-period of its 30 terminationGracePeriodSeconds. This bypasses the PDB of the pod and the
do-not-disrupt annotation.
```

**Repair bypasses drain**, confirmed. The pool self-healed back to 5 nodes.

This is the first result in this whole effort that tests repair **execution** rather than condition
**publication**, and it required everything K0–K4 established: the feature gate, the ≥5-node pool
sizing, and the sustained-fault insight from F-K1-1.

### K5.2 A CORRECTION TO MY OWN ATTRIBUTION

I initially credited the repair to the sustained-fault Job. **That is wrong, and the Job's own
status shows it:**

```
sustained-ipamd-fault: FailureTarget=True BackoffLimitExceeded ... failed=1
```

The Job's pod was running **on the node being repaired**, so Karpenter's forceful termination killed
it, and with `backoffLimit: 0` the Job failed rather than rescheduling.

**What actually held the condition false** was the **earlier one-shot injection** from A3 (~22:36).
So on this node the condition did *not* self-clear the way F-K1-1 observed — it persisted for 32
minutes unaided.

**Two consequences, both recorded rather than smoothed over:**

1. **The repair result stands.** It rests on the measured 32-minute unhealthy duration and
   Karpenter's own deletion log, neither of which depends on which injector held the condition.
2. **F-K1-1's self-clearing is INTERMITTENT, not deterministic.** In K1 the condition cleared in
   ~15 min; here it held 32. The `ipamd.log` reader evidently re-reports while the line remains the
   newest in the file, and whether it clears depends on subsequent log activity. **So a repair test
   must still use a sustained fault** — but the sustained Job needs
   `backoffLimit > 0` *and* to run somewhere other than the target node, or it dies with the very
   node it is testing. Fixed in the manifest.

### K5.3 A second termination that needs explaining: `nodep` node 66-73

`ip-192-168-66-73` (nodep pool) was **also** deleted at 23:07:49Z, and **I never injected a fault
there**. Candidate causes, in order of likelihood:

- **`h1-collide` / `fk4-verify` fallout.** Both test DaemonSets ran on the nodep pool with
  deliberately broken configuration, and `h1-collide` was in CrashLoopBackOff on that node. A
  crash-looping agent stops publishing conditions, and a **stale** condition can age into
  `Unknown`, which Karpenter treats as unhealthy after 30 min.
- **Consolidation.** `consolidateAfter: 1m` is deliberately aggressive, and the pool churned while
  I added and removed test DaemonSets.

**I cannot currently distinguish these**, because the events that would say so have aged out.
Recorded as **unattributed** rather than guessed, and it is a gap in the harness: `watch-repair.sh`
observes one node, so a termination elsewhere has no recorded reason.

**Action:** K5 part 2 needs a cluster-wide termination watcher that captures the disruption *reason*
for every node as it happens, so "a node went away" is never again a fact without a cause.
Critically, this also means **A5 (no spurious Fatal) is not yet established** — a node was replaced
without a known reason, and until that is attributable I cannot claim no spurious repair occurred.

---

## K5.4 — K5.3's "unattributed termination" RESOLVED, and it is the strongest result so far

**The termination I could not attribute was attributable after all**, and reconstructing it turned a
loose end into the central A5 result.

Karpenter's own `"deleted node"` log lines, all three of them:

```
23:07:49Z  nodep  ip-192-168-66-73    <- K3-A4 injected IPAMDNotReady
23:08:04Z  dep    ip-192-168-32-223   <- K3-A4 injected IPAMDNotReady
23:08:24Z  main   ip-192-168-14-177   <- K3-A4 injected IPAMDNotReady
```

**Three terminations. Three injected faults. One per pool. Zero unexplained.**

K3's A4 latency test injected `IPAMDNotReady` into `items[0]` of *each* pool to compare detection
speed — and those are exactly the three node names Karpenter later repaired. So my earlier
"a nodep node was terminated and I never injected a fault there" was **wrong**: I had injected one,
in the A4 test, and then forgotten it while looking at the sustained-fault Job.

### What this establishes

**A3 / A6 — repair execution, on ALL THREE variants:**

| pool | unhealthy before deletion | outcome |
|---|---|---|
| `main` | **32.0 min** (measured from `lastTransitionTime`) | repaired |
| `dep` | ~21–31 min (injection-time estimate; node gone, so `lastTransitionTime` unavailable) | repaired |
| `nodep` | ~21–31 min (same caveat) | repaired |

The `main` figure is exact. The other two are estimates because the node objects were deleted and
their `lastTransitionTime` went with them — **stated as an estimate rather than dressed up**, and it
is a harness gap: the transition time must be captured *before* the node can disappear.

**A5 — no spurious repair.** Across ~54 minutes, 21 nodes, 1,200 pod-churn completions and three
pools, **every** termination maps to an injected fault. No healthy node was replaced.

**H2 — did NOT fire, on any variant.** After 1,200 churn completions creating and destroying veths
continuously, **zero** nodes on any pool reported `InterfaceNotUp`, `InterfaceNotRunning` or
`MissingLoopbackInterface`:

```
H2 check across all Karpenter nodes: NONE
```

The two-adjacent-period guard (`interfaceHasConsistentIssue`) held under Karpenter-rate churn. This
was the goal file's highest-suspicion hazard and the measured answer is that it does not reproduce —
recorded as a negative result, which is worth as much as a positive one here.

### The watcher's empty log was CORRECT, not broken

`watch-terminations.sh` recorded nothing, which looked like the same "check that did not run"
failure this project keeps producing. It was not: all three terminations happened at **23:07–23:08**
and the watcher started at **23:12:40** — it was simply started too late. Verified by testing the
detection logic against a synthetic diff (it correctly flags a disappearance) and by pulling the
timestamps from Karpenter's log.

**Lesson worth keeping:** an empty result from a *correct* check and an empty result from a *broken*
check look identical, and the only way to tell them apart is to test the check itself. The watcher
is now in place for K5 part 2, started before any fault.

---

## K6 — edge cases. E4/E5 pass, and a design property worth knowing

**Date:** 2026-07-29 · **Status:** E4, E5 complete; the rest carried

### K6.1 E5 PASS — a fresh Karpenter node does not emit a spurious Fatal

This was flagged in the goal file as one of the two highest-value cases: a brand-new node starts
with an **empty interface cache**, and `interfaceHasConsistentIssue` needs a *previous* observation
to compare against. A bug in the `if !ok { continue }` path would mean **a spurious Fatal on every
new node**, i.e. Karpenter replacing healthy nodes in a loop.

Karpenter handed me three brand-new nodes at 23:07 (the K5 replacements), one per pool. After
>20 minutes — past two 5-minute monitor periods, so the cache had been populated and compared:

```
[main]  ip-192-168-57-26   age=20m  NetworkingReady=True/NetworkingIsReady
[dep]   ip-192-168-55-242  age=20m  NetworkingReady=True/NetworkingIsReady
[nodep] ip-192-168-27-187  age=20m  NetworkingReady=False/IPAMDNotReady   <- MY injected fault
```

**No node reported `InterfaceNotUp`, `InterfaceNotRunning` or `MissingLoopbackInterface`.** The
`nodep` node's `IPAMDNotReady` is the `fault-nodep` Job I was running there — verified by listing
the Job's pod and its node — so it is an injected condition, not a spurious one. Checking the
*reason* rather than just "is it False" is what makes that distinction possible.

**E5 passes on all three variants.**

### K6.2 E4 PASS — evict the agent, and its empty cache still produces no spurious Fatal

Deleted the `nma-nodep` pod on a node and waited past two monitor periods:

```
new pod nma-nodep-rjg9m  Running  restarts=0
NetworkingReady=False/IPAMDNotReady   <- still the injected fault, no interface reason
```

**E4 passes:** a rescheduled agent rebuilding its cache from empty does not fabricate an interface
Fatal.

### K6.3 FINDING F-K6-1 — a log-triggered Fatal has NO clearing path

While waiting for the injected condition to clear after deleting the fault Job, it **did not** —
held `False/IPAMDNotReady` for **10+ minutes**, well past the two monitor periods I expected.

Cause, read from the code rather than guessed:

- `IPAMDNotReady` is raised by **`handleIPAMDLogs(line)`** — a **log-observer** callback that fires
  when a matching line is *tailed* from `/var/log/aws-routed-eni/ipamd.log`
  (`monitors/networking/monitor.go:294-308`).
- Conditions are set to `True` **only at startup**: `main.go:273` states plainly that
  *"NodeExporter unconditionally sets all provided conditions to ConditionTrue"*, once, when
  building `conditionConfigs`.
- **There is no periodic re-check that resets `IPAMDNotReady`.** Unlike
  `InterfaceNotUp`/`InterfaceNotRunning`, which are re-evaluated every 5 minutes against live
  interface state, a log-triggered reason is **one-way**: once observed, the condition stays False
  for the lifetime of the agent process.

**This is a design property, not obviously a defect** — for a genuine IPAM-D failure, "it printed an
error once" arguably *should* be sticky, and node replacement is the intended remedy. But the
consequences are worth stating:

1. **A transient IPAM-D blip permanently marks the node unhealthy**, and after 30 minutes Karpenter
   will forcefully replace it. A single log line, even from a condition that self-recovered, is
   sufficient to destroy a node.
2. **It explains F-K1-1's intermittency.** I recorded the condition "self-clearing" in ~15 min in K1
   and holding 32 min in K5, and called that intermittent. The real mechanism is that **the
   condition only clears when the agent process restarts** — in K1 the pod was being restarted by
   the probe-port bug (F-K3-1, 4 restarts each), which reset the condition. Once that was fixed, the
   condition stopped clearing. **My "intermittent self-clearing" was actually a side effect of a
   different bug I had not yet fixed.**
3. **It affects all three variants identically** — this is stock `main` behaviour in
   `monitors/networking`, untouched by either fork. **Classification: PRE-EXISTING**, and the K2
   phase ordering is what allows that to be said with confidence.

**Not filed as a blocker for the forks**, because it is neither caused nor worsened by them. Raised
as an NMA finding for the owners: *is a permanently sticky Fatal from a single log line the intended
behaviour, given Karpenter will now forcefully replace the node 30 minutes later?*
