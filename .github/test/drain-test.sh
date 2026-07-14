#!/usr/bin/env bash
# Graceful drain test: verify that Connection: close is sent during drain.
# When --drain is enabled, SIGTERM triggers VCL that adds Connection: close
# to every response, signalling clients to close their connections.
# Requires: curl, kubectl
set -eu

# --- Port-forward setup -----------------------------------------------------

pf_pid=""
cleanup() { kill "$pf_pid" 2>/dev/null || true; }
trap cleanup EXIT

pod=$(kubectl get pods -l app=k8s-httpcache --no-headers | awk '$3 == "Running" {print $1; exit}')

start_pf() {
  kubectl port-forward "$pod" 8081:8080 >/dev/null 2>&1 &
  pf_pid=$!
}

# ensure_pf restarts the port-forward if its process died: the kubelet can
# close forwarding streams once the pod starts Terminating, and a dead
# forward would fail every remaining retry even though the draining pod is
# still alive and serving for the whole drain-delay window.
ensure_pf() {
  if ! kill -0 "$pf_pid" 2>/dev/null; then
    echo "  (port-forward died, restarting)"
    start_pf
    sleep 1
  fi
}

start_pf

for _ in $(seq 1 30); do
  curl -sf http://localhost:8081/backend/ >/dev/null 2>&1 && break
  sleep 1
done

# --- Verify Connection: close is NOT present during normal operation ---------

conn=$(curl -sf -D- -o /dev/null http://localhost:8081/backend/ 2>/dev/null |
  grep -i '^connection:' | tr -d '\r' | awk '{print tolower($2)}' || true)

if [ "$conn" = "close" ]; then
  echo "FAIL: Connection: close present before drain (got '$conn')"
  exit 1
fi
echo "PASS: no Connection: close during normal operation (got '${conn:-<empty>}')"

# --- Trigger drain by deleting the pod --------------------------------------

kubectl delete pod "$pod" --wait=false
sleep 2

# --- Retry loop: wait for Connection: close to appear -----------------------

found=false
for i in $(seq 1 10); do
  ensure_pf
  conn=$(curl -sf -D- -o /dev/null http://localhost:8081/backend/ 2>/dev/null |
    grep -i '^connection:' | tr -d '\r' | awk '{print tolower($2)}' || true)
  if [ "$conn" = "close" ]; then
    echo "PASS: Connection: close detected on attempt $i"
    found=true
    break
  fi
  echo "Attempt $i: Connection header = '${conn:-<empty>}', retrying..."
  sleep 1
done

if [ "$found" != "true" ]; then
  echo "FAIL: Connection: close not detected within retry window"
  exit 1
fi

# --- Wait for deployment to recover -----------------------------------------

kubectl rollout status deployment/k8s-httpcache --timeout=120s
echo "PASS: deployment recovered after drain test"
