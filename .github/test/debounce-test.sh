#!/usr/bin/env bash
# Debounce e2e test: verify that rapid endpoint events are coalesced
# (debounce) and that sustained activity forces reloads (debounce-max).
# Requires: curl, kubectl
set -eu

# --- Port-forward setup -----------------------------------------------------

pf_pids=""
cleanup() {
  kill "$pf_pids" 2>/dev/null || true
  # Restore backend to 3 replicas regardless of test outcome.
  kubectl scale deployment/backend --replicas=3 2>/dev/null || true
}
trap cleanup EXIT

pod=$(kubectl get pods -l app=k8s-httpcache --no-headers | awk '$3 == "Running" {print $1; exit}')
kubectl port-forward "$pod" 9102:9101 >/dev/null &
pf_pids="$pf_pids $!"

for _ in $(seq 1 30); do
  curl -sf http://localhost:9102/metrics > /dev/null 2>&1 && break
  sleep 1
done

# --- Helpers -----------------------------------------------------------------

metric_value() {
  curl -sf http://localhost:9102/metrics \
    | awk -v prefix="$1" 'index($0, prefix) == 1 {s+=$2} END{printf "%d\n", s}'
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

# --- Part 1: Debounce coalescing test ---------------------------------------
# Scale backend 3 -> 5. This produces a burst of EndpointSlice events that
# should be coalesced by the 2s debounce window into fewer reloads.

echo "--- Part 1: debounce coalescing ---"

before_events=$(metric_value 'k8s_httpcache_debounce_events_total{group="backend"}')
before_fires=$(metric_value 'k8s_httpcache_debounce_fires_total{group="backend"}')
before_latency=$(metric_value 'k8s_httpcache_debounce_latency_seconds_count{group="backend"}')
before_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')

echo "Before: events=$before_events fires=$before_fires latency_count=$before_latency reloads=$before_reloads"

kubectl scale deployment/backend --replicas=5
kubectl rollout status deployment/backend --timeout=60s

# Wait for debounce timer to fire and reload to complete.
sleep 4

after_events=$(metric_value 'k8s_httpcache_debounce_events_total{group="backend"}')
after_fires=$(metric_value 'k8s_httpcache_debounce_fires_total{group="backend"}')
after_latency=$(metric_value 'k8s_httpcache_debounce_latency_seconds_count{group="backend"}')
after_reloads=$(metric_value 'k8s_httpcache_vcl_reloads_total{result="success"}')

echo "After:  events=$after_events fires=$after_fires latency_count=$after_latency reloads=$after_reloads"

events_delta=$((after_events - before_events))
fires_delta=$((after_fires - before_fires))
latency_delta=$((after_latency - before_latency))
reloads_delta=$((after_reloads - before_reloads))

# Events received must have increased.
if [ "$events_delta" -le 0 ]; then
  echo "FAIL: debounce_events_total did not increase (delta=$events_delta)"
  exit 1
fi
echo "PASS: debounce_events_total increased (delta=$events_delta)"

# Timer must have fired at least once.
if [ "$fires_delta" -le 0 ]; then
  echo "FAIL: debounce_fires_total did not increase (delta=$fires_delta)"
  exit 1
fi
echo "PASS: debounce_fires_total increased (delta=$fires_delta)"

# Histogram must have observed at least one sample.
if [ "$latency_delta" -le 0 ]; then
  echo "FAIL: debounce_latency_seconds_count did not increase (delta=$latency_delta)"
  exit 1
fi
echo "PASS: debounce_latency_seconds_count increased (delta=$latency_delta)"

# VCL reload must have happened.
if [ "$reloads_delta" -le 0 ]; then
  echo "FAIL: vcl_reloads_total did not increase (delta=$reloads_delta)"
  exit 1
fi
echo "PASS: vcl_reloads_total increased (delta=$reloads_delta)"

# Coalescing: more events than fires means debounce is working.
if [ "$events_delta" -le "$fires_delta" ]; then
  echo "FAIL: events_delta ($events_delta) <= fires_delta ($fires_delta); coalescing not observed"
  exit 1
fi
echo "PASS: events ($events_delta) > fires ($fires_delta) — coalescing confirmed"

# --- Part 2: Debounce-max enforcement test ----------------------------------
# To trigger debounce-max we need events that keep arriving for longer than
# the 5s max deadline without a 2s quiet gap.  ConfigMap patches produce
# instant watcher events (no pod startup latency), so timing is predictable.
# We patch the values ConfigMap every 0.5s for 8s — well past the 5s max.

echo ""
echo "--- Part 2: debounce-max enforcement ---"

before_max=$(metric_value 'k8s_httpcache_debounce_max_enforcements_total{group="backend"}')
echo "Before: max_enforcements=$before_max"

# Rapid ConfigMap patches: 16 × 0.5s = 8s of sustained activity (> 5s max).
for i in $(seq 1 16); do
  kubectl patch configmap k8s-httpcache-values-test \
    --type merge -p "{\"data\":{\"debounce-test\":\"$i\"}}" > /dev/null
  sleep 0.5
done

# Wait for debounce-max to force the reload.
sleep 4

after_max=$(metric_value 'k8s_httpcache_debounce_max_enforcements_total{group="backend"}')
echo "After:  max_enforcements=$after_max"

max_delta=$((after_max - before_max))
if [ "$max_delta" -le 0 ]; then
  echo "FAIL: debounce_max_enforcements_total did not increase (delta=$max_delta)"
  exit 1
fi
echo "PASS: debounce_max_enforcements_total increased (delta=$max_delta)"

# --- Cleanup: remove test key and scale backend back to 3 -------------------

echo ""
echo "--- Cleanup ---"
kubectl patch configmap k8s-httpcache-values-test \
  --type json -p '[{"op":"remove","path":"/data/debounce-test"}]' > /dev/null
kubectl scale deployment/backend --replicas=3
kubectl rollout status deployment/backend --timeout=60s
echo "PASS: cleanup complete"
