#!/usr/bin/env bash
# Values-dir live update test: verify that patching a mounted ConfigMap
# directory triggers a VCL reload and the new value appears in response
# headers. Unlike --values (K8s API informer), --values-dir uses file
# polling, so the update goes through kubelet volume sync + file poll +
# debounce before taking effect.
# Requires: curl, kubectl
set -eu

# --- Port-forward setup -----------------------------------------------------

pf_pids=""
cleanup() {
  kill "$pf_pids" 2>/dev/null || true
  # Restore the ConfigMap to its original value regardless of test outcome.
  kubectl patch configmap k8s-httpcache-values-dir-test --type merge \
    -p '{"data":{"greeting.yaml":"hello-from-dir"}}' 2>/dev/null || true
}
trap cleanup EXIT

pod=$(kubectl get pods -l app=k8s-httpcache --no-headers | awk '$3 == "Running" {print $1; exit}')
kubectl port-forward "$pod" 9105:9101 8085:8080 >/dev/null &
pf_pids="$pf_pids $!"

for _ in $(seq 1 30); do
  curl -sf http://localhost:9105/metrics > /dev/null 2>&1 && break
  sleep 1
done

for _ in $(seq 1 30); do
  curl -sf http://localhost:8085/backend/ > /dev/null 2>&1 && break
  sleep 1
done

# --- Helpers -----------------------------------------------------------------

metric_value() {
  curl -sf http://localhost:9105/metrics \
    | awk -v prefix="$1" 'index($0, prefix) == 1 {s+=$2} END{printf "%d\n", s}'
}

# --- Verify current header value ---------------------------------------------

echo "--- Verifying current X-ValuesDir-Test header ---"

header=$(curl -sf -D- -o /dev/null http://localhost:8085/backend/ 2>/dev/null \
  | grep -i '^x-valuesdir-test:' | tr -d '\r' | awk '{print $2}')

if [ "$header" != "hello-from-dir" ]; then
  echo "FAIL: expected X-ValuesDir-Test=hello-from-dir, got '$header'"
  exit 1
fi
echo "PASS: X-ValuesDir-Test = hello-from-dir"

# --- Snapshot metrics --------------------------------------------------------

before_values=$(metric_value 'k8s_httpcache_values_updates_total{configmap="dirtest"}')
before_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')
echo "Before: values_updates=$before_values reloads=$before_reloads"

# --- Patch the ConfigMap -----------------------------------------------------

echo "--- Patching ConfigMap ---"
kubectl patch configmap k8s-httpcache-values-dir-test --type merge \
  -p '{"data":{"greeting.yaml":"updated-dir-value"}}'

# --- Wait for values-dir update + debounced reload ---------------------------
# kubelet volume sync (~5s) + file poll (2s) + debounce (2s) ≈ 10s typical.

echo "--- Waiting for values-dir update + reload (up to 120s) ---"
found=false
for i in $(seq 1 120); do
  current_values=$(metric_value 'k8s_httpcache_values_updates_total{configmap="dirtest"}')
  current_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')
  if [ "$current_values" -gt "$before_values" ] \
    && [ "$current_reloads" -gt "$before_reloads" ]; then
    echo "Values-dir update detected and reloaded after ${i}s (updates=$current_values reloads=$current_reloads)"
    found=true
    break
  fi
  if [ $((i % 10)) -eq 0 ]; then
    echo "Still waiting... (${i}s elapsed, updates=$current_values reloads=$current_reloads)"
  fi
  sleep 1
done

if [ "$found" != "true" ]; then
  echo "FAIL: values-dir update did not complete within 120s"
  exit 1
fi

# --- Assert metrics increased ------------------------------------------------

after_values=$(metric_value 'k8s_httpcache_values_updates_total{configmap="dirtest"}')
after_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')
echo "After: values_updates=$after_values reloads=$after_reloads"

values_delta=$((after_values - before_values))
reloads_delta=$((after_reloads - before_reloads))

echo "PASS: values_updates_total increased (delta=$values_delta)"
echo "PASS: vcl_reloads_total{result=success} increased (delta=$reloads_delta)"

# --- Verify header changed ---------------------------------------------------

header=$(curl -sf -D- -o /dev/null http://localhost:8085/backend/ 2>/dev/null \
  | grep -i '^x-valuesdir-test:' | tr -d '\r' | awk '{print $2}' || true)
if [ "$header" != "updated-dir-value" ]; then
  echo "FAIL: X-ValuesDir-Test = '$header' (expected 'updated-dir-value')"
  exit 1
fi
echo "PASS: X-ValuesDir-Test = updated-dir-value"

# --- Restore original value --------------------------------------------------

echo "--- Restoring original ConfigMap value ---"
kubectl patch configmap k8s-httpcache-values-dir-test --type merge \
  -p '{"data":{"greeting.yaml":"hello-from-dir"}}'

before_restore=$(metric_value 'k8s_httpcache_values_updates_total{configmap="dirtest"}')
echo "--- Waiting for restore (up to 120s) ---"
for i in $(seq 1 120); do
  current_values=$(metric_value 'k8s_httpcache_values_updates_total{configmap="dirtest"}')
  if [ "$current_values" -gt "$before_restore" ]; then
    echo "PASS: original value restored (after ${i}s)"
    break
  fi
  if [ $((i % 10)) -eq 0 ]; then
    echo "Still waiting for restore... (${i}s elapsed)"
  fi
  sleep 1
done
echo "PASS: values-dir-update-test complete"
