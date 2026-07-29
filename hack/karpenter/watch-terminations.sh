#!/usr/bin/env bash
# Cluster-wide node-termination watcher, with the DISRUPTION REASON captured as it happens.
#
# WHY THIS EXISTS. In K5 part 1 a nodep node was terminated that I never injected a fault into, and
# by the time I looked, the events that would have explained it had aged out. So the record said "a
# node went away" with no cause -- which is exactly the shape of finding that cannot be
# investigated later.
#
# That gap blocks claim A5 ("zero spurious Fatal conditions"). A5 is only meaningful if EVERY
# termination has an attributable reason: an unexplained replacement is indistinguishable from a
# spurious repair, and reporting "no spurious Fatal observed" while a node vanished unexplained
# would be exactly the kind of clean-looking-but-empty result this project keeps producing.
#
# WHAT IT CAPTURES, per node, continuously:
#   - the node set, so a disappearance is detected within one poll
#   - Karpenter's own disruption events (reason: Underutilized / Empty / Drifted / Expired /
#     NodeRepair / Interruption) BEFORE the node is gone
#   - the NMA condition state at the moment of disappearance
#   - which variant pool it belonged to
#
# The reason matters more than the count: consolidation terminating an empty node is Karpenter
# working correctly, while NodeRepair on a node with no injected fault is the BLOCKER this whole
# goal exists to find. Both look identical in a node count.

set -uo pipefail

OUT="${OUT:-/tmp/karpenter-terminations}"
POLL="${POLL:-15}"
DURATION="${DURATION:-0}"     # 0 = until interrupted

mkdir -p "$OUT"
LOG="$OUT/terminations.log"
: > "$LOG"

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$LOG"; }

if ! kubectl get nodes >/dev/null 2>&1; then
  ada credentials update --provider isengard --account 569190534191 --role Admin --once >/dev/null 2>&1
  kubectl get nodes >/dev/null 2>&1 || { echo "FAIL: cluster unreachable; refusing to report an empty termination list" >&2; exit 1; }
fi

# snapshot returns "node<TAB>pool<TAB>conditions-not-True".
snapshot() {
  kubectl get nodes -o json 2>/dev/null | python3 -c '
import json,sys
NMA={"KernelReady","NetworkingReady","StorageReady","ContainerRuntimeReady","AcceleratedHardwareReady"}
try:
    d=json.load(sys.stdin)
except Exception:
    sys.exit(1)
for n in d.get("items",[]):
    name=n["metadata"]["name"]
    pool=n["metadata"].get("labels",{}).get("nma-variant","-")
    bad=[c["type"]+"="+c["status"]+"/"+c.get("reason","")
         for c in n.get("status",{}).get("conditions",[])
         if c["type"] in NMA and c["status"]!="True"]
    print(name+"\t"+pool+"\t"+(",".join(bad) if bad else "healthy"))
'
}

prev=$(snapshot)
[ -n "$prev" ] || { echo "FAIL: initial snapshot empty -- not proceeding, every later 'termination' would be spurious" >&2; exit 1; }
log "watching $(printf '%s\n' "$prev" | grep -c .) node(s); poll=${POLL}s"

start=$(date -u +%s)
while :; do
  sleep "$POLL"

  # Karpenter's disruption events, captured CONTINUOUSLY so they are recorded before they age out.
  kubectl get events -A --field-selector reason=DisruptionTerminating,reason=Unhealthy \
    -o jsonpath='{range .items[*]}{.involvedObject.name}{"\t"}{.reason}{"\t"}{.message}{"\n"}{end}' 2>/dev/null \
    >> "$OUT/disruption-events.raw" || true
  kubectl logs -n kube-system -l app.kubernetes.io/name=karpenter --since="${POLL}s" 2>/dev/null \
    | grep -iE 'disrupt|unhealthy|repair|deleted node|deleted nodeclaim' >> "$OUT/karpenter.raw" || true

  cur=$(snapshot)
  if [ -z "$cur" ]; then
    log "WARN: snapshot failed (transient API error?) -- skipping this poll rather than reporting every node terminated"
    continue
  fi

  # Disappeared nodes.
  while IFS=$'\t' read -r name pool conds; do
    [ -n "$name" ] || continue
    if ! printf '%s\n' "$cur" | cut -f1 | grep -qx "$name"; then
      log "TERMINATED $name pool=$pool last-conditions=[$conds]"
      # Attribute it. Karpenter names the node in its own log lines.
      reason=$(grep -h "$name" "$OUT/karpenter.raw" 2>/dev/null | grep -oiE 'NodeRepair|Underutilized|Empty|Drifted|Expired|Interruption' | tail -1)
      if [ -n "$reason" ]; then
        log "  reason=$reason"
      else
        log "  reason=UNATTRIBUTED  <-- investigate: this blocks claim A5"
      fi
      # The condition state at disappearance is the A5-relevant part: healthy + terminated means
      # something other than an NMA Fatal drove it.
      case "$conds" in
        healthy) log "  NOTE: node was HEALTHY per NMA conditions at termination -> not an NMA-driven repair" ;;
        *)       log "  NOTE: node had non-True NMA conditions [$conds] -> consistent with NMA-driven repair" ;;
      esac
    fi
  done <<< "$prev"

  # New nodes, so replacements are visible in the same record.
  while IFS=$'\t' read -r name pool _; do
    [ -n "$name" ] || continue
    printf '%s\n' "$prev" | cut -f1 | grep -qx "$name" || log "LAUNCHED   $name pool=$pool"
  done <<< "$cur"

  prev="$cur"

  if [ "$DURATION" -gt 0 ]; then
    now=$(date -u +%s)
    [ $((now-start)) -ge "$DURATION" ] && { log "watch complete (${DURATION}s)"; break; }
  fi
done
