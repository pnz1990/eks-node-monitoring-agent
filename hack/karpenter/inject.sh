#!/usr/bin/env bash
# Inject a deterministic Fatal NMA condition on a node, and observe whether it appears.
#
# MECHANISM (from e2e/suites/monitors/node_exporter.go): a privileged pod writes a KNOWN log line
# to a host path that a monitor scans. Deterministic, reversible, and needs no special hardware.
#
#   NetworkingReady / IPAMDNotReady  (FATAL)
#     echo 'Unable to reach API Server' >> /host/var/log/aws-routed-eni/ipamd.log
#     detection: monitors/networking/monitor.go:302
#
# WHY IPAMDNotReady IS THE PRIMARY PROBE: it is Fatal, it is one line, it needs no accelerator, and
# NetworkingReady carries a 30-minute Karpenter toleration -- so it exercises the exact path
# Karpenter acts on.
#
# DEADLINES ARE DERIVED FROM THE CODE, NOT GUESSED (monitors/networking/monitor.go):
#   interfaceMonitorPeriod = 5m   -> a 6-minute deadline for a log-scanned condition
# A test that waits 30s for a 5-minute monitor is a test that always passes.
#
# EVERY FAILURE PATH IS LOUD. A query that returns nothing is reported as a failure, never as a
# zero -- this project has repeatedly produced clean-looking results from checks that never ran.

set -uo pipefail

NODE="${1:-}"
DEADLINE="${DEADLINE:-360}"     # 6 min: 1 monitor period + margin
POLL="${POLL:-15}"

if [ -z "$NODE" ]; then
  echo "usage: $0 <node-name>   (env: DEADLINE=360 POLL=15)" >&2
  exit 2
fi

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# GUARD: credentials and node must be real BEFORE anything is measured.
kubectl get node "$NODE" >/dev/null 2>&1 || fail "node $NODE not found (or credentials are dead)"

# GUARD: an agent must actually be running on this node, or "condition never appeared" would be
# reported for a node that was never monitored -- a false negative that looks like a defect.
agent=$(kubectl get pods -n kube-system --field-selector "spec.nodeName=$NODE,status.phase=Running" \
          --no-headers 2>/dev/null | grep -cE 'nma-main-agent|eks-node-monitoring-agent|nma-nodep')
[ "${agent:-0}" -gt 0 ] || fail "no NMA agent Running on $NODE -- nothing would detect the fault"

echo "node    : $NODE"
echo "agents  : $agent"
echo "deadline: ${DEADLINE}s (interfaceMonitorPeriod=5m + margin)"

# Baseline: the condition must NOT already be false, or "it appeared" proves nothing.
before=$(kubectl get node "$NODE" -o jsonpath='{range .status.conditions[?(@.type=="NetworkingReady")]}{.status}/{.reason}{end}' 2>/dev/null)
echo "before  : NetworkingReady=${before:-<absent>}"
case "$before" in
  False/*) fail "NetworkingReady is ALREADY False ($before) -- cannot attribute a new condition" ;;
esac

pod="inject-ipamd-$RANDOM"
cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: $pod
  namespace: default
spec:
  restartPolicy: Never
  nodeName: $NODE
  tolerations: [{operator: Exists}]
  containers:
    - name: inject
      image: public.ecr.aws/amazonlinux/amazonlinux:2023-minimal
      securityContext: {privileged: true}
      command: ["/bin/sh","-c"]
      args:
        - |
          mkdir -p /host/var/log/aws-routed-eni
          echo 'Unable to reach API Server' | tee -a /host/var/log/aws-routed-eni/ipamd.log
      volumeMounts: [{name: host-root, mountPath: /host}]
  volumes: [{name: host-root, hostPath: {path: /}}]
EOF

for _ in $(seq 30); do
  st=$(kubectl get "pod/$pod" -n default -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$st" = "Succeeded" ] || [ "$st" = "Failed" ] && break
  sleep 2
done
if [ "$st" != "Succeeded" ]; then
  kubectl logs "$pod" -n default 2>&1 | tail -5 >&2
  kubectl delete "pod/$pod" -n default --wait=false >/dev/null 2>&1
  fail "injection pod did not succeed (phase=$st) -- the fault was never written"
fi
kubectl delete "pod/$pod" -n default --wait=false >/dev/null 2>&1
echo "injected: 'Unable to reach API Server' -> /var/log/aws-routed-eni/ipamd.log"

start=$(date -u +%s)
while :; do
  now=$(date -u +%s); el=$((now-start))
  cur=$(kubectl get node "$NODE" -o jsonpath='{range .status.conditions[?(@.type=="NetworkingReady")]}{.status}/{.reason}{end}' 2>/dev/null)
  case "$cur" in
    False/IPAMDNotReady)
      echo "RESULT  : DETECTED after ${el}s  (NetworkingReady=$cur)"
      echo "observed_latency_seconds=$el"
      exit 0 ;;
  esac
  if [ "$el" -ge "$DEADLINE" ]; then
    echo "RESULT  : NOT DETECTED within ${DEADLINE}s (last seen: NetworkingReady=${cur:-<absent>})"
    exit 1
  fi
  sleep "$POLL"
done
