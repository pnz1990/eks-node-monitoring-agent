# GOAL — Production-grade hardening before upstream submission

**Type:** executable goal file. Invoked via `/goal`. Companion to `GOAL.md` (which is met).
**Owner:** rrroizma (EKS, addons org)
**Created:** 2026-07-28
**Journal:** `JOURNAL.md` — append-only progress log, written as work proceeds so context loss
never destroys findings. **Read it first on any resumption.**

## Preconditions (already met, do not redo)

`GOAL.md` is complete and evidenced: opt-in `/metrics` endpoint at 100% unit coverage, parity proven
against upstream `v1.12.1` on EKS 1.36 (305 shared metric names, zero asymmetry; V3 99.26% within 5%),
live e2e green, dashboards with a working (and falsifiable) trust verdict. 14 commits on
`pnz1990/eks-node-monitoring-agent:feat/prometheus-node-exporter-parity`.

**What that does NOT establish, and this goal must:**

1. The endpoint has only ever run on a **2-node, 20-pod, idle** cluster. Every latency and memory
   number so far is measured under conditions no production cluster resembles.
2. The fork inherits upstream's **209 open issues / 12 open bugs** wholesale. Shipping without
   triaging them means knowingly importing known defects.
3. Nothing has stressed the shared-fate boundary between the scrape path and NMA's monitor loop under
   real contention.
4. Coverage is 100% of *statements*, which is not the same as covering *edge cases*. Statement
   coverage with no adversarial input is a blind spot, not a guarantee.

---

## 1. Objective

Make the endpoint production-grade, and be able to **prove** it is not merely equivalent to
prometheus-node-exporter but *better* on the axes that matter for EKS — by triaging upstream's known
bugs, finding unknown ones under load, and closing both with falsifiable, reproducible tests.

Success is not "it works." Success is: **every known upstream bug that applies to our configuration is
either fixed, mitigated, or documented with evidence; and pressure testing has surfaced and closed
issues that no amount of unit testing would have found.**

---

## 2. Upstream bug triage — the real data

Pulled 2026-07-28 from `prometheus/node_exporter`:

```
$ gh api -X GET search/issues -f q='repo:prometheus/node_exporter is:issue is:open' --jq '.total_count'
209
$ gh api -X GET search/issues -f q='repo:prometheus/node_exporter is:issue is:open label:bug' --jq '.total_count'
12
```

### The 12 open bugs, triaged against our enabled collector set

Our endpoint enables **49 collectors** (verified identical to upstream's default set). Cross-referencing
the bug list against `node_scrape_collector_success` from a live scrape:

| Issue | Collector | Enabled for us? | Applies to EKS? | Summary |
|---|---|---|---|---|
| [#1841](https://github.com/prometheus/node_exporter/issues/1841) | `netclass`/`bonding` | **YES** | **HIGH** | netclass/bonding causes **scrape timeouts**. Reporter is on Kubernetes filtering `veth.+`, `cali.+`, `br-.+`. |
| [#1915](https://github.com/prometheus/node_exporter/issues/1915) | `netclass` | **YES** | **HIGH** | netclass fails while reading *ignored* devices — interface churn hits this. |
| [#1710](https://github.com/prometheus/node_exporter/issues/1710) | `cpufreq` | **YES** | **MED** | `strconv.ParseUint: parsing "<unknown>"` — collector fails outright. `cpufreq` is our slowest collector at baseline. |
| [#1672](https://github.com/prometheus/node_exporter/issues/1672) | `filesystem` | **YES** | **MED** | `node_filesystem_{free,avail}_bytes` > `node_filesystem_size_bytes` — impossible values reaching dashboards. |
| [#2514](https://github.com/prometheus/node_exporter/issues/2514) | `filesystem` | **YES** | **LOW** | Cannot deduplicate multihomed NFS mounts. Needs NFS. |
| [#3500](https://github.com/prometheus/node_exporter/issues/3500) | `mdadm` | **YES** | **LOW** | Fails on delayed RAID resync. Needs software RAID. |
| [#2799](https://github.com/prometheus/node_exporter/issues/2799) | `nfsd` | **YES** | **LOW** | Unknown NFSd metric lines on kernel 6.6-rc1. We run 6.18 — **must check whether this regressed further**. |
| [#1498](https://github.com/prometheus/node_exporter/issues/1498) | `filesystem` | YES | **N/A** | ZFS filesystem sizes. No ZFS on EKS AMIs. |
| [#2906](https://github.com/prometheus/node_exporter/issues/2906) | `cpu` | YES | **N/A** | Darwin/M3 power status. Linux-only for us. |
| [#2217](https://github.com/prometheus/node_exporter/issues/2217) | — | n/a | **N/A** | macOS M1 code signing. |
| [#1844](https://github.com/prometheus/node_exporter/issues/1844) | `wifi` | no | **N/A** | Default-disabled upstream and for us. |
| [#1007](https://github.com/prometheus/node_exporter/issues/1007) | `supervisord` | no | **N/A** | **Panic** when supervisord absent. Default-disabled — but a panic in our process kills the *agent*, so the opt-in path must be guarded. |

**Six bugs touch collectors we ship enabled; four are HIGH or MED for EKS.**

### Triage protocol (do not skip steps)

For each of the HIGH/MED issues, in that order:

1. **Reproduce or refute.** Write a failing test against the fixture-driven collector path. If it
   cannot be reproduced on our configuration, say so with evidence and move on — do **not** fix
   phantom bugs.
2. **Decide the disposition**, and record it:
   - **Fix in fork** — patch behind a clearly-commented deviation, with a test. Prefer this only when
     the fix is small and self-contained.
   - **Mitigate by configuration** — e.g. ship a safer default (`--collector.netclass.ignore-invalid-speed`,
     an EKS-appropriate `ignored-devices` regexp). **Cheapest and most honest** for upstream bugs:
     it does not fork collector internals and it is visible to operators.
   - **Guard at the boundary** — for panics and hangs, make the *endpoint* resilient regardless of
     collector behaviour (see §3, the single most important item here).
   - **Document as accepted** — with the reason and blast radius.
3. **Upstream it where the fix belongs upstream.** A patch to a collector is a contribution to
   `prometheus/node_exporter`, not something to hide in a fork. Record the intent.

**Anti-goal:** do not fork collector internals to fix bugs that only manifest on hardware absent from
every EKS node. That trades real maintenance cost for imaginary benefit.

### Beyond the 12

The `bug` label is maintainer-applied and lags. Also sweep:
- `label:accepted` open issues (maintainer-acknowledged, not yet labelled bug)
- Issues mentioning `timeout`, `panic`, `leak`, `deadlock`, `race`, `OOM`, `hang`
- Closed-as-stale issues with recent comments (real problems that lost attention)
- The `procfs` dependency's own open issues — several node_exporter bugs are actually procfs bugs
  (#3500 was fixed via `prometheus/procfs#786`)

---

## 3. The structural risk that outranks every individual bug

**In upstream node_exporter, a collector that panics or hangs kills a process whose only job is
serving metrics. In our fork, it kills the process responsible for node health monitoring and
`NodeCondition` reporting — which feeds EKS node auto repair.**

That asymmetry means the fork must be *strictly more defensive* than upstream, and it is the clearest
axis on which we can be **better than PNE** rather than merely equal:

| Requirement | Why | Test |
|---|---|---|
| **R1** A panicking collector must not crash the agent | #1007 proves collectors can panic | Inject a panicking collector; assert the agent survives, the endpoint returns 500 for that collector, and NodeConditions keep flowing |
| **R2** A hanging collector must not stall the scrape indefinitely | #1841 proves collectors can hang | Per-scrape timeout; assert a hung collector yields a partial scrape, not an infinite request |
| **R3** A slow/hung collector must not delay NodeCondition reporting | The shared-fate risk from `PARITY-PLAN.md` F7b | Assert `DetectionDelay` is unaffected while a collector is wedged |
| **R4** Endpoint failure must not affect agent liveness | `/healthz` must not depend on `/metrics` | Kill the metrics listener; assert liveness probe still passes |
| **R5** Unbounded cardinality must not OOM the agent | veth churn on a busy node creates unbounded label sets | Assert memory stays bounded with thousands of interfaces |

**R1 and R2 are the highest-value work in this entire goal.** They convert "we inherited upstream's
bugs" into "upstream's bugs cannot take down our agent," which is a defensible improvement over PNE.

---

## 4. Pressure testing

Everything measured so far is on 2 nodes / 20 pods / idle. That is not evidence about production.

### 4.1 Scale the cluster

| Parameter | Current | Target |
|---|---|---|
| Nodes | 2 × t3.large | **6+**, mixed types incl. `m7g` (arm64) and one larger node |
| Pods | 20 | **400+** (density drives veth/cgroup/mount cardinality) |
| Architecture | amd64 only | amd64 **and** arm64 (catches arch-specific bugs like #1844) |

Cost matters — this is a personal account. Get explicit approval with an estimate before scaling, and
tear down promptly. Prefer a short intense run over a long idle one.

### 4.2 Apply real pressure

Each pressure type targets a specific collector's failure mode. Use `stress-ng` where possible.

| Pressure | Targets | Expected signal |
|---|---|---|
| **CPU saturation** (all cores, sustained) | `cpu`, `cpufreq`, `loadavg`, `stat`, `pressure` | Scrape latency under contention; #1710 `cpufreq` failure |
| **Pod churn** (rapid create/delete, 100s of pods) | `netclass`, `netdev` | **#1841 / #1915** — interface appears/disappears mid-scrape. Highest-value test. |
| **Memory pressure / near-OOM** | `meminfo`, `vmstat`, `pressure` | Agent's own memory headroom; does it get OOMKilled first? |
| **Disk I/O saturation** | `diskstats`, `filesystem` | Scrape latency; #1672 impossible filesystem values |
| **Mount churn** (many volumes mounted/unmounted) | `filesystem` | Stale mount handling, `statfs` hangs on dead mounts |
| **Scrape storm** (many concurrent scrapers) | endpoint | `MaxRequestsInFlight` behaviour; 503 vs queue vs collapse |
| **Long soak** (≥4h under load) | everything | Memory growth, fd leaks, goroutine leaks |

### 4.3 Observe the agent itself, not just its output

This is the part that finds unknown issues. **Scrape NMA's own metrics and logs throughout**, and
compare against PNE under identical pressure:

- `go_goroutines` — monotonic growth ⇒ goroutine leak
- `go_memstats_heap_inuse_bytes`, `process_resident_memory_bytes` — growth ⇒ leak
- `process_open_fds` vs `process_max_fds` — fd leak (NMA has had fd-leak bugs before: see CHANGELOG
  "Fix auto mode diagnostics fd leaks")
- `node_scrape_collector_duration_seconds` per collector — which degrades first under which pressure
- `node_scrape_collector_success` — flapping ⇒ intermittent collector failure
- `container_cpu_cfs_throttled_periods_total` — throttling at the configured limit
- Agent logs — `ERROR`/`WARN` rate, new error strings, repeated messages indicating a hot loop
- **NMA's `DetectionDelay` metric** — the health-monitoring mission must not degrade
- **PNE's equivalents side by side** — if PNE degrades identically, it is upstream behaviour, not our
  regression. This comparison is what makes the finding attributable.

Enable `includeExporterMetrics: true` and `--pprof-address` for these runs.

### 4.4 Falsifiability and reproducibility

Every finding must be recorded as:
1. **Reproduction** — exact commands/manifests, committed under `hack/pressure/`
2. **Observation** — the metric or log line that shows it, with values
3. **Root cause** — code path, cited by `file:line`
4. **Fix** — with a test that **fails before and passes after** (demonstrate both)
5. **Attribution** — does PNE exhibit it too? Upstream bug vs our regression.

A finding without a reproduction is an anecdote. Do not put anecdotes in the report.

---

## 5. Edge and corner case coverage — beyond statement coverage

100% statement coverage with benign inputs is a blind spot. Add adversarial tests:

| Class | Cases |
|---|---|
| **Malformed procfs/sysfs** | `<unknown>` values (#1710), empty files, truncated reads, permission denied, non-numeric where numeric expected, unexpected extra fields (#2799) |
| **Absent paths** | Missing `/proc`, missing `/sys`, `HOST_ROOT` pointing somewhere wrong, dangling symlinks |
| **Concurrency** | Concurrent scrapes (done), scrape during shutdown, shutdown during scrape, repeated start/stop, context cancelled mid-collection |
| **Config edge cases** | Port 0, port in use, invalid address, unknown collector name, contradictory flags (`--collector.x --no-collector.x`), absurd `MaxRequests`, empty and huge `extraArgs` |
| **Resource exhaustion** | fd limit reached, memory limit near, thousands of interfaces/mounts/cgroups |
| **Race detector** | `go test -race` across the package — **currently never run** |
| **Fuzzing** | Fuzz the metrics-path handler and any parsing the fork owns |
| **Mutation testing** | Verify tests actually detect injected faults (`mcp__builder-mcp__MutationTestTool` or manual) |

**Coverage of branches, not just statements.** Where a `for` or `select` has an untaken path, test it.

---

## 6. Code quality — production-grade and elegant

- **`golangci-lint`** — node_exporter ships `.golangci.yml`; NMA does not. Run a strict linter over the
  new code and fix what it finds.
- **`go vet` + `staticcheck` + `gosec`** on the new packages.
- **Re-read the diff as a reviewer would.** Every exported symbol documented; no dead code; no
  speculative generality; naming consistent with NMA (the `NodeExporter` collision noted in F1).
- **Simplify.** The injected seams in `pkg/metrics` earned 100% coverage — re-examine whether all of
  them are still justified or whether some can be simplified without losing the tested paths. Prefer
  the simplest structure that keeps the error paths reachable.
- **Error messages** must name the thing that failed and be actionable.
- **No `panic` in library code**; no ignored errors; no `context.TODO()`.

---

## 7. Journal requirement (mandatory, continuous)

Maintain **`JOURNAL.md`** at the repo root, append-only, updated **as work happens** — not
reconstructed afterwards. It is the durable memory for this effort.

Each entry:

```markdown
## [ISO timestamp] <short title>
**Phase:** <triage | pressure | coverage | quality>
**Status:** <investigating | confirmed | fixed | refuted | deferred>
**What I did:** commands run, files touched
**What I observed:** actual output/values, not a summary
**Conclusion:** what this means
**Next:** the immediate next action
```

Rules:
- Append; never rewrite history. A wrong conclusion gets a **new** entry correcting it, so the
  reasoning trail survives.
- Record **refutations** as prominently as confirmations — "reproduced #1710? no, and here's why" is
  as valuable as a fix, and prevents re-investigating.
- Record measurement mistakes. The premature `rate()` window error in `GOAL.md` Phase 5b is the model:
  the error itself is a finding about methodology.
- Paste real values. "Latency was fine" is useless; "p99 42ms at 400 pods" is evidence.

---

## 8. Phases and gates

### P0 — Journal + triage infrastructure (~0.5 day)
Create `JOURNAL.md`. Snapshot all 209 open issues + 12 bugs to `evidence/upstream-issues/` so the
triage is reproducible against a fixed point in time (issues change).
- **Gate:** journal exists; issue snapshot committed.

### P1 — Resilience boundary: R1–R5 (~3 days) ← **highest value, do first**
Implement panic recovery, per-collector timeout, and shared-fate isolation. This is what makes the
fork better than PNE, and it makes every upstream collector bug non-fatal.
- **Gate:** all of R1–R5 have a test that fails without the guard and passes with it. Demonstrate both
  directions — a guard whose test cannot fail is not a guard.

### P2 — Upstream bug triage (~3 days)
Work the HIGH/MED list: #1841, #1915, #1710, #1672, then LOW. Reproduce-or-refute each; disposition
each; record in the journal.
- **Gate:** every one of the 12 has a written disposition with evidence. Zero left "unknown."

### P3 — Scale + pressure (~4 days)
Scale the cluster (with cost approval), apply §4.2 pressures, observe §4.3 signals, compare against
PNE throughout.
- **Gate:** every pressure type executed; findings journaled with reproductions committed under
  `hack/pressure/`; a written statement of what was measured **and what was not**.

### P4 — Close the findings (~3 days)
Fix what P2/P3 found. Each fix gets a failing-then-passing test.
- **Gate:** no open finding without a fix, a mitigation, or a documented acceptance with rationale.

### P5 — Edge cases + quality (~2 days)
§5 adversarial tests, `go test -race`, linters, §6 code review pass.
- **Gate:** race detector clean; linters clean; branch coverage reviewed for untaken paths.

### P6 — Final evidence + submission readiness (~1 day)
Update `evidence/`, refresh the `CONTRIBUTING.md` compliance audit, produce a
**"why this is better than PNE"** summary grounded in P1–P4 results.
- **Gate:** the improvement claim is backed by specific evidence, or it is not made.

**~16 days of work.** Sequence P1 before P2 deliberately: resilience makes individual collector bugs
survivable, which changes the disposition of several of them from "must fix" to "safely contained."

---

## 9. Pre-registered predictions

Committed before measuring. If these are wrong, the model behind this plan is wrong.

| # | Prediction | Falsified by |
|---|---|---|
| PR1 | Pod churn reproduces netclass instability (#1841/#1915) on a busy node | Sustained churn shows no netclass failure or latency growth |
| PR2 | Without a guard, a panicking collector crashes the whole agent | Injected panic is already contained |
| PR3 | Scrape latency grows superlinearly with pod count for `netclass`/`netdev` | Latency stays flat to 400+ pods |
| PR4 | The agent's memory grows with interface cardinality and does not return after churn | Memory returns to baseline |
| PR5 | ≥3 of the 12 upstream bugs are non-reproducible on our config | Fewer than 3 refuted |
| PR6 | Pressure testing finds ≥1 issue absent from upstream's tracker | No novel findings after all pressures |
| PR7 | PNE degrades similarly under the same pressure (so findings are upstream, not our regression) | NMA degrades materially worse — indicates our integration is at fault |

**PR7 is the load-bearing one.** If NMA degrades worse than PNE under identical pressure, the
in-process design is the problem and that must be reported plainly, not buried.

---

## 10. Definition of done

1. All 12 upstream bugs have a written disposition with evidence; none "unknown."
2. R1–R5 implemented, each with a test demonstrated to fail without the guard.
3. Every §4.2 pressure executed on a scaled cluster, with side-by-side PNE comparison.
4. Every finding has a committed reproduction and a failing-then-passing test.
5. `go test -race` clean; linters clean; adversarial edge cases covered.
6. `JOURNAL.md` is a complete, honest record — including refutations and measurement errors.
7. A "better than PNE" claim that cites specific evidence, or an explicit statement that the fork is
   *equivalent but more resilient*, if that is what the data supports.
8. Cluster torn down or its cost explicitly accepted.

**Reporting discipline:** state what was tested and what was not. An unrun pressure type is a blind
spot and must be named as one. If the data does not support "better than PNE," say so — the honest
finding is more useful than the flattering one.
