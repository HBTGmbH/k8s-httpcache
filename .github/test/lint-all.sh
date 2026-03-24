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
markdownlint-cli2 "**/*.md"

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

echo "All linting checks passed."
