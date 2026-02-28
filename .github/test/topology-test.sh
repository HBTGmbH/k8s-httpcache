#!/usr/bin/env bash
# Topology-aware routing test: verify that each Varnish pod routes to a
# backend in its own zone when frontend sharding is bypassed (X-No-Shard).
# Requires: curl, kubectl, jq
set -eu

# --- Rebalance backend pods across zones ------------------------------------
# The topologySpreadConstraints should distribute pods evenly, but if the
# cluster was reused or pods were rescheduled, the distribution may be uneven.
# Delete pods from over-represented zones until every zone has a backend.

expected_zones=("europe-west3-a" "europe-west3-b" "europe-west3-c")

for attempt in $(seq 1 5); do
  # Build a map: zone -> list of pod names.
  declare -A zone_pods=()
  while IFS=$'\t' read -r pod zone; do
    zone_pods["$zone"]="${zone_pods[$zone]:-} $pod"
  done < <(kubectl get pods -l app=backend -o json \
    | jq -r '.items[] | select(.status.phase == "Running") | [.metadata.name, (.spec.nodeName // "")] | @tsv' \
    | while IFS=$'\t' read -r pod node; do
        zone=$(kubectl get node "$node" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}')
        printf '%s\t%s\n' "$pod" "$zone"
      done)

  # Check if all expected zones have at least one backend.
  missing=()
  for z in "${expected_zones[@]}"; do
    if [ -z "${zone_pods[$z]:-}" ]; then
      missing+=("$z")
    fi
  done

  if [ ${#missing[@]} -eq 0 ]; then
    echo "Backend pods are distributed across all zones."
    break
  fi

  echo "Attempt $attempt: zones without backends: ${missing[*]}"

  # Find a zone with more than one pod and delete one to trigger rescheduling.
  deleted=false
  for z in "${!zone_pods[@]}"; do
    # shellcheck disable=SC2086
    set -- ${zone_pods[$z]}
    if [ $# -gt 1 ]; then
      echo "  Deleting pod $1 from zone $z (has $# pods)"
      kubectl delete pod "$1" --wait=false
      deleted=true
      break
    fi
  done

  if [ "$deleted" = false ]; then
    echo "  No zone has excess pods to delete, waiting for scheduler..."
  fi

  kubectl rollout status deployment/backend --timeout=60s
  sleep 2
  unset zone_pods
done

# --- Build zone maps --------------------------------------------------------

# Map each backend pod name to its zone.
declare -A backend_zone
while IFS=$'\t' read -r pod zone; do
  backend_zone["$pod"]="$zone"
done < <(kubectl get pods -l app=backend -o json \
  | jq -r '.items[] | select(.status.phase == "Running") | [.metadata.name, (.spec.nodeName // "")] | @tsv' \
  | while IFS=$'\t' read -r pod node; do
      zone=$(kubectl get node "$node" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}')
      printf '%s\t%s\n' "$pod" "$zone"
    done)

echo "Backend pod zones:"
for pod in "${!backend_zone[@]}"; do
  echo "  $pod -> ${backend_zone[$pod]}"
done

# Map each Varnish pod name to its zone.
declare -A varnish_zone
while IFS=$'\t' read -r pod zone; do
  varnish_zone["$pod"]="$zone"
done < <(kubectl get pods -l app=k8s-httpcache -o json \
  | jq -r '.items[] | select(.status.phase == "Running" and .metadata.deletionTimestamp == null) | [.metadata.name, (.spec.nodeName // "")] | @tsv' \
  | while IFS=$'\t' read -r pod node; do
      zone=$(kubectl get node "$node" -o jsonpath='{.metadata.labels.topology\.kubernetes\.io/zone}')
      printf '%s\t%s\n' "$pod" "$zone"
    done)

echo "Varnish pod zones:"
for pod in "${!varnish_zone[@]}"; do
  echo "  $pod -> ${varnish_zone[$pod]}"
done

# --- Test each Varnish pod ---------------------------------------------------

pf_pid=""
cleanup() { kill "$pf_pid" 2>/dev/null || true; }
trap cleanup EXIT

pass=0
fail=0

for vpod in "${!varnish_zone[@]}"; do
  vzone="${varnish_zone[$vpod]}"

  kubectl port-forward "$vpod" "18080:8080" >/dev/null 2>&1 &
  pf_pid="$!"

  # Wait for the port-forward to be ready.
  for _ in $(seq 1 30); do
    curl -sf "http://localhost:18080/ready" > /dev/null 2>&1 && break
    sleep 1
  done

  # Send multiple requests with X-No-Shard to bypass shard routing.
  # Use a unique query parameter per request to bust the Varnish cache,
  # ensuring each request actually hits the backend director.
  all_local=true
  for i in $(seq 1 20); do
    body=$(curl -sf -H "X-No-Shard: true" "http://localhost:18080/backend/?_topo=${vpod}&_i=${i}")
    # Trim trailing newline.
    backend_pod=$(echo "$body" | tr -d '\n')

    bzone="${backend_zone[$backend_pod]:-unknown}"
    if [ "$bzone" != "$vzone" ]; then
      echo "FAIL: Varnish $vpod (zone=$vzone) routed to backend $backend_pod (zone=$bzone)"
      all_local=false
      break
    fi
  done

  kill "$pf_pid" 2>/dev/null || true
  wait "$pf_pid" 2>/dev/null || true

  if [ "$all_local" = true ]; then
    echo "PASS: Varnish $vpod (zone=$vzone) -> all 20 requests routed to same-zone backend"
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
  fi
done

echo ""
echo "Results: $pass passed, $fail failed"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
echo "PASS: topology-aware routing verified for all Varnish pods"
