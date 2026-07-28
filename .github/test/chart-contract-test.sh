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

echo "=== chart contract: autoscaling requires an explicit scaling target ==="
# With no targetCPU/targetMemory the rendered HPA has a null metrics list and
# silently falls back to the API server's implicit 80%-CPU default (which
# additionally needs CPU requests to do anything); that must fail at render
# time instead.
if helm template "$CHART" "${IMG[@]}" --set autoscaling.enabled=true \
  >/dev/null 2>&1; then
  fail "autoscaling.enabled without a scaling target rendered instead of failing"
fi
cpu="$(helm template "$CHART" "${IMG[@]}" --set autoscaling.enabled=true \
  --set autoscaling.targetCPU=80 --show-only templates/hpa.yaml |
  awk '/averageUtilization:/ {print $2; exit}')"
[ "$cpu" = "80" ] ||
  fail "autoscaling.targetCPU=80 did not render an 80% utilization target, got ${cpu}"
mem="$(helm template "$CHART" "${IMG[@]}" --set autoscaling.enabled=true \
  --set autoscaling.targetMemory=70 --show-only templates/hpa.yaml |
  awk '/name: memory/ {found=1} found && /averageUtilization:/ {print $2; exit}')"
[ "$mem" = "70" ] ||
  fail "autoscaling.targetMemory=70 alone did not render a memory target, got ${mem}"

echo "=== chart contract: namespace override needs namespace-qualified references ==="
# --namespace is the DEFAULT namespace for --service-name and for every
# --backend/--values/--secrets/--tls-cert/--backend-selector, but the chart
# creates the frontend Service and all RBAC in the RELEASE namespace. An
# unqualified reference therefore watches a namespace that has neither, so the
# watch is Forbidden, the initial snapshot never arrives and the pod exits at
# --startup-timeout (CrashLoopBackoff). It must fail at render time instead.
if helm template "$CHART" "${IMG[@]}" --namespace rel-ns --set namespace=other-ns \
  --set 'backends[0].name=api' --set 'backends[0].service=my-app' >/dev/null 2>&1; then
  fail "namespace=other-ns with unqualified references rendered instead of failing"
fi
if helm template "$CHART" "${IMG[@]}" --namespace rel-ns --set namespace=other-ns \
  >/dev/null 2>&1; then
  fail "namespace=other-ns with an unqualified serviceName rendered instead of failing"
fi
helm template "$CHART" "${IMG[@]}" --namespace rel-ns --set namespace=other-ns \
  --set serviceName=rel-ns/frontend --set 'backends[0].name=api' \
  --set 'backends[0].service=other-ns/my-app' >/dev/null ||
  fail "namespace=other-ns with fully qualified references must still render"

echo "=== chart contract: istio AuthorizationPolicy requires rules ==="
# Istio treats a policy with no rules as "the match will never occur": an ALLOW
# policy with rules: [] still selects the cache pods and denies every request
# ("RBAC: access denied") - client traffic, broadcast fan-out and mesh scrapes.
if helm template "$CHART" "${IMG[@]}" --set istio.authorizationPolicy.enabled=true \
  >/dev/null 2>&1; then
  fail "istio.authorizationPolicy.enabled with empty rules rendered instead of failing"
fi
ap="$(helm template "$CHART" "${IMG[@]}" --set istio.authorizationPolicy.enabled=true \
  --set 'istio.authorizationPolicy.rules[0].to[0].operation.methods[0]=GET' \
  --show-only templates/istio-authorizationpolicy.yaml)" ||
  fail "istio.authorizationPolicy with rules must render"
echo "$ap" | grep -q 'methods:' ||
  fail "istio.authorizationPolicy rules not rendered"

echo "=== chart contract: PodDisruptionBudget sets exactly one budget field ==="
# A PDB with neither field pins disruptionsAllowed to 0 (expectedCount stays 0
# in the disruption controller), so every eviction - kubectl drain, autoscaler
# scale-down, node upgrade - is refused forever; with both fields the API
# server rejects the manifest. A numeric 0 must survive template truthiness.
if helm template "$CHART" "${IMG[@]}" --set podDisruptionBudget.enabled=true \
  >/dev/null 2>&1; then
  fail "podDisruptionBudget.enabled without a budget field rendered instead of failing"
fi
if helm template "$CHART" "${IMG[@]}" --set podDisruptionBudget.enabled=true \
  --set podDisruptionBudget.minAvailable=1 --set podDisruptionBudget.maxUnavailable=1 \
  >/dev/null 2>&1; then
  fail "podDisruptionBudget with both minAvailable and maxUnavailable rendered instead of failing"
fi
pdb="$(helm template "$CHART" "${IMG[@]}" --set podDisruptionBudget.enabled=true \
  --set podDisruptionBudget.maxUnavailable=0 --show-only templates/poddisruptionbudget.yaml |
  awk '/maxUnavailable:/ {print $2; exit}')"
[ "$pdb" = "0" ] ||
  fail "podDisruptionBudget.maxUnavailable=0 rendered as '${pdb}', want 0 (with drops numeric zero)"
pdb="$(helm template "$CHART" "${IMG[@]}" --set podDisruptionBudget.enabled=true \
  --set podDisruptionBudget.minAvailable=0 --show-only templates/poddisruptionbudget.yaml |
  awk '/minAvailable:/ {print $2; exit}')"
[ "$pdb" = "0" ] ||
  fail "podDisruptionBudget.minAvailable=0 rendered as '${pdb}', want 0 (with drops numeric zero)"

echo "=== chart contract: default VCL accepts broadcast PURGEs from peer pods ==="
# Kubernetes does not SNAT pod-to-pod traffic, so a fanned-out PURGE reaches
# varnishd with the SENDING pod's IP as client.ip - never 127.0.0.1, not even
# for the self-directed leg. A loopback-only `acl purge` 405s every broadcast
# while the broadcast server still answers 200, so nothing is ever purged.
vcl="$(helm template "$CHART" "${IMG[@]}" --show-only templates/configmap-vcl.yaml)"
echo "$vcl" | awk '/acl purge \{/,/\}/' | grep -q '\.Frontends' ||
  fail "default vclTemplateContent's acl purge is loopback-only; broadcast PURGEs from peer pods are rejected with 405"

echo "=== chart contract: cluster-scoped RBAC names are unique per namespace ==="
# Two releases of the same name in different namespaces render the same
# cluster-scoped objects; under a GitOps controller the last reconcile wins and
# the loser's ServiceAccount silently loses nodes:get, which degrades zone
# detection (empty .LocalEndpoints) without the pod ever leaving Ready.
cra="$(helm template "$CHART" "${IMG[@]}" --namespace team-a \
  --show-only templates/clusterrole.yaml | awk '/^  name:/ {print $2; exit}')"
crb="$(helm template "$CHART" "${IMG[@]}" --namespace team-b \
  --show-only templates/clusterrole.yaml | awk '/^  name:/ {print $2; exit}')"
if [ -z "$cra" ] || [ "$cra" = "$crb" ]; then
  fail "ClusterRole name '${cra}' is identical across namespaces; two releases collide"
fi
binding="$(helm template "$CHART" "${IMG[@]}" --namespace team-a \
  --show-only templates/clusterrolebinding.yaml)"
echo "$binding" | grep -q "name: ${cra}$" ||
  fail "ClusterRoleBinding roleRef does not match the ClusterRole name ${cra}"

echo "=== chart contract: ReferenceGrant requires a 'from' entry ==="
# ReferenceGrantSpec.From carries MinItems=1, so the default empty list renders
# 'from: []' and the API server rejects the whole release at install time.
if helm template "$CHART" "${IMG[@]}" --set referenceGrant.enabled=true \
  >/dev/null 2>&1; then
  fail "referenceGrant.enabled with an empty 'from' rendered instead of failing"
fi
rg="$(helm template "$CHART" "${IMG[@]}" --set referenceGrant.enabled=true \
  --set 'referenceGrant.from[0].group=gateway.networking.k8s.io' \
  --set 'referenceGrant.from[0].kind=HTTPRoute' \
  --set 'referenceGrant.from[0].namespace=other-ns' \
  --show-only templates/referencegrant.yaml)" ||
  fail "referenceGrant with a 'from' entry must render"
echo "$rg" | grep -q 'kind: HTTPRoute' || fail "referenceGrant.from not rendered"

echo "=== chart contract: valuesDirs mounts always have a matching volume ==="
# The volumeMount loop used to be unconditional while the volume loop is gated
# on .configMap, so an entry without the documented-optional configMap produced
# a mount with no volume and the API server rejected the whole Deployment.
vd="$(helm template "$CHART" "${IMG[@]}" --set 'valuesDirs[0].name=mydir' \
  --set 'valuesDirs[0].path=/etc/values-dir' --show-only templates/workload.yaml)"
if echo "$vd" | grep -q 'name: values-dir-mydir'; then
  fail "valuesDirs entry without configMap renders a volumeMount with no matching volume"
fi
echo "$vd" | grep -q -- '--values-dir=mydir:/etc/values-dir' ||
  fail "--values-dir flag missing for a valuesDirs entry without configMap"
vd="$(helm template "$CHART" "${IMG[@]}" --set 'valuesDirs[0].name=mydir' \
  --set 'valuesDirs[0].path=/etc/values-dir' --set 'valuesDirs[0].configMap=dir-cm' \
  --show-only templates/workload.yaml)"
[ "$(echo "$vd" | grep -c 'name: values-dir-mydir')" -eq 2 ] ||
  fail "valuesDirs entry with configMap must render exactly one volume and one volumeMount"

echo "=== chart contract: valuesDirs volume names are valid RFC 1123 labels ==="
# The entry name doubles as the Go-template key in the VCL (.Values.<name>.<key>),
# where a dash is a parse error - so camelCase/snake_case names are the natural
# choice, and both are illegal in a Kubernetes volume name.
vd="$(helm template "$CHART" "${IMG[@]}" --set 'valuesDirs[0].name=myTuning_v2' \
  --set 'valuesDirs[0].path=/etc/values-dir' --set 'valuesDirs[0].configMap=dir-cm' \
  --show-only templates/workload.yaml)"
for vn in $(echo "$vd" | sed -n 's/^ *- name: \(values-dir-.*\)$/\1/p'); do
  echo "$vn" | grep -qE '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' ||
    fail "valuesDirs volume name '${vn}' is not a lowercase RFC 1123 label"
done
echo "$vd" | grep -q -- '--values-dir=myTuning_v2:/etc/values-dir' ||
  fail "sanitising the volume name must not change the --values-dir template key"

echo "=== chart contract: container ports match the ports the processes bind ==="
# container.*Port only feeds containerPort/probes/Service targetPort/NetworkPolicy;
# what the processes bind comes from --listen-addr/--metrics-addr/--broadcast-addr.
# A mismatch means probes on a closed port (CrashLoopBackoff) and a Service that
# blackholes, so the two must be validated against each other at render time.
if helm template "$CHART" "${IMG[@]}" --set-string 'metrics.addr=:9200' >/dev/null 2>&1; then
  fail "metrics.addr=:9200 with container.httpMetricsPort=9101 rendered instead of failing"
fi
if helm template "$CHART" "${IMG[@]}" --set container.httpMetricsPort=9200 >/dev/null 2>&1; then
  fail "container.httpMetricsPort=9200 with the default metrics.addr rendered instead of failing"
fi
if helm template "$CHART" "${IMG[@]}" --set container.httpPort=8081 >/dev/null 2>&1; then
  fail "container.httpPort=8081 without a matching listenAddrs entry rendered instead of failing"
fi
if helm template "$CHART" "${IMG[@]}" --set-string 'listenAddrs[0]=http=:8081\,HTTP' \
  >/dev/null 2>&1; then
  fail "listenAddrs on :8081 with container.httpPort=8080 rendered instead of failing"
fi
if helm template "$CHART" "${IMG[@]}" --set-string 'broadcast.addr=:9000' >/dev/null 2>&1; then
  fail "broadcast.addr=:9000 with container.httpBroadcastPort=8088 rendered instead of failing"
fi
helm template "$CHART" "${IMG[@]}" --set-string 'listenAddrs[0]=http=:8081\,HTTP' \
  --set container.httpPort=8081 --set-string 'metrics.addr=:9200' \
  --set container.httpMetricsPort=9200 --set-string 'broadcast.addr=:9000' \
  --set container.httpBroadcastPort=9000 >/dev/null ||
  fail "matching container ports and listen addresses must render"
if helm template "$CHART" "${IMG[@]}" --set-string 'listenAddrs[0]=http=:8080\,HTTP' \
  --set-string 'listenAddrs[1]=https=:9443\,https' --set 'tlsCerts[0].name=fe' \
  --set 'tlsCerts[0].secret=my-tls' >/dev/null 2>&1; then
  fail "https listener on :9443 with container.httpsPort=8443 rendered instead of failing"
fi
helm template "$CHART" "${IMG[@]}" --set-string 'listenAddrs[0]=http=:8080\,HTTP' \
  --set-string 'listenAddrs[1]=https=:9443\,https' --set 'tlsCerts[0].name=fe' \
  --set 'tlsCerts[0].secret=my-tls' --set container.httpsPort=9443 >/dev/null ||
  fail "matching https listener and container.httpsPort must render"

echo "=== chart contract: explicit zero values reach the rendered manifest ==="
# Go templates treat the integer 0 as empty, so every knob documented as
# "0 = no limit" / "0 disables" was dropped by its `with` guard and the app (or
# Kubernetes) default silently won - the exact opposite of what was asked for.
zeros="$(helm template "$CHART" "${IMG[@]}" --set timing.startupTimeout=0 \
  --set vcl.reloadRetries=0 --set vcl.kept=0 --set revisionHistoryLimit=0 \
  --set drain.timeout=0 --set metrics.writeTimeout=0 --set debounce.max=0 \
  --show-only templates/workload.yaml)"
for expect in --startup-timeout=0 --vcl-reload-retries=0 --vcl-kept=0 \
  --drain-timeout=0 --metrics-write-timeout=0 --debounce-max=0; do
  echo "$zeros" | grep -q -- "$expect" ||
    fail "explicit 0 dropped from the rendered args: ${expect} missing"
done
echo "$zeros" | grep -qE '^  revisionHistoryLimit: 0$' ||
  fail "revisionHistoryLimit: 0 dropped from the rendered Deployment"

echo "=== chart contract: broadcast write timeout exceeds read + client ==="
# The binary refuses to start unless --broadcast-write-timeout exceeds
# --broadcast-read-timeout + --broadcast-client-timeout; raising one alone
# renders a single flag whose two operands are invisible app defaults.
if helm template "$CHART" "${IMG[@]}" --set-string broadcast.clientTimeout=30s \
  >/dev/null 2>&1; then
  fail "broadcast.clientTimeout=30s alone rendered instead of failing (binary rejects it at startup)"
fi
if helm template "$CHART" "${IMG[@]}" --set-string broadcast.readTimeout=60s \
  >/dev/null 2>&1; then
  fail "broadcast.readTimeout=60s alone rendered instead of failing (binary rejects it at startup)"
fi
helm template "$CHART" "${IMG[@]}" --set-string broadcast.clientTimeout=30s \
  --set-string broadcast.writeTimeout=60s >/dev/null ||
  fail "broadcast.clientTimeout=30s with writeTimeout=60s must render"

echo "=== chart contract: commonAnnotations reach every rendered resource ==="
# commonAnnotations is documented as "annotations to add to all resources", but
# the pod template, the Grafana dashboard ConfigMaps and the Argo
# AnalysisTemplates bypassed the annotations helper - exactly the inconsistency
# that breaks GitOps ownership/prune policies and pod-level injectors.
ca_values="$(mktemp)"
cat >"$ca_values" <<'YAML'
commonAnnotations:
  team: platform
grafanaDashboards:
  enabled: true
  dashboards:
    overview: |
      {"title":"k8s-httpcache","panels":[]}
argoRollouts:
  enabled: true
  analysisTemplates:
    - name: success-rate
      spec:
        metrics:
          - name: success-rate
            interval: 1m
YAML
for tpl in workload grafana-dashboards analysistemplates; do
  helm template "$CHART" "${IMG[@]}" --values "$ca_values" \
    --show-only "templates/${tpl}.yaml" | grep -q 'team: platform' ||
    fail "commonAnnotations missing from templates/${tpl}.yaml"
done
# The pod template must carry it too, not just the workload metadata.
podann="$(helm template "$CHART" "${IMG[@]}" --values "$ca_values" \
  --show-only templates/workload.yaml | grep -c 'team: platform')"
[ "$podann" -ge 2 ] ||
  fail "commonAnnotations reached the workload metadata but not the pod template"
rm -f "$ca_values"

echo "=== chart contract: extraArgs render as quoted YAML scalars ==="
# An unquoted arg containing ": " parses as a YAML mapping (the Deployment is
# rejected when decoded) and a trailing space is silently folded away.
ea_values="$(mktemp)"
cat >"$ea_values" <<'YAML'
extraArgs:
  - '--varnishncsa-prefix=access: '
YAML
ea="$(helm template "$CHART" "${IMG[@]}" --values "$ea_values" \
  --show-only templates/workload.yaml)"
rm -f "$ea_values"
echo "$ea" | grep -qF -- '- "--varnishncsa-prefix=access: "' ||
  fail "extraArgs entry with ': ' and a trailing space is not rendered as a quoted scalar"

echo "=== chart contract: endpoint alerts name the backend, not the cache Service ==="
# The Prometheus Operator's ServiceMonitor unconditionally sets a `service`
# target label from the scraped Service, which (honor_labels: false) overwrites
# the metric's own `service` label and renames it to exported_service - so the
# alert summary named the cache instead of the failing backend.
pr="$(helm template "$CHART" "${IMG[@]}" --set prometheusRule.enabled=true \
  --show-only templates/prometheusrule.yaml)"
echo "$pr" | grep -q 'exported_service' ||
  fail "endpoint alert summaries do not fall back to \$labels.exported_service (ServiceMonitor collision)"
echo "$pr" | grep -q 'labels.service }}' ||
  fail "endpoint alert summaries dropped \$labels.service (correct under podMonitor)"

echo "=== chart contract: values.yaml descriptions survive helm-docs ==="
# helm-docs splits a "# -- " comment at the LAST " --" in the line, so a
# description containing a bare " --flag" is filed under a bogus key and the
# real key ships with an empty Description cell in the generated README.
bare="$(grep -nE '^[[:space:]]*#[[:space:]]--[[:space:]].*[[:space:]]--' "$CHART/values.yaml" || true)"
[ -z "$bare" ] ||
  fail "values.yaml description(s) contain a bare ' --' and render empty in README.md: ${bare}"
emptydesc="$(grep -cE '^\|.*\|[[:space:]]*\|$' "$CHART/README.md" || true)"
[ "$emptydesc" -eq 0 ] ||
  fail "${emptydesc} values row(s) in README.md have an empty Description column"

echo "All chart contract checks passed."
