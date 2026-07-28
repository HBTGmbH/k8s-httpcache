{{/*
Expand the name of the chart.
*/}}
{{- define "k8s-httpcache.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
Precedence: fullnameOverride > nameOverride > release name.
*/}}
{{- define "k8s-httpcache.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- .Values.nameOverride | default .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "k8s-httpcache.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Render a value as a Helm template using << >> as the delimiters, so values.yaml can
carry template expressions without quoting {{ }} (which YAML parses as a mapping).
Behaviour: if the (stringified) value contains "<<", rewrite << ... >> to {{ ... }} and run
`tpl`; otherwise return it unchanged - so releases that never use << >> render byte-for-byte
as before (fully backward compatible). Strings pass straight through; maps/lists are toYaml'd
first (pipe the call site through nindent).

DO NOT use this for vclTemplateContent or staticFiles: there << >> is the *application's*
runtime delimiter (rendered by plain `tpl`; use {{ }} for Helm-render-time values there).

Usage:
  scalar:    {{ include "k8s-httpcache.tpl" (dict "value" .Values.serviceName "ctx" $) }}
  structure: {{ include "k8s-httpcache.tpl" (dict "value" .Values.podAnnotations "ctx" $) | nindent 8 }}
*/}}
{{- define "k8s-httpcache.tpl" -}}
{{- $v := .value -}}
{{- if not (kindIs "string" $v) -}}
{{- $v = toYaml $v -}}
{{- end -}}
{{- if contains "<<" $v -}}
{{- tpl (regexReplaceAll "(?s)<<(.*?)>>" $v "{{${1}}}") .ctx -}}
{{- else -}}
{{- $v -}}
{{- end -}}
{{- end }}

{{/*
Like k8s-httpcache.tpl but ALWAYS runs `tpl`, so both << >> and {{ }} are rendered.
Used for sites that already ran `tpl` unconditionally (extraManifests), to stay backward
compatible while also accepting the << >> convention.
*/}}
{{- define "k8s-httpcache.render" -}}
{{- $v := .value -}}
{{- if not (kindIs "string" $v) -}}
{{- $v = toYaml $v -}}
{{- end -}}
{{- tpl (regexReplaceAll "(?s)<<(.*?)>>" $v "{{${1}}}") .ctx -}}
{{- end }}

{{/*
Whether a value is explicitly set. `with`/`if` treat the integer 0 (and false)
as empty, so every knob the chart documents as "0 = no limit" / "0 disables"
used to be dropped from the rendered args and the app default silently won -
the exact opposite of what the operator asked for. Returns "true" for 0 and
false, and "" for nil and "".
Usage: {{ if include "k8s-httpcache.isSet" .Values.timing.startupTimeout }}
*/}}
{{- define "k8s-httpcache.isSet" -}}
{{- if not (kindIs "invalid" .) -}}
{{- if ne (toString .) "" -}}true{{- end -}}
{{- end -}}
{{- end }}

{{/*
Volume name for a valuesDirs entry. The entry name doubles as the Go-template
key used in the VCL (.Values.<name>.<key>), where a dash is a parse error, so
camelCase/snake_case names are the natural choice - and both are illegal in a
Kubernetes volume name (a lowercase RFC 1123 label).
*/}}
{{- define "k8s-httpcache.valuesDirVolumeName" -}}
{{- printf "values-dir-%s" (regexReplaceAll "[^a-z0-9-]" (lower (toString .)) "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Cluster-scoped RBAC object name. Cluster-scoped objects share one global
namespace, so the release namespace has to be part of the name: two releases
with the same name in different namespaces would otherwise render the SAME
ClusterRole/ClusterRoleBinding, and under a GitOps controller the last
reconcile wins - the loser's ServiceAccount silently loses nodes:get and zone
detection degrades to empty .LocalEndpoints with the pod still Ready.
*/}}
{{- define "k8s-httpcache.clusterRoleName" -}}
{{- printf "%s-%s-nodes" (include "k8s-httpcache.fullname" .) .Release.Namespace | trunc 253 | trimSuffix "-" }}
{{- end }}

{{/*
Parse a Go duration ("30s", "1m30s", "500ms", "0") into milliseconds. Returns
"" for anything else (e.g. fractional durations), so callers skip validation
rather than guess.
*/}}
{{- define "k8s-httpcache.durationMillis" -}}
{{- $s := trim (toString .) -}}
{{- if eq $s "0" -}}
0
{{- else if regexMatch "^([0-9]+(ms|s|m|h))+$" $s -}}
{{- $ms := 0 -}}
{{- range regexFindAll "[0-9]+(ms|s|m|h)" $s -1 -}}
{{- $n := regexFind "[0-9]+" . | int64 -}}
{{- $u := regexFind "[a-z]+$" . -}}
{{- if eq $u "ms" -}}{{- $ms = add $ms $n -}}
{{- else if eq $u "s" -}}{{- $ms = add $ms (mul $n 1000) -}}
{{- else if eq $u "m" -}}{{- $ms = add $ms (mul $n 60000) -}}
{{- else -}}{{- $ms = add $ms (mul $n 3600000) -}}{{- end -}}
{{- end -}}
{{- $ms -}}
{{- end -}}
{{- end }}

{{/*
Ports the processes actually bind, collected from listenAddrs. Returns
{"http": [...], "https": [...]} as JSON; an entry is an https listener when one
of its protocol tokens is "https".
*/}}
{{- define "k8s-httpcache.listenPorts" -}}
{{- $http := list }}
{{- $https := list }}
{{- range .Values.listenAddrs }}
  {{- $parts := splitList "," (trim (include "k8s-httpcache.tpl" (dict "value" (toString .) "ctx" $))) }}
  {{- $addr := first $parts }}
  {{- if contains "=" $addr }}
    {{- $addr = last (splitList "=" $addr) }}
  {{- end }}
  {{- $port := regexFind "[0-9]+$" (trim $addr) }}
  {{- if $port }}
    {{- $isTLS := false }}
    {{- range rest $parts }}
      {{- if eq (lower (trim .)) "https" }}{{- $isTLS = true }}{{- end }}
    {{- end }}
    {{- if $isTLS }}
      {{- $https = append $https $port }}
    {{- else }}
      {{- $http = append $http $port }}
    {{- end }}
  {{- end }}
{{- end }}
{{- dict "http" $http "https" $https | toJson }}
{{- end }}

{{/*
Fail fast when the declared container ports do not match the ports the
processes bind. container.*Port only feeds containerPort/probes/Service
targetPort/NetworkPolicy, while varnishd, the metrics server and the broadcast
server bind --listen-addr / --metrics-addr / --broadcast-addr; nothing derives
one from the other. A mismatch means probes against a closed port (the kubelet
restarts the container forever) and a Service that blackholes, with a Ready
pod and no error anywhere - so it has to be a render-time failure.
*/}}
{{- define "k8s-httpcache.validatePorts" -}}
{{- $lp := include "k8s-httpcache.listenPorts" . | fromJson }}
{{- $httpPort := toString (.Values.container.httpPort | default 8080) }}
{{- if $lp.http }}
  {{- if not (has $httpPort $lp.http) }}
    {{- fail (printf "container.httpPort (%s) matches no http listenAddrs port (%s): the startupProbe, the Service targetPort 'http' and the default NetworkPolicy all use container.httpPort while varnishd binds --listen-addr, so the pod would never pass its startup probe. Set container.httpPort to the listenAddrs port." $httpPort (join ", " $lp.http)) }}
  {{- end }}
{{- else if and (not .Values.listenAddrs) (ne $httpPort "8080") }}
  {{- fail (printf "container.httpPort is %s but no listenAddrs entry is configured, so varnishd binds its default http=:8080: the startupProbe and the Service would target a closed port. Add a matching listener, e.g. listenAddrs: [\"http=:%s,HTTP\"]." $httpPort $httpPort) }}
{{- end }}
{{- if .Values.tlsCerts }}
  {{- $httpsPort := toString (.Values.container.httpsPort | default 8443) }}
  {{- if and $lp.https (not (has $httpsPort $lp.https)) }}
    {{- fail (printf "container.httpsPort (%s) matches no https listenAddrs port (%s): the Service targetPort 'https' would blackhole while varnishd serves TLS on another port. Set container.httpsPort to the https listener's port." $httpsPort (join ", " $lp.https)) }}
  {{- end }}
{{- end }}
{{- if .Values.metrics.enabled }}
  {{- $metricsPort := toString (.Values.container.httpMetricsPort | default 9101) }}
  {{- $addr := trim (toString (.Values.metrics.addr | default "")) }}
  {{- if $addr }}
    {{- if eq (lower $addr) "none" }}
      {{- fail "metrics.addr is \"none\": disable the metrics server with metrics.enabled: false instead, so the chart also drops the metrics port, probes and Service port" }}
    {{- end }}
    {{- $p := regexFind "[0-9]+$" $addr }}
    {{- if not $p }}
      {{- fail (printf "metrics.addr (%s) has no port; it must be a host:port listen address, e.g. \":9101\"" $addr) }}
    {{- end }}
    {{- if ne $p $metricsPort }}
      {{- fail (printf "container.httpMetricsPort (%s) does not match the port in metrics.addr (%s): the liveness/readiness probes, the Service targetPort 'http-m' and the ServiceMonitor/PodMonitor all use container.httpMetricsPort while /healthz and /readyz are served on --metrics-addr, so the kubelet would restart the container in a loop. Set container.httpMetricsPort: %s." $metricsPort $addr $p) }}
    {{- end }}
  {{- else if ne $metricsPort "9101" }}
    {{- fail (printf "container.httpMetricsPort is %s but metrics.addr is empty, so the metrics server binds its default :9101: the liveness/readiness probes would target a closed port. Set metrics.addr: \":%s\"." $metricsPort $metricsPort) }}
  {{- end }}
{{- end }}
{{- if .Values.broadcast.enabled }}
  {{- $bcastPort := toString (.Values.container.httpBroadcastPort | default 8088) }}
  {{- $addr := trim (toString (.Values.broadcast.addr | default "")) }}
  {{- if $addr }}
    {{- if eq (lower $addr) "none" }}
      {{- fail "broadcast.addr is \"none\": disable the broadcast server with broadcast.enabled: false instead, so the chart also drops the broadcast port and Service port" }}
    {{- end }}
    {{- $p := regexFind "[0-9]+$" $addr }}
    {{- if not $p }}
      {{- fail (printf "broadcast.addr (%s) has no port; it must be a host:port listen address, e.g. \":8088\"" $addr) }}
    {{- end }}
    {{- if ne $p $bcastPort }}
      {{- fail (printf "container.httpBroadcastPort (%s) does not match the port in broadcast.addr (%s): the Service targetPort 'http-b' and the default NetworkPolicy would point at a closed port, so PURGE fan-out is unreachable. Set container.httpBroadcastPort: %s." $bcastPort $addr $p) }}
    {{- end }}
  {{- else if ne $bcastPort "8088" }}
    {{- fail (printf "container.httpBroadcastPort is %s but broadcast.addr is empty, so the broadcast server binds its default :8088. Set broadcast.addr: \":%s\"." $bcastPort $bcastPort) }}
  {{- end }}
{{- end }}
{{- end }}

{{/*
Fail fast on a broadcast timeout combination the controller rejects at startup
("--broadcast-write-timeout must exceed --broadcast-read-timeout +
--broadcast-client-timeout"). Raising one of the three alone renders a single
flag whose two operands are invisible app defaults, so the CrashLoopBackoff is
impossible to see in the rendered manifest.
*/}}
{{- define "k8s-httpcache.validateBroadcastTimeouts" -}}
{{- if .Values.broadcast.enabled }}
  {{- $write := "30s" }}
  {{- if include "k8s-httpcache.isSet" .Values.broadcast.writeTimeout }}
    {{- $write = toString .Values.broadcast.writeTimeout }}
  {{- end }}
  {{- $read := "15s" }}
  {{- if include "k8s-httpcache.isSet" .Values.broadcast.readTimeout }}
    {{- $read = toString .Values.broadcast.readTimeout }}
  {{- end }}
  {{- $client := "3s" }}
  {{- if include "k8s-httpcache.isSet" .Values.broadcast.clientTimeout }}
    {{- $client = toString .Values.broadcast.clientTimeout }}
  {{- end }}
  {{- $w := include "k8s-httpcache.durationMillis" $write }}
  {{- $r := include "k8s-httpcache.durationMillis" $read }}
  {{- $c := include "k8s-httpcache.durationMillis" $client }}
  {{- if and $w $r $c }}
    {{- if and (gt (int64 $w) (int64 0)) (le (int64 $w) (add (int64 $r) (int64 $c))) }}
      {{- fail (printf "broadcast.writeTimeout (%s) must exceed broadcast.readTimeout (%s) + broadcast.clientTimeout (%s): the controller rejects this combination at startup and every pod CrashLoopBackOffs (unset values use the app defaults write 30s / read 15s / client 3s)" $write $read $client) }}
    {{- end }}
  {{- end }}
{{- end }}
{{- end }}

{{/*
Fail fast when `namespace` points somewhere other than the release namespace
while references rely on it as their implicit default. --namespace is only the
DEFAULT namespace for --service-name and for every
--backend/--values/--secrets/--tls-cert/--backend-selector, but the chart
creates the frontend Service in the release namespace and the cross-namespace
RBAC helper only understands the per-reference "ns/name" syntax. An unqualified
reference therefore watches a namespace with neither the Service nor a Role:
the watch is Forbidden, the initial endpoint snapshot never arrives and the pod
exits 1 at --startup-timeout.
*/}}
{{- define "k8s-httpcache.validateNamespace" -}}
{{- $ns := .Values.namespace | default "" }}
{{- if and $ns (ne $ns .Release.Namespace) }}
  {{- $unqualified := list }}
  {{- if not (contains "/" (.Values.serviceName | default "")) }}
    {{- $unqualified = append $unqualified "serviceName (the chart's own frontend Service is created in the release namespace)" }}
  {{- end }}
  {{- range .Values.backends }}
    {{- if not (contains "/" (.service | default "")) }}
      {{- $unqualified = append $unqualified (printf "backends[%s].service" (toString .name)) }}
    {{- end }}
  {{- end }}
  {{- range .Values.values }}
    {{- if not (contains "/" (.configmap | default "")) }}
      {{- $unqualified = append $unqualified (printf "values[%s].configmap" (toString .name)) }}
    {{- end }}
  {{- end }}
  {{- range .Values.secrets }}
    {{- if not (contains "/" (.secret | default "")) }}
      {{- $unqualified = append $unqualified (printf "secrets[%s].secret" (toString .name)) }}
    {{- end }}
  {{- end }}
  {{- range .Values.tlsCerts }}
    {{- if not (contains "/" (.secret | default "")) }}
      {{- $unqualified = append $unqualified (printf "tlsCerts[%s].secret" (toString .name)) }}
    {{- end }}
  {{- end }}
  {{- range $i, $d := .Values.backendDiscovery }}
    {{- if not (or $d.namespace $d.allNamespaces) }}
      {{- $unqualified = append $unqualified (printf "backendDiscovery[%d]" $i) }}
    {{- end }}
  {{- end }}
  {{- if $unqualified }}
    {{- fail (printf "namespace (%s) differs from the release namespace (%s), but these references have no explicit \"ns/name\" prefix and therefore default to %s: %s. The chart creates the frontend Service and all RBAC in %s, so the controller would watch a namespace with no Service and no Role (Forbidden -> CrashLoopBackoff at --startup-timeout). Qualify every reference (e.g. serviceName: %s/frontend, backends[].service: %s/my-app) or remove the namespace value." $ns .Release.Namespace $ns (join ", " $unqualified) .Release.Namespace $ns $ns) }}
  {{- end }}
{{- end }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "k8s-httpcache.labels" -}}
helm.sh/chart: {{ include "k8s-httpcache.chart" . }}
{{ include "k8s-httpcache.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ include "k8s-httpcache.tpl" (dict "value" . "ctx" $) }}
{{- end }}
{{- end }}

{{/*
Common annotations. Renders commonAnnotations merged with any extra annotations
passed as the template's context (use: include "k8s-httpcache.annotations" (dict "extra" .Values.foo.annotations "root" .))
When called without extra: include "k8s-httpcache.annotations" (dict "root" .)
*/}}
{{- define "k8s-httpcache.annotations" -}}
{{- $common := .root.Values.commonAnnotations | default dict }}
{{- $extra := .extra | default dict }}
{{- $merged := mustMergeOverwrite (deepCopy $common) $extra }}
{{- if $merged }}
{{- include "k8s-httpcache.tpl" (dict "value" $merged "ctx" .root) }}
{{- end }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "k8s-httpcache.selectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-httpcache.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- with .Values.selectorLabels }}
{{ include "k8s-httpcache.tpl" (dict "value" . "ctx" $) }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "k8s-httpcache.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "k8s-httpcache.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image reference. Supports an optional registry (falling back to global.imageRegistry)
and pinning by digest (image.digest takes precedence over image.tag).
*/}}
{{- define "k8s-httpcache.image" -}}
{{- $repo := required "image.repository is required" .Values.image.repository }}
{{- $registry := .Values.image.registry | default (.Values.global).imageRegistry }}
{{- $ref := $repo }}
{{- if $registry }}
{{- $ref = printf "%s/%s" $registry $repo }}
{{- end }}
{{- if .Values.image.digest }}
{{- printf "%s@%s" $ref .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" $ref (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/*
Fully qualified in-cluster Service DNS name.
*/}}
{{- define "k8s-httpcache.serviceFQDN" -}}
{{- printf "%s.%s.svc.%s" (include "k8s-httpcache.fullname" .) .Release.Namespace (.Values.clusterDomain | default "cluster.local") }}
{{- end }}

{{/*
Workload apiVersion/kind for scale targets (Argo Rollouts Rollout vs Deployment).
*/}}
{{- define "k8s-httpcache.workloadApiVersion" -}}
{{- if .Values.argoRollouts.enabled -}}argoproj.io/v1alpha1{{- else -}}apps/v1{{- end -}}
{{- end }}
{{- define "k8s-httpcache.workloadKind" -}}
{{- if .Values.argoRollouts.enabled -}}Rollout{{- else -}}Deployment{{- end -}}
{{- end }}

{{/*
Collect unique foreign namespaces from backends, values, and secrets.
Returns a dict with namespace as key and a dict of needed resource types as value.
Usage: {{ include "k8s-httpcache.foreignNamespaces" . }}
*/}}
{{- define "k8s-httpcache.foreignNamespaces" -}}
{{- $foreign := dict }}
{{- $releaseNs := .Release.Namespace }}
{{- /* The frontend Service may live in another namespace
       (serviceName: "ns/name"); the frontend EndpointSlice watcher and the
       event recorder both operate there and need RBAC. */}}
{{- if contains "/" (.Values.serviceName | default "") }}
  {{- $parts := splitList "/" .Values.serviceName }}
  {{- $ns := first $parts }}
  {{- if ne $ns $releaseNs }}
    {{- $existing := dict }}
    {{- if hasKey $foreign $ns }}
      {{- $existing = get $foreign $ns }}
    {{- end }}
    {{- $_ := set $existing "services" true }}
    {{- $_ := set $existing "endpointslices" true }}
    {{- $_ := set $foreign $ns $existing }}
  {{- end }}
{{- end }}
{{- range .Values.backends }}
  {{- if contains "/" .service }}
    {{- $parts := splitList "/" .service }}
    {{- $ns := first $parts }}
    {{- if ne $ns $releaseNs }}
      {{- $existing := dict }}
      {{- if hasKey $foreign $ns }}
        {{- $existing = get $foreign $ns }}
      {{- end }}
      {{- $_ := set $existing "services" true }}
      {{- $_ := set $existing "endpointslices" true }}
      {{- $_ := set $foreign $ns $existing }}
    {{- end }}
  {{- end }}
{{- end }}
{{- range .Values.values }}
  {{- if contains "/" .configmap }}
    {{- $parts := splitList "/" .configmap }}
    {{- $ns := first $parts }}
    {{- if ne $ns $releaseNs }}
      {{- $existing := dict }}
      {{- if hasKey $foreign $ns }}
        {{- $existing = get $foreign $ns }}
      {{- end }}
      {{- $_ := set $existing "configmaps" true }}
      {{- $_ := set $foreign $ns $existing }}
    {{- end }}
  {{- end }}
{{- end }}
{{- range .Values.secrets }}
  {{- if contains "/" .secret }}
    {{- $parts := splitList "/" .secret }}
    {{- $ns := first $parts }}
    {{- if ne $ns $releaseNs }}
      {{- $existing := dict }}
      {{- if hasKey $foreign $ns }}
        {{- $existing = get $foreign $ns }}
      {{- end }}
      {{- $_ := set $existing "secrets" true }}
      {{- $_ := set $foreign $ns $existing }}
    {{- end }}
  {{- end }}
{{- end }}
{{- range .Values.tlsCerts }}
  {{- if contains "/" .secret }}
    {{- $parts := splitList "/" .secret }}
    {{- $ns := first $parts }}
    {{- if ne $ns $releaseNs }}
      {{- $existing := dict }}
      {{- if hasKey $foreign $ns }}
        {{- $existing = get $foreign $ns }}
      {{- end }}
      {{- $_ := set $existing "secrets" true }}
      {{- $_ := set $foreign $ns $existing }}
    {{- end }}
  {{- end }}
{{- end }}
{{- range .Values.backendDiscovery }}
  {{- if and .namespace (not .allNamespaces) }}
    {{- $ns := .namespace }}
    {{- if ne $ns $releaseNs }}
      {{- $existing := dict }}
      {{- if hasKey $foreign $ns }}
        {{- $existing = get $foreign $ns }}
      {{- end }}
      {{- $_ := set $existing "services" true }}
      {{- $_ := set $existing "endpointslices" true }}
      {{- $_ := set $foreign $ns $existing }}
    {{- end }}
  {{- end }}
{{- end }}
{{- $foreign | toJson }}
{{- end }}

{{/*
Whether the ClusterRole should be created. In "auto" mode it is needed for
either of two independent reasons: the node lookup for zone auto-detection
(when template.zone is not set explicitly), or cluster-wide Service /
EndpointSlice watches (when any backendDiscovery entry sets allNamespaces) -
without the latter the all-namespace watch is Forbidden and the pod crash
loops at the startup timeout.
*/}}
{{- define "k8s-httpcache.createClusterRole" -}}
{{- if and .Values.rbac.create (not (eq (toString .Values.rbac.createClusterRole) "false")) }}
  {{- if or (eq (toString .Values.rbac.createClusterRole) "true") (and (eq (toString .Values.rbac.createClusterRole) "auto") (or (not .Values.template.zone) (include "k8s-httpcache.discoveryAllNamespaces" .))) }}
    {{- true }}
  {{- end }}
{{- end }}
{{- end }}

{{/*
Whether any backendDiscovery entry uses allNamespaces mode.
*/}}
{{- define "k8s-httpcache.discoveryAllNamespaces" -}}
{{- range .Values.backendDiscovery }}
  {{- if .allNamespaces }}
    {{- true }}
  {{- end }}
{{- end }}
{{- end }}
