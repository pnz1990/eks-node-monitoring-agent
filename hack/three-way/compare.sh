#!/usr/bin/env bash
# N6: three-way comparison of pne (:9100), nma-dep (:9101) and nma-nodep (:9102).
#
# WHAT THIS COMPARES, AND WHY IN THIS ORDER
#
# The three endpoints run on the SAME node, scraped within seconds of each other, so the
# only variables are the collector implementations. Three tiers, cheapest first:
#
#   T1 STRUCTURAL   exact metric-NAME set equality. A missing or extra family is a
#                   contract break regardless of values.
#   T2 COLLECTOR    node_scrape_collector_success per collector. This is where the
#                   "hwmon must FAIL" inversion gets checked -- a variant that made it
#                   succeed would pass T1 and be wrong.
#   T3 SERIES       per-family series COUNTS. Catches a family that exists but has lost
#                   a device or CPU, which T1 cannot see.
#
# VALUES ARE NOT COMPARED HERE. Two scrapes seconds apart legitimately differ on every
# counter, and on this cluster a rate() comparison needs a Prometheus window rather than
# two curls -- that is the V3/V4 tier of the dependency branch's harness, driven from
# Grafana. Comparing raw values across scrapes would produce noise indistinguishable
# from a real defect, which is worse than not comparing them.
#
# THE POSITIVE CONTROL IS pne vs nma-dep. That pair was already measured at 298/298 on
# the dependency branch, so if THIS harness reports them as differing, the harness is
# wrong rather than the code. A comparison that cannot be shown to work on a
# known-passing pair proves nothing about the pair under test.
#
# EXPECTED, NOT-A-DEFECT DIFFERENCES, each verified rather than assumed:
#
#   1. promhttp_metric_handler_requests_total / _requests_in_flight exist on pne only.
#      These are promhttp's own HANDLER instrumentation, added by
#      promhttp.InstrumentMetricHandler, which upstream's node_exporter calls and this
#      agent does not. They describe the scrape endpoint, not the node -- so their absence
#      is a difference in agent instrumentation, not in host metrics. Both agents DO have
#      promhttp_metric_handler_errors_total, which comes from HandlerFor itself.
#      Excluded, and if the agent should adopt InstrumentMetricHandler that is a separate
#      decision recorded in the design doc.
#
#   2. node_filesystem_readonly / _device_error: pne 25 series, both agents 4.
#      This is the EKS mount-point exclusion working as designed. pne reports per-pod
#      mounts -- volume-subpaths/config/grafana/0, containerd sandbox shm, one per pod
#      with a unique UID -- which is unbounded churn cardinality. The agents report only
#      real filesystems: / /boot/efi /run /tmp. Measured and deliberate, documented in
#      docs/parity-exceptions.md on the dependency branch.
#
#   3. nma-dep and nma-nodep both add node_collector_{panics,timeouts}_total (the
#      resilience boundary); pne has no equivalent.
#
#   4. nma-nodep reports 39 collectors, the other two 49. The 10 absent are the
#      out-of-scope set: bcache bcachefs bonding fibrechannel ipvs nfs nfsd rapl
#      tapestats zfs.
#
#   5. node_scrape_collector_duration_seconds values differ by construction.

set -euo pipefail

NAMESPACE="${NAMESPACE:-kube-system}"
OUT="${OUT:-/tmp/three-way}"
CURL_IMAGE="${CURL_IMAGE:-curlimages/curl:8.5.0}"

mkdir -p "$OUT"

log() { printf '%s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAILED=1; }
FAILED=0

# ---------------------------------------------------------------------------
# scrape
# ---------------------------------------------------------------------------

node=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
ip=$(kubectl get node "$node" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')
log "node: $node ($ip)"

# One pod, three curls, so the scrapes are as close together in time as possible.
# Separate pods would add pod-startup latency between them, and on a counter-heavy
# endpoint that widens every legitimate difference for no reason.
log "scraping all three endpoints from a single pod..."
pod="threeway-scrape-$RANDOM"
kubectl run "$pod" --restart=Never --image="$CURL_IMAGE" --command -- \
  sh -c "for p in 9100 9101 9102; do echo \"===PORT \$p===\"; curl -s --max-time 30 http://$ip:\$p/metrics; done" \
  >/dev/null 2>&1

kubectl wait --for=condition=Ready=false --timeout=120s "pod/$pod" >/dev/null 2>&1 || true
for _ in $(seq 60); do
  phase=$(kubectl get "pod/$pod" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
  [ "$phase" = "Succeeded" ] || [ "$phase" = "Failed" ] && break
  sleep 2
done

kubectl logs "$pod" > "$OUT/raw.txt" 2>&1
kubectl delete "pod/$pod" --wait=false >/dev/null 2>&1 || true

# Split the combined output back into three files.
awk -v out="$OUT" '
  /^===PORT 9100===$/ { f=out"/pne.prom"; next }
  /^===PORT 9101===$/ { f=out"/nma-dep.prom"; next }
  /^===PORT 9102===$/ { f=out"/nma-nodep.prom"; next }
  f { print > f }
' "$OUT/raw.txt"

for f in pne nma-dep nma-nodep; do
  if [ ! -s "$OUT/$f.prom" ]; then
    log "FAIL: $f produced no output; the scrape did not work, so no verdict is possible"
    exit 1
  fi
  lines=$(grep -cv '^#' "$OUT/$f.prom" || true)
  log "  $f: $lines series"
done

# GUARD: a scrape that returned an error page rather than metrics would otherwise be
# compared as "a very small metric set" and reported as a huge difference.
for f in pne nma-dep nma-nodep; do
  if ! grep -q '^node_cpu_seconds_total' "$OUT/$f.prom"; then
    log "FAIL: $f has no node_cpu_seconds_total -- this is not a valid scrape"
    exit 1
  fi
done

# ---------------------------------------------------------------------------
# extract
# ---------------------------------------------------------------------------

names() { grep -v '^#' "$1" | sed 's/[{ ].*//' | sort -u; }
counts() { grep -v '^#' "$1" | sed 's/[{ ].*//' | sort | uniq -c | awk '{print $2" "$1}' | sort; }
collectors() {
  grep '^node_scrape_collector_success{' "$1" \
    | sed 's/.*collector="\([^"]*\)".* \(.*\)/\1 \2/' | sort
}

for f in pne nma-dep nma-nodep; do
  names "$OUT/$f.prom" > "$OUT/$f.names"
  counts "$OUT/$f.prom" > "$OUT/$f.counts"
  collectors "$OUT/$f.prom" > "$OUT/$f.collectors"
done

# Metrics that are expected to differ and are excluded from the NAME comparison, each
# with its reason. Everything else must match exactly.
cat > "$OUT/expected-extra.txt" <<'EXPECTED'
node_collector_panics_total
node_collector_timeouts_total
promhttp_metric_handler_requests_total
promhttp_metric_handler_requests_in_flight
EXPECTED

# Families whose SERIES COUNT is expected to differ, with the reason. Kept separate from
# the name exclusions above: these families exist on all three, so T1 must still compare
# them -- only their cardinality is expected to differ.
cat > "$OUT/expected-count-diff.txt" <<'EXPECTED'
node_filesystem_readonly
node_filesystem_device_error
EXPECTED

# ---------------------------------------------------------------------------
# T1 structural: exact name-set equality
# ---------------------------------------------------------------------------

log ""
log "=== T1 STRUCTURAL: metric name sets ==="

compare_names() {
  local a="$1" b="$2" label="$3"
  local only_a only_b
  only_a=$(comm -23 "$OUT/$a.names" "$OUT/$b.names" | grep -vxF -f "$OUT/expected-extra.txt" || true)
  only_b=$(comm -13 "$OUT/$a.names" "$OUT/$b.names" | grep -vxF -f "$OUT/expected-extra.txt" || true)

  local n_a n_b
  n_a=$(printf '%s' "$only_a" | grep -c . || true)
  n_b=$(printf '%s' "$only_b" | grep -c . || true)

  log "  $label: only-$a=$n_a only-$b=$n_b"
  [ "$n_a" -gt 0 ] && { log "    only in $a:"; printf '      %s\n' $only_a; }
  [ "$n_b" -gt 0 ] && { log "    only in $b:"; printf '      %s\n' $only_b; }

  if [ "$n_a" -ne 0 ] || [ "$n_b" -ne 0 ]; then
    fail "$label name sets differ"
    return 1
  fi
  return 0
}

# POSITIVE CONTROL first. If this pair fails, the harness is suspect, not the code.
if compare_names pne nma-dep "POSITIVE CONTROL pne vs nma-dep"; then
  log "    positive control passed -- the harness detects agreement on a known-good pair"
else
  log "    POSITIVE CONTROL FAILED -- treat every other verdict below as unreliable"
fi

compare_names pne nma-nodep "pne vs nma-nodep" || true
compare_names nma-dep nma-nodep "nma-dep vs nma-nodep" || true

# ---------------------------------------------------------------------------
# T2 collector success
# ---------------------------------------------------------------------------

log ""
log "=== T2 COLLECTOR SUCCESS: per-collector agreement ==="

# Compare only the collectors PRESENT IN BOTH. The 10 out-of-scope ones are absent from
# nma-nodep by design, and a comparison that reported them as failures would be reporting
# the scope decision rather than a defect.
for pair in "pne nma-dep" "pne nma-nodep" "nma-dep nma-nodep"; do
  set -- $pair
  a="$1"; b="$2"
  shared=$(comm -12 <(cut -d' ' -f1 "$OUT/$a.collectors") <(cut -d' ' -f1 "$OUT/$b.collectors"))
  diff_count=0
  for c in $shared; do
    va=$(awk -v c="$c" '$1==c {print $2}' "$OUT/$a.collectors")
    vb=$(awk -v c="$c" '$1==c {print $2}' "$OUT/$b.collectors")
    if [ "$va" != "$vb" ]; then
      log "    DIFF $c: $a=$va $b=$vb"
      diff_count=$((diff_count + 1))
    fi
  done
  n_shared=$(printf '%s' "$shared" | grep -c . || true)
  log "  $a vs $b: $n_shared shared collectors, $diff_count disagree"
  [ "$diff_count" -ne 0 ] && fail "$a vs $b disagree on $diff_count collector(s)"
done

# The hwmon inversion, checked explicitly. It is the ONE collector that must report
# success=0 on EKS, and a variant that "fixed" it would pass T1 and be wrong.
log ""
log "  hwmon (must be 0 on all three -- absent hardware, and upstream returns ErrNoData):"
for f in pne nma-dep nma-nodep; do
  v=$(awk '$1=="hwmon" {print $2}' "$OUT/$f.collectors")
  log "    $f: ${v:-ABSENT}"
  [ "${v:-}" != "0" ] && fail "$f reports hwmon=${v:-ABSENT}, expected 0"
done

# ---------------------------------------------------------------------------
# T3 series counts
# ---------------------------------------------------------------------------

log ""
log "=== T3 SERIES COUNTS: per-family cardinality ==="

# node_scrape_collector_* legitimately differ (39 vs 49 collectors), so they are excluded
# and reported separately rather than counted as defects.
compare_counts() {
  local a="$1" b="$2"
  local diffs
  diffs=$(join "$OUT/$a.counts" "$OUT/$b.counts" \
    | awk '$2 != $3 {print $1" "$2" "$3}' \
    | grep -v 'node_scrape_collector_' | grep -v 'node_collector_' \
    | grep -vwF -f "$OUT/expected-count-diff.txt" \
    | awk '{print "    DIFF "$1": '"$a"'="$2" '"$b"'="$3}' || true)
  local n
  n=$(printf '%s' "$diffs" | grep -c . || true)
  log "  $a vs $b: $n families differ in series count"
  [ "$n" -gt 0 ] && printf '%s\n' "$diffs"
  # Cardinality differences are REPORTED but do not fail the run: a veth appearing or
  # disappearing between two scrapes seconds apart is normal on a cluster with pod churn,
  # and treating that as a defect would make this harness cry wolf. A name-set difference
  # (T1) is unambiguous; a count difference needs a human to look at which family.
  return 0
}

compare_counts pne nma-dep
compare_counts pne nma-nodep
compare_counts nma-dep nma-nodep

# The excluded filesystem families, reported explicitly rather than hidden. The point is
# that the EKS exclusion is WORKING, and the numbers should be seen.
log ""
log "  EKS mount exclusion (expected difference, not a defect):"
for fam in node_filesystem_readonly node_filesystem_device_error; do
  p=$(awk -v f="$fam" '$1==f {print $2}' "$OUT/pne.counts")
  d=$(awk -v f="$fam" '$1==f {print $2}' "$OUT/nma-dep.counts")
  n=$(awk -v f="$fam" '$1==f {print $2}' "$OUT/nma-nodep.counts")
  log "    $fam: pne=$p nma-dep=$d nma-nodep=$n"
  if [ "$d" != "$n" ]; then
    fail "the two agents disagree on $fam ($d vs $n) -- they share this exclusion, so they must match"
  fi
done

# ---------------------------------------------------------------------------
# self-test: prove all three tiers can actually fail
# ---------------------------------------------------------------------------
#
# A comparison that reports "no differences" is worthless unless it can be shown to
# report differences when they exist. So the captured scrape is corrupted three ways --
# one per tier -- and each tier must catch its own defect.
#
# This runs on every invocation rather than behind a flag: the whole point is that the
# PASS verdict above is only meaningful if the checks below also pass, and a self-test
# nobody runs is not a self-test.

log ""
log "=== SELF-TEST: each tier must detect an injected defect ==="

nc="$OUT/selftest"
rm -rf "$nc"; mkdir -p "$nc"
cp "$OUT"/*.prom "$nc/"

# T1: remove a whole family. T2: flip hwmon's success. T3: drop one CPU's series.
grep -v '^node_cpu_guest_seconds_total' "$nc/nma-nodep.prom" > "$nc/tmp" && mv "$nc/tmp" "$nc/nma-nodep.prom"
sed -i 's/^node_scrape_collector_success{collector="hwmon"} 0/node_scrape_collector_success{collector="hwmon"} 1/' "$nc/nma-nodep.prom"
grep -v '^node_cpu_seconds_total{cpu="1"' "$nc/nma-nodep.prom" > "$nc/tmp" && mv "$nc/tmp" "$nc/nma-nodep.prom"

names "$nc/nma-nodep.prom" > "$nc/nma-nodep.names"
counts "$nc/nma-nodep.prom" > "$nc/nma-nodep.counts"
collectors "$nc/nma-nodep.prom" > "$nc/nma-nodep.collectors"

st_t1=$(comm -23 "$OUT/nma-dep.names" "$nc/nma-nodep.names" \
  | grep -vxF -f "$OUT/expected-extra.txt" | grep -c . || true)
st_t2=$(join "$OUT/nma-dep.collectors" "$nc/nma-nodep.collectors" \
  | awk '$2 != $3' | grep -c . || true)
st_t3=$(join "$OUT/nma-dep.counts" "$nc/nma-nodep.counts" \
  | awk '$2 != $3 {print $1}' \
  | grep -v 'node_scrape_collector_' | grep -v 'node_collector_' \
  | grep -vwF -f "$OUT/expected-count-diff.txt" | grep -c . || true)

log "  T1 detected $st_t1 injected name difference(s)  (expect >= 1)"
log "  T2 detected $st_t2 injected collector difference(s)  (expect >= 1)"
log "  T3 detected $st_t3 injected count difference(s)  (expect >= 1)"

for tier in "T1 $st_t1" "T2 $st_t2" "T3 $st_t3"; do
  set -- $tier
  if [ "$2" -lt 1 ]; then
    fail "SELF-TEST: $1 did not detect its injected defect -- its PASS verdict above is meaningless"
  fi
done

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------

log ""
log "=== SUMMARY ==="
for f in pne nma-dep nma-nodep; do
  s=$(grep -cv '^#' "$OUT/$f.prom" || true)
  fam=$(wc -l < "$OUT/$f.names")
  col=$(wc -l < "$OUT/$f.collectors")
  log "  $f: $s series, $fam families, $col collectors"
done

if [ "$FAILED" -ne 0 ]; then
  log ""
  log "VERDICT: DIFFERENCES FOUND -- see the FAIL lines above"
  exit 1
fi

log ""
log "VERDICT: all three agree on metric names and collector success"
log "         (series-count differences, if any, are reported above for inspection)"
