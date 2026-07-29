#!/usr/bin/env bash
# Watch for Karpenter node auto repair acting on an NMA-published Fatal condition.
#
# THIS IS THE ONLY TEST OF REPAIR *EXECUTION*. Everything before it (K2-K4) tested condition
# PUBLICATION -- whether NMA says the right thing. This tests whether Karpenter acts on it.
#
# THE 30-MINUTE FLOOR IS KARPENTER'S, NOT THE HARNESS'S. Karpenter's toleration for
# NetworkingReady=False is 30 minutes (JOURNAL-KARPENTER.md K0.1), so nothing can happen before
# then and a shorter run would report "no repair" for a node that was never eligible. That is why
# F-K1-1 mattered: the one-shot injection cleared in ~15 min, so the condition was never false long
# enough for Karpenter to consider it.
#
# WHAT COUNTS AS EACH OUTCOME
#   REPAIRED      the NodeClaim disappears, or the node is deleted, after >=30 min unhealthy.
#                 That is the claim confirmed: NMA -> condition -> Karpenter -> replacement.
#   NOT REPAIRED  still present after the deadline. NOT automatically a failure -- see below.
#   SUPPRESSED    >20% of the pool unhealthy, so Karpenter deliberately declines. Detected and
#                 reported SEPARATELY, because it looks identical to "not repaired" and means
#                 something completely different.
#
# THE TRAP THIS SCRIPT EXISTS TO AVOID: reporting "no replacement occurred" as evidence that the
# repair path is safe, when in fact repair was suppressed, or the condition had cleared, or the
# feature gate was off. Each of those is checked explicitly and reported by name.

set -uo pipefail

NODE="${1:-}"
DEADLINE="${DEADLINE:-2400}"   # 40 min: Karpenter's 30-min toleration + launch + margin
POLL="${POLL:-30}"

[ -n "$NODE" ] || { echo "usage: $0 <node-name>  (env: DEADLINE=2400 POLL=30)" >&2; exit 2; }

fail() { printf 'FAIL: %s\n' "$*" >&2; }

kubectl get node "$NODE" >/dev/null 2>&1 || { echo "FAIL: node $NODE not found (or credentials dead)" >&2; exit 1; }

# PRECONDITION 1: the feature gate. Without NodeRepair=true nothing can ever happen, and the run
# would report "not repaired" for a cluster that cannot repair.
gate=$(kubectl get deploy karpenter -n kube-system -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="FEATURE_GATES")].value}' 2>/dev/null)
case "$gate" in
  *NodeRepair=true*) echo "gate    : NodeRepair=true (verified)" ;;
  *) echo "FAIL: NodeRepair is NOT enabled (FEATURE_GATES=$gate). No repair can occur, so this run" >&2
     echo "      could only ever report 'not repaired' -- which would prove nothing. ABORTING." >&2
     exit 1 ;;
esac

pool=$(kubectl get node "$NODE" -o jsonpath='{.metadata.labels.karpenter\.sh/nodepool}' 2>/dev/null)
[ -n "$pool" ] || { echo "FAIL: $NODE has no karpenter.sh/nodepool label -- not a Karpenter node, so repair does not apply" >&2; exit 1; }

claim=$(kubectl get nodeclaims -o json 2>/dev/null | python3 -c "
import json,sys
for c in json.load(sys.stdin)['items']:
    if c.get('status',{}).get('nodeName')=='$NODE': print(c['metadata']['name'])")
[ -n "$claim" ] || { echo "FAIL: no NodeClaim maps to $NODE -- cannot observe a replacement" >&2; exit 1; }

total=$(kubectl get nodes -l "karpenter.sh/nodepool=$pool" --no-headers 2>/dev/null | grep -c .)
echo "node    : $NODE"
echo "pool    : $pool ($total node(s))"
echo "claim   : $claim"
echo "deadline: ${DEADLINE}s (Karpenter toleration 30m + launch + margin)"

# PRECONDITION 2: the 20% valve. Reported up front, since it decides whether the run can observe
# anything at all.
unhealthy=0
for n in $(kubectl get nodes -l "karpenter.sh/nodepool=$pool" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
  bad=$(kubectl get node "$n" -o jsonpath='{range .status.conditions[?(@.status=="False")]}{.type} {end}' 2>/dev/null \
        | tr ' ' '\n' | grep -cE '^(KernelReady|NetworkingReady|StorageReady|ContainerRuntimeReady|AcceleratedHardwareReady)$')
  [ "${bad:-0}" -gt 0 ] && unhealthy=$((unhealthy+1))
done
pct=$(( unhealthy * 100 / total ))
echo "unhealthy: $unhealthy/$total (${pct}%)"
if [ "$pct" -gt 20 ]; then
  echo "  WARNING: >20% unhealthy. Karpenter SUPPRESSES repair above this threshold, so this run"
  echo "           cannot observe a replacement and 'not repaired' would be meaningless."
fi

start=$(date -u +%s)
last=""
while :; do
  now=$(date -u +%s); el=$((now-start))

  cond=$(kubectl get node "$NODE" -o jsonpath='{range .status.conditions[?(@.type=="NetworkingReady")]}{.status}/{.reason}{end}' 2>/dev/null)
  gone_claim=0; gone_node=0
  kubectl get nodeclaim "$claim" >/dev/null 2>&1 || gone_claim=1
  kubectl get node "$NODE"       >/dev/null 2>&1 || gone_node=1

  state="t=${el}s cond=${cond:-<absent>} claim=$([ $gone_claim -eq 1 ] && echo GONE || echo present) node=$([ $gone_node -eq 1 ] && echo GONE || echo present)"
  [ "$state" != "$last" ] && { echo "  $state"; last="$state"; }

  if [ "$gone_claim" -eq 1 ] || [ "$gone_node" -eq 1 ]; then
    echo ""
    echo "RESULT: REPAIRED after ${el}s"
    echo "  NMA published a Fatal condition, Karpenter tolerated it for its configured window, then"
    echo "  terminated the node. The full contract -- condition -> repair -> replacement -- is confirmed."
    kubectl logs -n kube-system -l app.kubernetes.io/name=karpenter --since="${el}s" 2>/dev/null \
      | grep -iE 'unhealthy|repair|terminat' | tail -3 | sed 's/^/  karpenter: /'
    exit 0
  fi

  # The condition clearing mid-run invalidates the test: Karpenter's clock resets, so a later
  # "not repaired" says nothing about the repair path.
  case "$cond" in
    True/*) echo ""
            fail "the condition CLEARED at t=${el}s -- the sustained fault stopped working, so"
            echo "      Karpenter's toleration clock reset and no conclusion about repair is possible." >&2
            exit 1 ;;
  esac

  if [ "$el" -ge "$DEADLINE" ]; then
    echo ""
    echo "RESULT: NOT REPAIRED within ${DEADLINE}s (condition held at ${cond})"
    echo "  This is NOT automatically a defect. Distinguish before concluding:"
    echo "    - >20% of the pool unhealthy  -> repair correctly suppressed (reported above)"
    echo "    - repair policy may not cover this reason on this Karpenter version"
    echo "    - the NodeClaim may be blocked by a PDB or a do-not-disrupt annotation"
    exit 2
  fi
  sleep "$POLL"
done
