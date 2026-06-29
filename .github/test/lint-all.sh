#!/usr/bin/env bash
# Run all linting checks sequentially, aborting on first failure.
set -eu

cd "$(dirname "$0")/../.."

echo "=== golangci-lint ==="
golangci-lint run

echo "=== yamllint ==="
yamllint --strict .

echo "=== shellcheck ==="
shellcheck .github/test/*.sh

echo "=== markdownlint ==="
markdownlint --config .markdownlint.yaml "**/*.md"

echo "=== govulncheck ==="
govulncheck ./...

echo "=== deadcode ==="
deadcode -test ./...

echo "=== actionlint ==="
actionlint

echo "=== hadolint ==="
hadolint .github/build/Dockerfile .github/test/*/Dockerfile

echo "=== helm lint ==="
helm lint --strict charts/k8s-httpcache

echo "=== kube-linter ==="
helm template charts/k8s-httpcache --set image.repository=ghcr.io/example/k8s-httpcache \
  | kube-linter lint --config .kube-linter.yaml -

echo "=== pint (Prometheus rules) ==="
pr="$(mktemp)"
helm template charts/k8s-httpcache --set image.repository=ghcr.io/example/k8s-httpcache \
  --set prometheusRule.enabled=true --show-only templates/prometheusrule.yaml > "$pr"
pint --offline lint "$pr"
rm -f "$pr"

echo "=== gitleaks ==="
gitleaks dir . --redact --no-banner

echo "All linting checks passed."
