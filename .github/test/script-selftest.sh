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

echo "=== script selftest: custom gate scripts are wired into CI ==="
# chart-contract-test.sh and this selftest are the only lint-all.sh checks a
# stock linter cannot replace; if the CI lint job drops them, chart-vs-code
# drift lands green on main. Pin their workflow wiring here.
workflow=.github/workflows/test-and-build.yml
grep -qE '^\s*run: \.github/test/chart-contract-test\.sh' "$workflow" ||
  fail "chart-contract-test.sh is not invoked by ${workflow}; the chart-vs-code contract gate only runs locally"
grep -qE '^\s*run: \.github/test/script-selftest\.sh' "$workflow" ||
  fail "script-selftest.sh is not invoked by ${workflow}; the script selftest gate only runs locally"

echo "=== script selftest: the secret scan runs on every change ==="
# gitleaks is the repo's only secret scan and its step deliberately carries no
# `if:` -- but a step cannot escape its job's gate. While the scan lived in the
# path-filtered `lint` job, a change matching none of the paths-filter patterns
# (release-please-config.json, a new root .env, deploy/*.json, ...) skipped the
# whole job, so the scan never ran -- and ci-gate counts skipped jobs as
# passing, so such a PR merged green. Pin that the scan sits in an ungated job
# that ci-gate depends on.
job_key_re='^  [a-z0-9_-]+:[[:space:]]*$'
scan_job="$(awk -v key="$job_key_re" '
  /^jobs:[[:space:]]*$/ { injobs = 1; next }
  /^[^[:space:]#]/ { injobs = 0 }
  injobs && $0 ~ key { job = substr($1, 1, length($1) - 1) }
  /gitleaks dir \./ { print job; exit }
' "$workflow")"
[ -n "$scan_job" ] || fail "no gitleaks secret-scan step found in ${workflow}"
scan_gate="$(awk -v key="$job_key_re" -v target="$scan_job" '
  /^jobs:[[:space:]]*$/ { injobs = 1; next }
  /^[^[:space:]#]/ { injobs = 0 }
  injobs && $0 ~ key { injob = (substr($1, 1, length($1) - 1) == target) }
  injob && /^    (needs|if):/ { printf "%s ", $1 }
' "$workflow")"
[ -z "$scan_gate" ] ||
  fail "the gitleaks secret scan lives in job '${scan_job}', which is gated by '${scan_gate}'; a path-filtered job skips the scan and ci-gate treats the skip as success"
ci_gate_needs="$(awk -v key="$job_key_re" '
  /^jobs:[[:space:]]*$/ { injobs = 1; next }
  /^[^[:space:]#]/ { injobs = 0 }
  injobs && $0 ~ key { injob = (substr($1, 1, length($1) - 1) == "ci-gate") }
  injob && /^    needs:/ { print; exit }
' "$workflow")"
case "$ci_gate_needs" in
  *"$scan_job"*) ;;
  *) fail "ci-gate does not depend on the '${scan_job}' job (${ci_gate_needs:-no needs: found}); a failing secret scan would not block the PR" ;;
esac

echo "=== script selftest: release-please keeps Chart.yaml appVersion in sync ==="
# release-please's helm updater rewrites `version:` only, so appVersion sat at
# 0.1.0 while the app shipped 1.2.4: the chart's default image tag
# (`default .Chart.AppVersion .Values.image.tag`) rendered <repo>:0.1.0, a tag
# that was never published, and every resource carried a wrong version label.
# The root (app) package now writes its version into $.appVersion via
# extra-files; pin that wiring so a config edit cannot silently drop it.
rp_config=release-please-config.json
grep -qF '"path": "charts/k8s-httpcache/Chart.yaml"' "$rp_config" ||
  fail "${rp_config} has no extra-files entry for charts/k8s-httpcache/Chart.yaml; release-please leaves appVersion stale"
grep -qF '"jsonpath": "$.appVersion"' "$rp_config" ||
  fail "${rp_config} does not target \$.appVersion; the chart's default image tag would keep rendering a never-published version"

echo "=== script selftest: every pinned Dockerfile base image is covered by dependabot ==="
# .github/test/varnish9-race/Dockerfile had no dependabot entry at all, so its
# varnish and golang pins were frozen while the varnish9 leg it mirrors kept
# being bumped. Pin that every Dockerfile with a real (non-scratch) base image
# lives in a directory dependabot actually visits.
dependabot=.github/dependabot.yml
while IFS= read -r dockerfile; do
  awk 'toupper($1) == "FROM" && tolower($2) != "scratch" { found = 1 } END { exit(found ? 0 : 1) }' "$dockerfile" ||
    continue
  dir="/$(dirname "$dockerfile")"
  grep -qE "^[[:space:]]*(-[[:space:]]+|directory:[[:space:]]+)${dir}[[:space:]]*\$" "$dependabot" ||
    fail "${dockerfile} pins a base image but ${dependabot} never visits ${dir}; its base images are never updated"
done < <(find .github -type f -name Dockerfile | sort)

echo "=== script selftest: the race E2E leg mirrors the varnish9 leg's image ==="
# The race leg only reproduces the varnish9 leg under -race if both run the
# exact same image. Both directories are bumped by one dependabot entry; a
# one-sided bump would leave the race detector covering a different Varnish
# than the leg it stands in for.
varnish_pin() {
  awk 'toupper($1) == "FROM" && $2 ~ /^varnish:/ { print $2; exit }' "$1"
}
v9_pin="$(varnish_pin .github/test/varnish9/Dockerfile)"
v9_race_pin="$(varnish_pin .github/test/varnish9-race/Dockerfile)"
[ -n "$v9_pin" ] || fail "no varnish base image found in .github/test/varnish9/Dockerfile"
[ "$v9_pin" = "$v9_race_pin" ] ||
  fail "varnish9 pins ${v9_pin} but varnish9-race pins ${v9_race_pin}; the race leg no longer mirrors the varnish9 leg"

echo "All script selftests passed."
