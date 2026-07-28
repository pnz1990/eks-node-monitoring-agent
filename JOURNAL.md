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
