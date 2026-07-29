#!/usr/bin/env bash
# Mechanically verify that pkg/hostmetrics carries NO dependency on
# prometheus/node_exporter -- the central claim of this branch.
#
# WHY THIS IS NOT A BARE `grep -r node_exporter`
#
# The goal file's original N4 gate was:
#
#     grep -rn "prometheus/node_exporter" --include="*.go" --include=go.mod . && exit 1
#
# That gate FAILS on this branch, and correctly so: pkg/metrics -- the
# dependency-based approach from the other branch -- is still present here on
# purpose, because N5/N6 deploy all three variants side by side and compare them.
# Removing it to make the grep pass would destroy the thing being measured.
#
# So the claim has to be scoped to the package that makes it. The check that
# actually matters is the TRANSITIVE import closure of pkg/hostmetrics: if
# node_exporter appears nowhere in `go list -deps`, then the collectors are
# genuinely independent of it, regardless of what else lives in the module.
#
# A source grep alone would also be too weak in the other direction -- it cannot
# see a transitive import arriving through an intermediate package. `go list -deps`
# sees both.
#
# NEGATIVE CONTROL: run with --self-test to confirm the gate can actually fail. It
# checks pkg/metrics, which is KNOWN to depend on node_exporter, and expects a
# non-zero verdict. A gate that cannot fail proves nothing.

set -euo pipefail

cd "$(dirname "$0")/.."

CLEAN_PKG="./pkg/hostmetrics/..."
DIRTY_PKG="./pkg/metrics/..."   # known-dependent, used as the negative control
UPSTREAM="github.com/prometheus/node_exporter"

# deps_count prints how many packages in the transitive closure of $1 belong to
# the upstream module.
deps_count() {
  go list -deps "$1" 2>/dev/null | grep -c "$UPSTREAM" || true
}

# source_refs prints how many source lines under $1 mention the upstream module.
source_refs() {
  grep -rn "$UPSTREAM" --include='*.go' "$1" 2>/dev/null | wc -l | tr -d ' '
}

if [[ "${1:-}" == "--self-test" ]]; then
  echo "=== NEGATIVE CONTROL: the gate must FAIL for a package that IS dependent ==="
  n=$(deps_count "$DIRTY_PKG")
  echo "pkg/metrics transitive node_exporter packages: $n"
  if [[ "$n" -eq 0 ]]; then
    echo "SELF-TEST FAILED: pkg/metrics depends on node_exporter by design, but the"
    echo "gate reported 0. The gate is broken and its PASS verdict is meaningless."
    exit 1
  fi
  echo "SELF-TEST PASSED: gate correctly detects a real dependency ($n packages)."
  echo
fi

echo "=== GATE: pkg/hostmetrics must not depend on $UPSTREAM ==="

transitive=$(deps_count "$CLEAN_PKG")
echo "transitive import closure : $transitive package(s)"

refs=$(source_refs pkg/hostmetrics)
echo "source references         : $refs line(s)"

# Provenance comments name upstream FILES (e.g. "node_exporter/collector/cpu.go")
# as attribution, which is required by the Apache-2.0 derivation and is not an
# import. Only a real Go import path constitutes a dependency.
imports=$(go list -f '{{range .Imports}}{{.}}
{{end}}{{range .TestImports}}{{.}}
{{end}}' "$CLEAN_PKG" 2>/dev/null | grep -c "$UPSTREAM" || true)
echo "declared imports          : $imports"

if [[ "$transitive" -ne 0 || "$imports" -ne 0 ]]; then
  echo
  echo "FAIL: pkg/hostmetrics depends on $UPSTREAM."
  go list -deps "$CLEAN_PKG" 2>/dev/null | grep "$UPSTREAM" || true
  exit 1
fi

echo
echo "PASS: pkg/hostmetrics has zero node_exporter imports, direct or transitive."
echo
echo "NOTE: $UPSTREAM remains in go.mod because pkg/metrics (the dependency-based"
echo "approach) is intentionally still present on this branch for the three-way"
echo "comparison in N5/N6. That is scope, not leakage: the collectors in"
echo "pkg/hostmetrics do not reach it. Shipping only the no-dependency variant"
echo "would drop it from go.mod entirely."
