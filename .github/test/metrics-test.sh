#!/usr/bin/env bash
# Metrics and broadcast smoke test.
# Sets up port-forwards if the ports are not already reachable,
# runs hurl assertions, then verifies metric values.
set -eu

# --- Port-forward setup (skipped if ports are already reachable) -----------

pf_pids=""
cleanup() { kill "$pf_pids" 2>/dev/null || true; }
trap cleanup EXIT

if ! curl -sf http://localhost:9101/metrics > /dev/null 2>&1; then
  pod=$(kubectl get pods -l app=k8s-httpcache -o jsonpath='{.items[0].metadata.name}')
  kubectl port-forward "$pod" 9101:9101 &
  pf_pids="$pf_pids $!"
fi

if ! curl -sf http://localhost:8088/backend/ > /dev/null 2>&1; then
  kubectl port-forward svc/k8s-httpcache 8088:8088 &
  pf_pids="$pf_pids $!"
fi

for _ in $(seq 1 30); do
  curl -sf http://localhost:9101/metrics > /dev/null 2>&1 && break
  sleep 1
done

for _ in $(seq 1 30); do
  curl -sf http://localhost:8088/backend/ > /dev/null 2>&1 && break
  sleep 1
done

# --- Hurl assertions -------------------------------------------------------

hurl --test .github/test/metrics.hurl .github/test/broadcast.hurl

# --- Metric value assertions ------------------------------------------------

# Extract the summed value of a Prometheus metric by fixed-string prefix.
metric_value() {
  curl -sf http://localhost:9101/metrics \
    | awk -v prefix="$1" 'index($0, prefix) == 1 {s+=$2} END{printf "%d\n", s}'
}

assert_eq() {
  local actual
  actual=$(metric_value "$1")
  if [ "$actual" -ne "$2" ]; then
    echo "FAIL: $1 = $actual (expected $2)"
    exit 1
  fi
  echo "PASS: $1 = $2"
}

assert_gt() {
  local actual
  actual=$(metric_value "$1")
  if [ "$actual" -le "$2" ]; then
    echo "FAIL: $1 = $actual (expected > $2)"
    exit 1
  fi
  echo "PASS: $1 = $actual (> $2)"
}

# Gauges: exact expected values.
assert_eq 'k8s_httpcache_broadcast_fanout_targets ' 3
assert_eq 'k8s_httpcache_endpoints{role="frontend",service="k8s-httpcache"} ' 3
assert_gt 'k8s_httpcache_endpoints{role="backend",service="backend"} ' 0

# Counters: must have fired at least once during startup.
assert_gt 'k8s_httpcache_vcl_reloads_total{result="success"} ' 0
assert_gt 'k8s_httpcache_endpoint_updates_total{' 0

# Error counters: must be zero in a healthy deployment.
assert_eq 'k8s_httpcache_vcl_render_errors_total ' 0
assert_eq 'k8s_httpcache_vcl_template_parse_errors_total ' 0
assert_eq 'k8s_httpcache_vcl_rollbacks_total ' 0

# Counter increment: broadcast_requests_total must increase after a request.
before=$(metric_value 'k8s_httpcache_broadcast_requests_total{')
curl -sf http://localhost:8088/backend/ > /dev/null
after=$(metric_value 'k8s_httpcache_broadcast_requests_total{')
echo "broadcast_requests_total: before=$before after=$after"
if [ "$after" -le "$before" ]; then
  echo "FAIL: broadcast_requests_total did not increase ($before -> $after)"
  exit 1
fi
echo "PASS: broadcast_requests_total increased ($before -> $after)"
