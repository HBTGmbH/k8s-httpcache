#!/usr/bin/env bash
# Chart-vs-code contract checks. The E2E suite deploys a hand-maintained
# manifest, not the rendered chart, so these invariants between the chart
# templates and the controller's behaviour are asserted here statically.
set -eu

cd "$(dirname "$0")/../.."

CHART=charts/k8s-httpcache
IMG=(--set image.repository=ghcr.io/example/k8s-httpcache)

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

echo "=== chart contract: startupProbe budget covers --startup-timeout ==="
# varnishd only listens after the initial endpoint snapshot was collected,
# which the controller allows --startup-timeout (default 180s) for. A probe
# budget below that kills the pod while it is still legitimately starting.
rendered="$(helm template "$CHART" "${IMG[@]}" --show-only templates/workload.yaml)"
threshold="$(echo "$rendered" | awk '/startupProbe:/,/periodSeconds:/' | awk '/failureThreshold:/ {print $2}')"
period="$(echo "$rendered" | awk '/startupProbe:/,/periodSeconds:/' | awk '/periodSeconds:/ {print $2}')"
budget=$((threshold * period))
[ "$budget" -ge 180 ] || fail "startupProbe budget ${budget}s < 180s (--startup-timeout default)"

echo "=== chart contract: http containerPort defaults to 8080 ==="
# The fallback must match varnishd's default listen port, not the https port.
port="$(helm template "$CHART" "${IMG[@]}" --set container.httpPort=null --show-only templates/workload.yaml |
  awk '/name: http$/ {getline; print $2}')"
[ "$port" = "8080" ] || fail "http containerPort fallback is ${port}, want 8080"

echo "=== chart contract: cross-namespace serviceName gets RBAC ==="
# The frontend watcher and event recorder operate in serviceName's namespace;
# a foreign namespace needs a Role there.
foreign="$(helm template "$CHART" "${IMG[@]}" --set serviceName=other-ns/frontend \
  --show-only templates/role-crossnamespace.yaml)"
echo "$foreign" | grep -q "namespace: other-ns" ||
  fail "no Role rendered in the cross-namespace serviceName namespace"
echo "$foreign" | grep -q "endpointslices" ||
  fail "cross-namespace serviceName Role lacks endpointslices access"

echo "All chart contract checks passed."
