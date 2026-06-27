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
# handles the request, and a request forwarded to a peer whose VCL is still
# mid-reload can briefly fail with a 503. The smoke.hurl shard-consistency
# assertion captures one owner and then asserts every one of 500 requests
# returns 200 with that same owner, which is flaky until every pod agrees on the
# ring. Poll until a full window of requests (spread across the entry pods by
# the ingress) all return 200 and report the same single owner. The status is
# checked explicitly because a transient 503 during convergence means the ring
# is not settled yet -- a `curl -sf` request would silently hide it and let the
# gate pass prematurely.
wait_for_shard_convergence() {
  local url="http://localhost:8080/backend/shard-test"
  local samples=100
  local seen
  for _ in $(seq 1 30); do
    seen=$(for _ in $(seq 1 "$samples"); do
      resp=$(curl -s -D- -o /dev/null "$url" 2>/dev/null)
      status=$(printf '%s\n' "$resp" | awk 'NR==1{print $2}')
      owner=$(printf '%s\n' "$resp" | grep -i '^x-shard-owner:' | awk '{print $2}' | tr -d '\r')
      echo "${status:-000} ${owner:-none}"
    done | sort -u)
    if [ "$(printf '%s\n' "$seen" | grep -c .)" -eq 1 ] && printf '%s\n' "$seen" | grep -q '^200 '; then
      echo "Shard ring converged on $(printf '%s' "$seen" | awk '{print $2}')"
      return 0
    fi
    echo "Shard ring not yet converged (saw: $(printf '%s' "$seen" | tr '\n' '; ')); retrying..."
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
