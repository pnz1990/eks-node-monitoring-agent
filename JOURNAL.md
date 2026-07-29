# Journal — NMA node_exporter parity, production hardening

Append-only. Newest entries at the bottom. **Read this first on any resumption** — it is the durable
memory for this effort and outlives any single context window.

Entry format: timestamp, phase, status, what I did, what I observed (real values), conclusion, next.

Rules: never rewrite history — a wrong conclusion gets a *new* correcting entry. Record refutations
and measurement mistakes as prominently as successes.

---

## [2026-07-28T21:15Z] Baseline: GOAL.md complete

**Phase:** setup
**Status:** confirmed

**What I did:** Completed the original `GOAL.md`. 14 commits on
`pnz1990/eks-node-monitoring-agent:feat/prometheus-node-exporter-parity`.

**What I observed:**
- `pkg/metrics` coverage **100.0%** of statements, `.covignore` unchanged (0 lines diff)
- Full suite: 34 packages ok, 0 FAIL; `helm lint` 0 failed; zero codegen drift
- Parity vs upstream `v1.12.1` on EKS 1.36: **305 shared node metric names, zero asymmetry**
- V2 invariants: 18 series, 0 mismatches
- V3 counter rates: **99.26%** within 5%, median relative error **0.00000%**, p95 1.03%
- 49 enabled collectors on each endpoint, **identical sets**
- e2e on live cluster: 3/3 metrics features pass
- Dashboard trust verdict demonstrated **RED** (V1=51 missing, V3=25%) then **GREEN**

**Bugs found by live testing during GOAL.md (4):**
1. Chart mount condition gated only on `nodeAgent.monitors` — ConfigMap rendered but never mounted, so
   a metrics-only config silently left the endpoint disabled.
2. Dashboard V1 query used `unless` without `on(__name__)` — reported 0 missing while 51 were absent.
   Would have been a green light incapable of turning red.
3. My own V3 measurement: first run reported 41% within tolerance. Cause was a partially-filled
   `rate()[5m]` window (8 of 20 samples) after only 5 min of runtime, not a fork defect. **Methodology
   error, recorded because it will recur.**
4. My e2e assertion required zero collector failures. Upstream PNE fails the same 10 on an EKS node
   (absent hardware). Corrected to assert *parity of failures*.

**Conclusion:** Parity is established and evidenced. Production-readiness is not.

**Next:** Triage upstream's open bugs; the fork inherits them all.

---

## [2026-07-28T21:40Z] Upstream issue triage — real counts and applicability

**Phase:** triage
**Status:** confirmed

**What I did:**
```bash
gh api -X GET search/issues -f q='repo:prometheus/node_exporter is:issue is:open' --jq '.total_count'
gh api -X GET search/issues -f q='repo:prometheus/node_exporter is:issue is:open label:bug' --jq '.total_count'
gh issue list --repo prometheus/node_exporter --state open --label bug --limit 20
```
Then cross-referenced each bug's collector against `node_scrape_collector_success` from our live NMA
scrape (`/tmp/nma.prom`) to determine which are actually enabled for us.

**What I observed:**
- **209** open issues, **12** labelled `bug` — matches the user's figures exactly.
- Triage against our 49 enabled collectors:

| Issue | Collector | Enabled | EKS relevance |
|---|---|---|---|
| #1841 | netclass/bonding | YES | **HIGH** — scrape timeouts; reporter on Kubernetes filtering `veth.+`/`cali.+` |
| #1915 | netclass | YES | **HIGH** — fails reading ignored devices |
| #1710 | cpufreq | YES | **MED** — `ParseUint "<unknown>"`; cpufreq is our slowest collector |
| #1672 | filesystem | YES | **MED** — free/avail > size (impossible values) |
| #2514 | filesystem | YES | LOW — needs NFS |
| #3500 | mdadm | YES | LOW — needs software RAID |
| #2799 | nfsd | YES | LOW — kernel 6.6-rc1; we run **6.18**, check for further regression |
| #1498 | filesystem | YES | N/A — ZFS absent on EKS AMIs |
| #2906 | cpu | YES | N/A — Darwin/M3 |
| #2217 | — | n/a | N/A — macOS code signing |
| #1844 | wifi | no | N/A — default-disabled |
| #1007 | supervisord | no | N/A default-disabled, but it is a **panic** — opt-in path needs guarding |

- **6 of 12 touch collectors we ship enabled; 4 are HIGH/MED.**
- #1841's reporter config is telling: they filter `^(lo|docker[0-9]|kube-ipvs0|dummy0|veth.+|br\-.+|cali\w{11}|tunl0|tun\-.+)$`.
  **Our defaults filter nothing** (`netclassIgnoredDevices` defaults to `^$`).
- Mitigation flags already exist upstream: `--collector.netclass.ignored-devices`,
  `--collector.netclass.ignore-invalid-speed` (`collector/netclass_linux.go:33-34`).
- #3500 was actually fixed in the **procfs dependency** (`prometheus/procfs#786`), not node_exporter —
  so the procfs tracker must be swept too.

**Baseline scrape durations on our live nodes (NMA):**
```
cpufreq     0.0176s
vmstat      0.0089s
netclass    0.0067s
filesystem  0.0058s
sockstat    0.0023s
```
Cluster state: **2 nodes, 20 pods, idle.**

**Conclusion:** Baseline latency is trivial (17ms worst) but proves nothing — #1841/#1915 are
churn-driven and this cluster has almost no churn. The measurement environment is the limiting factor,
not the code.

**Two structural insights that shape the plan:**
1. The highest-value work is **not** fixing individual collector bugs. It is making collector failure
   *non-fatal*: in upstream, a panicking collector kills a metrics process; in our fork it would kill
   the agent that reports `NodeCondition`s feeding EKS node auto repair. Guarding that boundary makes
   every one of these 12 bugs survivable, and is a defensible "better than PNE" claim.
2. Several bugs are likely non-reproducible on our config. Refuting them with evidence is as valuable
   as fixing them, and cheaper — but must be *recorded* so it is not re-investigated.

**Next:** P0 — snapshot the 209 issues to `evidence/upstream-issues/` for a reproducible triage point,
then P1 (resilience R1–R5) before individual bug fixes.

---
## [2026-07-28T22:05Z] P0 complete — issue snapshot + fatal-failure-mode sweep

**Phase:** triage
**Status:** confirmed

**What I did:**
```bash
gh issue list --repo prometheus/node_exporter --state open --limit 300 --json ... \
  > evidence/upstream-issues/open-issues-2026-07-28.tsv     # 209 rows
gh issue list --repo prometheus/node_exporter --state open --label bug ... \
  > evidence/upstream-issues/open-bugs-2026-07-28.tsv       # 12 rows
grep -iE "panic|deadlock|hang|leak|timeout|OOM|race|crash|stuck" open-issues-*.tsv
```

**What I observed** — the `bug` label undercounts fatal failure modes. Beyond the 12:

| Issue | Label | Failure mode |
|---|---|---|
| #1007 | bug | **Panic + crash** when supervisord absent |
| #3346 | *(none)* | **SIGSEGV** `fatal error: unexpected signal during runtime execution`, linux/amd64, 2025-06 |
| #1987 | *(none)* | **Crash** with textfile collector enabled |
| #1353 | *(none)* | **Stuck NFS mount** — the indefinite-hang class |
| #1841 | bug | Scrape **timeouts** from netclass/bonding |
| #2585 | *(none)* | RFE: systemd collector needs a **timeout** (i.e. it has none) |
| #3649 | *(none)* | RFE: systemd collector needs **DBus timeouts** |

**Conclusion:** This substantially strengthens the P1-before-P2 ordering. There are at least **three
distinct crash/segfault reports** and **three distinct hang/timeout reports** in the open tracker, only
two of which carry the `bug` label. Upstream tolerates this because a crashed node_exporter loses
metrics; in our fork the same crash takes down `NodeCondition` reporting that feeds EKS node auto
repair.

Two of these (#2585, #3649) are *feature requests asking upstream for timeouts that still do not
exist* — meaning upstream has no per-collector timeout mechanism at all. Anything we build there is a
genuine improvement over PNE, not a catch-up.

**Next:** P1 — implement and test R1 (panic containment) and R2 (per-collector timeout). Both must be
demonstrated to fail without the guard.

---
## [2026-07-28T22:25Z] R1 root cause proven — panic in collector goroutine is unrecoverable

**Phase:** pressure (P1)
**Status:** confirmed

**What I did:** Inspected upstream's collection path, then built two minimal reproductions to
distinguish which panics are contained and which are fatal.

```bash
grep -n "recover()" node_exporter/collector/collector.go   # -> NO MATCHES
sed -n '145,178p' node_exporter/collector/collector.go     # Collect() + execute()
```

**What I observed:**

Upstream `NodeCollector.Collect` (`collector/collector.go:145-157`) fans every collector out to its own
goroutine and calls `execute()`, which invokes `c.Update(ch)` bare:

```go
for name, c := range n.Collectors {
    go func(name string, c Collector) {      // collector.go:150
        execute(name, c, ch, n.logger)
        wg.Done()
    }(name, c)
}
```
There is **no `recover()` anywhere in the file**, and no timeout.

Two reproductions, same panic, opposite outcomes:

| Panic location | Result |
|---|---|
| Inside `Collect()` on the calling goroutine | **Contained.** `promhttp` recovers → HTTP 500, process survives. Verified: `REACHED: process survived, status = 500` |
| Inside a goroutine spawned by `Collect()` (**what upstream does**) | **FATAL.** Process dies. Verified: `panic: collector exploded in its own goroutine` + stack, non-zero exit |

**Correction to my own earlier assumption:** I had assumed any collector panic would crash the process.
That is wrong — `promhttp` does recover panics on the request goroutine. The fatal case is specifically
the *spawned* goroutine, which is precisely the path upstream uses for every collector. So the risk is
real but narrower and better understood than I first stated.

**Conclusion:** R1 is justified by a reproducible fact. Because upstream fans out to goroutines with no
recover, a single panicking collector (#1007 supervisord, #3346 SIGSEGV, #1987 textfile) terminates the
*entire agent process* — taking `NodeCondition` reporting and EKS node auto repair signalling with it.
`promhttp`'s recovery does not help because the HTTP handler is not on the panicking stack.

Implication for the design: the guard cannot live in the HTTP layer. It must wrap each collector's
`Update` call so the `recover()` is on the same goroutine as the panic.

**Next:** implement `pkg/metrics` collector wrapper providing R1 (per-collector recover) and R2
(per-collector timeout), with tests that fail without the guard.

---
## [2026-07-28T23:20Z] P1 complete — R1/R2 implemented; race detector found a real bug

**Phase:** P1 resilience
**Status:** fixed

**What I did:** Implemented `pkg/metrics/resilience.go` — a `resilientCollector` that replaces
upstream's `NodeCollector`, providing per-collector panic recovery (R1) and a per-collector timeout
(R2, default 5s). Added `resilience_test.go`. Ran the race detector for the first time.

**What I observed:**

*Three bugs found in my own implementation while building it:*

1. **Send on closed channel would panic unrecoverably.** An abandoned (timed-out) collector may keep
   emitting after `Collect` returns, by which point the registry has closed `ch`. That send would panic
   on the abandoned goroutine where nothing can recover — the timeout guard would have become a *new*
   crash source. Fixed with a relay channel plus a `drain` goroutine.
2. **Healthy metrics were silently dropped.** My first version closed `stopForwarding` on the success
   path, but `Update` sends to `done` *before* the deferred `close(relay)` runs, so metrics still in
   flight were cut off. Caught by `TestPanicIsContained` asserting the sibling collector's metric
   survived. Fixed: success path waits on `<-forwarded` instead.
3. **Defer ordering left the forwarder hung on panic.** The `return` inside the recover skipped
   `close(relay)`. Fixed by registering `defer close(relay)` *first* so it runs last on unwind.

*Race detector — a real production data race, never previously run:*
```
WARNING: DATA RACE
Write at 0x... by goroutine 49: (*Server).Start()   server.go:250
Previous read at 0x... by goroutine 50: (*Server).Address()  server.go:233
```
`Start()` writes `s.listener` while `Address()` reads it, unsynchronized. Any caller polling
`Address()` during startup hits this. Fixed with a `sync.RWMutex`. **`go test -race` now clean.**

*Verification after the changes:*
| Check | Result |
|---|---|
| `pkg/metrics` coverage | **100.0%** of statements |
| `go test -race ./pkg/metrics/...` | **clean** |
| Full suite | 34 packages ok, 0 FAIL |
| **Parity vs upstream** | **298 names / 298 series shapes, empty diff** |

**Conclusion:** R1 and R2 are in place and the resilience layer does **not** change the metric output —
parity is byte-identical after the change, which was the main risk of wrapping the collector path.

The three self-inflicted bugs are worth noting as a pattern: every one was in the *concurrency* of the
guard, not its logic. A panic/timeout guard that mishandles channel lifetime is more dangerous than no
guard, because it converts a contained failure into an unrecoverable one. The tests that caught them
(sibling-metric survival, blocked-send detach) are the ones worth keeping green.

**Not yet done for P1:** R3 (NodeCondition isolation under a wedged collector) needs a live cluster
test, R4 (liveness independence) and R5 (cardinality/OOM bound) still open.

**Next:** R3/R4/R5, then P2 upstream bug triage — starting with #1841/#1915 netclass, since our
defaults filter no interfaces at all (`netclassIgnoredDevices` default `^$`).

---
## [2026-07-28T23:45Z] P2: #1915/#1841 netclass — root cause found and REPRODUCED

**Phase:** P2 triage
**Status:** confirmed

**What I did:** Read `node_exporter/collector/netclass_linux.go`, then built a fixture-based
reproduction under `/tmp/netclassrepro` using the real `procfs/sysfs` package.

**Root cause** — `getNetClassInfo`, `collector/netclass_linux.go`:
```go
for _, device := range netDevices {
    if c.ignoredDevicesPattern.MatchString(device) { continue }
    interfaceClass, err := c.fs.NetClassByIface(device)
    if err != nil {
        return netClass, err     // <-- ONE bad device aborts the WHOLE collector
    }
    netClass[device] = *interfaceClass
}
```
The device list is enumerated first, then each device is read. A device present at listing time but
gone at read time returns an error, and the collector returns **nothing at all** — not partial data.

**Reproduction output (deterministic, not timing-dependent):**
```
devices listed: [eth0 veth1234 veth5678]
simulated churn: veth5678 removed after listing
UPSTREAM: abort on device "veth5678": open sysroot/class/net/veth5678: no such file or directory
RESULT: 0 of 3 devices reported -> netclass emits NOTHING
```

**Why this matters far more on EKS than on a static host:** veth interfaces are created and destroyed
on every pod schedule/teardown. The listing→read window is microseconds, but with hundreds of pods
churning, hitting it is routine rather than rare. On a static bare-metal host, interfaces essentially
never disappear, which is why upstream has tolerated this since 2020.

This also explains #1841 (netclass → scrape timeouts): the same loop does a full sysfs read per device
with no bound, so latency scales with interface count, which on Kubernetes scales with pod count.

**Our exposure is worse than the reporters':** `netclassIgnoredDevices` defaults to `^$` (matches
nothing), so we read **every** veth. The #1841 reporter had explicitly configured
`^(lo|docker[0-9]|veth.+|cali\w{11}|br\-.+|tunl0)$` and *still* hit timeouts.

**Disposition:** two-part, no forking of collector internals.
1. **Mitigate by configuration** — ship an EKS-appropriate default `ignored-devices` regexp covering
   veth/cni/br/docker interfaces. These are pod-side interface halves, not node networking; nobody
   dashboards them, and excluding them removes both the churn race and most of the latency.
2. **Contained by R2 already** — the per-collector timeout means even an unmitigated netclass stall
   degrades one collector instead of hanging the scrape. This is why P1 was sequenced first.

Note: the fix for the *underlying* all-or-nothing behaviour belongs upstream (skip the failed device,
keep the rest). Worth filing, but not something to carry as a fork patch.

**Next:** implement the safe default and prove it (a) removes the churn failure and (b) does not remove
any metric a real dashboard uses.

---
## [2026-07-29T00:15Z] EKS collector defaults — parity regression found, scope narrowed

**Phase:** P2 triage
**Status:** fixed (scope narrowed after measurement)

**What I did:** Implemented `eksCollectorDefaults` excluding pod-side interfaces from netclass/netdev,
then ran the parity harness. Then probed a real EKS node to check the assumption.

**What I observed — the defaults BROKE parity:**
```
$ hack/parity-test.sh --upstream ... --agent ...
-TYPE node_network_speed_bytes gauge      # lost, 1 metric name
```
Exactly one metric name lost, zero gained.

First hypothesis was `--collector.netclass.ignore-invalid-speed`. Removing it **did not fix it**, so the
hypothesis was wrong. The real cause is the interface exclusion itself.

Dev host (`/sys/class/net`):
```
br-4bf631fc931d speed=10000    excluded
docker0         speed=-1       excluded
veth*           speed=10000    excluded  (x3)
eth0            speed=unreadable  KEPT
lo              speed=unreadable  excluded
```
Only `eth0` survives, and its speed is unreadable → the family disappears.

**Then I checked a real EKS node rather than trusting the dev box:**
```
enib38dc4367fe speed=10000     <- excluded by my regex (eni pod-side half)
enid027c7c99a9 speed=10000     <- excluded
ens5           speed=unreadable   KEPT (primary ENI)
ens6           speed=unreadable   KEPT
lo             speed=unreadable   excluded
```

**Conclusion:** the regression is real on EKS too, not a dev-host artifact. On an EKS node the *only*
interfaces that report a link speed are the `eni*` pod-side halves; the primary `ens*` interfaces do
not expose a readable speed. So excluding pod interfaces removes `node_network_speed_bytes` entirely.

**Correction to my own earlier claim:** I wrote in `collectors.go` that these exclusions "do not change a
metric a dashboard would use." That was wrong, and measurement caught it. `node_network_speed_bytes`
is a real metric that disappears.

**Disposition — narrow the default rather than abandon it:**
- The churn hazard (#1915) comes from *device count and volatility*, which `veth*`/`eni*` drive.
- But excluding them from **netdev** is unnecessary for that hazard: netdev reads `/proc/net/dev`, a
  single file, so it has no listing→read race and no per-device cost.
- Keep the exclusion for **netclass only** (where the per-device sysfs read is the actual problem), and
  do not exclude for netdev. That preserves netdev's device label space.
- `node_network_speed_bytes` comes from **netclass**, so it is still affected. Trade-off must be stated
  explicitly rather than hidden: on EKS this metric only ever describes pod-side interfaces, whose link
  speed is not a meaningful node signal. Document it as an intentional deviation in
  `docs/parity-exceptions.md` and leave an override path.

**Next:** narrow to netclass-only, re-run parity, document the deviation honestly, then continue triage
with #1710 cpufreq.

---
## [2026-07-29T00:40Z] Decision: no EKS collector defaults — parity beats redundant mitigation

**Phase:** P2 triage
**Status:** fixed (decided)

**What I did:** Measured both candidate mitigations for #1915/#1841 against the parity harness.

**What I observed:**

| Option | Parity result |
|---|---|
| `--collector.netclass.ignored-devices=<pod ifaces>` | **297** names vs upstream 298 — loses `node_network_speed_bytes` |
| `--collector.netclass.netlink` | **299** names vs upstream 298 — adds `node_network_altnames_info` |
| No defaults | **298 / 298, empty diff** |

Verified on a live EKS node why the exclusion costs a metric: the only interfaces exposing a readable
link speed are the pod-side `eni*` halves (`speed=10000`); the primary `ens5`/`ens6` report an
unreadable speed. So excluding pod interfaces removes the family entirely.

**Conclusion — ship no defaults.** The reasoning that decided it:

R2 (per-collector timeout, already implemented) *already contains* the failure these defaults would
prevent. A netclass stall now degrades one collector for one scrape instead of hanging the endpoint. So
the defaults would buy redundant protection at the cost of a real, measurable parity regression — a bad
trade, given parity is the whole premise of replacing PNE.

Both options are documented with copyable flags for operators on churn-heavy clusters, where the
trade may be worth making locally.

**Two invariant tests added** so this decision cannot be undone silently:
- `TestApplyEKSDefaultsPreservesOperatorPrecedence` — defaults must precede operator flags, since
  kingpin is last-wins. If the order flips, user configuration is silently ignored.
- `TestEKSDefaultsAreEmptyToPreserveParity` — fails if anyone adds a default without re-running the
  parity harness.

**Meta-observation worth keeping:** I nearly shipped a mitigation that traded a real metric for
protection I had already built. What caught it was running the parity harness on every change rather
than only at the end. Two of my hypotheses along the way were also wrong (first blamed
`ignore-invalid-speed`, then assumed the dev host was representative) — both corrected by measuring
instead of reasoning.

**Status of the four HIGH/MED bugs:**
| Issue | Disposition |
|---|---|
| #1841 netclass timeouts | **Contained by R2.** Optional mitigation documented. |
| #1915 netclass all-or-nothing | **Contained by R2**, root cause reproduced and documented. Upstream fix belongs upstream. |
| #1710 cpufreq ParseUint | **Not yet triaged** |
| #1672 filesystem impossible values | **Not yet triaged** |

**Next:** triage #1710 cpufreq and #1672 filesystem.

---
## [2026-07-29T01:20Z] UNKNOWN FINDING: filesystem cardinality grows with pod count (hostPID)

**Phase:** P3 exploratory (found while triaging #1672)
**Status:** confirmed — **novel, not in upstream's tracker**

**What I did:** While refuting #1672 (impossible filesystem values) I noticed the two exporters report
different filesystem counts on the same node, and chased the discrepancy.

**What I observed:**
```
NMA: 17 filesystems     PNE: 4 filesystems     NMA-only: 13, PNE-only: 0
```
Every one of the 13 extra series is a per-pod ephemeral mount:
```
/run/containerd/io.containerd.grpc.v1.cri/sandboxes/<sandbox-id>/shm            (x7)
/var/lib/kubelet/pods/<pod-uid>/volumes/kubernetes.io~projected/kube-api-access-*  (x6)
```

**Hypotheses tested and REFUTED:**
1. *"PNE's chart ships a mount-points-exclude default we lack"* — no; the PNE pod's args contain no
   exclusion flag.
2. *"Mount propagation differs"* — it does (PNE has `HostToContainer` on `/host/root`, we have none),
   but this predicts PNE seeing *more* mounts, the opposite of what happens.
3. *"`--path.rootfs` prefix filtering differs"* — no; `rootfsStripPrefix` only rewrites the label, and
   filtering happens on the stripped path.

**Decisive experiment:** ran the *upstream* `node-exporter:v1.12.1` image with NMA's *exact* path flags
(`--path.procfs=/host/proc --path.rootfs=/host`) on the same node:
```
node_filesystem_size_bytes series: 4      per-pod mounts: 0
```
So identical flags produce identical-to-PNE output. **The flags are not the cause.**

**Actual root cause** — `collector/filesystem_linux.go:185`:
```go
// Fallback to `/proc/self/mountinfo` if `/proc/1/mountinfo` is missing due hidepid.
mountInfo, err = fs.GetMounts()   // primary path reads <procfs>/1/mountinfo
```
The collector reads **PID 1's** mount table. Our DaemonSet sets `hostPID: true`, so PID 1 is the host's
init and its mount namespace contains every per-pod mount on the node. The PNE DaemonSet does not set
`hostPID`, so its PID 1 is its own container, whose namespace holds only the handful of real
filesystems. Confirmed by probe: the host init namespace has 58 mounts of which 13 are pod mounts.

**Why this matters — it is a scaling defect, not a cosmetic difference:**
- 13 extra series at **20 pods**. These mounts are per-pod, so the count grows roughly linearly with
  pod density. At 110 pods/node (the EKS default max) this is plausibly ~70+ extra series per node,
  and each is high-churn: pod UIDs and sandbox IDs are unique and never repeat.
- High-churn unique label values are the classic Prometheus cardinality problem: every pod
  create/delete permanently adds a new series to the TSDB for the retention window.
- It also means `node_filesystem_*` on our endpoint is **not** interchangeable with PNE's for
  aggregation queries such as `sum(node_filesystem_size_bytes)`, which would double-count tmpfs mounts.

**This is a genuine unknown finding.** It is not in upstream's tracker, because it is not an upstream
bug: it is an interaction between upstream's PID-1 mount table read and *our* `hostPID: true`, which
the agent needs for its health-monitoring mission and cannot drop.

**Confirms pre-registered prediction PR6** (pressure/exploratory work finds ≥1 issue absent from
upstream's tracker) — and it was found at 20 pods on an idle cluster, before any pressure testing.

**Disposition:** must fix. This is exactly the class of defect this goal exists to catch, and the fix
belongs in our layer since the cause is our pod spec. Candidate fixes to evaluate next:
1. Add the per-pod mount paths to `--collector.filesystem.mount-points-exclude`. Cheap, but changes the
   metric surface (needs a parity re-measure and a documented deviation).
2. Ship the exclusion only for paths that are provably pod-ephemeral
   (`/var/lib/kubelet/pods/.+`, `/run/containerd/.+/sandboxes/.+`), which upstream's own default
   already gestures at with `var/lib/docker/.+` and `var/lib/containers/storage/.+` — i.e. this is
   consistent with upstream intent, not a divergence from it.

Option 2 looks right: upstream already excludes container-runtime scratch paths for exactly this
reason, and simply has no entry for containerd-on-Kubernetes.

**Next:** implement option 2, measure the parity impact, quantify the series reduction, and add a
regression test.

---
## [2026-07-29T01:55Z] FIXED: pod-scaling filesystem cardinality — verified on live cluster

**Phase:** P4 fix
**Status:** fixed

**What I did:** Added `--collector.filesystem.mount-points-exclude` as the first EKS default, extending
upstream's `defMountPointsExcluded` with the two per-pod path families. Measured parity, deployed to
the live cluster, and verified.

**The exclusion** (upstream's defaults repeated verbatim, since supplying the flag *replaces* rather
than appends):
```
^/(dev|proc|run/credentials/.+|sys|var/lib/docker/.+|var/lib/containers/storage/.+
  |run/containerd/.+/sandboxes/.+          <- containerd pod sandbox shm
  |var/lib/kubelet/pods/.+                 <- kubelet per-pod volumes
 )($|/)
```

**What I observed:**

| Check | Before | After |
|---|---|---|
| Parity vs upstream | 298/298 | **298/298, empty diff** |
| NMA filesystem series (live) | **17** | **8** |
| PNE filesystem series (live) | 4 | 8 |
| NMA pod-ephemeral series | **13** | **0** |
| `pkg/metrics` coverage | 100.0% | **100.0%** |
| `go test -race` | clean | **clean** |

Both exporters now report **8** filesystem series with **zero** pod-ephemeral mounts. (Both moved from
4→8 because the earlier snapshot was taken before Prometheus/Grafana were installed, which added real
tmpfs mounts — the meaningful number is that the two now agree and the churning series are gone.)

**Why this is parity-neutral:** the exclusion removes *series within* the `node_filesystem_*` families,
not the families themselves. The harness compares metric names, types and label key sets, so 298/298
holds. The removed series described mounts that only existed because of `hostPID`, which upstream never
reports at all — so removing them moves us *toward* upstream's output, not away.

**Tests added:**
- `TestFilesystemExclusionCoversPodEphemeralMounts` — asserts real observed paths (actual pod UIDs and
  sandbox IDs from the live node) are excluded, and that `/`, `/boot/efi`, `/run`, `/tmp`, `/var`,
  `/var/lib/kubelet` are **kept**. The must-keep list is the important half: an over-broad regexp that
  swallowed `/var/lib/kubelet` would hide a real disk-full condition.
- `TestEKSDefaultsAreParityNeutral` — replaced the earlier "defaults must be empty" invariant with an
  allowlist plus the recorded measurement for each rejected candidate. The old test did its job: it
  failed the moment I added this default and forced a parity re-measure before proceeding.

**Note on the invariant test working as designed:** `TestEKSDefaultsAreEmptyToPreserveParity` failed
immediately when I added the flag. That is the intended behaviour — it stopped me shipping a default
without re-running the parity harness. Replacing it with a reasoned allowlist keeps the guard while
permitting a measured exception.

**Next:** #2799 nfsd on kernel 6.18 (we run far newer than the 6.6-rc1 in the report), then remaining
LOW bugs, then P3 scale/pressure testing.

---
## [2026-07-29T02:10Z] P2 complete — all 12 upstream bugs dispositioned

**Phase:** P2 triage
**Status:** confirmed (all 12 have a written disposition; zero left unknown)

**What I did:** Compared `node_scrape_collector_success` per collector between the two live endpoints on
the same node, which settles applicability empirically rather than by reading issue text.

**What I observed** — for every LOW bug, both exporters behave *identically*:
```
nfsd   NMA success=0   PNE success=0     (no NFS server on an EKS node)
nfs    NMA success=0   PNE success=0     (no NFS client mounts)
zfs    NMA success=0   PNE success=0     (no ZFS)
mdadm  NMA success=1   PNE success=1     (no software RAID; succeeds trivially)
```
Identical values mean no fork-specific exposure: whatever upstream does here, we do the same.

### Final disposition of all 12 open bugs

| Issue | Collector | Disposition | Evidence |
|---|---|---|---|
| #1841 | netclass/bonding | **Contained by R2** (per-collector timeout). Optional mitigation documented; rejected as a default because it costs `node_network_speed_bytes`. | timeout test; parity measurement 297 vs 298 |
| #1915 | netclass | **Contained by R2**; root cause reproduced deterministically. Real fix (skip failed device, keep the rest) belongs upstream. | `/tmp/netclassrepro` — 0 of 3 devices reported |
| #1710 | cpufreq | **REFUTED for EKS.** cpufreq succeeds in 0.13ms and emits zero frequency metrics on *both* exporters; EC2 does not expose cpufreq sysfs, so the vulnerable `ParseUint` path is never reached. | `success=1`, 0 `node_cpu_scaling_*` series on both |
| #1672 | filesystem | **REFUTED on our nodes.** Zero impossible values (free/avail > size) across all filesystems on both exporters. | scripted check over both scrapes: 0 violations |
| #2514 | filesystem | **N/A** — needs NFS mounts; `nfs` collector fails on both. | success=0 both |
| #3500 | mdadm | **N/A** — needs software RAID; collector succeeds trivially on both. Note the real fix landed in procfs (#786), not node_exporter. | success=1 both |
| #2799 | nfsd | **N/A** — no NFS server; fails identically on both, including on kernel **6.18** (far newer than the 6.6-rc1 in the report, so no regression for us). | success=0 both |
| #1498 | filesystem/ZFS | **N/A** — no ZFS on EKS AMIs. | zfs success=0 both |
| #2906 | cpu | **N/A** — Darwin/M3 only. | Linux-only deployment |
| #2217 | — | **N/A** — macOS code signing. | Linux-only |
| #1844 | wifi | **N/A** — default-disabled upstream and for us. | absent from both enabled sets |
| #1007 | supervisord | **N/A by default, and now contained.** Default-disabled, but it is a *panic*, so R1 makes the opt-in path safe rather than fatal. | panic containment test |

**Score against prediction PR5** ("≥3 of the 12 are non-reproducible on our config"): **confirmed** —
2 actively refuted (#1710, #1672) and 8 not applicable, so 10 of 12 do not affect us. The two that do
are contained rather than fixed, which is the correct outcome: their real fixes belong upstream, and R2
means we do not have to wait for them.

**Honest note on what this triage does and does not show.** It is based on an *idle 20-pod cluster*.
#1841/#1915 are churn-driven, so "contained by R2" is a claim about the guard, not evidence that the
bugs never fire here. P3 pressure testing is what would actually exercise them, and until that runs
this remains a design argument rather than a measurement.

**Next:** P3 — scale the cluster and apply pressure. Requires cost approval first (personal account).

---
## [2026-07-29T01:40Z] P3 pressure testing — results, and the memory finding

**Phase:** P3 pressure
**Status:** confirmed

**Setup:** scaled to 6 nodes (4x t3.large amd64, 2x m7g.large arm64), EKS 1.36, both exporters
co-resident, one Prometheus. Applied pod churn (~2,888 live pods, 3,000-completion Job at parallelism
30), CPU saturation (all cores, every node), and mount churn (4 emptyDir volumes per pod).

**Incidental finding — multi-arch was required and initially broken.** The first arm64 rollout hit
`exec /opt/bin/eks-node-monitoring-agent: exec format error` on both m7g nodes: the image was amd64-only.
Rebuilt with `docker buildx --platform linux/amd64,linux/arm64`. Also learned that a backgrounded
`docker buildx` is killed when its wrapper exits — the first attempt reported exit 0 while never
pushing, and only checking ECR revealed it. **Do not trust an exit code for a detached build; verify the
artifact.**

**Results under pressure:**

| Measure | NMA | PNE | Reading |
|---|---|---|---|
| Total series | 4,726 | 4,737 | parity holds under churn |
| Filesystem series | 24 | 24 | the hostPID fix holds under churn |
| **Max collector duration** | **0.138s** | 0.681s | **NMA ~5x faster** |
| `up` over 15m | 1.0 | 1.0 | neither dropped a scrape |
| Goroutines | 115 (peak 117) | 8 (peak 9) | flat |
| Open fds | 28 (peak 29) | 9 | flat |
| Resident memory | 65MB (peak 81MB) | 23MB | investigated below |
| panics / timeouts contained | 0 / 0 | n/a | guard never had to fire |

**An earlier reading was misleading and I nearly recorded it as a finding.** Mid-rollout I measured NMA
3,005 series vs PNE 5,271 and briefly took it as a real divergence. It was rollout skew — NMA pods were
restarting while PNE's were steady. After the rollout settled the counts converged (4,726 vs 4,737).
**Lesson repeated from the rate() window error: do not measure across a rollout.**

**The memory finding, investigated properly rather than asserted:**

Initial reading was alarming — `deriv(process_resident_memory_bytes[15m])` = **+10,147 B/s** for NMA
versus +79 B/s for PNE, with a 27MB range. That is the shape of a leak.

Tested by stopping all churn and watching for recovery:
```
during churn   deriv=+10147 B/s   peak 80.6MB
churn drained  deriv= +3971 B/s   now  65.1MB
+9 min         deriv= +1258 B/s   now  65.6MB
```
Memory *fell* from the 81MB peak back to ~65MB, the derivative decayed monotonically, and goroutines and
fds stayed flat the whole time. Per-node values converged tightly (64.6–66.4MB) across **both**
architectures.

**Conclusion: Go heap working set under load, not an unbounded leak.** But this is still a real finding
worth reporting rather than dismissing: 65MB steady-state against the chart's **200Mi** limit is a third
of the budget before the health monitors' own usage, and it was measured with `includeExporterMetrics`
enabled. The resource envelope needs revisiting before wide rollout, and on Auto Mode it feeds
`EKSTachyonAMIOverhead` -> Karpenter bin-packing -> customer allocatable.

**Prediction PR7 (the load-bearing one) — outcome:** NMA did **not** degrade worse than PNE under
identical pressure. It was ~5x faster on max collector duration and equal on scrape success. The
in-process design is not a liability under load. Memory is the single axis where it costs more, and that
cost is bounded and explained.

**Predictions PR1/PR3 — NOT confirmed, and I should say so plainly:** pod churn at 2,888 pods did *not*
reproduce netclass instability (#1841/#1915), and collector latency did *not* grow superlinearly —
netclass peaked at 0.331s and the guard never fired. Either the churn window is narrower than the
reproduction suggests, VPC CNI's warm-pool behaviour means veth devices are not created and destroyed as
aggressively as I assumed, or 2,888 pods over ~40 minutes is not enough churn *rate*. The deterministic
fixture reproduction in `/tmp/netclassrepro` still stands as proof the code path is broken; what is
unproven is how often it fires in practice on EKS.

**Not tested (named blind spots, not assumed passes):** memory-pressure/near-OOM, disk-I/O saturation,
GPU/Neuron nodes, Bottlerocket, multi-hour soak, scrape-storm (manifest written but not run).

**Next:** document design and tradeoffs (done: `docs/design/node-exporter-parity.md`), then P5 edge
cases and linters. Scale back to 2 nodes to cut cost.

---
## [2026-07-29T01:35Z] P5 complete — edge cases found two more real bugs

**Phase:** P5 edge cases + quality
**Status:** fixed

**What I did:** Added `pkg/metrics/edgecases_test.go` — adversarial inputs drawn from real upstream bug
shapes (malformed procfs, absent paths, invalid config, lifecycle races, resource exhaustion). Ran
`staticcheck`, `gofmt`, `go vet`, and the race detector.

**Two real bugs found, neither reachable by the existing tests:**

1. **Crash on `MetricsPath: "/"`.** `newHandler` registered both the metrics path and the landing page,
   and `http.ServeMux` **panics** on a duplicate pattern. Setting the metrics path to `/` — a plausible
   operator choice — killed the agent at startup.
   ```
   panic: ... net/http.(*ServeMux).register ... server.go:2882
     newHandler  pkg/metrics/server.go:210
   ```
   Fixed: register the landing page only when the metrics path is not `/`; serving metrics wins, since
   that is what the operator explicitly asked for.

2. **A second data race**, on `s.server`. `Start` wrote the field while its own serve goroutine read it,
   so calling `Start` twice raced. Found by `TestRepeatedStartAfterShutdown`. Fixed by building the
   `http.Server` into a local, publishing it under the existing mutex, and having the goroutine and the
   shutdown path use the local rather than the field.

**Linter findings fixed:** three files unformatted (`gofmt -w`), and `staticcheck` flagged
`eksIgnoredNetDevices` as unused (U1000) — dead code left behind when the netclass default was rejected.
Moved it into a doc comment rather than keeping an unused constant.

**A harness weakness worth recording.** Mid-run the parity harness reported a violation whose diff was
full of `addon_controller_*` and `controller_runtime_*` series. Those cannot come from node_exporter: a
stale process from an earlier run still held port 19101, so the harness compared the agent against
*that*. It reported PARITY VIOLATED for a reason unrelated to the agent.

I nearly chased this as a code regression. Hardened the harness with two guards:
- refuse to start if either port is already bound
- verify the "upstream" endpoint actually serves `node_exporter_build_info` before trusting it

**Negative control on the guard** (a harness guard that cannot fire is worthless): stood up a fake HTTP
server serving `controller_runtime_reconcile_total` on the upstream port. The harness exits **2** with
`the endpoint on 19601 does not look like node_exporter ... Refusing to compare against it`. Confirmed
working.

**Final state of the quality gates:**
| Gate | Result |
|---|---|
| `pkg/metrics` coverage | **100.0%** of statements |
| `go test -race` | **clean** |
| `staticcheck` | **clean** |
| `gofmt` / `go vet` | **clean** |
| Parity vs upstream | **298 / 298, empty diff, exit 0** |

**Running tally of bugs found by this work: 8.** Four during the original parity work, one novel
cardinality defect, three in P1/P5 (the guard's own channel-lifetime bugs counted as one group), plus
two races and the ServeMux panic. Every one was found by *running* something — a live cluster, the race
detector, a linter, or an adversarial input — not by reading code.

**Next:** P6 — refresh the evidence bundle and the CONTRIBUTING compliance audit, and write the
"better than PNE" summary grounded in what P1–P5 actually measured.

---
## [2026-07-29T01:50Z] P6 complete — final assessment written

**Phase:** P6
**Status:** confirmed

**What I did:** Re-ran every gate, then wrote `evidence/production-readiness.md` answering the question
the goal actually asked: is this better than PNE?

**Final gate state:**
```
pkg/metrics coverage    100.0%   (.covignore unchanged)
go test -race           clean
staticcheck             clean
gofmt / go vet          clean
full suite              34 packages, 0 failures
check-generate          no drift
helm lint               0 failed
parity vs upstream      298/298, empty diff, exit 0
live e2e (EKS 1.36)     3/3 pass
```

**The answer, stated as the data supports it:**
- **output** — equivalent (that is the goal; a non-equivalent replacement is not a replacement)
- **resilience** — better, tested, but the guard never fired in ~40 min at 2,888 pods, so it is
  insurance against a documented failure class rather than a fix for something observed
- **latency under load** — better, 0.138s vs 0.681s max collector duration
- **memory** — worse, 65MB vs 23MB; bounded and non-leaking but a third of the 200Mi chart limit
- **upstream bug exposure** — 10 of 12 non-applicable or refuted, 2 contained

**Bugs found by this work: 9.** Every one found by *running* something (live cluster, race detector,
linter, adversarial input, cross-exporter comparison), none by reading code. Plus three measurement
errors of my own, recorded because the methodology is the reusable part: an unfilled `rate()` window, a
comparison taken across a rollout, and a parity run against a stale port. The harness now refuses the
third.

**Recommendation recorded:** ready for upstream discussion, not ready to enable by default. Three
prerequisites before recommending broadly — resolve the resource envelope with NMA owners (and ENO for
Auto Mode), run memory-pressure and soak tests, and test Bottlerocket plus one accelerated instance type.

**Eight blind spots named explicitly** rather than implied as passing: memory-pressure/OOM, disk-I/O
saturation, GPU/Neuron, Bottlerocket, multi-hour soak, scrape-storm, live netclass churn reproduction,
and Auto Mode.

**Cost note:** the two pressure nodegroups were deleted after testing; the original 2-node group remains
so the dashboards stay reachable.

---
