#!/usr/bin/env bash
# ConfigMap values live update test: verify that patching a values ConfigMap
# triggers a VCL reload and the new value appears in response headers.
# Requires: curl, kubectl
set -eu

# --- Port-forward setup -----------------------------------------------------

pf_pids=""
cleanup() {
  kill "$pf_pids" 2>/dev/null || true
  # Restore the ConfigMap to its original value regardless of test outcome.
  kubectl patch configmap k8s-httpcache-values-test --type merge \
    -p '{"data":{"greeting":"hello-from-values"}}' 2>/dev/null || true
}
trap cleanup EXIT

pod=$(kubectl get pods -l app=k8s-httpcache -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$pod" 9103:9101 2>/dev/null &
pf_pids="$pf_pids $!"

for _ in $(seq 1 30); do
  curl -sf http://localhost:9103/metrics > /dev/null 2>&1 && break
  sleep 1
done

# --- Helpers -----------------------------------------------------------------

metric_value() {
  curl -sf http://localhost:9103/metrics \
    | awk -v prefix="$1" 'index($0, prefix) == 1 {s+=$2} END{printf "%d\n", s}'
}

# --- Verify current header value ---------------------------------------------

echo "--- Verifying current X-Values-Test header ---"

header=$(curl -sf -D- -o /dev/null http://localhost:8080/backend/ 2>/dev/null \
  | grep -i '^x-values-test:' | tr -d '\r' | awk '{print $2}')

if [ "$header" != "hello-from-values" ]; then
  echo "FAIL: expected X-Values-Test=hello-from-values, got '$header'"
  exit 1
fi
echo "PASS: X-Values-Test = hello-from-values"

# --- Snapshot metrics --------------------------------------------------------

before_values=$(metric_value 'k8s_httpcache_values_updates_total{configmap="test"}')
before_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')
echo "Before: values_updates=$before_values reloads=$before_reloads"

# --- Patch the ConfigMap -----------------------------------------------------

echo "--- Patching ConfigMap ---"
kubectl patch configmap k8s-httpcache-values-test --type merge \
  -p '{"data":{"greeting":"updated-value"}}'

# --- Wait for new header value -----------------------------------------------

echo "--- Waiting for updated header (up to 15s) ---"
found=false
for i in $(seq 1 15); do
  header=$(curl -sf -D- -o /dev/null http://localhost:8080/backend/ 2>/dev/null \
    | grep -i '^x-values-test:' | tr -d '\r' | awk '{print $2}' || true)
  if [ "$header" = "updated-value" ]; then
    echo "PASS: X-Values-Test = updated-value (attempt $i)"
    found=true
    break
  fi
  echo "Attempt $i: X-Values-Test = '${header:-<empty>}', retrying..."
  sleep 1
done

if [ "$found" != "true" ]; then
  echo "FAIL: X-Values-Test did not update within 15s"
  exit 1
fi

# --- Assert metrics increased ------------------------------------------------

after_values=$(metric_value 'k8s_httpcache_values_updates_total{configmap="test"}')
after_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')
echo "After: values_updates=$after_values reloads=$after_reloads"

values_delta=$((after_values - before_values))
reloads_delta=$((after_reloads - before_reloads))

if [ "$values_delta" -le 0 ]; then
  echo "FAIL: values_updates_total did not increase (delta=$values_delta)"
  exit 1
fi
echo "PASS: values_updates_total increased (delta=$values_delta)"

if [ "$reloads_delta" -le 0 ]; then
  echo "FAIL: vcl_reloads_total{result=success} did not increase (delta=$reloads_delta)"
  exit 1
fi
echo "PASS: vcl_reloads_total{result=success} increased (delta=$reloads_delta)"

# --- Restore original value --------------------------------------------------

echo "--- Restoring original ConfigMap value ---"
kubectl patch configmap k8s-httpcache-values-test --type merge \
  -p '{"data":{"greeting":"hello-from-values"}}'

echo "--- Waiting for restored header (up to 15s) ---"
for i in $(seq 1 15); do
  header=$(curl -sf -D- -o /dev/null http://localhost:8080/backend/ 2>/dev/null \
    | grep -i '^x-values-test:' | tr -d '\r' | awk '{print $2}' || true)
  if [ "$header" = "hello-from-values" ]; then
    echo "PASS: X-Values-Test restored to hello-from-values (attempt $i)"
    break
  fi
  echo "Attempt $i: X-Values-Test = '${header:-<empty>}', waiting..."
  sleep 1
done
echo "PASS: values-update-test complete"
