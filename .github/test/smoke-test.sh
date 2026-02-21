#!/usr/bin/env bash
# Smoke test: wait for ingress readiness, then run hurl assertions.
# Extra arguments are forwarded to hurl (e.g. --report-junit report.xml).
set -eu

for i in $(seq 1 30); do
  curl -sf http://localhost:8080/backend/ > /dev/null 2>&1 && break
  sleep 2
done

hurl --test "$@" .github/test/smoke.hurl
