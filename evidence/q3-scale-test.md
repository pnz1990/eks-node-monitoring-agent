# Q3 — scale test at 6 nodes / ~2,900 pods

**Date:** 2026-07-29 · **Cluster:** `nma-pne-parity-test`, us-west-2, EKS 1.36
**Nodes:** 6 × t3.large, kernel 6.18.38, Amazon Linux 2023
**Variants, all co-resident on every node:** pne v1.12.1 (:9100), nma-dep (:9101), nma-nodep (:9102)
**Both agents built from `d790288`** (includes the Q9 handler fix)

Q3 asked whether the native branch holds at the scale the dependency branch was previously
measured at (~2,888 pods). It does. **It also produced the first field reproduction of upstream
#1915**, which the earlier run at comparable scale explicitly failed to reproduce.

---

## 1. What was run

`hack/pressure/01-pod-churn.yaml` (Job, parallelism 30, 3,000 completions),
`03-mount-churn.yaml` (4 emptyDir volumes per pod), `02-cpu-saturation.yaml` (DaemonSet, all
cores, all 6 nodes). Sampled every ~2 min for ~25 min from a single pod scraping all three
endpoints in one pass, so the three are always simultaneous.

**Reached:** peak **2,938 concurrent pods**, **3,000 pod-churn completions** (Job ran to
completion), **16 valid samples × 3 variants** — 12 under pressure (315–2,938 pods) and 4 after
the churn ended (46 pods), which gives a recovery baseline as well as a loaded one.

**A precondition that had to be fixed first.** The pressure manifests select
`nodeSelector: pressure=true`. The 4 newly-scaled nodes had no such label, so every churn pod
sat `Unschedulable`. `kubectl apply` still succeeded, and the cluster pod count still *rose*
(the cpu-saturation DaemonSet tolerates everything), so the first run reported a full
"under pressure" column that was really a second baseline — and passed. Fixed in `3536fca`:
the label is ensured up front and the churn Jobs are now verified via their own
`.status.active`/`.status.succeeded` rather than via cluster pod count.

---

## 2. Result

All 16 samples, split by phase:

```
UNDER PRESSURE (12 samples, 315-2,938 pods)
  variant   wall med  wall max       series    failed  restarts
  pne         0.4077    1.3611     640-1226  [10, 11]         8
  nma-dep     0.3820    1.7837     618-1123      [10]         0
  nma-nodep   0.2903    2.1309     598-1084       [1]         0

AT REST (4 samples, 46 pods, after the churn ended)
  variant   wall med  wall max       series    failed  restarts
  pne         0.0151    0.0186      565-606      [10]         9
  nma-dep     0.0149    0.0153      547-584      [10]         0
  nma-nodep   0.0128    0.2491      527-564       [1]         0
```

**Both agents: 0 panics, 0 timeouts, 0 restarts, and a constant collector-failure count
throughout.** The native branch holds at this scale, which is what Q3 asked.

Scrape wall times are comparable across all three (pressure medians 0.29–0.41s) and rise ~25×
from their ~0.013–0.015s at-rest figures — expected, since series counts scale with pod count
(565 → 1,226).

**The 4 post-pressure samples are the useful addition.** Series counts and wall times return to
their pre-pressure values on all three variants (pne 565–606 series / 0.0151s median; both
agents likewise), so the degradation under churn is **transient load, not a leak or a wedged
collector**. Without these samples the loaded numbers would have been consistent with either.

pne's restart counter reads 9 in this phase, but the 9th restart occurred at `15:05:34Z` —
*during* pressure — and the pod has been up continuously since `15:10:43Z`. pne recovers once
the churn stops; it does not keep restarting at rest.

---

## 3. The finding: upstream #1915 reproduced in the field

**pne's `netclass` collector failed under churn. Both agents' did not, on the same node in the
same scrape pass.**

```
node_scrape_collector_success{collector="netclass"}
  s    pne  dep  nodep   pods
  1     1    1     1      315
  2     1    1     1      736
  3     1    1     1     1167
  4     1    1     1     1569
  5     1    1     1     2033
  6     0    1     1     2434   <-- pne netclass FAILED
  7     1    1     1     2802
  8     1    1     1     2146
  9     1    1     1     2553
 10     1    1     1     2938
 11     1    1     1     2843
 12     1    1     1     1072
 13-16  1    1     1       46   (post-pressure)
```

**Rate: 1 of 12 pressure samples**, and 0 of 4 at rest. Not correlated with peak pod count —
it fired at 2,434 pods and did *not* fire at 2,938. That is consistent with the mechanism being
a race against interface teardown rather than a function of load level: what matters is whether
an interface disappears inside the listing→reading window, which is a matter of timing, not of
how many pods exist.

pne's failing set went from the 10 absent-hardware collectors (`bcachefs`, `bonding`,
`fibrechannel`, `hwmon`, `ipvs`, `nfs`, `nfsd`, `rapl`, `tapestats`, `zfs`) to **11**, the
addition being `netclass`. That is upstream #1915/#1841 exactly: the collector enumerates
network devices then reads each one, returning on the first read error, so a veth interface
disappearing between listing and reading suppresses metrics for **every** interface.

`netdev` was unaffected in the same samples, which corroborates rather than contradicts: its
default backend is netlink, so it never does the per-device sysfs reads that #1915 is about.

**Why this matters more than the code reading did.** The dependency branch's design doc argued
#1915 is "reachable, routinely" on EKS from first principles. The previous 2,888-pod run did
**not** reproduce it, and that was recorded plainly as *"PR1 — did not reproduce"*. It now has.
The prediction was right and the earlier negative result was a matter of churn rate, not of the
argument being wrong.

**Both agents avoid it, for different reasons, and the distinction is the whole design
question:**
- `nma-nodep` **fixes it at the cause** — `netclass.go` skips an unreadable device and reports
  the rest.
- `nma-dep` **contains it** — the resilience boundary stops it becoming worse, but the
  collector would still return nothing. That it did *not* fail here is not proof of a fix.

---

## 4. Second finding: pne restarted 8 times; neither agent restarted

pne hit **8 restarts** across the run. Cause established rather than assumed:

```
Warning  Unhealthy  Liveness probe failed: Get "http://…:9100/": context deadline exceeded
Normal   Killing    Container node-exporter failed liveness probe, will be restarted
Last state: terminated, reason=Error, exit=143   (SIGTERM — the kubelet, not the OOM killer)
```

`MemoryPressure=False` on the node throughout, so this is **not** an OOM. pne became too slow
to answer its own 1s liveness probe under churn, three times consecutively, and was killed.

### The mechanism, measured

pne's `node_filesystem_readonly` series count **tracks pod churn**, while both agents stay
pinned at 4:

| sample | pne | nma-dep | nma-nodep |
|---|---|---|---|
| 1 | 64 | 4 | 4 |
| 2 | 23 | 4 | 4 |
| 3 | 49 | 4 | 4 |

That is the EKS pod-mount exclusion. pne enumerates every per-pod mount
(`volume-subpaths/…`, containerd sandbox shm, one per pod with a unique UID); the agents report
only real filesystems (`/ /boot/efi /run /tmp`).

And it shows up directly in latency. Same node, same instant, on the affected node:

```
pne       :9100   0.422s     <- liveness timeout is 1s, failureThreshold 3
nma-dep   :9101   0.089s     4.8x faster
nma-nodep :9102   0.093s     4.5x faster
```

**So the exclusion is not only cardinality hygiene — it is availability.** Unbounded mount
cardinality made pne's own endpoint slow enough to fail its health check.

### The qualification, stated because it changes how much credit the agents get

The agents' liveness probe targets **`/healthz` on port 8002**, a separate endpoint; pne's
targets **`/` on the metrics port itself**. So part of the agents' immunity is *probe design* —
their health check does not share a path with metric collection — and not solely a faster
endpoint.

Both effects are real and they are separable:
- the **4.7× endpoint speed-up** under churn is measured on the metrics ports directly, and is
  attributable to the mount exclusion
- the **0-vs-8 restart difference** is that speed-up **plus** the probe not sharing the metrics
  path

Claiming the restart difference purely for the collectors would overstate it.

---

## 5. Limits of this run

- **Single ~38-minute window, 16 samples** (12 loaded, 4 at rest). `netclass` failed in **1 of
  12** pressure samples. That establishes the failure is reachable and gives a crude rate for
  *this* churn profile; it does not establish a rate in general, and n=1 failure cannot
  distinguish "rare" from "unlucky sampling".
- **The agents were not observed failing `netclass`, which is not proof they cannot.** For
  `nma-dep` the fix is containment, not repair, so absence here is evidence and not a guarantee.
- **Values still not compared** across variants — structural and success-value agreement only
  (see §7 of the design doc). Q9's 3,182 silent gather errors lived in exactly that blind spot.
- **Wall time is measured client-side** from a pod on the same node, so it includes kubelet
  network path. That is the honest cost for a Prometheus scrape but it is not pure collection
  time.
- 4 of 6 nodes ran all three variants; a stray `grafana-prometheus-node-exporter` release
  holds `hostPort 9100` on the other 2, so pne is `Pending` there. Sampling used a node
  verified to run all three.
