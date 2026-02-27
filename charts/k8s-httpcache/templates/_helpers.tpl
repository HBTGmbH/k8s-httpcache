{{/*
Expand the name of the chart.
*/}}
{{- define "k8s-httpcache.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "k8s-httpcache.fullname" -}}
{{- .Values.nameOverride | default .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "k8s-httpcache.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
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
{{ toYaml . }}
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
{{- toYaml $merged }}
{{- end }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "k8s-httpcache.selectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-httpcache.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- with .Values.selectorLabels }}
{{ toYaml . }}
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
Image reference.
*/}}
{{- define "k8s-httpcache.image" -}}
{{- $repo := required "image.repository is required" .Values.image.repository }}
{{- $tag := default .Chart.AppVersion .Values.image.tag }}
{{- if .Values.image.registry }}
{{- printf "%s/%s:%s" .Values.image.registry $repo $tag }}
{{- else }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}
{{- end }}

{{/*
Collect unique foreign namespaces from backends, values, and secrets.
Returns a dict with namespace as key and a dict of needed resource types as value.
Usage: {{ include "k8s-httpcache.foreignNamespaces" . }}
*/}}
{{- define "k8s-httpcache.foreignNamespaces" -}}
{{- $foreign := dict }}
{{- $releaseNs := .Release.Namespace }}
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
{{- $foreign | toJson }}
{{- end }}

{{/*
Whether the ClusterRole for node access should be created.
*/}}
{{- define "k8s-httpcache.createClusterRole" -}}
{{- if and .Values.rbac.create (not (eq (toString .Values.rbac.createClusterRole) "false")) }}
  {{- if or (eq (toString .Values.rbac.createClusterRole) "true") (and (eq (toString .Values.rbac.createClusterRole) "auto") (not .Values.template.zone)) }}
    {{- true }}
  {{- end }}
{{- end }}
{{- end }}
