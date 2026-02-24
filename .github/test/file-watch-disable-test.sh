#!/usr/bin/env bash
# File-watch disable test: verify that --file-watch=false disables filesystem-
# based watchers (VCL template file watcher and values-dir directory poller)
# while leaving the Kubernetes API ConfigMap informer (--values) operational.
# Requires: curl, kubectl, jq
set -eu

# --- Cleanup (trap EXIT) ----------------------------------------------------

pf_pids=""
cleanup() {
  kill "$pf_pids" 2>/dev/null || true
  # Restore all three ConfigMaps to original values.
  kubectl patch configmap k8s-httpcache-values-test --type merge \
    -p '{"data":{"greeting":"hello-from-values"}}' 2>/dev/null || true
  kubectl patch configmap k8s-httpcache-values-dir-test --type merge \
    -p '{"data":{"greeting.yaml":"hello-from-dir"}}' 2>/dev/null || true
  if [ -f /tmp/vcl-original-fwd.tmpl ]; then
    kubectl create configmap k8s-httpcache-vcl \
      --from-file='vcl.tmpl=/tmp/vcl-original-fwd.tmpl' \
      --dry-run=client -o json | kubectl apply -f - 2>/dev/null || true
    rm -f /tmp/vcl-original-fwd.tmpl
  fi
  # Restore deployment (manifest has --file-watch, removing =false).
  kubectl apply -f .github/test/manifest.yaml 2>/dev/null || true
  kubectl rollout status deployment/k8s-httpcache --timeout=120s 2>/dev/null || true
}
trap cleanup EXIT

# --- Patch deployment to add --file-watch=false ------------------------------

echo "=== Setup: patching deployment with --file-watch=false ==="
kubectl get deployment k8s-httpcache -o json \
  | jq '.spec.template.spec.containers[0].args |= map(if . == "--file-watch" then "--file-watch=false" else . end)' \
  | kubectl apply -f -

echo "--- Waiting for rollout ---"
kubectl rollout status deployment/k8s-httpcache --timeout=120s

# --- Port-forward setup -----------------------------------------------------

pod=$(kubectl get pods -l app=k8s-httpcache --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$pod" 9106:9101 2>/dev/null &
pf_pids="$pf_pids $!"

for _ in $(seq 1 30); do
  curl -sf http://localhost:9106/metrics > /dev/null 2>&1 && break
  sleep 1
done

# --- Helpers -----------------------------------------------------------------

metric_value() {
  curl -sf http://localhost:9106/metrics \
    | awk -v prefix="$1" 'index($0, prefix) == 1 {s+=$2} END{printf "%d\n", s}'
}

# --- Save original VCL ConfigMap ---------------------------------------------

echo "--- Saving original VCL template ---"
kubectl get configmap k8s-httpcache-vcl -o jsonpath='{.data.vcl\.tmpl}' > /tmp/vcl-original-fwd.tmpl

# =============================================================================
# Part 1: Negative — file-based changes NOT detected
# =============================================================================

echo ""
echo "=== Part 1: file-based changes should NOT be detected ==="

before_vcl_changes=$(metric_value 'k8s_httpcache_vcl_template_changes_total ')
before_dir_updates=$(metric_value 'k8s_httpcache_values_updates_total{configmap="dirtest"}')
echo "Before: vcl_template_changes=$before_vcl_changes values_updates(dirtest)=$before_dir_updates"

# Patch VCL ConfigMap (add a marker header via jq gsub, same pattern as
# vcl-update-test).
marker="fwd-test-$(date +%s)"
echo "--- Patching VCL ConfigMap (marker: $marker) ---"
kubectl get configmap k8s-httpcache-vcl -o json \
  | jq --arg marker "$marker" '.data["vcl.tmpl"] |= gsub(
      "sub vcl_deliver [{]";
      "sub vcl_deliver {\n      set resp.http.X-Fwd-Disabled = \"\($marker)\";"
    )' \
  | kubectl apply -f -

# Patch values-dir ConfigMap.
echo "--- Patching values-dir ConfigMap ---"
kubectl patch configmap k8s-httpcache-values-dir-test --type merge \
  -p '{"data":{"greeting.yaml":"file-watch-disabled"}}'

# Wait 30s (3x normal detection time) — changes should NOT be picked up.
echo "--- Waiting 30s (negative assertion window) ---"
for i in $(seq 1 30); do
  if [ $((i % 10)) -eq 0 ]; then
    echo "  ${i}s elapsed..."
  fi
  sleep 1
done

after_vcl_changes=$(metric_value 'k8s_httpcache_vcl_template_changes_total ')
after_dir_updates=$(metric_value 'k8s_httpcache_values_updates_total{configmap="dirtest"}')
echo "After 30s: vcl_template_changes=$after_vcl_changes values_updates(dirtest)=$after_dir_updates"

if [ "$after_vcl_changes" -gt "$before_vcl_changes" ]; then
  echo "FAIL: vcl_template_changes_total increased (expected no change)"
  exit 1
fi
echo "PASS: vcl_template_changes_total did NOT increase"

if [ "$after_dir_updates" -gt "$before_dir_updates" ]; then
  echo "FAIL: values_updates_total{configmap=dirtest} increased (expected no change)"
  exit 1
fi
echo "PASS: values_updates_total{configmap=dirtest} did NOT increase"

# Restore both ConfigMaps for Part 2.
echo "--- Restoring VCL and values-dir ConfigMaps ---"
kubectl create configmap k8s-httpcache-vcl \
  --from-file='vcl.tmpl=/tmp/vcl-original-fwd.tmpl' \
  --dry-run=client -o json | kubectl apply -f -
kubectl patch configmap k8s-httpcache-values-dir-test --type merge \
  -p '{"data":{"greeting.yaml":"hello-from-dir"}}'

# =============================================================================
# Part 2: Positive — K8s API ConfigMap values STILL work
# =============================================================================

echo ""
echo "=== Part 2: K8s API ConfigMap informer should still work ==="

before_api_updates=$(metric_value 'k8s_httpcache_values_updates_total{configmap="test"}')
before_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')
echo "Before: values_updates(test)=$before_api_updates reloads=$before_reloads"

# Verify current header value via ingress.
echo "--- Verifying current X-Values-Test header ---"
header=$(curl -sf -D- -o /dev/null http://localhost:8080/backend/ 2>/dev/null \
  | grep -i '^x-values-test:' | tr -d '\r' | awk '{print $2}')
if [ "$header" != "hello-from-values" ]; then
  echo "FAIL: expected X-Values-Test=hello-from-values, got '$header'"
  exit 1
fi
echo "PASS: X-Values-Test = hello-from-values"

# Patch values ConfigMap (K8s API informer path).
echo "--- Patching values ConfigMap ---"
kubectl patch configmap k8s-httpcache-values-test --type merge \
  -p '{"data":{"greeting":"file-watch-test-value"}}'

# Wait for the new header value via ingress (up to 15s).
echo "--- Waiting for updated header (up to 15s) ---"
found=false
for i in $(seq 1 15); do
  header=$(curl -sf -D- -o /dev/null http://localhost:8080/backend/ 2>/dev/null \
    | grep -i '^x-values-test:' | tr -d '\r' | awk '{print $2}' || true)
  if [ "$header" = "file-watch-test-value" ]; then
    echo "PASS: X-Values-Test = file-watch-test-value (attempt $i)"
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

# Assert metrics increased.
after_api_updates=$(metric_value 'k8s_httpcache_values_updates_total{configmap="test"}')
after_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')
echo "After: values_updates(test)=$after_api_updates reloads=$after_reloads"

api_delta=$((after_api_updates - before_api_updates))
reloads_delta=$((after_reloads - before_reloads))

if [ "$api_delta" -le 0 ]; then
  echo "FAIL: values_updates_total{configmap=test} did not increase (delta=$api_delta)"
  exit 1
fi
echo "PASS: values_updates_total{configmap=test} increased (delta=$api_delta)"

if [ "$reloads_delta" -le 0 ]; then
  echo "FAIL: vcl_reloads_total{result=success} did not increase (delta=$reloads_delta)"
  exit 1
fi
echo "PASS: vcl_reloads_total{result=success} increased (delta=$reloads_delta)"

# Restore values ConfigMap and wait for header to revert.
echo "--- Restoring values ConfigMap ---"
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
echo "PASS: file-watch-disable-test complete"
