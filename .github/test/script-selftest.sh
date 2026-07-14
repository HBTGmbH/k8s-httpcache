#!/usr/bin/env bash
# Self-tests for regression-prone behaviors of the test scripts themselves.
# Runs as part of lint-all.sh; requires no cluster.
set -euo pipefail

cd "$(dirname "$0")/../.."

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

echo "=== script selftest: tls-test.sh skip paths exit 0 ==="
# With an unreachable kube API the script must take its version-skip path and
# exit 0. This pins the cleanup-trap fix: the trap's `[ -n "$certdir" ] && rm`
# form used to turn every skip (certdir never created) into exit 1.
if command -v kubectl >/dev/null; then
  out="$(KUBECONFIG=/nonexistent-kubeconfig bash .github/test/tls-test.sh 2>&1)" ||
    fail "tls-test.sh skip path exited non-zero (cleanup trap regression): ${out}"
  echo "${out}" | grep -q '^SKIP' || fail "tls-test.sh did not take the skip path: ${out}"
else
  echo "kubectl not installed; skipping tls-test.sh selftest"
fi

echo "=== script selftest: deadcode gate detects unreachable code ==="
# Pins the lint-all deadcode wrapper's assumptions: the tool reports findings
# on stdout but always exits 0, so the gate must check output, not exit code.
if command -v deadcode >/dev/null; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  printf 'module selftest\n\ngo 1.26\n' >"$tmp/go.mod"
  printf 'package main\n\nfunc main() {}\n\nfunc dead() {}\n' >"$tmp/main.go"
  dead_out="$(cd "$tmp" && deadcode ./...)" ||
    fail "deadcode exited non-zero; the lint-all wrapper would double-report"
  [ -n "$dead_out" ] ||
    fail "deadcode reported nothing for an unreachable function; the lint-all gate would be vacuous"
else
  echo "deadcode not installed; skipping deadcode selftest"
fi

echo "All script selftests passed."
