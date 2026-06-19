#!/usr/bin/env bash
# Static file serving test.
# Verifies a file mounted from the k8s-httpcache-static ConfigMap is served by
# Varnish via std.fileread (vcl_recv -> synth(700,"static") -> vcl_synth), with
# the expected status, Content-Type and body, and that normal (non-static)
# requests still reach the backend.
# Requires: curl, kubectl
set -eu

pf_pids=""
cleanup() {
  kill "$pf_pids" 2>/dev/null || true
  rm -f /tmp/robots-body.txt
}
trap cleanup EXIT

pod=$(kubectl get pods -l app=k8s-httpcache --no-headers | awk '$3 == "Running" {print $1; exit}')
kubectl port-forward "$pod" 8086:8080 >/dev/null &
pf_pids="$pf_pids $!"

# Wait for the frontend to answer.
for _ in $(seq 1 30); do
  curl -sf http://localhost:8086/backend/ > /dev/null 2>&1 && break
  sleep 1
done

echo "=== Static file serving (/robots.txt via std.fileread) ==="

headers=$(curl -s -D- -o /tmp/robots-body.txt http://localhost:8086/robots.txt)
status=$(printf '%s\n' "$headers" | awk 'NR==1{print $2}')
ctype=$(printf '%s\n' "$headers" | grep -i '^content-type:' | tr -d '\r' | awk '{print $2}')
body=$(cat /tmp/robots-body.txt)

echo "status=$status content-type=$ctype"
echo "--- body ---"
echo "$body"
echo "------------"

fail=0

if [ "$status" = "200" ]; then
  echo "PASS: HTTP 200"
else
  echo "FAIL: expected HTTP 200, got '$status'"
  fail=1
fi

case "$ctype" in
  text/plain*) echo "PASS: Content-Type text/plain" ;;
  *) echo "FAIL: expected text/plain Content-Type, got '$ctype'"; fail=1 ;;
esac

if printf '%s' "$body" | grep -q 'static-file-marker-e2e'; then
  echo "PASS: body contains static-file-marker-e2e"
else
  echo "FAIL: body missing static-file-marker-e2e marker"
  fail=1
fi

# Non-static requests must still be proxied to the backend.
backend_status=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8086/backend/ || true)
if [ "$backend_status" = "200" ]; then
  echo "PASS: backend still served (status=200)"
else
  echo "FAIL: backend request failed (status=$backend_status)"
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "FAIL: static-files-test"
  exit 1
fi
echo "PASS: static-files-test complete"
