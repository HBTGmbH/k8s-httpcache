#!/usr/bin/env bash
# Run all linting checks sequentially, aborting on first failure.
set -eu

cd "$(dirname "$0")/../.."

echo "=== golangci-lint ==="
golangci-lint run

echo "=== go.mod tidy ==="
go mod tidy -diff
go mod verify

echo "=== yamllint ==="
yamllint --strict .

echo "=== shellcheck ==="
shellcheck .github/test/*.sh

echo "=== shfmt ==="
shfmt -d -i 2 -ci .github/test/*.sh

echo "=== typos ==="
typos

echo "=== markdownlint ==="
markdownlint --config .markdownlint.yaml "**/*.md"

echo "=== govulncheck ==="
govulncheck ./...

echo "=== nilaway ==="
nilaway -include-pkgs="k8s-httpcache" -exclude-test-files ./...

echo "=== deadcode ==="
deadcode -test ./...

echo "=== actionlint ==="
actionlint

echo "=== hadolint ==="
hadolint .github/build/Dockerfile .github/test/*/Dockerfile

echo "=== helm lint ==="
helm lint --strict charts/k8s-httpcache

echo "=== chart contract checks ==="
.github/test/chart-contract-test.sh

echo "=== helm-docs up to date ==="
# The chart README is generated from values.yaml comments; CI fails when a
# values.yaml change lands without the regenerated README. Regenerate into a
# temp copy so this lint check never modifies the working tree.
hd_tmp="$(mktemp -d)"
cp -R charts/k8s-httpcache "$hd_tmp/chart"
helm-docs --chart-search-root "$hd_tmp/chart" --log-level warning
if ! diff -u charts/k8s-httpcache/README.md "$hd_tmp/chart/README.md"; then
  rm -rf "$hd_tmp"
  echo "charts/k8s-httpcache/README.md is out of date; run helm-docs and commit it." >&2
  exit 1
fi
rm -rf "$hd_tmp"

echo "=== kubeconform ==="
helm template charts/k8s-httpcache --set image.repository=ghcr.io/example/k8s-httpcache |
  kubeconform -strict -summary -ignore-missing-schemas
kubeconform -strict -summary -ignore-missing-schemas \
  .github/test/manifest.yaml .github/test/manifest-tls.yaml

echo "=== kube-linter ==="
helm template charts/k8s-httpcache --set image.repository=ghcr.io/example/k8s-httpcache |
  kube-linter lint --config .kube-linter.yaml -

echo "=== pint (Prometheus rules) ==="
pr="$(mktemp)"
helm template charts/k8s-httpcache --set image.repository=ghcr.io/example/k8s-httpcache \
  --set prometheusRule.enabled=true --show-only templates/prometheusrule.yaml >"$pr"
pint --offline lint "$pr"
rm -f "$pr"

echo "=== gitleaks ==="
gitleaks dir . --redact --no-banner

echo "All linting checks passed."
