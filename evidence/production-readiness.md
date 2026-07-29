# Production readiness: is this better than prometheus-node-exporter?

Final assessment for `GOAL-PRODUCTION-READINESS.md`. Every claim cites a measurement in
`JOURNAL.md` or `evidence/`. Where the data does not support a claim, that is stated instead.

---

## The honest answer

**On metric output: equivalent, by design and by measurement.** 298 metric names, 298 series shapes,
empty diff against upstream `v1.12.1`. That is the goal — a replacement that isn't equivalent is not a
replacement.

**On resilience: better, and this is the substantive claim.** Upstream has no per-collector panic
recovery and no timeout. This fork has both. That difference is not cosmetic here, because the process
serving metrics is also the process publishing `NodeCondition`s to EKS node auto repair.

**On collection latency under load: better, measured.** Max collector duration 0.138s vs PNE's 0.681s
at ~2,888 churning pods.

**On resource footprint: worse.** 65MB steady vs PNE's 23MB. Bounded and non-leaking, but a third of
the chart's current 200Mi limit.

**On upstream bug exposure: substantially reduced, mostly because they don't apply.** 10 of 12 open
bugs are non-applicable or refuted on EKS; the 2 that apply are contained rather than fixed.

---

## 1. Resilience: the defensible improvement

Upstream `NodeCollector.Collect` fans every collector into its own goroutine and calls `Update()` bare
(`collector/collector.go:145-157`). Verified: **no `recover()` in that file, no timeout.**

Why that matters more here than upstream — proven, not asserted:

| Panic location | Outcome |
|---|---|
| On the request goroutine | contained by `promhttp` → HTTP 500, process survives |
| In a goroutine spawned by `Collect` (**upstream's actual path**) | **fatal** — nothing can recover it |

A guard in the HTTP handler cannot help, because the handler is not on the panicking stack.

The risk is documented in upstream's own tracker: three crash reports (#1007 panic, #3346 SIGSEGV on
linux/amd64, #1987 crash) and three hang/timeout reports (#1841, #1353, plus **#2585 and #3649 which are
open requests for timeouts upstream still does not have**).

**What this fork adds:** per-collector `recover()`, a 5s per-collector timeout, and
`node_collector_panics_total` / `node_collector_timeouts_total` so a contained fault is visible.

**Tested, each with a test demonstrated to fail without the guard:**
- panic contained; sibling collectors unaffected; failure reported as `success=0`
- `panic(nil)` edge case
- hung collector abandoned; scrape completes; siblings still succeed
- collector abandoned mid-send released via the drain (no goroutine leak)
- `ErrNoData` still treated as upstream treats it

**Caveat stated plainly:** in ~40 minutes at 2,888 pods the guard **never fired** — 0 panics, 0
timeouts. So this is proven correct in tests and proven *not needed* in that window. It is insurance
against a documented class of upstream failure, not a fix for something observed in production.

## 2. What pressure testing measured

6 nodes (4× t3.large amd64, 2× m7g.large arm64), EKS 1.36, both exporters co-resident, one Prometheus,
~2,888 churning pods plus full CPU saturation.

| Measure | NMA | PNE | Verdict |
|---|---|---|---|
| Total series | 4,726 | 4,737 | parity holds under churn |
| Filesystem series | 24 | 24 | the hostPID fix holds under churn |
| **Max collector duration** | **0.138s** | 0.681s | **NMA ~5× faster** |
| `up` over 15m | 1.0 | 1.0 | neither dropped a scrape |
| Goroutines | 115 (peak 117) | 8 (peak 9) | flat — no leak |
| Open fds | 28 (peak 29) | 9 | flat — no leak |
| Resident memory | 65MB (peak 81MB) | 23MB | see §4 |

**Prediction PR7 — the load-bearing one — held:** NMA did not degrade worse than PNE under identical
pressure. The in-process design is not a liability under load.

**Two predictions did NOT hold, recorded as such:**
- **PR1** (churn reproduces netclass instability) — did not reproduce at 2,888 pods.
- **PR3** (latency grows superlinearly with pod count) — did not; netclass peaked at 0.331s.

The deterministic fixture reproduction still proves the netclass code path is broken. What is unproven
is how often it fires on a real EKS node. Possible explanations: VPC CNI's warm-pool behaviour means
veth devices are not created/destroyed as aggressively as assumed, or the listing→read window is
narrower than the churn *rate* achieved here.

## 3. Upstream bug exposure

209 open issues, 12 labelled `bug`, snapshotted to `evidence/upstream-issues/`.

| Disposition | Count | Issues |
|---|---|---|
| **Refuted on our config** | 2 | #1710 (cpufreq emits nothing on EC2, so the vulnerable `ParseUint` is never reached), #1672 (zero impossible filesystem values measured) |
| **Not applicable** | 8 | need NFS, software RAID, ZFS, Darwin, macOS, or a default-disabled collector |
| **Contained by the timeout** | 2 | #1841, #1915 (netclass) |

Applicability was settled empirically, by comparing `node_scrape_collector_success` per collector between
the two live endpoints — not by reading issue text. For every non-applicable bug, both exporters behave
*identically*, so there is no fork-specific exposure.

**Prediction PR5 confirmed** (≥3 of 12 non-reproducible): 10 of 12 do not affect us.

## 4. Where this is worse than PNE

**Memory: 65MB vs 23MB.** Investigated as a possible leak and refuted:
- derivative decayed 10,147 → 3,971 → 1,258 B/s as load drained
- memory fell from the 81MB peak back to ~65MB
- goroutines and fds stayed flat throughout
- all six nodes converged tightly (64.6–66.4MB), arm64 and amd64 alike

So it is Go heap working set, not unbounded growth. **But it is still a real finding:** 65MB against the
chart's 200Mi limit is a third of the budget before the health monitors' own usage, and it was measured
with `includeExporterMetrics` enabled. The resource envelope needs revisiting before wide rollout, and on
Auto Mode it feeds `EKSTachyonAMIOverhead` → Karpenter bin-packing → customer allocatable.

**`node_procs_running` bias.** 30-minute average 6.33 vs PNE's 1.11, because the metric counts runnable
threads and NMA is a larger process. `node_load1` agrees exactly (0.12 vs 0.12), so the machines are
identical — only the instantaneous count differs, and it differs because of who is asking. Alerts
thresholded on its absolute value need retuning.

## 5. Bugs this work found: 9

Every one was found by **running** something — never by reading code.

| # | Bug | Found by |
|---|---|---|
| 1 | Chart mounted the ConfigMap only when `monitors` was set, so a metrics-only config silently left the endpoint disabled | deploying to a live cluster |
| 2 | Dashboard verdict query reported 0 missing metrics while 51 were absent (`unless` needs `on(__name__)`) | the negative control |
| 3 | e2e asserted zero collector failures; upstream fails the same 10 on EKS | comparing against PNE |
| 4 | **Novel:** per-pod filesystem mounts leaked unbounded cardinality via `hostPID` | comparing series counts under churn |
| 5 | Resilience guard would send on a closed channel — turning containment into a new crash | reasoning through channel lifetime while building it |
| 6 | Guard dropped healthy metrics by closing the forwarder too early | a sibling-survival assertion |
| 7 | **Data race:** `Start` wrote `s.listener` while `Address` read it | `go test -race`, first run |
| 8 | **Data race:** `Start` wrote `s.server` while its serve goroutine read it | an adversarial lifecycle test |
| 9 | **Startup panic:** `MetricsPath: "/"` panicked via duplicate `ServeMux` pattern | an adversarial config test |

Also three measurement errors of my own, recorded because the methodology matters: a `rate()` window
measured before it filled (reported 41% agreement, actually 99.26%), a comparison taken across a rollout
(reported a series divergence that was skew), and a parity run against a stale process on the port. The
harness now refuses the third case.

## 6. Quality gates

| Gate | Result |
|---|---|
| `pkg/metrics` coverage | **100.0%** of statements, `.covignore` unchanged |
| `go test -race` | **clean** |
| `staticcheck` | **clean** |
| `gofmt`, `go vet` | **clean** |
| Full suite | **34 packages, 0 failures** |
| `hack/check-generate.sh` | **no drift** |
| `helm lint` | **0 failed** |
| Parity vs upstream | **298/298, empty diff, exit 0** |
| Live e2e (EKS 1.36) | **3/3 pass** |

## 7. Blind spots — named, not assumed passed

1. **Memory-pressure / near-OOM** not tested. Given §4, this is the most valuable remaining test.
2. **Disk-I/O saturation** not tested.
3. **GPU / Neuron nodes** not tested; the NVIDIA and Neuron collectors were never exercised.
4. **Bottlerocket** not tested — only AL2023. Different filesystem layout and an immutable root.
5. **Multi-hour soak** not run; the longest observation was ~40 minutes under load.
6. **Scrape-storm** manifest written but not executed.
7. **netclass churn hazard** not reproduced on a live cluster (§2), only in a fixture.
8. **Auto Mode** not tested at all — it needs the Tachyon AMI path, out of scope here.

## 8. Recommendation

**Ready for upstream discussion; not ready to enable by default.**

Ship as opt-in, which is what it already is. Before recommending it broadly:
1. Resolve the resource envelope (§4) with the NMA owners, and for Auto Mode with ENO/Atlas.
2. Run the memory-pressure and soak tests (§7.1, §7.5).
3. Test on Bottlerocket and one accelerated instance type (§7.3, §7.4).

**On "better than PNE":** the fork is *equivalent in output, more resilient in failure, faster under
load, and heavier in memory*. The resilience advantage is real and tested, but it insures against a
failure class that did not occur during this testing. That is a reasonable trade for a process whose
other job is node health reporting — and it should be presented as insurance, not as a bug fix.
