# GOAL — A node_exporter-compatible metrics implementation with no node_exporter dependency

**Type:** executable goal file. Invoked via `/goal`. Do not execute on read.
**Owner:** rrroizma (EKS, addons org)
**Created:** 2026-07-29
**Branch:** `feat/metrics-no-upstream-dependency` (new, branched from
`feat/prometheus-node-exporter-parity` so all tests and validation carry over)
**Journal:** `JOURNAL-NO-DEPENDENCY.md` — append-only, updated as work happens.
**Slack:** post updates to `#eks-nma-pne-parity` (`C0BLA2BN4AF`), robot emoji prefix, report only
what changed.

## Preconditions (done, reuse — do not rebuild)

The dependency-based branch is complete and evidenced: opt-in `/metrics` at 100% unit coverage, parity
298/298 against upstream `v1.12.1`, resilience boundary, live e2e on EKS 1.36, cross-validation
harness, Grafana trust-verdict dashboards, pressure manifests, 9 bugs found and fixed.

**Everything in that branch is reusable and most of it should be reused verbatim** — `hack/parity-test.sh`,
the V1–V4 cross-validation method, the pressure manifests, the Grafana dashboards, the resilience layer,
the adversarial edge cases. The new work is the *collector implementation*, not the validation apparatus.

---

## 1. Objective

Implement the host-metrics endpoint with **no build-time or runtime dependency on
`github.com/prometheus/node_exporter`**, while keeping metric output identical to it. Upstream becomes
*reference material* — read it, learn from it, credit it — not a `go.mod` entry.

Then have **all three** running on one cluster simultaneously and compare them:

| Deployment | What it is | Port |
|---|---|---|
| `pne` | upstream prometheus-node-exporter, unmodified | 9100 |
| `nma-dep` | the current branch: NMA importing `node_exporter/collector` | 9101 |
| `nma-nodep` | this goal: NMA with its own collector implementation | 9102 |

Success is **three-way parity** on the metric contract, plus a documented comparison of metrics *and
logs* under stress, with any issue found in **either** of our two versions fixed.

### 1.1 Read this before planning the work: the dependency you cannot remove by vendoring

Measured on the pinned trees:

| Component | Non-test lines | Role |
|---|---|---|
| `node_exporter/collector` (all platforms) | 26,075 | flag registration, per-collector `Update()`, metric descriptors |
| `node_exporter/collector` (linux only) | 19,636 | the subset we'd need |
| **the 49 collectors we actually enable** | **~10,653** across 59 files | the realistic scope |
| **`prometheus/procfs`** | **21,639** across 13 packages | **the actual `/proc` and `/sys` parsing** |

**54 of 152 collector files import `prometheus/procfs`.** That matters more than the headline number:
node_exporter's collectors are largely a *thin metric-descriptor layer* over `procfs`, which does the
real parsing. Vendoring `collector/` while keeping `procfs` removes the smaller half of the code and
leaves the harder half as an external dependency.

**So "no dependency" has three possible meanings, and the goal must pick one explicitly:**

| Interpretation | Removes | Keeps | Vendored lines |
|---|---|---|---|
| **A — drop node_exporter only** | `node_exporter` | `procfs`, `client_golang`, kingpin-free | ~10.6k |
| **B — drop node_exporter and procfs** | both | `client_golang` only | ~32k |
| **C — clean-room reimplementation** | both, no code copied | `client_golang` | write from scratch |

**Decision: pursue A, and state the residual dependency honestly.** Rationale:
- `prometheus/procfs` is a *general-purpose, well-tested `/proc` parsing library* under the Prometheus
  org. Depending on it is not the same as depending on a competing exporter — it is the same class of
  dependency as `client_golang`, which we keep regardless because Prometheus metric types come from it.
- Interpretation B doubles the vendored surface for little strategic gain, and re-implementing
  `/proc/mountinfo` and `/sys` parsing is exactly where subtle, silent bugs live.
- Interpretation C forfeits the one thing that makes this tractable: upstream's parsing logic is
  battle-tested across thousands of deployments, and reproducing it blind would guarantee divergence.

**If the intent behind the ask is specifically "no code from a competing exporter in our supply chain,"
A satisfies it. If the intent is "zero Prometheus-org dependencies," A does not, and that must be
surfaced as a finding rather than glossed over.** Flag this in the first Slack update and confirm
before Phase 3 begins.

### 1.2 What "no dependency" buys, and what it costs

Be honest about both, because this decision is a trade rather than an improvement.

**Gains**
- **No upstream version coupling.** Today a `node_exporter` release can change our metric output; after
  this, it cannot.
- **We can fix upstream bugs directly** instead of containing them. The netclass all-or-nothing failure
  (#1915) becomes a two-line fix in our tree rather than a timeout we hide behind.
- **Smaller attack/audit surface** — we ship only the 49 collectors that do anything on EKS, not 90+
  including ZFS, WiFi, DRBD, and four BSD variants.
- **No kingpin.** The `sync.Once` global-flag hack (`collectors.go`) disappears entirely; flags become
  native `pflag`, matching the rest of the agent.

**Costs**
- **We own ~10.6k lines of `/proc` parsing** we did not write, forever. Upstream fixes no longer arrive
  for free; someone must watch their tracker and port.
- **Parity becomes a maintenance burden, not a one-time proof.** Upstream adds metrics; we drift unless
  the parity harness runs in CI against a pinned upstream.
- **Apache-2.0 attribution obligations** on every copied file, plus NOTICE updates.
- **This is a fork of a fork.** Contributing back upstream becomes harder, not easier — which cuts
  against the original motivation for the dependency-based approach.

**State this trade in the design doc and in the final Slack summary.** A recommendation that only lists
gains is not a recommendation.

---

## 2. Deliverables

1. New branch `feat/metrics-no-upstream-dependency` from the current branch.
2. `pkg/hostmetrics/` — our collector implementation, no `node_exporter` import anywhere.
3. `go.mod` with **zero** `github.com/prometheus/node_exporter` entry (verified mechanically).
4. Attribution: per-file provenance headers, `NOTICE` update, `docs/attribution.md`.
5. Native `pflag` configuration replacing the kingpin bridge.
6. **Three-way** parity harness — `hack/parity-test-3way.sh`.
7. All existing tests carried over and passing against the new implementation.
8. Three-way deployment on the cluster, with dashboards extended to a third job.
9. Stress/load comparison of **metrics and logs** across all three.
10. `docs/design/no-dependency-approach.md` — design, trade-offs, and a recommendation between the two.
11. `JOURNAL-NO-DEPENDENCY.md` — continuous.

---

## 3. Scope: which collectors, and how they get here

**Port only the 49 collectors observed enabled on a live EKS node** (measured, not guessed):

```
arp bcache bcachefs bonding btrfs conntrack cpu cpufreq diskstats dmi dmmultipath
edac entropy fibrechannel filefd filesystem hwmon infiniband ipvs kernel_hung
loadavg mdadm meminfo netclass netdev netstat nfs nfsd nvme os powersupplyclass
pressure rapl schedstat selinux sockstat softnet stat tapestats textfile
thermal_zone time timex udp_queues uname vmstat watchdog xfs zfs
```

Plus the shared framework: registry, `Update()` dispatch, `--path.*` handling, `ErrNoData`, the
per-collector meta metrics.

**Deliberately NOT ported:** every non-Linux variant, and the default-disabled collectors that never
run on EKS (`supervisord`, `runit`, `systemd`, `wifi`, `drbd`, `perf`, `buddyinfo`, …). This is the
"opinionated subset" decision the first goal deferred — and it must be recorded as a **known parity
limitation**, because a customer who enables `--collector.buddyinfo` today would lose it.

> **Parity target changes here, and that must be stated plainly.** The dependency-based branch achieves
> parity on *all* upstream collectors including default-disabled ones. This branch cannot, by design.
> The honest claim is **"parity on the 49 collectors that run on EKS; the remaining upstream collectors
> are not implemented."** Record every unimplemented collector in `docs/parity-exceptions-nodep.md`.

### Porting method — per collector, in this order

1. Read upstream's implementation as reference.
2. Write our version in `pkg/hostmetrics/`, using `procfs` where upstream does.
3. **Copy upstream's test fixtures verbatim** — they are the ground truth for parsing behaviour, and
   re-deriving them would be both wasteful and less trustworthy.
4. Port upstream's unit tests, adapted to our package.
5. Run the three-way parity harness for that collector before moving on.

**Do not port in bulk and validate at the end.** One collector at a time, each validated, each
committed. A 10.6k-line commit that fails parity is undebuggable.

---

## 4. Attribution — a hard requirement, not a formality

Both projects are Apache-2.0, so this is permitted. It is not permitted to be silent about it.

- Every file derived from upstream gets a header naming the source file and the upstream commit
  (`b401dcfc`), retaining upstream's copyright line alongside ours.
- `NOTICE` gains an entry for the derived work, plus upstream's own MIT-licensed transitive
  attributions if any survive the port.
- `docs/attribution.md` maps our file → upstream file → what changed and why.

**Gate:** no file lands without provenance. A reviewer must be able to diff any of our collectors
against its upstream original.

---

## 5. Reuse from the existing branch — explicit inventory

| Artifact | Action |
|---|---|
| `hack/parity-test.sh` | **extend** to three-way; keep the stale-port and `build_info` guards |
| V1–V4 cross-validation method | **reuse**, add third job |
| `hack/pressure/*.yaml` | **reuse as-is** |
| Grafana dashboards | **extend** — third job on every panel; verdict spans all three |
| `pkg/metrics/resilience.go` | **reuse** — panic/timeout containment applies identically |
| `pkg/metrics/edgecases_test.go` | **reuse** — adversarial inputs are implementation-independent |
| `pkg/metrics/server.go` | **reuse** — listener, landing page, lifecycle unchanged |
| Chart opt-in plumbing | **extend** — a switch selecting which implementation serves |
| `evidence/`, `JOURNAL.md` | **keep**, new journal for this branch |

**The validation apparatus is the most valuable thing carried over.** It already caught 9 bugs; it will
be the thing that proves or disproves this branch.

---

## 6. Phases and gates

### N0 — Branch, journal, and the interpretation decision (~0.5 day)
Create the branch and journal. Post the §1.1 interpretation question to Slack and **get an answer
before N3.**
- **Gate:** branch exists; journal seeded; interpretation A confirmed or redirected.

### N1 — Three-way harness first (~1 day)
Extend the parity harness and dashboards to three jobs *before* writing any collector, so every
increment is measurable from the first commit.
- **Gate:** `hack/parity-test-3way.sh` runs with `nma-nodep` stubbed out and correctly reports it as
  failing. **A harness that cannot fail the new implementation is useless** — prove it fails first.

### N2 — Framework, no collectors (~2 days)
`pkg/hostmetrics/`: registry, dispatch, `--path.*`, `ErrNoData`, meta metrics, native pflag config.
Reuse `resilience.go` and `server.go`.
- **Gate:** endpoint serves only `node_scrape_collector_*` and `node_exporter_build_info`-equivalent;
  `go.mod` has no `node_exporter`; race and lint clean.

### N3 — Port collectors in priority order (~8–10 days)
Order by dashboard value: `cpu meminfo filesystem diskstats netdev netstat loadavg stat vmstat uname
os time pressure` → then the rest.
- **Per-collector gate:** fixtures copied, tests ported, three-way parity green for that collector,
  committed individually.
- **Phase gate:** all 49 at parity; unimplemented collectors recorded in
  `docs/parity-exceptions-nodep.md`.

### N4 — Dependency removal, verified mechanically (~0.5 day)
```bash
grep -rn "prometheus/node_exporter" --include="*.go" --include="go.mod" --include="go.sum" . && exit 1
go mod tidy && go mod why github.com/prometheus/node_exporter   # must report not needed
```
- **Gate:** both checks pass; build and full suite green.

### N5 — Three-way deployment (~1 day)
All three on the cluster: `pne` 9100, `nma-dep` 9101, `nma-nodep` 9102. One Prometheus, three jobs,
identical scrape interval.
- **Gate:** three targets up; all three scraped; dashboards render all three.

### N6 — Three-way validation, metrics AND logs (~2 days)
Run every existing test against all three, then the V1–V4 tiers pairwise:
`pne↔nma-dep`, `pne↔nma-nodep`, `nma-dep↔nma-nodep`.

**Logs are explicitly in scope** and were never compared before:
- error/warn rate per implementation
- new or missing error strings
- repeated messages indicating a hot loop
- startup log differences
- **any log line one implementation emits and another does not**

- **Gate:** V1 exact set equality within the implemented scope · V2 exact value equality · V3 ≥99%
  within 5% · V4 documented tolerance · log comparison written up with any asymmetry explained.

### N7 — Stress and load across all three (~2 days)
Reuse `hack/pressure/`. Requires cost approval to scale — ask, do not assume.

Compare across all three: scrape latency per collector, memory, goroutines, fds, series counts, panics
and timeouts contained, and **NMA's `DetectionDelay`** for both of ours.

- **Gate:** every pressure type run against all three; results tabulated; **any issue found in *either*
  of our versions fixed**, not just the new one. If `nma-dep` has a latent bug that only shows under
  three-way comparison, that is a finding and it gets fixed on that branch too.

### N8 — Design doc, recommendation, final Slack summary (~1 day)
`docs/design/no-dependency-approach.md`: the interpretation decision, what was ported, attribution,
measured results, and **a recommendation between the two approaches** with the trade stated in both
directions.
- **Gate:** the recommendation cites measurements, names what is worse, and does not present the trade
  as a pure win.

**~18–20 days.** N3 dominates and is the risk.

---

## 7. Pre-registered predictions

| # | Prediction | Falsified by |
|---|---|---|
| ND1 | Three-way parity is achievable on the 49 collectors | any collector that cannot match without copying code verbatim |
| ND2 | `nma-nodep` is **faster** than `nma-dep` (no kingpin, fewer registered collectors) | equal or slower scrape latency |
| ND3 | `nma-nodep` uses **less memory** than `nma-dep`'s 65MB | equal or more |
| ND4 | Porting reveals ≥1 upstream bug not visible from the outside | clean port, no defects found |
| ND5 | ≥1 collector diverges subtly on first port and is caught by fixtures, not by review | all collectors parity-clean first try |
| ND6 | Log output will differ across all three, and PNE↔ours will differ most | logs match closely |

**ND5 is the one to watch.** If every collector ports cleanly first time, the fixtures are probably not
being exercised properly — suspect the harness before believing the result.

---

## 8. Definition of done

1. `go.mod` has no `node_exporter`, verified mechanically.
2. All 49 collectors at three-way parity; unimplemented ones documented.
3. Every carried-over test passes against the new implementation.
4. All three deployed and compared: metrics **and** logs.
5. Stress/load run on all three; issues fixed in **both** our versions.
6. Attribution complete and reviewable.
7. Coverage ≥ the current 100% on new packages, `.covignore` untouched.
8. `-race`, `staticcheck`, `gofmt`, `go vet` clean; no codegen drift.
9. Design doc with a recommendation that names the costs.
10. Journal complete, including refutations and measurement errors.
11. Slack updated throughout.

**Reporting discipline:** this branch is a *trade*, not an upgrade. If the measurements show
`nma-nodep` is slower, heavier, or less complete than `nma-dep`, say so and recommend against it. The
honest comparison is the deliverable — not a predetermined conclusion that removing the dependency was
the right call.
