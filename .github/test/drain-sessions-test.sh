#!/usr/bin/env bash
# Drain timing verification test: verify that pod deletion honours the
# drain-delay (15s) before terminating. Without drain logic, pod deletion
# would be near-instant; with it, the pod waits at least drain-delay seconds.
# Requires: kubectl
set -eu

# --- Pick a pod --------------------------------------------------------------

pod=$(kubectl get pods -l app=k8s-httpcache --no-headers | awk '$3 == "Running" {print $1; exit}')
echo "Selected pod: $pod"

# --- Delete pod and measure elapsed time -------------------------------------

echo "--- Deleting pod (with --wait=true) ---"
start=$(date +%s)
kubectl delete pod "$pod" --wait=true
end=$(date +%s)
elapsed=$((end - start))
echo "Pod deletion took ${elapsed}s"

# --- Assert drain-delay is honoured ------------------------------------------

# drain-delay is 15s; the pod should take at least that long to terminate.
if [ "$elapsed" -lt 15 ]; then
  echo "FAIL: pod terminated in ${elapsed}s (expected >= 15s drain-delay)"
  exit 1
fi
echo "PASS: pod took >= 15s to terminate (drain-delay honoured)"

# drain-delay (15s) + drain-timeout (30s) = 45s max; add margin for scheduling.
if [ "$elapsed" -gt 60 ]; then
  echo "FAIL: pod took ${elapsed}s to terminate (expected < 60s)"
  exit 1
fi
echo "PASS: pod terminated within 60s (drain-delay + drain-timeout + margin)"

# --- Wait for deployment to recover ------------------------------------------

kubectl rollout status deployment/k8s-httpcache --timeout=120s
echo "PASS: deployment recovered after drain-sessions-test"
