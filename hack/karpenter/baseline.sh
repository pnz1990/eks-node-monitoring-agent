#!/usr/bin/env bash
# Capture the NMA->Karpenter contract as observed on one variant's NodePool.
#
# Run on stock `main` FIRST (K2). That ordering is the whole point: a finding that reproduces on
# main is PRE-EXISTING and must not be attributed to either fork. Running the forks first would
# make that distinction unrecoverable.
#
# WHAT IS CAPTURED, and why each matters for the claim in GOAL-KARPENTER-INTEGRATION.md §0:
#   condition TYPES   A1 -- the set must be identical across variants
#   condition VALUES  A5 -- any False on a healthy node is a spurious Fatal
#   Karpenter view    A6 -- NodeClaim conditions; a node Karpenter thinks is unhealthy
#   agent health      B5/B6 -- errors, panics, restarts
#   NodeDiagnostics   A7 -- one per live node, none orphaned
#
# EVERY QUERY IS GUARDED. A kubectl call that returns nothing is reported as a FAILURE, never as a
# zero or an empty set. This project has repeatedly produced clean-looking results from checks that
# never ran (expired credentials reporting "0 restarts"; a pressure run with no pressure that
# passed), so the guards are the load-bearing part of this script.

set -uo pipefail

VARIANT="${1:-}"
OUT="${OUT:-/tmp/karpenter-baseline}"

case "$VARIANT" in
  main)  DS_LABEL="nma-main-agent" ;;
  dep)   DS_LABEL="eks-node-monitoring-agent" ;;
  nodep) DS_LABEL="nma-nodep" ;;
  *) echo "usage: $0 <main|dep|nodep>" >&2; exit 2 ;;
esac

mkdir -p "$OUT"
log()  { printf '%s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAILED=1; }
FAILED=0

# --- credential + reachability guard, BEFORE any measurement ------------------
if ! kubectl get nodes >/dev/null 2>&1; then
  ada credentials update --provider isengard --account 569190534191 --role Admin --once >/dev/null 2>&1
  kubectl get nodes >/dev/null 2>&1 || { echo "FAIL: cluster unreachable and credential refresh failed -- ABORTING rather than reporting zeros" >&2; exit 1; }
fi

nodes=$(kubectl get nodes -l "nma-variant=$VARIANT" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
if [ -z "$nodes" ]; then
  echo "FAIL: no nodes carry label nma-variant=$VARIANT -- the pool is empty, so there is nothing to baseline" >&2
  exit 1
fi
n_nodes=$(printf '%s\n' "$nodes" | grep -c .)

log "variant : $VARIANT"
log "pool    : $n_nodes node(s)"
log "agent DS: $DS_LABEL"

# GUARD: >=5 nodes is required for the REPAIR-loop tier, because Karpenter skips repair above 20%
# unhealthy and 1-of-4 is 25%. Reported rather than failed, since condition-publication tiers are
# valid at any size -- but it must be visible, or a later "no replacement" is unattributable.
if [ "$n_nodes" -lt 5 ]; then
  log "  NOTE: <5 nodes. Condition-publication checks are valid, but a REPAIR test on this pool"
  log "        would be suppressed by Karpenter's 20%-unhealthy safety valve (1 of $n_nodes ="
  log "        $((100/n_nodes))%), and 'no replacement' would prove nothing."
fi

# --- agents present on every node --------------------------------------------
log ""
log "=== AGENT PRESENCE (a node with no agent is unmonitored, not healthy) ==="
missing=0
for n in $nodes; do
  st=$(kubectl get pods -n kube-system -l "app.kubernetes.io/name=$DS_LABEL" \
        --field-selector "spec.nodeName=$n" -o jsonpath='{.items[0].status.phase}' 2>/dev/null)
  printf '  %-50s %s\n' "${n##ip-}" "${st:-ABSENT}"
  [ "$st" = "Running" ] || { missing=$((missing+1)); }
done
[ "$missing" -eq 0 ] || fail "$missing node(s) have no Running agent -- hazard H1, and any condition result below is unattributable for those nodes"

# --- condition types and values ----------------------------------------------
log ""
log "=== NODE CONDITIONS (types = A1, values = A5) ==="
# Extracted with kubectl's own jsonpath rather than embedded Python. The first version used a
# python3 -c heredoc whose f-strings contained escaped quotes -- a SyntaxError, so the conditions
# file was written EMPTY and every type was reported "missing on 5 nodes". Five false FAILs from a
# broken extractor, which is the same failure family as every other harness bug in this project:
# the check did not run, and it reported something anyway.
#
# GUARD BELOW: the file must be non-empty before any conclusion is drawn from it.
: > "$OUT/$VARIANT.conditions"
for n in $nodes; do
  kubectl get node "$n" -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}' \
    >> "$OUT/$VARIANT.conditions" 2>/dev/null
done

lines=$(grep -c . "$OUT/$VARIANT.conditions" 2>/dev/null || echo 0)
if [ "${lines:-0}" -eq 0 ]; then
  echo "FAIL: condition extraction produced NO lines for $n_nodes node(s). Every 'missing type'" >&2
  echo "      verdict below would be an artefact of the extractor, not a finding. ABORTING." >&2
  exit 1
fi
log "  extracted $lines condition line(s) from $n_nodes node(s)"

# Truncated per run: appending across runs made the optional-count lookup return multiple values
# and the verdict print "0\n0/5".
: > "$OUT/$VARIANT.optional-counts"

# The five NMA types Karpenter names in its repair policy, from pkg/conditions/conditions.go
# cross-checked against Karpenter's doc (JOURNAL K0.1).
#
# AcceleratedHardwareReady IS EXPECTED TO BE ABSENT on non-accelerated instances. Measured on this
# cluster: absent on all 11 nodes (t3.large), on both MNG and Karpenter pools, while the nvidia and
# neuron monitors DO register ("monitor available"). So the monitor runs and correctly publishes
# nothing when there is no device -- the same success-with-no-data contract the metrics work
# established for absent hardware.
#
# It is therefore listed as OPTIONAL rather than required. Treating it as required produced a FAIL
# on a correctly-behaving agent, which would have made the baseline unusable as a comparison point.
# It is still CHECKED: if it appears, it must be True, and its presence/absence must MATCH across
# variants (that cross-variant check is A1's job, in the three-way comparison).
NMA_TYPES_REQUIRED="ContainerRuntimeReady KernelReady NetworkingReady StorageReady"
NMA_TYPES_OPTIONAL="AcceleratedHardwareReady"

for t in $NMA_TYPES_REQUIRED; do
  cnt=$(awk -F'\t' -v t="$t" '$1==t' "$OUT/$VARIANT.conditions" | wc -l)
  bad=$(awk -F'\t' -v t="$t" '$1==t && $2!="True"' "$OUT/$VARIANT.conditions" | wc -l)
  printf '  %-26s present on %s/%s node(s), %s not-True\n' "$t" "$cnt" "$n_nodes" "$bad"
  [ "$cnt" -eq "$n_nodes" ] || fail "$t missing on $((n_nodes-cnt)) node(s) -- Karpenter's repair policy names this type, so its absence means no repair signal"
  if [ "$bad" -gt 0 ]; then
    awk -F'\t' -v t="$t" '$1==t && $2!="True" {print "      -> "$1"="$2" ("$3")"}' "$OUT/$VARIANT.conditions"
    fail "$t is not True on $bad node(s) with no injected fault -- candidate SPURIOUS FATAL (A5)"
  fi
done

for t in $NMA_TYPES_OPTIONAL; do
  cnt=$(awk -F'\t' -v t="$t" '$1==t' "$OUT/$VARIANT.conditions" | wc -l)
  bad=$(awk -F'\t' -v t="$t" '$1==t && $2!="True"' "$OUT/$VARIANT.conditions" | wc -l)
  if [ "$cnt" -eq 0 ]; then
    printf '  %-26s absent (expected: no accelerator on this instance type)\n' "$t"
  else
    printf '  %-26s present on %s/%s node(s), %s not-True\n' "$t" "$cnt" "$n_nodes" "$bad"
    [ "$bad" -eq 0 ] || fail "$t is not True on $bad node(s) with no injected fault -- candidate SPURIOUS FATAL (A5)"
  fi
  # Recorded for the cross-variant comparison: the COUNT must match across variants even when 0.
  echo "$t $cnt" >> "$OUT/$VARIANT.optional-counts"
done

# --- Karpenter's own view ----------------------------------------------------
log ""
log "=== KARPENTER VIEW (A6: does Karpenter consider these nodes healthy?) ==="
claims=$(kubectl get nodeclaims -l "karpenter.sh/nodepool=nma-$VARIANT" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
if [ -z "$claims" ]; then
  fail "no NodeClaims for nodepool nma-$VARIANT -- either the pool is not Karpenter-managed or the query failed; not reporting this as healthy"
else
  for c in $claims; do
    s=$(kubectl get nodeclaim "$c" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {end}' 2>/dev/null)
    printf '  %-22s %s\n' "$c" "$s"
    case "$s" in *"Ready=False"*) fail "NodeClaim $c is Ready=False";; esac
  done
fi

# --- agent health -------------------------------------------------------------
log ""
log "=== AGENT HEALTH (B5 errors/panics, B6 restarts) ==="
restarts=$(kubectl get pods -n kube-system -l "app.kubernetes.io/name=$DS_LABEL" \
  -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' 2>/dev/null \
  | awk '{s+=$1} END {print s+0}')
pods=$(kubectl get pods -n kube-system -l "app.kubernetes.io/name=$DS_LABEL" --no-headers 2>/dev/null | wc -l)
if [ "$pods" -eq 0 ]; then
  fail "agent pod query returned nothing -- restart count of '$restarts' would be meaningless"
else
  log "  pods=$pods  restarts=$restarts"
  [ "${restarts:-0}" -eq 0 ] || fail "$restarts agent restart(s)"
fi

errs=0
for p in $(kubectl get pods -n kube-system -l "app.kubernetes.io/name=$DS_LABEL" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
  e=$(kubectl logs "$p" -n kube-system 2>/dev/null | grep -icE '"level":"(ERROR|FATAL)"|\bpanic\b' || true)
  errs=$((errs+e))
done
log "  ERROR/FATAL/panic log lines: $errs"
[ "$errs" -eq 0 ] || fail "$errs error/panic line(s) in agent logs"

# --- NodeDiagnostics ----------------------------------------------------------
log ""
log "=== NodeDiagnostic CRs (A7: none orphaned) ==="
nds=$(kubectl get nodediagnostics -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c . || true)
live=$(kubectl get nodes --no-headers 2>/dev/null | wc -l)
log "  nodediagnostics=$nds  live nodes=$live"
orphans=0
for nd in $(kubectl get nodediagnostics -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null); do
  kubectl get node "$nd" >/dev/null 2>&1 || { log "    ORPHAN: $nd (no such node)"; orphans=$((orphans+1)); }
done
[ "$orphans" -eq 0 ] || fail "$orphans orphaned NodeDiagnostic(s) -- A7"

# --- verdict ------------------------------------------------------------------
log ""
if [ "$FAILED" -ne 0 ]; then
  log "VERDICT[$VARIANT]: problems found -- see FAIL lines"
  exit 1
fi
# Wording matters here: an earlier version claimed "all 5 condition types present" while
# AcceleratedHardwareReady was legitimately absent. Overstating a pass is how a later reader
# concludes something was verified that was not.
ahr=$(awk '$1=="AcceleratedHardwareReady" {print $2}' "$OUT/$VARIANT.optional-counts" 2>/dev/null)
log "VERDICT[$VARIANT]: contract intact -- the 4 required NMA condition types present and True"
log "                   on all $n_nodes node(s); AcceleratedHardwareReady on ${ahr:-0}/$n_nodes"
log "                   (absent is expected without an accelerator). Karpenter sees every"
log "                   NodeClaim Ready. 0 restarts, 0 error/panic lines, 0 orphaned"
log "                   NodeDiagnostics."
log ""
log "                   NOT tested here: repair EXECUTION. This is condition publication only."
