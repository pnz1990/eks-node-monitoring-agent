#!/usr/bin/env bash
# N7: stress and load across all three variants, measured on the same nodes.
#
# WHAT THIS IS FOR, AND WHAT IT IS NOT
#
# The point is NOT to prove the cluster survives load -- it is to drive collector code
# paths an idle cluster never reaches, and to compare the three variants under the SAME
# pressure so a finding is attributable to the collectors rather than to the load.
#
# The pressure manifests already exist (hack/pressure/) and target specific failure modes:
#   01-pod-churn      -> netclass/netdev: an interface appearing and disappearing
#                        MID-SCRAPE. This is upstream #1915, the one defect reachable in
#                        the field on EKS, and the one the native port FIXES rather than
#                        contains.
#   02-cpu-saturation -> cpu/loadavg/stat/pressure under contention; scrape latency
#   03-mount-churn    -> filesystem: whether the pod-mount exclusion holds under churn
#
# WHAT IS MEASURED, and why each matters
#
#   scrape latency        node_scrape_collector_duration_seconds, summed per variant.
#                         The native variant has no reason to be slower -- it reads the
#                         same files -- so a large gap means a structural problem.
#   series stability       total series before vs during. A collector that loses devices
#                         under churn shows here and nowhere else.
#   collector failures     node_scrape_collector_success == 0 count. Must not GROW under
#                         pressure: that is the difference between "absent hardware" and
#                         "broke when it got busy".
#   panics and timeouts    the resilience counters. Non-zero under pressure is the single
#                         most important signal in this whole run.
#   restarts               an escaped panic restarts the container rather than logging.
#
# CRITICALLY: the three are measured in the SAME scrape pass, from one pod, so a
# difference cannot be an artefact of measuring them minutes apart under different load.

set -euo pipefail

NAMESPACE="${NAMESPACE:-kube-system}"
OUT="${OUT:-/tmp/three-way-stress}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.5.0}"
PRESSURE_DIR="${PRESSURE_DIR:-hack/pressure}"
SETTLE="${SETTLE:-60}"

mkdir -p "$OUT"
log() { printf '%s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAILED=1; }
FAILED=0

node=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
ip=$(kubectl get node "$node" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')

# ---------------------------------------------------------------------------
# measurement
# ---------------------------------------------------------------------------

# scrape_all captures all three endpoints in one pod, so the samples are simultaneous.
scrape_all() {
  local phase="$1"
  local pod="stress-scrape-$phase-$RANDOM"

  kubectl run "$pod" --restart=Never --image="$CURL_IMAGE" --command -- \
    sh -c "for p in 9100 9101 9102; do echo \"===PORT \$p===\"; curl -s --max-time 40 http://$ip:\$p/metrics; done" \
    >/dev/null 2>&1

  for _ in $(seq 90); do
    local st
    st=$(kubectl get "pod/$pod" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    [ "$st" = "Succeeded" ] || [ "$st" = "Failed" ] && break
    sleep 2
  done

  kubectl logs "$pod" > "$OUT/$phase-raw.txt" 2>&1
  kubectl delete "pod/$pod" --wait=false >/dev/null 2>&1 || true

  awk -v out="$OUT" -v ph="$phase" '
    /^===PORT 9100===$/ { f=out"/"ph"-pne.prom"; next }
    /^===PORT 9101===$/ { f=out"/"ph"-nma-dep.prom"; next }
    /^===PORT 9102===$/ { f=out"/"ph"-nma-nodep.prom"; next }
    f { print > f }
  ' "$OUT/$phase-raw.txt"

  # GUARD: an unusable scrape must stop the run rather than be reported as "0 series,
  # which is a big difference". A stress result computed from a failed scrape is worse
  # than no result.
  for v in pne nma-dep nma-nodep; do
    if ! grep -q '^node_cpu_seconds_total' "$OUT/$phase-$v.prom" 2>/dev/null; then
      fail "$phase/$v: not a valid scrape (no node_cpu_seconds_total)"
      return 1
    fi
  done
  return 0
}

report() {
  local phase="$1"
  log ""
  log "  --- $phase ---"
  printf '  %-11s %8s %8s %9s %8s %9s\n' variant series failed "latency" panics timeouts
  for v in pne nma-dep nma-nodep; do
    local f="$OUT/$phase-$v.prom"
    local series failed latency panics timeouts
    series=$(grep -cv '^#' "$f" || true)
    failed=$(awk '/^node_scrape_collector_success/ && $2==0' "$f" | wc -l)
    # Summed rather than maxed: the max is one slow collector, the sum is the scrape's
    # total collection cost, which is what a scrape interval has to accommodate.
    latency=$(awk '/^node_scrape_collector_duration_seconds/ {s+=$2} END {printf "%.4f", s+0}' "$f")
    panics=$(awk '/^node_collector_panics_total/ {s+=$2} END {print s+0}' "$f")
    timeouts=$(awk '/^node_collector_timeouts_total/ {s+=$2} END {print s+0}' "$f")
    printf '  %-11s %8s %8s %9s %8s %9s\n' "$v" "$series" "$failed" "$latency" "$panics" "$timeouts"
    echo "$phase $v $series $failed $latency $panics $timeouts" >> "$OUT/measurements.txt"
  done
}

: > "$OUT/measurements.txt"

# ---------------------------------------------------------------------------
# baseline
# ---------------------------------------------------------------------------

log "node: $node ($ip)"
log ""
log "=== BASELINE (no pressure) ==="
scrape_all baseline || exit 1
report baseline

baseline_pods=$(kubectl get pods -A --no-headers | wc -l)
log ""
log "  cluster: $baseline_pods pods"

# ---------------------------------------------------------------------------
# apply pressure
# ---------------------------------------------------------------------------

log ""
log "=== APPLYING PRESSURE ==="
applied=()
for m in "$PRESSURE_DIR"/01-pod-churn.yaml "$PRESSURE_DIR"/03-mount-churn.yaml "$PRESSURE_DIR"/02-cpu-saturation.yaml; do
  [ -f "$m" ] || { log "  skip (absent): $m"; continue; }
  if kubectl apply -f "$m" >/dev/null 2>&1; then
    log "  applied: $(basename "$m")"
    applied+=("$m")
  else
    log "  FAILED to apply: $(basename "$m")"
  fi
done

# CLEANUP ON EXIT, including on failure. Leaving a cpu-saturation DaemonSet running on a
# billed cluster after the script dies is the kind of thing that is only noticed on the
# invoice.
cleanup() {
  log ""
  log "=== CLEANUP ==="
  for m in "${applied[@]:-}"; do
    [ -n "$m" ] && kubectl delete -f "$m" --wait=false >/dev/null 2>&1 && \
      log "  deleted: $(basename "$m")"
  done
}
trap cleanup EXIT

log ""
log "  settling for ${SETTLE}s so the pressure is actually in effect when measured..."
sleep "$SETTLE"

pressure_pods=$(kubectl get pods -A --no-headers | wc -l)
log "  cluster: $pressure_pods pods (was $baseline_pods)"

# A pressure run where the pressure did not materialise proves nothing. Reported rather
# than failed, because pod-churn is transient by design and the count can legitimately be
# similar at the instant it is sampled.
if [ "$pressure_pods" -le "$baseline_pods" ]; then
  log "  NOTE: pod count did not rise. Pod churn is transient, so this may be a sampling"
  log "        artefact -- but if CPU saturation also failed to schedule, the 'under"
  log "        pressure' numbers below are really a second baseline."
fi

# ---------------------------------------------------------------------------
# under pressure
# ---------------------------------------------------------------------------

log ""
log "=== UNDER PRESSURE ==="
scrape_all pressure || exit 1
report pressure

# ---------------------------------------------------------------------------
# verdict
# ---------------------------------------------------------------------------

log ""
log "=== ANALYSIS ==="

for v in pne nma-dep nma-nodep; do
  b_series=$(awk -v v="$v" '$1=="baseline" && $2==v {print $3}' "$OUT/measurements.txt")
  p_series=$(awk -v v="$v" '$1=="pressure" && $2==v {print $3}' "$OUT/measurements.txt")
  b_failed=$(awk -v v="$v" '$1=="baseline" && $2==v {print $4}' "$OUT/measurements.txt")
  p_failed=$(awk -v v="$v" '$1=="pressure" && $2==v {print $4}' "$OUT/measurements.txt")
  b_lat=$(awk -v v="$v" '$1=="baseline" && $2==v {print $5}' "$OUT/measurements.txt")
  p_lat=$(awk -v v="$v" '$1=="pressure" && $2==v {print $5}' "$OUT/measurements.txt")
  p_panics=$(awk -v v="$v" '$1=="pressure" && $2==v {print $6}' "$OUT/measurements.txt")
  p_timeouts=$(awk -v v="$v" '$1=="pressure" && $2==v {print $7}' "$OUT/measurements.txt")

  ratio=$(awk -v a="$p_lat" -v b="$b_lat" 'BEGIN{printf "%.2f", (b>0)? a/b : 0}')
  log "  $v: series $b_series -> $p_series | failed $b_failed -> $p_failed | latency ${b_lat}s -> ${p_lat}s (${ratio}x)"

  # A collector that FAILS only under pressure is the finding this whole run exists to
  # surface: it distinguishes "absent hardware" from "broke when it got busy".
  if [ "$p_failed" -gt "$b_failed" ]; then
    fail "$v: collector failures rose under pressure ($b_failed -> $p_failed)"
  fi
  # Panics and timeouts are the resilience boundary reporting that it had to intervene.
  [ "${p_panics:-0}" != "0" ] && fail "$v: $p_panics collector panic(s) under pressure"
  [ "${p_timeouts:-0}" != "0" ] && fail "$v: $p_timeouts collector timeout(s) under pressure"
done

log ""
log "  restarts (an escaped panic restarts rather than logging):"
for pair in "eks-node-monitoring-agent nma-dep" "nma-nodep nma-nodep"; do
  set -- $pair
  r=$(kubectl get pods -l "app.kubernetes.io/name=$1" -n "$NAMESPACE" \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' 2>/dev/null \
    | awk '{s+=$1} END {print s+0}')
  log "    $2: $r"
  [ "$r" -gt 0 ] && fail "$2 restarted $r time(s) under pressure"
done

# The native variant must not be materially slower than the dependency one: it reads the
# same files through the same procfs library, so a large gap would mean a structural
# problem rather than a measurement artefact.
#
# MEASURED RESULT (2026-07-29, 2x t3.large, 70 pods under pressure): the native variant is
# ~19x FASTER over the 39 SHARED collectors -- 0.0135s versus 0.2578s. That is the opposite
# direction from the concern, and it is large enough to need an explanation rather than a
# celebration. See FINDING F-N7-1 in the journal: the dependency branch's relay channel is
# UNBUFFERED, and its per-collector duration floor is ~3.5ms against pne's ~0.009ms. A
# standalone benchmark attributes only ~4x to the channel, so the floor is only PARTLY
# explained -- the remainder is not yet established and is recorded as open rather than
# guessed at.
dep_lat=$(awk '$1=="pressure" && $2=="nma-dep" {print $5}' "$OUT/measurements.txt")
nodep_lat=$(awk '$1=="pressure" && $2=="nma-nodep" {print $5}' "$OUT/measurements.txt")
log ""
if [ -n "$dep_lat" ] && [ -n "$nodep_lat" ]; then
  rel=$(awk -v a="$nodep_lat" -v b="$dep_lat" 'BEGIN{printf "%.2f", (b>0)? a/b : 0}')
  log "  native/upstream collection-time ratio under pressure: ${rel}x"
  log "    (native runs 39 collectors, upstream 49, so <1 is expected)"
  over=$(awk -v r="$rel" 'BEGIN{print (r > 2.0) ? 1 : 0}')
  [ "$over" -eq 1 ] && fail "the native variant takes ${rel}x as long despite running FEWER collectors"

  # The comparison that is actually apples-to-apples: the 39 collectors BOTH run. Comparing
  # totals credits the native variant for simply running 10 fewer, which is scope rather
  # than performance.
  log ""
  log "  over the 39 SHARED collectors only (apples to apples):"
  python3 - "$OUT/pressure-nma-dep.prom" "$OUT/pressure-nma-nodep.prom" <<'PYEOF' || true
import re, sys
def durs(p):
    out = {}
    for line in open(p):
        m = re.match(r'node_scrape_collector_duration_seconds\{collector="([^"]+)"\}\s+(\S+)', line)
        if m:
            out[m.group(1)] = float(m.group(2))
    return out
d, n = durs(sys.argv[1]), durs(sys.argv[2])
shared = sorted(set(d) & set(n))
sd, sn = sum(d[c] for c in shared), sum(n[c] for c in shared)
print(f"    {len(shared)} shared: nma-dep={sd:.4f}s  nma-nodep={sn:.4f}s"
      f"  ratio={sn/sd:.3f}x" if sd else "    no data")
print(f"    per-collector floor: nma-dep min={min(d.values()):.6f}s  "
      f"nma-nodep min={min(n.values()):.6f}s")
PYEOF
fi

log ""
if [ "$FAILED" -ne 0 ]; then
  log "VERDICT: problems under pressure -- see the FAIL lines above"
  exit 1
fi
log "VERDICT: all three variants held under pressure -- no new collector failures,"
log "         no panics, no timeouts, no restarts"
