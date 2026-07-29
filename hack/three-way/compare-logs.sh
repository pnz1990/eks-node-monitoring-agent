#!/usr/bin/env bash
# N6 (logs half): compare what the three variants LOG, not just what they export.
#
# WHY LOGS AND NOT JUST METRICS. A collector can produce a correct metric set while
# logging an error on every scrape -- the metrics comparison would pass and an operator's
# log volume would still double. It can also log nothing while silently degrading, which
# is the failure mode the resilience boundary's counters exist to catch. So both halves
# are needed, and they answer different questions:
#
#   metrics: is the exported contract the same?
#   logs:    is the agent as quiet, and does it complain about the same things?
#
# WHAT COUNTS AS A DEFECT HERE
#
#   ERROR or PANIC at any level     -> a defect. Neither variant should log these on a
#                                      healthy node, and one that does is degrading in a
#                                      way the metrics may not show.
#   a WARN the other does not have  -> reported. Not automatically a defect (the two
#                                      variants legitimately warn about different things --
#                                      udev, for instance) but worth a human look.
#   DEBUG volume                    -> reported as a RATIO, not a count. The two agents
#                                      run at the same verbosity here, so a large ratio
#                                      means one is chattier per scrape, which is a real
#                                      operational cost at fleet scale.
#
# PNE IS NOT DIRECTLY COMPARABLE on log CONTENT: it is a different program with its own
# log format and its own idea of what is worth saying. So pne is checked only for the
# absolute bar -- no errors, no panics -- while the two agents are compared against each
# other, since they share a codebase and log format and any divergence is attributable.

set -euo pipefail

NAMESPACE="${NAMESPACE:-kube-system}"
PNE_NAMESPACE="${PNE_NAMESPACE:-monitoring}"
OUT="${OUT:-/tmp/three-way-logs}"
TAIL="${TAIL:-2000}"

mkdir -p "$OUT"

log() { printf '%s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; FAILED=1; }
FAILED=0

# ---------------------------------------------------------------------------
# collect
# ---------------------------------------------------------------------------

log "collecting the last $TAIL log lines from each variant..."

kubectl logs -l app.kubernetes.io/name=eks-node-monitoring-agent -n "$NAMESPACE" \
  --tail="$TAIL" > "$OUT/nma-dep.log" 2>&1 || true
kubectl logs -l app.kubernetes.io/name=nma-nodep -n "$NAMESPACE" \
  --tail="$TAIL" > "$OUT/nma-nodep.log" 2>&1 || true
kubectl logs -l app.kubernetes.io/name=prometheus-node-exporter -n "$PNE_NAMESPACE" \
  --tail="$TAIL" > "$OUT/pne.log" 2>&1 || true

# NOTE ON COUNTS: kubectl logs -l applies --tail PER POD, so with 2 nodes the totals are
# up to 2 x TAIL. That is why nma-dep can show 4000 lines for TAIL=2000 -- not a bug, and
# it is why the volume comparison below uses a ratio rather than an absolute threshold.
for f in pne nma-dep nma-nodep; do
  n=$(wc -l < "$OUT/$f.log")
  log "  $f: $n lines (up to $TAIL per pod)"
  # GUARD: an empty log is not "a quiet agent", it is a failed collection. Reporting
  # "0 errors" from a file that was never populated would be the worst kind of pass.
  if [ "$n" -eq 0 ]; then
    fail "$f produced no log lines -- collection failed, so no verdict is possible"
  fi
done

[ "$FAILED" -ne 0 ] && exit 1

# ---------------------------------------------------------------------------
# the absolute bar: no errors, no panics, on any variant
# ---------------------------------------------------------------------------

log ""
log "=== ERRORS AND PANICS (must be zero on all three) ==="

for f in pne nma-dep nma-nodep; do
  # Matched on the structured level field for the agents and on plain text for pne,
  # which uses a different format. Both forms are counted so neither is missed.
  errs=$(grep -icE '"level":"(ERROR|FATAL)"|level=(error|fatal)|\bpanic\b' "$OUT/$f.log" || true)
  log "  $f: $errs"
  if [ "$errs" -gt 0 ]; then
    fail "$f logged $errs error/panic line(s)"
    grep -iE '"level":"(ERROR|FATAL)"|level=(error|fatal)|\bpanic\b' "$OUT/$f.log" | head -5 | sed 's/^/      /'
  fi
done

# The resilience counters are the other half of this check: a CONTAINED panic logs an
# error, but a panic that somehow escaped containment would show as a restart instead.
log ""
log "=== CONTAINER RESTARTS (a contained panic logs; an escaped one restarts) ==="
for pair in "eks-node-monitoring-agent $NAMESPACE nma-dep" "nma-nodep $NAMESPACE nma-nodep"; do
  set -- $pair
  restarts=$(kubectl get pods -l "app.kubernetes.io/name=$1" -n "$2" \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount}{"\n"}{end}' 2>/dev/null \
    | awk '{s+=$1} END {print s+0}')
  log "  $3: $restarts restart(s)"
  [ "$restarts" -gt 0 ] && fail "$3 has restarted $restarts time(s) -- containment may have failed"
done

# ---------------------------------------------------------------------------
# the two agents, compared against each other
# ---------------------------------------------------------------------------

log ""
log "=== WARN MESSAGES: the two agents compared ==="

# Grouped by MESSAGE, not by line: the same warning about 40 different mount points is
# one problem, not 40, and counting lines would make a chatty-but-fine collector look
# worse than a quiet-but-broken one.
warnmsgs() {
  grep -oE '"level":"WARN"[^}]*"msg":"[^"]*"' "$1" 2>/dev/null \
    | sed 's/.*"msg":"\([^"]*\)".*/\1/' | sort -u || true
}

warnmsgs "$OUT/nma-dep.log" > "$OUT/nma-dep.warns"
warnmsgs "$OUT/nma-nodep.log" > "$OUT/nma-nodep.warns"

only_dep=$(comm -23 "$OUT/nma-dep.warns" "$OUT/nma-nodep.warns")
only_nodep=$(comm -13 "$OUT/nma-dep.warns" "$OUT/nma-nodep.warns")

log "  distinct WARN messages: nma-dep=$(wc -l < "$OUT/nma-dep.warns") nma-nodep=$(wc -l < "$OUT/nma-nodep.warns")"
if [ -n "$only_dep" ]; then
  log "  only nma-dep warns about:"
  printf '      %s\n' "$only_dep"
fi
if [ -n "$only_nodep" ]; then
  log "  only nma-nodep warns about:"
  printf '      %s\n' "$only_nodep"
fi
[ -z "$only_dep" ] && [ -z "$only_nodep" ] && log "  (identical warning sets)"

# ---------------------------------------------------------------------------
# volume
# ---------------------------------------------------------------------------

log ""
log "=== LOG VOLUME: is the native variant as quiet? ==="

# Compared as a RATIO over the same tail window. Both agents run at the same verbosity,
# so a ratio far from 1 means one is genuinely chattier per scrape -- an operational cost
# at fleet scale even when nothing is wrong.
dep_lines=$(wc -l < "$OUT/nma-dep.log")
nodep_lines=$(wc -l < "$OUT/nma-nodep.log")
dep_debug=$(grep -c '"level":"DEBUG"' "$OUT/nma-dep.log" || true)
nodep_debug=$(grep -c '"level":"DEBUG"' "$OUT/nma-nodep.log" || true)

log "  nma-dep:   $dep_lines lines ($dep_debug DEBUG)"
log "  nma-nodep: $nodep_lines lines ($nodep_debug DEBUG)"

if [ "$dep_debug" -gt 0 ]; then
  ratio=$(awk -v a="$nodep_debug" -v b="$dep_debug" 'BEGIN{printf "%.2f", a/b}')
  log "  DEBUG ratio nodep/dep: $ratio"
  # 3x is a judgement call, not a measured threshold: it is loose enough that normal
  # scrape-to-scrape variation in a tail window will not trip it, and tight enough to
  # catch a collector that logs per-device where the other logs per-scrape.
  over=$(awk -v r="$ratio" 'BEGIN{print (r > 3.0) ? 1 : 0}')
  [ "$over" -eq 1 ] && fail "the native variant logs ${ratio}x the DEBUG volume -- worth investigating"
fi

# ---------------------------------------------------------------------------
# what the logs confirm about behaviour
# ---------------------------------------------------------------------------

log ""
log "=== BEHAVIOUR VISIBLE IN LOGS ==="

# The mount exclusion is the one place where a DEBUG line is positive evidence: it proves
# the exclusion ran, rather than the mounts simply not existing.
for f in nma-dep nma-nodep; do
  # CASE-INSENSITIVE: nma-dep logs "Ignoring mount point" (upstream's capital I) while
  # nma-nodep logs it lowercase. My first version was case-sensitive and reported 0 for
  # nma-dep -- which then triggered the "the exclusion may not be running" note about a
  # collector that was working correctly. A message-text comparison across two codebases
  # has to be case-insensitive or it reports its own grep as a defect.
  excluded=$(grep -ic 'ignoring mount point' "$OUT/$f.log" || true)
  log "  $f: $excluded 'ignoring mount point' lines (the EKS exclusion working)"
  if [ "$excluded" -eq 0 ]; then
    log "      NOTE: zero exclusions could mean the tail window missed a scrape, OR that"
    log "            the exclusion is not running. Check node_filesystem_readonly count."
  fi
done

# The implementation actually selected, from the startup line. This is the check that
# catches a config that silently fell back to the wrong implementation -- which would make
# every metric comparison above a comparison of the same code against itself.
log ""
log "  implementation selected at startup:"

# READ PER POD, FROM THE FULL LOG. Two corrections to my first attempt, both of which
# produced a FALSE ALARM about the one thing that would invalidate every other comparison:
#
#   1. The startup line is written once and ages out of a --tail window on a pod that has
#      been up for a while, so it must be read without --tail.
#   2. `kubectl logs -l` against a multi-pod selector does NOT reliably return every pod's
#      full history -- it returned 20 lines where a single pod had 348. So each pod is
#      queried by name.
#
# Both mistakes reported "the native variant may be running the upstream implementation",
# which is the worst possible false alarm here: it would mean every metric comparison was
# comparing the same code against itself.
for pair in "nma-nodep nma-nodep" "eks-node-monitoring-agent nma-dep"; do
  set -- $pair
  : > "$OUT/$2.full.log"
  for pod in $(kubectl get pods -l "app.kubernetes.io/name=$1" -n "$NAMESPACE" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'); do
    kubectl logs "$pod" -n "$NAMESPACE" >> "$OUT/$2.full.log" 2>&1 || true
  done
done

# `|| true` on every grep in this section. Under `set -e`, a grep that finds nothing exits
# 1 and kills the script -- which is exactly what happened here: nma-dep does NOT log an
# implementation field (it predates the switch), so its grep failed and the script died
# silently at that line, printing the header and nothing else. The files were correct all
# along.
#
# This is the same failure shape as the SIGPIPE bug on the dependency branch: a check that
# reports nothing when the interesting case occurs. There, grep closing the pipe killed
# curl; here, grep finding nothing killed the script.
for f in nma-dep nma-nodep; do
  impl=$(grep -oE '"implementation":"[a-z]*"' "$OUT/$f.full.log" 2>/dev/null | head -1 \
    | sed 's/.*:"\(.*\)"/\1/' || true)
  log "    $f: ${impl:-not logged (predates the implementation switch)}"
done

native=$(grep -c 'using native collectors' "$OUT/nma-nodep.full.log" 2>/dev/null || true)
if [ "$native" -eq 0 ]; then
  fail "nma-nodep did not log 'using native collectors' -- it may be running the upstream implementation, which would make every metric comparison a comparison of the same code against itself"
else
  log "    nma-nodep confirmed running the NATIVE collectors ($native pod(s))"
fi

# And the converse: nma-dep must NOT be running native, or the two are the same thing.
dep_native=$(grep -c 'using native collectors' "$OUT/nma-dep.full.log" 2>/dev/null || true)
if [ "$dep_native" -ne 0 ]; then
  fail "nma-dep logged 'using native collectors' -- both variants are running the same implementation"
else
  log "    nma-dep confirmed running the UPSTREAM collectors"
fi

# ---------------------------------------------------------------------------

log ""
if [ "$FAILED" -ne 0 ]; then
  log "VERDICT: log comparison found problems -- see the FAIL lines above"
  exit 1
fi
log "VERDICT: no errors or panics on any variant; the two agents log comparably"
