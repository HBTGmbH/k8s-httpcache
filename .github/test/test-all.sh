#!/usr/bin/env bash
# Run all E2E test scripts sequentially, aborting on first failure.
set -eu

cd "$(dirname "$0")/../.."

tests=(
  smoke-test.sh
  metrics-test.sh
  debounce-test.sh
  shard-test.sh
  values-update-test.sh
  values-dir-update-test.sh
  vcl-update-test.sh
  file-watch-disable-test.sh
  drain-sessions-test.sh
  drain-test.sh
  topology-test.sh
  rollout-test.sh
)

for t in "${tests[@]}"; do
  echo "=== Running $t ==="
  .github/test/"$t"
  echo "=== $t passed ==="
  echo ""
done

echo "All tests passed."
