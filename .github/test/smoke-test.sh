#!/usr/bin/env bash
# Smoke test: wait for ingress readiness, then run hurl assertions.
# Extra arguments are forwarded to hurl (e.g. --report-junit report.xml).
set -eu

wait_for() {
  for _ in $(seq 1 30); do
    curl -sf "http://localhost:8080$1" > /dev/null 2>&1 && return 0
    sleep 2
  done
}

# Wait until the shard ring has converged across all Varnish pods.
# Each pod builds its shard director from its own view of the frontend (peer)
# pod set. Right after a rollout these views can differ for a moment, so the
# same URL can briefly resolve to different owners depending on which pod
# handles the request. The smoke.hurl shard-consistency assertion captures one
# owner and then asserts it stays constant across 500 requests, which is flaky
# until every pod agrees on the ring. Poll until a full window of requests
# (spread across the entry pods by the ingress) all report the same owner.
wait_for_shard_convergence() {
  local url="http://localhost:8080/backend/shard-test"
  local samples=40
  local owners
  for _ in $(seq 1 30); do
    owners=$(for _ in $(seq 1 "$samples"); do
      curl -sf -D- -o /dev/null "$url" 2>/dev/null \
        | grep -i '^x-shard-owner:' | awk '{print $2}' | tr -d '\r'
    done | sort -u)
    if [ -n "$owners" ] && [ "$(printf '%s\n' "$owners" | grep -c .)" -eq 1 ]; then
      echo "Shard ring converged on $owners"
      return 0
    fi
    echo "Shard ring not yet converged (saw: $(printf '%s' "$owners" | tr '\n' ' ')); retrying..."
    sleep 2
  done
  echo "FAIL: shard ring did not converge across pods" >&2
  return 1
}

wait_for /backend/
wait_for /backend-port/
wait_for /backend-named/
wait_for /backend-xns/
wait_for /backend-ext/

wait_for_shard_convergence

hurl --test "$@" .github/test/smoke.hurl
