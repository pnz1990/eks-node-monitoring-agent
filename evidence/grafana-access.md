# Grafana access — NMA vs PNE comparison dashboards

Live Grafana comparing the forked agent's metrics endpoint against upstream
prometheus-node-exporter, both running on the same nodes.

## Access

Everything runs in-cluster with `ClusterIP` services, so reach it with a
port-forward. Copy-paste from a clean shell:

```bash
# 1. Credentials (they expire; re-run if kubectl says Unauthorized)
ada credentials update --provider isengard --account 569190534191 --role Admin --once

# 2. Kubeconfig
aws eks update-kubeconfig --name nma-pne-parity-test --region us-west-2

# 3. Port-forward Grafana
kubectl port-forward -n monitoring svc/grafana 3000:80
```

Then open **http://localhost:3000**

| | |
|---|---|
| URL | http://localhost:3000 |
| Username | `admin` |
| Password | `nma-parity-admin` |

Dashboards are in the **"NMA vs PNE"** folder, or go direct:

| Dashboard | URL |
|---|---|
| Overview & Trust Verdict | http://localhost:3000/d/nma-pne-overview |
| Side by Side | http://localhost:3000/d/nma-pne-sidebyside |
| Delta & Trust Detail | http://localhost:3000/d/nma-pne-delta |

To reach Prometheus directly (for ad-hoc PromQL):

```bash
kubectl port-forward -n monitoring svc/prom-prometheus-server 9090:80
# http://localhost:9090
```

## Reading the verdict

**Start at the top panel of the Overview dashboard.** It is a computed verdict,
not a hand-set label:

| Panel reads | Meaning |
|---|---|
| 🟩 **METRICS TRUSTWORTHY** | V1 asymmetry = 0, V2 mismatches = 0, V3 ≥ 99% within 5%. The agent's metrics can be relied on in place of node_exporter's. |
| 🟧 **TRUSTWORTHY WITH DOCUMENTED EXCEPTIONS** | Structural and invariant checks pass, but a volatile gauge is outside tolerance. Check the Delta dashboard for which. |
| 🟥 **NOT TRUSTWORTHY — DISCREPANCIES DETECTED** | Metric names missing, an invariant mismatched, or counter rates diverged. **Do not rely on the agent's metrics in this state.** |

Beneath it, four per-tier panels attribute any failure to a specific tier:

- **V1 structural** — are the metric name sets identical? Counts must be 0.
- **V2 invariants** — do quantities that cannot change between scrapes match exactly? Must be 0.
- **V3 counter rates** — do `rate()` values agree within 5%? Must be ≥99%.

### What each dashboard is for

**Overview & Trust Verdict** — the verdict, the per-tier gates, scrape health, and
metric/series counts per job. This is the "is it OK?" page.

**Side by Side** — CPU, memory, filesystem, load, network and disk, with both
exporters overlaid on identical axes. Curves should be visually indistinguishable;
**visible separation means divergence.** This is the "show me, don't tell me" page.

**Delta & Trust Detail** — per-tier relative error over time with the tolerance
drawn as a threshold line. Includes a panel that deliberately surfaces the one
known divergence (`node_procs_running`) rather than hiding it.

## The verdict can actually fail — demonstrated

A green light that cannot turn red is decoration, not a verdict. This was proven
by inducing a real discrepancy: disabling the `cpu` and `meminfo` collectors on
the agent only, leaving upstream untouched.

```bash
# Induce (agent loses 51 metric names that upstream still exports)
helm upgrade eks-node-monitoring-agent ./charts/eks-node-monitoring-agent -n kube-system \
  --set nodeAgent.image.containerRegistry=569190534191.dkr.ecr.us-west-2.amazonaws.com \
  --set nodeAgent.image.tag=parity-dev \
  --set nodeAgent.metrics.enabled=true --set nodeAgent.metrics.port=9101 \
  --set 'nodeAgent.metrics.extraArgs={--no-collector.cpu,--no-collector.meminfo}'
kubectl rollout restart ds/eks-node-monitoring-agent -n kube-system
```

**Observed with the discrepancy induced:**

```
  V1 missing-from-NMA : 51.0
  V1 extra-in-NMA     : 0.0
  V2 mismatches       : 0.0
  V3 % within 5%      : 25.00
  VERDICT             : 0.0 -> RED — NOT TRUSTWORTHY
```

**Observed after restoring the healthy configuration** (drop `extraArgs`, restart,
wait ~15 min for `rate()` windows to refill):

```
  V1 missing-from-NMA : 0.0
  V1 extra-in-NMA     : 0.0
  V2 mismatches       : 0.0
  V3 % within 5%      : 100.00
  VERDICT             : 2.0 -> GREEN — METRICS TRUSTWORTHY
```

### This negative control caught a real bug in the dashboard

The first version of the V1 panel used `... unless count by(__name__)(...)`. That
reported **0 missing metrics even while 51 were genuinely absent**, because
`unless` compares full label sets and the `job` label differs between the two
series — so nothing ever matched and the difference was always empty.

The fix is the explicit matcher: `unless on(__name__) count by(__name__)(...)`.

Worth stating plainly: without running the negative control, the dashboard would
have shown a confident green light that was incapable of ever showing anything
else. Anyone re-deploying these dashboards should re-run the induced-discrepancy
check rather than trusting a green panel on sight.

## Caveats

- **Port 9101, not 9100.** Upstream PNE holds the conventional 9100 and the agent
  uses `hostNetwork`, so they cannot both bind it. The agent runs on 9101 **for
  this comparison only**; in production it takes over 9100 after PNE is removed.
- **Grafana has no persistence.** Dashboards are provisioned from the ConfigMap
  built out of `hack/grafana-dashboards/*.json`, so they survive a pod restart,
  but manual UI edits do not. Edit the JSON in the repo and re-apply.
- **`rate()` windows need time.** After any restart, wait ~15 minutes before
  trusting V3. A partially-filled window produces meaningless relative errors —
  an early measurement in this project reported 41% within tolerance purely
  because the exporters had only been running 5 minutes.

## Rebuilding the dashboards from the repo

```bash
cd hack/grafana-dashboards
kubectl create configmap grafana-parity-dashboards -n monitoring --from-file=. \
  --dry-run=client -o yaml \
  | yq '.metadata.labels.grafana_dashboard = "1"' \
  | kubectl apply -f -
# the Grafana sidecar picks the change up within ~60s
```
