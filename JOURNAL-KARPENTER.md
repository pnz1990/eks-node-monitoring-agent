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
