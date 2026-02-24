#!/usr/bin/env bash
# VCL template live reload + retry/rollback test.
# Part 1: Patch VCL ConfigMap with a valid change, verify it takes effect.
# Part 2: Patch VCL ConfigMap with broken VCL, verify retry/rollback metrics.
# Requires: curl, kubectl
set -eu

# --- Port-forward setup -----------------------------------------------------

pf_pids=""
cleanup() {
  kill "$pf_pids" 2>/dev/null || true
  # Restore original VCL ConfigMap if saved.
  if [ -f /tmp/vcl-original.tmpl ]; then
    kubectl create configmap k8s-httpcache-vcl \
      --from-file='vcl.tmpl=/tmp/vcl-original.tmpl' \
      --dry-run=client -o json | kubectl apply -f - 2>/dev/null || true
    rm -f /tmp/vcl-original.tmpl
  fi
}
trap cleanup EXIT

pod=$(kubectl get pods -l app=k8s-httpcache -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward "$pod" 9104:9101 8084:8080 2>/dev/null &
pf_pids="$pf_pids $!"

for _ in $(seq 1 30); do
  curl -sf http://localhost:9104/metrics > /dev/null 2>&1 && break
  sleep 1
done

for _ in $(seq 1 30); do
  curl -sf http://localhost:8084/backend/ > /dev/null 2>&1 && break
  sleep 1
done

# --- Helpers -----------------------------------------------------------------

metric_value() {
  curl -sf http://localhost:9104/metrics \
    | awk -v prefix="$1" 'index($0, prefix) == 1 {s+=$2} END{printf "%d\n", s}'
}

# --- Save original VCL ConfigMap ---------------------------------------------

echo "--- Saving original VCL template ---"
kubectl get configmap k8s-httpcache-vcl -o jsonpath='{.data.vcl\.tmpl}' > /tmp/vcl-original.tmpl

# =============================================================================
# Part 1: Live reload (valid template change)
# =============================================================================

echo ""
echo "=== Part 1: VCL template live reload ==="

# Use a unique marker so the test cannot be fooled by stale headers from a
# previous (possibly failed) run.
marker="vcl-test-$(date +%s)"
echo "Using marker: $marker"

before_changes=$(metric_value 'k8s_httpcache_vcl_template_changes_total ')
before_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')
echo "Before: template_changes=$before_changes reloads=$before_reloads"

# Patch VCL to add a custom header in vcl_deliver.
# Note: [{] is a PCRE character class for literal {; jq does not support \{.
echo "--- Patching VCL ConfigMap (adding X-Vcl-Updated header) ---"
kubectl get configmap k8s-httpcache-vcl -o json \
  | jq --arg marker "$marker" '.data["vcl.tmpl"] |= gsub(
      "sub vcl_deliver [{]";
      "sub vcl_deliver {\n      set resp.http.X-Vcl-Updated = \"\($marker)\";"
    )' \
  | kubectl apply -f -

# Wait for kubelet to sync the ConfigMap volume, the file watcher to detect
# the change, the debounced reload to complete, AND the header to appear in
# responses from this pod.
echo "--- Waiting for template change + reload + header (up to 120s) ---"
found=false
for i in $(seq 1 120); do
  current_changes=$(metric_value 'k8s_httpcache_vcl_template_changes_total ')
  current_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')
  header=$(curl -s -D- -o /dev/null http://localhost:8084/backend/ 2>/dev/null \
    | grep -i '^x-vcl-updated:' | tr -d '\r' | awk '{print $2}' || true)
  if [ "$current_changes" -gt "$before_changes" ] \
    && [ "$current_reloads" -gt "$before_reloads" ] \
    && [ "$header" = "$marker" ]; then
    echo "Template changed, reloaded, and header verified after ${i}s (changes=$current_changes reloads=$current_reloads)"
    found=true
    break
  fi
  if [ $((i % 10)) -eq 0 ]; then
    echo "Still waiting... (${i}s elapsed, changes=$current_changes reloads=$current_reloads header='$header')"
  fi
  sleep 1
done

if [ "$found" != "true" ]; then
  after_changes=$(metric_value 'k8s_httpcache_vcl_template_changes_total ')
  after_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')
  status=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8084/backend/ 2>/dev/null || true)
  echo "FAIL: VCL update did not complete within 120s (changes=$after_changes reloads=$after_reloads header='$header' status=$status)"
  exit 1
fi
echo "PASS: vcl_template_changes_total increased"
echo "PASS: vcl_reloads_total{result=success} increased"
echo "PASS: X-Vcl-Updated = $marker"

# =============================================================================
# Part 2: Broken template → retry & rollback
# =============================================================================

echo ""
echo "=== Part 2: Broken VCL → retry & rollback ==="

before_changes2=$(metric_value 'k8s_httpcache_vcl_template_changes_total ')
before_errors=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="error"}')
before_retries=$(metric_value 'k8s_httpcache_vcl_reload_retries_total ')
before_rollbacks=$(metric_value 'k8s_httpcache_vcl_rollbacks_total ')
echo "Before: changes=$before_changes2 errors=$before_errors retries=$before_retries rollbacks=$before_rollbacks"

# Patch VCL with intentionally broken content.
echo "--- Patching VCL ConfigMap (broken VCL) ---"
kubectl create configmap k8s-httpcache-vcl \
  --from-literal='vcl.tmpl=THIS IS BROKEN VCL;' \
  --dry-run=client -o json | kubectl apply -f -

# Wait for the template change to be detected (kubelet sync + file watcher).
echo "--- Waiting for template change detection (up to 120s) ---"
for i in $(seq 1 120); do
  current_changes=$(metric_value 'k8s_httpcache_vcl_template_changes_total ')
  if [ "$current_changes" -gt "$before_changes2" ]; then
    echo "Template change detected after ${i}s (changes=$current_changes)"
    break
  fi
  if [ $((i % 10)) -eq 0 ]; then
    echo "Still waiting for detection... (${i}s elapsed, changes=$current_changes)"
  fi
  sleep 1
done

# Wait for retries to complete (3 retries × 1s interval + margin).
echo "--- Waiting 10s for retries to complete ---"
sleep 10

after_errors=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="error"}')
after_retries=$(metric_value 'k8s_httpcache_vcl_reload_retries_total ')
after_rollbacks=$(metric_value 'k8s_httpcache_vcl_rollbacks_total ')
echo "After: errors=$after_errors retries=$after_retries rollbacks=$after_rollbacks"

errors_delta=$((after_errors - before_errors))
retries_delta=$((after_retries - before_retries))
rollbacks_delta=$((after_rollbacks - before_rollbacks))

if [ "$errors_delta" -le 0 ]; then
  echo "FAIL: vcl_reloads_total{result=error} did not increase (delta=$errors_delta)"
  exit 1
fi
echo "PASS: vcl_reloads_total{result=error} increased (delta=$errors_delta)"

if [ "$retries_delta" -le 0 ]; then
  echo "FAIL: vcl_reload_retries_total did not increase (delta=$retries_delta)"
  exit 1
fi
echo "PASS: vcl_reload_retries_total increased (delta=$retries_delta)"

if [ "$rollbacks_delta" -le 0 ]; then
  echo "FAIL: vcl_rollbacks_total did not increase (delta=$rollbacks_delta)"
  exit 1
fi
echo "PASS: vcl_rollbacks_total increased (delta=$rollbacks_delta)"

# Verify service still works (old VCL is still active after rollback).
status=$(curl -sf -o /dev/null -w '%{http_code}' http://localhost:8084/backend/ || true)
if [ "$status" != "200" ]; then
  echo "FAIL: service not working after rollback (status=$status)"
  exit 1
fi
echo "PASS: service still working after rollback (status=200)"

# =============================================================================
# Cleanup: restore original VCL
# =============================================================================

echo ""
echo "=== Cleanup: restoring original VCL ==="
kubectl create configmap k8s-httpcache-vcl \
  --from-file='vcl.tmpl=/tmp/vcl-original.tmpl' \
  --dry-run=client -o json | kubectl apply -f -

before_restore=$(metric_value 'k8s_httpcache_vcl_template_changes_total ')
echo "--- Waiting for restore to take effect (up to 120s) ---"
for i in $(seq 1 120); do
  current_changes=$(metric_value 'k8s_httpcache_vcl_template_changes_total ')
  if [ "$current_changes" -gt "$before_restore" ]; then
    echo "PASS: original VCL restored (after ${i}s)"
    break
  fi
  if [ $((i % 10)) -eq 0 ]; then
    echo "Still waiting for restore... (${i}s elapsed, changes=$current_changes)"
  fi
  sleep 1
done
rm -f /tmp/vcl-original.tmpl
echo "PASS: vcl-update-test complete"
