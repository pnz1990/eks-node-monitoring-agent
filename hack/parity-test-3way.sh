#!/usr/bin/env bash
#
# Three-way parity comparison between:
#   pne        upstream prometheus/node_exporter, unmodified
#   nma-dep    the agent importing node_exporter/collector
#   nma-nodep  the agent with its own collector implementation, no node_exporter dep
#
# All three are scraped on the same host at nearly the same instant, and their
# metric CONTRACTS are diffed pairwise. Values are not compared: counters advance
# and gauges move between scrapes, so value equality would be flaky by
# construction and would prove nothing. The contract is name + type + label key
# set, which is what dashboards and recording rules actually depend on.
#
# Parity cannot be asserted from a static list of metric names. Several upstream
# names are generated at runtime from host state -- node_memory_MemAvailable_bytes
# appears nowhere in upstream's source, it is built from /proc/meminfo keys via
# BuildFQName. Only a live comparison is sound.
#
# Usage:
#   hack/parity-test-3way.sh --pne <bin> --nma-dep <bin> --nma-nodep <bin>
#
# Any binary may be omitted with --skip-<name>, which is how this runs before the
# no-dependency implementation exists.
#
# Exit codes:
#   0  all requested pairs at parity
#   1  a pair diverged (the diff is printed)
#   2  usage or setup error

set -o errexit
set -o nounset
set -o pipefail

PNE_BIN=""
NMA_DEP_BIN=""
NMA_NODEP_BIN=""
SKIP_NODEP=false
PNE_PORT="${PNE_PORT:-19700}"
NMA_DEP_PORT="${NMA_DEP_PORT:-19701}"
NMA_NODEP_PORT="${NMA_NODEP_PORT:-19702}"
OUT_DIR="${OUT_DIR:-$(mktemp -d)}"

usage() {
  sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --pne)         PNE_BIN="$2"; shift 2 ;;
    --nma-dep)     NMA_DEP_BIN="$2"; shift 2 ;;
    --nma-nodep)   NMA_NODEP_BIN="$2"; shift 2 ;;
    --skip-nodep)  SKIP_NODEP=true; shift ;;
    --out)         OUT_DIR="$2"; shift 2 ;;
    -h|--help)     usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done

[[ -x "${PNE_BIN}" ]]     || { echo "ERROR: --pne must be an executable node_exporter" >&2; exit 2; }
[[ -x "${NMA_DEP_BIN}" ]] || { echo "ERROR: --nma-dep must be an executable agent" >&2; exit 2; }
if [[ "${SKIP_NODEP}" == false ]]; then
  [[ -x "${NMA_NODEP_BIN}" ]] || { echo "ERROR: --nma-nodep must be executable, or pass --skip-nodep" >&2; exit 2; }
fi

mkdir -p "${OUT_DIR}"

# Refuse to run if a port is already bound. A stale exporter left from an earlier
# run silently produces a nonsense diff: the contract fills with whatever that
# process serves and the harness reports a violation unrelated to the code under
# test. This happened during the dependency-based work, so it is checked.
for port in "${PNE_PORT}" "${NMA_DEP_PORT}" "${NMA_NODEP_PORT}"; do
  if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | grep -q ":${port} "; then
    echo "ERROR: port ${port} is already in use; a stale exporter would corrupt the comparison." >&2
    exit 2
  fi
done

PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do
    [[ -n "${pid}" ]] && kill "${pid}" 2>/dev/null || true
  done
}
trap cleanup EXIT

wait_for_endpoint() {
  local url="$1" name="$2" i
  for i in $(seq 1 60); do
    if curl -sf --max-time 2 "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  echo "ERROR: ${name} did not become ready at ${url}" >&2
  return 1
}

# --- start the exporters -------------------------------------------------

echo ">> starting pne on :${PNE_PORT}"
"${PNE_BIN}" --web.listen-address="127.0.0.1:${PNE_PORT}" > "${OUT_DIR}/pne.log" 2>&1 &
PIDS+=("$!")
wait_for_endpoint "http://127.0.0.1:${PNE_PORT}/metrics" "pne"

# Verify the thing answering really is node_exporter, so a stale unrelated
# process cannot silently become the reference.
#
# Retried: /metrics answers 200 as soon as the listener is up, but the registry
# is not necessarily populated on the very first scrape, so a single check races
# startup and produces a false negative. Observed in practice.
verify_is_node_exporter() {
  local port="$1" i body
  for i in $(seq 1 20); do
    # Captured into a variable rather than piped: `grep -q` exits on the first
    # match, which closes the pipe and kills curl with SIGPIPE. Under
    # `set -o pipefail` that surfaces as pipeline failure, so the check reported
    # "not node_exporter" against a perfectly healthy exporter. The tell was
    # "broken pipe" errors in the exporter's own log.
    body="$(curl -s --max-time 5 "http://127.0.0.1:${port}/metrics" || true)"
    if [[ "${body}" == *node_exporter_build_info* ]]; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}
if ! verify_is_node_exporter "${PNE_PORT}"; then
  echo "ERROR: endpoint on ${PNE_PORT} does not look like node_exporter" >&2
  echo "       (node_exporter_build_info absent after 10s). Refusing to compare against it." >&2
  exit 2
fi

echo ">> starting nma-dep on :${NMA_DEP_PORT}"
"${NMA_DEP_BIN}" --metrics-only --metrics-endpoint-address="127.0.0.1:${NMA_DEP_PORT}" \
  > "${OUT_DIR}/nma-dep.log" 2>&1 &
PIDS+=("$!")
wait_for_endpoint "http://127.0.0.1:${NMA_DEP_PORT}/metrics" "nma-dep"

if [[ "${SKIP_NODEP}" == false ]]; then
  echo ">> starting nma-nodep on :${NMA_NODEP_PORT}"
  "${NMA_NODEP_BIN}" --metrics-only --metrics-endpoint-address="127.0.0.1:${NMA_NODEP_PORT}" \
    > "${OUT_DIR}/nma-nodep.log" 2>&1 &
  PIDS+=("$!")
  wait_for_endpoint "http://127.0.0.1:${NMA_NODEP_PORT}/metrics" "nma-nodep"
fi

# --- scrape --------------------------------------------------------------

# Scraped back to back so host state is as close as possible across all three.
curl -s "http://127.0.0.1:${PNE_PORT}/metrics"     > "${OUT_DIR}/pne.prom"
curl -s "http://127.0.0.1:${NMA_DEP_PORT}/metrics" > "${OUT_DIR}/nma-dep.prom"
if [[ "${SKIP_NODEP}" == false ]]; then
  curl -s "http://127.0.0.1:${NMA_NODEP_PORT}/metrics" > "${OUT_DIR}/nma-nodep.prom"
fi

# --- contract extraction -------------------------------------------------

# Emits "TYPE <name> <type>" per metric name and "SERIES <name>{<sorted keys>}"
# per series, so a renamed or dropped label is caught as well as a missing metric.
extract_contract() {
  local file="$1"
  awk '
    /^# TYPE / { print "TYPE " $3 " " $4; next }
    /^#/ { next }
    NF {
      line = $0
      name = line
      sub(/[ {].*$/, "", name)
      if (match(line, /\{[^}]*\}/)) {
        labels = substr(line, RSTART + 1, RLENGTH - 2)
        n = split(labels, parts, ",")
        keys = ""
        for (i = 1; i <= n; i++) {
          k = parts[i]
          sub(/=.*$/, "", k)
          gsub(/^ +| +$/, "", k)
          keys = keys (keys == "" ? "" : ",") k
        }
        m = split(keys, karr, ",")
        for (a = 1; a < m; a++)
          for (b = a + 1; b <= m; b++)
            if (karr[a] > karr[b]) { t = karr[a]; karr[a] = karr[b]; karr[b] = t }
        keys = ""
        for (a = 1; a <= m; a++) keys = keys (keys == "" ? "" : ",") karr[a]
        print "SERIES " name "{" keys "}"
      } else {
        print "SERIES " name
      }
    }
  ' "$file" | sort -u
}

# Series that legitimately differ between two processes on the same host, or that
# describe the exporter rather than the node. Mirrors upstream's own skip regex in
# end-to-end-test.sh.
SKIP_RE='^(TYPE|SERIES) (go_|process_|promhttp_|node_exporter_build_info|node_scrape_collector_duration_seconds|node_textfile_mtime_seconds|node_time_(zone|seconds)|node_boot_time_seconds|node_collector_(panics|timeouts)_total)'

for impl in pne nma-dep nma-nodep; do
  [[ -f "${OUT_DIR}/${impl}.prom" ]] || continue
  extract_contract "${OUT_DIR}/${impl}.prom" | grep -Ev "${SKIP_RE}" > "${OUT_DIR}/${impl}.contract"
  printf ">> %-10s %s metric names, %s series shapes\n" "${impl}" \
    "$(grep -c '^TYPE' "${OUT_DIR}/${impl}.contract")" \
    "$(grep -c '^SERIES' "${OUT_DIR}/${impl}.contract")"
done

# --- pairwise comparison -------------------------------------------------

rc=0
compare() {
  local a="$1" b="$2"
  [[ -f "${OUT_DIR}/${a}.contract" && -f "${OUT_DIR}/${b}.contract" ]] || return 0
  if diff -u "${OUT_DIR}/${a}.contract" "${OUT_DIR}/${b}.contract" \
       > "${OUT_DIR}/diff-${a}-vs-${b}.txt"; then
    echo ">> PARITY  ${a} == ${b}"
  else
    echo ">> DIVERGED  ${a} != ${b}  ($(grep -c '^-[^-]' "${OUT_DIR}/diff-${a}-vs-${b}.txt") only in ${a}, $(grep -c '^+[^+]' "${OUT_DIR}/diff-${a}-vs-${b}.txt") only in ${b})" >&2
    head -40 "${OUT_DIR}/diff-${a}-vs-${b}.txt" >&2
    rc=1
  fi
}

echo
compare pne nma-dep
if [[ "${SKIP_NODEP}" == false ]]; then
  compare pne nma-nodep
  compare nma-dep nma-nodep
fi

echo
echo ">> artifacts in ${OUT_DIR}"
exit "${rc}"
