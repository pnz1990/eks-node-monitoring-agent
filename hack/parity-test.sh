#!/usr/bin/env bash
#
# Verifies that the agent's node_exporter compatible metrics endpoint is
# byte-identical to upstream prometheus/node_exporter for the same host state.
#
# Parity cannot be asserted from a static list of metric names: several names are
# generated at runtime from host state. For example node_memory_MemAvailable_bytes
# appears nowhere in the upstream source, it is built from /proc/meminfo keys via
# BuildFQName. The only sound check is to scrape both exporters on the same host
# at the same time and diff the result, which is what this script does.
#
# Usage:
#   hack/parity-test.sh --upstream /path/to/node_exporter --agent /path/to/agent
#
# Exit codes:
#   0  parity: no unexplained differences
#   1  parity violated: differing metric names (the diff is printed)
#   2  usage or setup error

set -o errexit
set -o nounset
set -o pipefail

UPSTREAM_BIN=""
AGENT_BIN=""
UPSTREAM_PORT="${UPSTREAM_PORT:-19101}"
AGENT_PORT="${AGENT_PORT:-19102}"
OUT_DIR="${OUT_DIR:-$(mktemp -d)}"

usage() {
  sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --upstream) UPSTREAM_BIN="$2"; shift 2 ;;
    --agent)    AGENT_BIN="$2"; shift 2 ;;
    --out)      OUT_DIR="$2"; shift 2 ;;
    -h|--help)  usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done

[[ -x "${UPSTREAM_BIN}" ]] || { echo "ERROR: --upstream must be an executable node_exporter binary" >&2; exit 2; }
[[ -x "${AGENT_BIN}" ]] || { echo "ERROR: --agent must be an executable agent binary" >&2; exit 2; }

mkdir -p "${OUT_DIR}"

# Refuse to run if either port is already bound. A stale exporter left over from a
# previous run silently produces a nonsense diff: the "upstream" contract fills
# with whatever that process serves, and the harness reports a parity violation
# that has nothing to do with the agent. Observed in practice, so it is checked.
for port in "${UPSTREAM_PORT}" "${AGENT_PORT}"; do
  if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | grep -q ":${port} "; then
    echo "ERROR: port ${port} is already in use; a stale exporter would corrupt the comparison." >&2
    echo "       Free it, or set UPSTREAM_PORT/AGENT_PORT to unused ports." >&2
    exit 2
  fi
done

cleanup() {
  [[ -n "${UPSTREAM_PID:-}" ]] && kill "${UPSTREAM_PID}" 2>/dev/null || true
  [[ -n "${AGENT_PID:-}" ]] && kill "${AGENT_PID}" 2>/dev/null || true
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

echo ">> starting upstream node_exporter on :${UPSTREAM_PORT}"
"${UPSTREAM_BIN}" --web.listen-address="127.0.0.1:${UPSTREAM_PORT}" \
  > "${OUT_DIR}/upstream.log" 2>&1 &
UPSTREAM_PID=$!
wait_for_endpoint "http://127.0.0.1:${UPSTREAM_PORT}/metrics" "upstream node_exporter"

# Sanity-check that the thing answering really is node_exporter. Without this a
# stale unrelated process on the port produces a meaningless diff.
# Buffered rather than piped: `grep -q` exits on the first match, which closes the
# pipe and kills curl with SIGPIPE. Under `set -o pipefail` that surfaces as
# pipeline failure, so the guard would reject a healthy exporter precisely when
# the pattern matched. Found while building the three-way harness.
upstream_body="$(curl -s "http://127.0.0.1:${UPSTREAM_PORT}/metrics" || true)"
if [[ "${upstream_body}" != *node_exporter_build_info* ]]; then
  echo "ERROR: the endpoint on ${UPSTREAM_PORT} does not look like node_exporter" >&2
  echo "       (node_exporter_build_info absent). Refusing to compare against it." >&2
  exit 2
fi

echo ">> starting agent metrics endpoint on :${AGENT_PORT}"
# --metrics-only makes the agent serve just the metrics endpoint, without
# requiring a Kubernetes API server.
"${AGENT_BIN}" --metrics-only --metrics-endpoint-address="127.0.0.1:${AGENT_PORT}" \
  > "${OUT_DIR}/agent.log" 2>&1 &
AGENT_PID=$!
wait_for_endpoint "http://127.0.0.1:${AGENT_PORT}/metrics" "agent metrics endpoint"

# Scrape both as close together as possible so host state matches.
curl -s "http://127.0.0.1:${UPSTREAM_PORT}/metrics" > "${OUT_DIR}/upstream.prom"
curl -s "http://127.0.0.1:${AGENT_PORT}/metrics"    > "${OUT_DIR}/agent.prom"

# Values change between scrapes (counters advance, gauges move), so parity is
# asserted on the metric CONTRACT: name, type and label key sets. Comparing raw
# values would be flaky by construction and would prove nothing.
extract_contract() {
  # Emits "<name> <type>" for every metric name, plus "<name>{<sorted label keys>}"
  # for every series, so a renamed or dropped label is caught.
  local file="$1"
  awk '
    /^# TYPE / { print "TYPE " $3 " " $4; next }
    /^#/ { next }
    NF {
      line = $0
      name = line
      sub(/[ {].*$/, "", name)
      labels = ""
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
        # sort keys for stable comparison
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

# Metrics that legitimately differ between two processes on the same host, or
# that describe the exporter rather than the node. This mirrors the skip regex in
# upstream's own end-to-end-test.sh.
SKIP_RE='^(TYPE|SERIES) (go_|process_|promhttp_|node_exporter_build_info|node_scrape_collector_duration_seconds|node_textfile_mtime_seconds|node_time_(zone|seconds)|node_boot_time_seconds)'

extract_contract "${OUT_DIR}/upstream.prom" | grep -Ev "${SKIP_RE}" > "${OUT_DIR}/upstream.contract"
extract_contract "${OUT_DIR}/agent.prom"    | grep -Ev "${SKIP_RE}" > "${OUT_DIR}/agent.contract"

echo ">> upstream: $(grep -c '^TYPE' "${OUT_DIR}/upstream.contract") metric names, $(grep -c '^SERIES' "${OUT_DIR}/upstream.contract") series shapes"
echo ">> agent:    $(grep -c '^TYPE' "${OUT_DIR}/agent.contract") metric names, $(grep -c '^SERIES' "${OUT_DIR}/agent.contract") series shapes"

if diff -u "${OUT_DIR}/upstream.contract" "${OUT_DIR}/agent.contract" > "${OUT_DIR}/parity-diff.txt"; then
  echo ">> PARITY: agent output matches upstream node_exporter"
  echo ">> artifacts in ${OUT_DIR}"
  exit 0
fi

echo ">> PARITY VIOLATED: differences between upstream and agent" >&2
echo "   (lines starting with '-' are present upstream but missing from the agent)" >&2
cat "${OUT_DIR}/parity-diff.txt" >&2
echo ">> artifacts in ${OUT_DIR}" >&2
exit 1
