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

echo "=== chart contract: terminationGracePeriodSeconds covers the shutdown budget ==="
# Mirrors main.go shutdownBudget(): the kubelet SIGKILLs varnishd mid-drain
# when the grace period is below the controller's worst-case sequential
# shutdown time computed from the rendered flags.
rendered="$(helm template "$CHART" "${IMG[@]}" --show-only templates/workload.yaml)"
grace="$(echo "$rendered" | awk '/terminationGracePeriodSeconds:/ {print $2}')"
args="$(echo "$rendered" | sed -n 's/^ *- //p' | tr -d '"')"
flagval() { echo "$args" | sed -n "s/^--$1=//p" | head -n 1; }
secs() { # $1 flag name, $2 default duration -> seconds
  d="$(flagval "$1")"
  [ -n "$d" ] || d="$2"
  echo "$d" | awk '{ n=0; while (match($0, /[0-9]+[a-z]+/)) { seg=substr($0, RSTART, RLENGTH);
    unit=seg; gsub(/[0-9]/,"",unit); v=seg; gsub(/[a-z]/,"",v);
    if (unit=="s") n+=v; else if (unit=="m") n+=v*60; else if (unit=="h") n+=v*3600;
    $0=substr($0, RSTART+RLENGTH) } print n }'
}
budget="$(secs shutdown-timeout 30s)"
if echo "$args" | grep -q '^--drain$'; then
  budget=$((budget + $(secs drain-delay 15s) + $(secs drain-timeout 0s)))
fi
if ! echo "$args" | grep -q '^--broadcast-addr=none$'; then
  budget=$((budget + $(secs broadcast-drain-timeout 30s) + $(secs broadcast-shutdown-timeout 5s)))
fi
if echo "$args" | grep -q '^--varnishncsa-enabled$'; then
  budget=$((budget + 5))
fi
if ! echo "$args" | grep -q '^--metrics-addr=none$'; then
  budget=$((budget + $(secs shutdown-timeout 30s)))
fi
[ "$grace" -ge "$budget" ] ||
  fail "terminationGracePeriodSeconds ${grace}s < worst-case shutdown budget ${budget}s (kubelet would SIGKILL varnishd mid-drain on default deploys)"

echo "=== chart contract: varnishstatExportFilter comma syntax reaches the collector ==="
# The binary disables comma-splitting of slice flags; a single joined
# "MAIN,SMA" value matches no counter group and silently exports nothing.
filters="$(helm template "$CHART" "${IMG[@]}" --set metrics.varnishstatExport=true \
  --set-string 'metrics.varnishstatExportFilter=MAIN\,SMA' --show-only templates/workload.yaml |
  grep -c -- '--varnishstat-export-filter=')"
[ "$filters" -eq 2 ] ||
  fail "comma-separated varnishstatExportFilter rendered ${filters} flag(s), want 2 repeated flags"
# A trailing comma must not render an empty filter flag.
trailing="$(helm template "$CHART" "${IMG[@]}" --set metrics.varnishstatExport=true \
  --set-string "metrics.varnishstatExportFilter=MAIN\," --show-only templates/workload.yaml |
  grep -c -- "--varnishstat-export-filter=")"
[ "$trailing" -eq 1 ] ||
  fail "trailing-comma varnishstatExportFilter rendered ${trailing} flag(s), want 1 (no empty flag)"

echo "=== chart contract: allNamespaces discovery renders a ClusterRole even with template.zone set ==="
# List/watch across all namespaces needs a ClusterRole; rbac.createClusterRole=auto
# used to consider only the zone lookup, rendering none -> Forbidden -> CrashLoopBackoff.
cr="$(helm template "$CHART" "${IMG[@]}" --set template.zone=eu-west-1a \
  --set 'backendDiscovery[0].selector.app=web' --set 'backendDiscovery[0].allNamespaces=true' \
  --show-only templates/clusterrole.yaml 2>/dev/null || true)"
echo "$cr" | grep -q 'endpointslices' ||
  fail "no ClusterRole with endpointslices access rendered for allNamespaces discovery"

echo "=== chart contract: varnishncsa string values render as quoted args ==="
# A JSON -F format string must not break YAML parsing, and a trailing space
# in the prefix must survive rendering.
# Values file rather than --set-string: helm's set parser mangles braces.
ncsa_values="$(mktemp)"
cat >"$ncsa_values" <<'YAML'
varnishncsa:
  enabled: true
  format: '{"time":"%t","host":"%h"}'
  prefix: 'ncsa: '
YAML
ncsa="$(helm template "$CHART" "${IMG[@]}" --values "$ncsa_values" \
  --show-only templates/workload.yaml)" ||
  fail "helm template failed on a JSON varnishncsa format string"
rm -f "$ncsa_values"
echo "$ncsa" | grep -qF -- '--varnishncsa-format={\"time\":\"%t\",\"host\":\"%h\"}' ||
  fail "JSON varnishncsa format string not preserved in rendered args"
echo "$ncsa" | grep -qF -- '--varnishncsa-prefix=ncsa: "' ||
  fail "trailing space in varnishncsa.prefix stripped by unquoted rendering"

echo "=== chart contract: fullnameOverride names the rendered resources ==="
# values.yaml and the README document fullnameOverride; the fullname helper
# must honor it (precedence: fullnameOverride > nameOverride > release name).
name="$(helm template "$CHART" "${IMG[@]}" --set fullnameOverride=custom-full \
  --show-only templates/service.yaml | awk '/^  name:/ {print $2; exit}')"
[ "$name" = "custom-full" ] ||
  fail "fullnameOverride ignored: Service name is ${name}, want custom-full"
name="$(helm template "$CHART" "${IMG[@]}" --set fullnameOverride=custom-full \
  --set nameOverride=other --show-only templates/service.yaml | awk '/^  name:/ {print $2; exit}')"
[ "$name" = "custom-full" ] ||
  fail "fullnameOverride must win over nameOverride: Service name is ${name}, want custom-full"
name="$(helm template "$CHART" "${IMG[@]}" --set nameOverride=other \
  --show-only templates/service.yaml | awk '/^  name:/ {print $2; exit}')"
[ "$name" = "other" ] ||
  fail "nameOverride fullname fallback broken: Service name is ${name}, want other"

echo "=== chart contract: monitors require the http-m metrics port ==="
# ServiceMonitor/PodMonitor scrape the named port http-m, which only exists
# when metrics.enabled is true; the combination must fail at render time
# instead of silently producing a scrape config for a nonexistent port.
if helm template "$CHART" "${IMG[@]}" --set metrics.enabled=false \
  --set serviceMonitor.enabled=true >/dev/null 2>&1; then
  fail "serviceMonitor.enabled with metrics.enabled=false rendered instead of failing"
fi
if helm template "$CHART" "${IMG[@]}" --set metrics.enabled=false \
  --set podMonitor.enabled=true >/dev/null 2>&1; then
  fail "podMonitor.enabled with metrics.enabled=false rendered instead of failing"
fi
port="$(helm template "$CHART" "${IMG[@]}" --set serviceMonitor.enabled=true \
  --show-only templates/servicemonitor.yaml | awk '/- port:/ {print $3; exit}')"
[ "$port" = "http-m" ] ||
  fail "ServiceMonitor with default metrics.enabled should scrape http-m, got ${port}"

echo "All chart contract checks passed."
