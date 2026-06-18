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

wait_for /backend/
wait_for /backend-port/
wait_for /backend-named/
wait_for /backend-xns/
wait_for /backend-ext/

hurl --test "$@" .github/test/smoke.hurl
