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
#   scrape latency        MEASURED AS WALL TIME (curl's total time), not as the sum of
#                         node_scrape_collector_duration_seconds. See the warning below --
#                         summing that metric across concurrent collectors overstates cost
#                         by roughly the collector count, and this script used to do it.
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
#
# ---------------------------------------------------------------------------------------
# CORRECTED 2026-07-29: DO NOT SUM node_scrape_collector_duration_seconds
# ---------------------------------------------------------------------------------------
#
# An earlier version of this script summed that metric per variant and reported the total
# as "collection cost". That produced FINDING F-N7-1 -- "the dependency branch is ~15x
# slower than native, median ~250x" -- which was recorded as a real but unexplained
# performance gap (Q8). It was neither.
#
# THE METRIC MEASURES WALL TIME, AND EVERY COLLECTOR RUNS CONCURRENTLY. With fewer usable
# cores than collectors, each collector's timer keeps running while its goroutine is
# descheduled, so all of them report roughly the same wall-clock window -- the batch's,
# not their own. Summing N concurrent measurements of the same window multiplies the real
# cost by up to N.
#
# Demonstrated in pkg/metrics (Q8 investigation, GOMAXPROCS swept 1..8):
#
#     GOMAXPROCS=1   dep sum=0.5552s  min=0.010862s med=0.011322s max=0.011806s
#                        ^ a 1.09x spread across 49 collectors doing wildly different
#                          work. That is one shared window reported 49 times, not 49
#                          similar measurements.
#     GOMAXPROCS=8   dep sum=0.0620s  min=0.000028s med=0.001314s max=0.005501s
#
# The same 49 collectors run SERIALLY through the same resilient wrapper cost 0.0166s in
# total, against the 0.9114s this script once reported. A ~55x overstatement.
#
# And the decisive check: WALL time is nearly identical between the two implementations
# (0.0121s dep vs 0.0110s native at GOMAXPROCS=1). There was never a ~250x gap.
#
# So: latency is measured below as the wall time of the HTTP request. The summed metric is
# still reported, clearly labelled, because it is what a Prometheus user would naively
# compute and the label is what stops the next person repeating the mistake.
#
# WHAT WAS REAL: both agents are capped at cpu=250m (the CHART DEFAULT) and are genuinely
# CPU-throttled -- measured from the cgroup, nma-dep 5124 throttle periods / 337.9s
# throttled, nma-nodep 2007 / 140.1s, pne unlimited and never throttled. That is a real
# finding about the shipped default, and it is NOT what produced the numbers above:
# raising the limit 8x (250m -> 2 cores) moved the reported median from 7.72ms to 8.06ms,
# i.e. not at all.

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

  # -w '%{time_total}' captures the WALL time of the scrape, which is the honest cost
  # measure (see the header). Emitted on its own marker line so the parser cannot confuse
  # it with a metric.
  kubectl run "$pod" --restart=Never --image="$CURL_IMAGE" --command -- \
    sh -c "for p in 9100 9101 9102; do echo \"===PORT \$p===\"; curl -s -o /tmp/m --max-time 40 -w '===WALL \$p %{time_total}===\n' http://$ip:\$p/metrics; cat /tmp/m; done" \
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
    /^===WALL / { print $2" "$3 > (out"/"ph"-wall.txt"); next }
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
  printf '  %-11s %8s %8s %9s %9s %8s %9s\n' variant series failed "wall" "sum(cc)" panics timeouts
  for v in pne nma-dep nma-nodep; do
    local f="$OUT/$phase-$v.prom" port wall
    local series failed summed panics timeouts
    case "$v" in
      pne) port=9100 ;; nma-dep) port=9101 ;; nma-nodep) port=9102 ;;
    esac
    series=$(grep -cv '^#' "$f" || true)
    failed=$(awk '/^node_scrape_collector_success/ && $2==0' "$f" | wc -l)

    # WALL is the honest cost: one number per scrape, from the client. Falls back to n/a
    # rather than 0 if the marker is missing, because a 0 here would read as "instant".
    wall=$(awk -v p="$port" '$1==p {printf "%.4f", $2}' "$OUT/$phase-wall.txt" 2>/dev/null)
    [ -n "$wall" ] || wall="n/a"

    # sum(cc) is the SUM OF CONCURRENT per-collector durations, kept only because it is
    # what a naive query computes -- and labelled so nobody reads it as cost again. It
    # overstates by roughly the collector count; see the header.
    summed=$(awk '/^node_scrape_collector_duration_seconds/ {s+=$2} END {printf "%.4f", s+0}' "$f")

    panics=$(awk '/^node_collector_panics_total/ {s+=$2} END {print s+0}' "$f")
    timeouts=$(awk '/^node_collector_timeouts_total/ {s+=$2} END {print s+0}' "$f")
    printf '  %-11s %8s %8s %9s %9s %8s %9s\n' "$v" "$series" "$failed" "$wall" "$summed" "$panics" "$timeouts"
    echo "$phase $v $series $failed $wall $panics $timeouts $summed" >> "$OUT/measurements.txt"
  done
  log "    wall    = client-observed scrape time (the real cost)"
  log "    sum(cc) = sum of CONCURRENT per-collector durations; NOT a cost, overstates by ~N"
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
  log "  $v: series $b_series -> $p_series | failed $b_failed -> $p_failed | wall ${b_lat}s -> ${p_lat}s (${ratio}x)"

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

# COST COMPARISON, on wall time.
#
# The threshold is applied to WALL time and to wall time only. The previous version
# compared summed per-collector durations and asserted a 2.0x bound on them, which is how
# F-N7-1 ("native is ~19x faster") was produced -- an artefact of summing concurrent
# measurements, not a difference in cost. See the header.
#
# The bound is generous on purpose: a scrape crosses the kubelet network, so run-to-run
# variance of a few tens of milliseconds is normal and a tight bound would flap.
dep_lat=$(awk '$1=="pressure" && $2=="nma-dep" {print $5}' "$OUT/measurements.txt")
nodep_lat=$(awk '$1=="pressure" && $2=="nma-nodep" {print $5}' "$OUT/measurements.txt")
log ""
if [ "$dep_lat" != "n/a" ] && [ "$nodep_lat" != "n/a" ] && [ -n "$dep_lat" ] && [ -n "$nodep_lat" ]; then
  rel=$(awk -v a="$nodep_lat" -v b="$dep_lat" 'BEGIN{printf "%.2f", (b>0)? a/b : 0}')
  log "  native/upstream WALL-time ratio under pressure: ${rel}x"
  log "    (native runs 39 collectors, upstream 49, so <=1 is expected)"
  over=$(awk -v r="$rel" 'BEGIN{print (r > 2.0) ? 1 : 0}')
  [ "$over" -eq 1 ] && fail "the native variant takes ${rel}x the WALL time despite running FEWER collectors"
else
  # An absent wall measurement must not silently skip the only cost check in this script.
  fail "wall-time measurement missing (dep=$dep_lat nodep=$nodep_lat) -- no cost verdict possible"
fi

# The per-collector distribution, reported as DIAGNOSIS ONLY and never as a total. Its
# shape is the useful part: a SPREAD near 1.0 means the durations are all reporting the
# same concurrent window rather than each collector's own work, which is the signature of
# the artefact described in the header. That is why spread is printed and sum is not.
log ""
log "  per-collector duration distribution (diagnostic, NOT a cost -- see header):"
python3 - "$OUT/pressure-nma-dep.prom" "$OUT/pressure-nma-nodep.prom" <<'PYEOF' || true
import re, sys, statistics
def durs(p):
    out = {}
    try:
        for line in open(p):
            m = re.match(r'node_scrape_collector_duration_seconds\{collector="([^"]+)"\}\s+(\S+)', line)
            if m:
                out[m.group(1)] = float(m.group(2))
    except OSError:
        return {}
    return out
d, n = durs(sys.argv[1]), durs(sys.argv[2])
shared = sorted(set(d) & set(n))
if not shared:
    print("    no shared collectors -- nothing to diagnose")
    sys.exit(0)
for label, s in (("nma-dep", d), ("nma-nodep", n)):
    v = sorted(s[c] for c in shared)
    spread = (v[-1] / v[0]) if v[0] > 0 else float("inf")
    print(f"    {label:10s} min={v[0]:.6f}s med={statistics.median(v):.6f}s "
          f"max={v[-1]:.6f}s spread={spread:.1f}x")
    if spread < 3.0:
        print(f"      ^ SPREAD NEAR 1: these {len(v)} collectors do very different amounts of")
        print( "        work, so near-identical durations mean they are all reporting the")
        print( "        SAME concurrent window. Do not sum these. See the script header.")
PYEOF

log ""
if [ "$FAILED" -ne 0 ]; then
  log "VERDICT: problems under pressure -- see the FAIL lines above"
  exit 1
fi
log "VERDICT: all three variants held under pressure -- no new collector failures,"
log "         no panics, no timeouts, no restarts"
