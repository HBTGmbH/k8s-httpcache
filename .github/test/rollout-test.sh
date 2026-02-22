#!/usr/bin/env bash
# Zero-downtime rollout restart test: send continuous traffic while
# restarting the deployment and verify no requests fail.
# Requires: oha, kubectl, jq
set -eu

oha -c 20 -q 200 -z 180s -u s -m POST --no-tui --output-format json http://localhost:8080/backend/ > /tmp/oha.json 2>/dev/null &
OHA_PID=$!

sleep 3
kubectl rollout restart deployment/k8s-httpcache
kubectl rollout status deployment/k8s-httpcache --timeout=120s
echo "Wait until all pods have successfully terminated to check for any aborted requests or connections..."
while kubectl get pods -l app=k8s-httpcache | grep -q Terminating; do sleep 1; done

kill -INT $OHA_PID
wait $OHA_PID || true

echo "Status code distribution:"
jq '.statusCodeDistribution' /tmp/oha.json
echo "Error distribution:"
jq '.errorDistribution' /tmp/oha.json

non200=$(jq '[.statusCodeDistribution | to_entries[] | select(.key != "200") | .value] | add // 0' /tmp/oha.json)
errors=$(jq '[.errorDistribution | to_entries[] | .value] | add // 0' /tmp/oha.json)
if [ "$non200" -gt 0 ] || [ "$errors" -gt 0 ]; then
  echo "FAIL: $non200 non-200 responses, $errors connection errors"
  exit 1
fi
echo "PASS: all requests returned 200 with no errors"
