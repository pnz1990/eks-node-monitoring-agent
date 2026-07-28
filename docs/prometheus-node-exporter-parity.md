# Prometheus node_exporter compatible metrics

The agent can serve host-level metrics with feature parity to
[prometheus/node_exporter](https://github.com/prometheus/node_exporter), so a
cluster does not need a separate node_exporter DaemonSet or the
`prometheus-node-exporter` EKS add-on.

The endpoint is **disabled by default**. Enabling it is an explicit opt-in.

## Why this lives in the agent

The agent already has everything node_exporter needs on every node: a DaemonSet
with `hostNetwork`, `hostPID`, the host root filesystem mounted at `/host`, and
the `system-node-critical` priority class. Reusing that footprint avoids running a
second privileged DaemonSet for the same data.

## Enabling the endpoint

### Helm

```yaml
nodeAgent:
  metrics:
    enabled: true
```

Full options:

```yaml
nodeAgent:
  metrics:
    # Enable the endpoint. Default: false.
    enabled: true
    # Listen port. 9100 is the node_exporter convention.
    port: 9100
    # Restrict which collectors run. Empty means the upstream default set,
    # which is what provides node_exporter parity.
    collectors: []
    # Upstream node_exporter flags.
    extraArgs:
      - "--no-collector.zfs"
      - "--collector.textfile.directory=/host/var/lib/node_exporter"
    # Include go_* and process_* metrics describing the agent itself.
    includeExporterMetrics: false
```

### Config file

The same settings can be provided directly at `/etc/nma/config.yaml`:

```yaml
metrics:
  enabled: true
  address: ":9100"
  collectors: []
  extraArgs: []
  includeExporterMetrics: false
monitors:
  kernel-monitor:
    enabled: true
```

## Scraping

With the endpoint enabled the agent listens on port `9100` on the node's host
network, so an existing node_exporter scrape configuration works unchanged.

For Prometheus Operator, a `PodMonitor` can be added through the chart's
`extraObjects`:

```yaml
extraObjects:
  - apiVersion: monitoring.coreos.com/v1
    kind: PodMonitor
    metadata:
      name: eks-node-monitoring-agent
      namespace: kube-system
    spec:
      selector:
        matchLabels:
          app.kubernetes.io/name: eks-node-monitoring-agent
      podMetricsEndpoints:
        - port: metrics
```

## Migrating from prometheus-node-exporter

Both listen on port `9100` and the agent uses the host network, so **they cannot
run on the same node at the same time**. Remove the existing exporter before
enabling this endpoint on a given node.

```bash
# 1. Remove the add-on (or the community Helm release)
aws eks delete-addon --cluster-name CLUSTER --addon-name prometheus-node-exporter
# or: helm uninstall prometheus-node-exporter -n prometheus-node-exporter

# 2. Confirm nothing holds port 9100, then enable the endpoint
helm upgrade eks-node-monitoring-agent ./charts/eks-node-monitoring-agent \
  --namespace kube-system --set nodeAgent.metrics.enabled=true

# 3. Verify the metric contract is intact
kubectl run -n kube-system parity-check --rm -i --restart=Never \
  --overrides='{"spec":{"hostNetwork":true}}' \
  --image=public.ecr.aws/docker/library/curlimages/curl:latest -- \
  curl -s http://127.0.0.1:9100/metrics | grep -c '^# TYPE'
```

Dashboards and recording rules need no change: metric names, types and labels are
identical, which is verified by `hack/parity-test.sh`.

## Verifying parity

Parity **cannot** be checked against a fixed list of metric names. Several
upstream names are generated at runtime from host state — for example
`node_memory_MemAvailable_bytes` appears nowhere in the node_exporter source,
because it is built from `/proc/meminfo` keys via `BuildFQName`. Two nodes with
different kernels or instance types therefore expose different name sets.

The only sound check is to run both exporters on the same host and diff the
result, which is what the harness does:

```bash
# Build both binaries, then:
hack/parity-test.sh \
  --upstream /path/to/node_exporter \
  --agent    /path/to/eks-node-monitoring-agent
```

Exit code `0` means the metric contract — every name, type and label key set — is
identical. A non-zero exit prints the diff, where `-` lines are present upstream
but missing from the agent.

The agent's `--metrics-only` flag serves just this endpoint, without joining a
cluster, so the comparison runs on any Linux host.

## Security considerations

Enabling this endpoint exposes an additional listener from a process that runs
`privileged` with `hostNetwork` and `hostPID`. That is why it is opt-in: when
disabled, no socket is bound at all.

Compared with the `prometheus-node-exporter` add-on, which runs unprivileged with
a read-only root filesystem and an optional `kube-rbac-proxy` sidecar, an
opted-in node serves metrics from a more privileged process. Consider whether
network policy or a node-local scrape path is appropriate for your environment
before enabling it on sensitive clusters.

## Collector coverage

All upstream collectors are available, including those with no meaning on a
typical EKS node (`zfs`, `bcachefs`, `infiniband`, `wifi`, `drbd`, `tapestats`).
They are included so the endpoint is a drop-in replacement rather than an
opinionated subset: a workload that scrapes any upstream metric keeps working.

Collectors that find no matching hardware simply export nothing, so the cost of
the unused ones is negligible. To trim the set explicitly, use `collectors` or
`extraArgs`:

```yaml
nodeAgent:
  metrics:
    enabled: true
    extraArgs:
      - "--no-collector.zfs"
      - "--no-collector.infiniband"
```
