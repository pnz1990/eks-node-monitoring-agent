# Pressure tests

Reproductions for the pressure testing in `GOAL-PRODUCTION-READINESS.md` §4.

Each manifest targets a specific collector failure mode. The point is not to
generate load for its own sake but to drive the code paths that an idle cluster
never reaches — and to compare the agent against upstream
prometheus-node-exporter under the *same* pressure, so a finding can be
attributed to our integration rather than to upstream.

## Prerequisites

Both exporters co-resident and scraped by one Prometheus, as set up in
`evidence/cross-validation.md`:

```bash
ada credentials update --provider isengard --account 569190534191 --role Admin --once
aws eks update-kubeconfig --name nma-pne-parity-test --region us-west-2
```

## The tests

| Manifest | Pressure | Targets | Expected signal |
|---|---|---|---|
| `01-pod-churn.yaml` | rapid pod create/delete | `netclass`, `netdev`, `filesystem` | **#1841 / #1915**: interface appears/disappears mid-scrape. Highest-value test — this is the failure mode reproduced deterministically in `JOURNAL.md`. |
| `02-cpu-saturation.yaml` | all cores pinned | `cpu`, `loadavg`, `stat`, `pressure` | scrape latency under contention; whether the agent's own CPU limit throttles collection |
| `03-mount-churn.yaml` | many volumes mounted/unmounted | `filesystem` | stale mount handling; validates the pod-ephemeral mount exclusion holds under churn |
| `04-scrape-storm.yaml` | many concurrent scrapers | endpoint | `MaxRequestsInFlight` behaviour: 503 vs queue vs collapse |

## What to observe

Per `GOAL-PRODUCTION-READINESS.md` §4.3, watch the agent itself, not only its
output. Leak indicators are the point:

```promql
# goroutine leak (monotonic growth is the signal, not the absolute value)
go_goroutines{job="nma"}

# memory leak
process_resident_memory_bytes{job="nma"}

# fd leak — NMA has shipped fd-leak bugs before (CHANGELOG: "Fix auto mode
# diagnostics fd leaks"), so this is a known-plausible failure mode
process_open_fds{job="nma"} / process_max_fds{job="nma"}

# which collector degrades first, and under which pressure
topk(10, node_scrape_collector_duration_seconds{job="nma"})

# intermittent collector failure (flapping)
changes(node_scrape_collector_success{job="nma"}[10m])

# the resilience boundary actually firing
node_collector_panics_total
node_collector_timeouts_total

# cardinality growth — the defect class already found once
count(node_filesystem_size_bytes{job="nma"})
count(node_network_up{job="nma"})

# the agent's OTHER mission must not degrade
# (NodeCondition reporting is what EKS node auto repair consumes)
problem_condition_count
```

Always run the same query for `job="pne"`. If both degrade equally the finding is
upstream behaviour; if only `nma` degrades, our integration is at fault. That
attribution is prediction **PR7** and it is the load-bearing one.

## Running

```bash
kubectl apply -f hack/pressure/01-pod-churn.yaml
# observe for >=15 min so rate() windows fill; a partially-filled window
# produces meaningless numbers (see JOURNAL.md 2026-07-28T21:15Z)
kubectl delete -f hack/pressure/01-pod-churn.yaml
```

Record findings in `JOURNAL.md` with real values, not summaries.
