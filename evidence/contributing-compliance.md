# CONTRIBUTING.md compliance audit

Every requirement from `CONTRIBUTING.md`, `.github/PULL_REQUEST_TEMPLATE.md` and
`.github/workflows/pr-check.yaml`, with the command or artifact that proves it.

Verified at `4dec1b5`, base `origin/main` @ `950440d`.

## CONTRIBUTING.md — Contributing via Pull Requests

| # | Requirement | Status | Proof |
|---|---|---|---|
| C1 | Working against latest `main` | **PASS** | `git rev-list --count HEAD..origin/main` → `0` |
| C2 | Checked existing/merged PRs for duplicate work | **PASS** | See "Prior art" below |
| C3 | Issue opened to discuss significant work | **PENDING** | Must be filed before the upstream PR |
| C4 | Repository forked | **PASS** | `pnz1990/eks-node-monitoring-agent` |
| C5 | Focused diff, no unrelated reformatting | **PASS** | 31 files, **5 deletions total**, all intentional (see below) |
| C6 | Local tests pass | **PASS** | `make test` → 34 packages ok, 0 FAIL, chart lint 0 failed |
| C7 | Clear, incremental commit messages | **PASS** | 13 commits, logically scoped, not squashed |
| C8 | PR template fully answered | **PENDING** | Draft prepared; filled when the PR is opened |
| C9 | CI failures addressed | **PENDING** | Requires the PR to exist |
| C10 | Apache-2.0 confirmation retained | **PENDING** | Template line kept in the PR body |
| C11 | No security issue filed publicly | **PASS** | Posture change discussed as a design tradeoff only; no vulnerability disclosed |

### C5 detail — every deletion in the diff

Only 5 lines are removed across the whole change, and each is a deliberate edit:

```
-            {{- if .Values.nodeAgent.monitors }}        daemonset.yaml  (volumeMount condition widened)
-        {{- if .Values.nodeAgent.monitors }}            daemonset.yaml  (volume condition widened)
-{{- if .Values.nodeAgent.monitors }}                    monitor-configmap.yaml (render condition widened)
-      {{- toYaml .Values.nodeAgent.monitors | nindent 6 }}  monitor-configmap.yaml (moved under `with`)
-	github.com/prometheus/common v0.70.1 // indirect     go.mod (promoted to direct)
```

No file appears in the diff because of formatting. `gofmt -l` is clean on every
touched Go file.

### Prior art checked (C2)

| Item | State | Relevance |
|---|---|---|
| PR #62 — "Add PodMonitor support for Prometheus Operator" | CLOSED | Closed as *subsumed by #140*, not rejected on principle |
| PR #140 — "add extraObjects support" | MERGED | The mechanism this change reuses for an optional `PodMonitor` |
| PR #177 — "Collect NPA metric and report as Node condition" | MERGED | Adds a condition, not a metrics endpoint |
| PR #182 — architecture docs | OPEN | Unrelated |
| Issue #61 | CLOSED | Closed alongside #62 |

**No existing or merged PR implements node_exporter parity**, so this is not
duplicate work. Worth noting for the PR conversation: upstream has already
accepted Prometheus-adjacent work (#140), which is a favourable precedent.

## PR template fields

| Field | Prepared content |
|---|---|
| `Issue #` | C3 issue number, once filed |
| `Description of changes` | Opt-in node_exporter compatible endpoint; in-process vs sidecar rationale; default-off reasoning; port 9100 convention and the migration constraint |
| `Testing Done` | Unit coverage 100% on `pkg/metrics`; parity harness empty diff; live EKS 1.36 e2e; V1–V4 cross-validation; verdict negative control |
| Apache-2.0 line | Retained verbatim |

## CI gates (`pr-check.yaml`)

| # | Gate | Status | Proof |
|---|---|---|---|
| C12 | `hack/check-generate.sh` — no codegen drift | **PASS** | `make generate` then `git diff` → empty |
| C13 | `make test` — Go tests + `helm-lint` | **PASS** | 34 ok / 0 FAIL; `1 chart(s) linted, 0 chart(s) failed` |
| C14 | Go version from `go.mod` | **PASS** | built with `go1.26.4`, matching the `go` directive |

## Coverage requirements

| # | Requirement | Status | Proof |
|---|---|---|---|
| C15 | 100% statements on new packages | **PASS** | `pkg/metrics` → `total: 100.0%` |
| C16 | 100% on new symbols in modified packages | **PASS** | `MetricsSettings.IsEnabled` 100.0%, `IsMetricsEnabled` 100.0%, `GetMetricsSettings` 100.0%. `pkg/config` totals 92.2% because of **pre-existing** untested code, not anything added here. |
| C17 | `.covignore` not used to inflate coverage | **PASS** | `git diff origin/main..HEAD -- .covignore` → **0 lines changed** |
| C18 | Error paths covered, not just happy paths | **PASS** | Tests cover flag-parse failure, collector-construction failure, version-collector conflict, node-collector conflict, listen failure, serve failure, shutdown failure, and `ErrServerClosed` treated as clean |
| C19 | No regressions in existing tests | **PASS** | 34 packages pass, 0 failures |
| C20 | e2e coverage on a real cluster | **PASS** | 3/3 metrics features pass on EKS 1.36 (`evidence/e2e-metrics.log`) |
| C21 | Negative controls — tests can fail | **PASS** | Parity harness detects a 256-series delta when a collector is removed; dashboard verdict observed RED (V1=51 missing, V3=25%) then GREEN |

### How 100% was reached without touching `.covignore`

`pkg/metrics` exposes injected seams (`listen`, `serve`, `shutdown`, and the
construction helpers `resolveFunc`/`collectorFunc`/`registerFunc`) purely so the
error branches are genuinely executable in tests. The alternative — marking files
`// IGNORE TEST COVERAGE` and regenerating `.covignore` — would have produced the
same number while testing less. The repo's existing exclusions are reserved for
code that needs real hardware (`dcgm_client.go`, `ipamd/*`), and that precedent
was left intact.

## Outstanding before the upstream PR

C3, C8, C9, C10 all require the PR/issue to exist on `aws/eks-node-monitoring-agent`.
Per `CONTRIBUTING.md` the issue must come **first**:

> "You open an issue to discuss any significant work - we would hate for your time to be wasted."

Opening that issue is a public post to an AWS-owned repository, so it needs the
maintainer's explicit go-ahead rather than being done unilaterally.
