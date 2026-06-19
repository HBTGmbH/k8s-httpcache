# k8s-httpcache

Running the Varnish/Vinyl HTTP cache on Kubernetes

**Homepage:** <https://github.com/HBTGmbH/k8s-httpcache>

## Installing the chart

The chart is published as an OCI artifact. To install a release named `my-release`:

```sh
helm install my-release oci://ghcr.io/hbtgmbh/charts/k8s-httpcache
```

To uninstall the release:

```sh
helm uninstall my-release
```

See the [project README](https://github.com/HBTGmbH/k8s-httpcache#readme) for full
configuration, VCL templating, broadcast/invalidation, TLS, metrics, and usage
documentation.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules |
| argoRollouts.analysisTemplates | list | `[]` | AnalysisTemplates to render (list of { name, spec }). Each is named after the chart fullname plus the entry name. Only rendered when argoRollouts.enabled is true. |
| argoRollouts.enabled | bool | `false` | Render the workload as an Argo Rollouts Rollout instead of a Deployment. The HPA/VPA scale target ref is switched to the Rollout automatically. |
| argoRollouts.strategy | object | `{}` | Rollout strategy (canary or blueGreen). When empty, defaults to a basic canary (`canary: {}`). Reference analysisTemplates from the steps here. |
| autoscaling.behavior | object | `{}` | Scaling behavior (scaleUp/scaleDown stabilization windows and policies) |
| autoscaling.enabled | bool | `false` | Enable HPA |
| autoscaling.maxReplicas | int | `10` | Maximum replicas |
| autoscaling.minReplicas | int | `1` | Minimum replicas |
| autoscaling.targetCPU | string | `""` | Target CPU utilization percentage |
| autoscaling.targetMemory | string | `""` | Target memory utilization percentage |
| backendDiscovery | list | `[]` | Backend service discovery by label selector (repeatable). Each entry: { selector (map), port (optional), namespace (optional), allNamespaces (optional) } Services matching the selector are automatically added as backends. The backend name in .Backends is the Service name. |
| backends | list | `[]` | Backend services (repeatable). Each entry: { name, service, port (optional) } Generates --backend=name:service or --backend=name:service:port |
| broadcast.addr | string | `""` | Listen address for the broadcast server (empty = app default ":8088") |
| broadcast.clientIdleTimeout | string | `""` | Max idle time for connections to Varnish pods (empty = app default 4s) |
| broadcast.clientTimeout | string | `""` | Timeout for each fan-out request (empty = app default 3s) |
| broadcast.drainTimeout | string | `""` | Time to wait for broadcast connections to drain (empty = app default 30s) |
| broadcast.enabled | bool | `true` |  |
| broadcast.readHeaderTimeout | string | `""` | Max time to read request headers on broadcast server (empty = app default 10s) |
| broadcast.serverIdleTimeout | string | `""` | Max idle time for client keep-alive connections (empty = app default 120s) |
| broadcast.shutdownTimeout | string | `""` | Time to wait for in-flight requests after draining (empty = app default 5s) |
| broadcast.targetListenAddr | string | `""` |  |
| clusterDomain | string | `"cluster.local"` | Cluster DNS domain, used to build the in-cluster Service FQDN (e.g. for Istio resources) |
| commonAnnotations | object | `{}` | Annotations to add to all resources |
| commonLabels | object | `{}` | Labels to add to all resources |
| container.httpBroadcastPort | int | `8088` | Boardcast port exposed by the k8s-httpcache container |
| container.httpMetricsPort | int | `9101` | Metrics port exposed by the k8s-httpcache container |
| container.httpPort | int | `8080` | HTTP port exposed by the varnishd process |
| container.httpsPort | int | `8443` | HTTPS port exposed by the varnishd process (only when tlsCerts is set). |
| debounce.backendDuration | string | `""` | Debounce duration for backend changes (empty = inherits duration) |
| debounce.backendMax | string | `""` | Max debounce duration for backend changes (empty = inherits max) |
| debounce.duration | string | `""` | Debounce duration for endpoint changes (empty = app default 2s) |
| debounce.frontendDuration | string | `""` | Debounce duration for frontend changes (empty = inherits duration) |
| debounce.frontendMax | string | `""` | Max debounce duration for frontend changes (empty = inherits max) |
| debounce.latencyBuckets | string | `""` | Histogram bucket boundaries for debounce_latency_seconds metric (empty = app default) |
| debounce.max | string | `""` | Maximum debounce duration before forced reload (empty = app default 0s) |
| dnsConfig | object | `{}` | Custom DNS configuration |
| dnsPolicy | string | `""` | DNS policy (ClusterFirst, Default, None, ClusterFirstWithHostNet) |
| drain.delay | string | `""` | Delay before polling for active sessions (empty = app default 15s) |
| drain.enabled | bool | `true` | Enable graceful connection draining on shutdown |
| drain.pollInterval | string | `""` | Poll interval for active sessions (empty = app default 1s) |
| drain.timeout | string | `""` | Max time to wait for sessions to reach 0 (empty = app default 0s) |
| enableServiceLinks | string | `""` | Inject Service environment variables. Empty = Kubernetes default (true); set false to reduce environment clutter. |
| excludeAnnotations | list | `[]` | Annotation keys or prefixes to exclude from backend `.Annotations` (repeatable). A trailing `*` matches a prefix (e.g. "kubectl.kubernetes.io/*"). `kubectl.kubernetes.io/last-applied-configuration` is always excluded by default. |
| extraArgs | list | `[]` |  |
| extraContainers | list | `[]` | Extra containers (sidecars) to add to the pod |
| extraEnv | list | `[]` | Extra environment variables to add to the container |
| extraInitContainers | list | `[]` | Extra init containers to add to the pod |
| extraManifests | list | `[]` | Extra raw manifests to render with the release. Each entry is a Kubernetes object (or a templated string) rendered through `tpl`, e.g. an ExternalSecret. |
| extraVolumeMounts | list | `[]` | Extra volume mounts to add to the container |
| extraVolumes | list | `[]` | Extra volumes to add to the pod |
| fullnameOverride | string | `""` | Override the full release name |
| global | object | `{"imagePullSecrets":[],"imageRegistry":""}` | Global values shared with subcharts (and consumed by this chart). |
| global.imagePullSecrets | list | `[]` | Global image pull secrets; merged with imagePullSecrets. |
| global.imageRegistry | string | `""` | Global image registry; used when image.registry is empty. |
| grafanaDashboards.annotations | object | `{}` | Annotations added to each dashboard ConfigMap (e.g. k8s-sidecar folder) |
| grafanaDashboards.dashboards | object | `{}` | Dashboards to render, keyed by name. Each value is dashboard JSON (string) or an object (rendered to JSON). Each becomes a ConfigMap data key, the entry name suffixed with .json. |
| grafanaDashboards.enabled | bool | `false` | Enable Grafana dashboard ConfigMaps (one per entry) |
| grafanaDashboards.label | string | `"grafana_dashboard"` | Label key the Grafana sidecar watches for dashboard auto-import |
| grafanaDashboards.labelValue | string | `"1"` | Value for the dashboard label |
| hostAliases | list | `[]` | Host aliases (extra entries for the pod's /etc/hosts) |
| httpRoute.annotations | object | `{}` | Annotations for the HTTPRoute |
| httpRoute.enabled | bool | `false` | Enable HTTPRoute (Gateway API) |
| httpRoute.hostnames | list | `[]` | Hostnames to match |
| httpRoute.parentRefs | list | `[]` | Parent gateway references |
| httpRoute.rules | list | `[{"matches":[{"path":{"type":"PathPrefix","value":"/"}}]}]` | Routing rules |
| image.digest | string | `""` | Image digest (e.g. "sha256:..."). When set, the image is referenced by digest (repository@digest) and takes precedence over tag. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.registry | string | `""` | Container image registry (e.g. "docker.io", "registry.example.com/proxy"). When set, the image reference becomes registry/repository:tag. |
| image.repository | string | `""` | Container image repository |
| image.tag | string | `""` | Image tag (defaults to appVersion) |
| imagePullSecrets | list | `[]` | Image pull secrets (merged with global.imagePullSecrets) |
| ingress.annotations | object | `{}` | Annotations for the Ingress |
| ingress.className | string | `""` | Ingress class name |
| ingress.enabled | bool | `false` | Enable Ingress |
| ingress.hosts | list | `[{"host":"chart-example.local","paths":[{"path":"/","pathType":"Prefix"}]}]` | Ingress hosts |
| ingress.tls | list | `[]` | TLS configuration |
| istio.authorizationPolicy.action | string | `"ALLOW"` | Action: ALLOW, DENY, AUDIT, CUSTOM |
| istio.authorizationPolicy.annotations | object | `{}` | Annotations for the AuthorizationPolicy |
| istio.authorizationPolicy.enabled | bool | `false` | Enable Istio AuthorizationPolicy (L7 access control) |
| istio.authorizationPolicy.provider | object | `{}` | External authorization provider (only for action: CUSTOM) |
| istio.authorizationPolicy.rules | list | `[]` | Authorization rules |
| istio.destinationRule.annotations | object | `{}` | Annotations for the DestinationRule |
| istio.destinationRule.enabled | bool | `false` | Enable Istio DestinationRule |
| istio.destinationRule.host | string | `""` | Host (empty = the in-cluster Service FQDN) |
| istio.destinationRule.subsets | list | `[]` | Subsets for traffic splitting |
| istio.destinationRule.trafficPolicy | object | `{}` | Traffic policy (load balancing, connection pool, outlier detection, TLS) |
| istio.peerAuthentication.annotations | object | `{}` | Annotations for the PeerAuthentication |
| istio.peerAuthentication.enabled | bool | `false` | Enable Istio PeerAuthentication (mTLS mode) |
| istio.peerAuthentication.mtls | object | `{"mode":"STRICT"}` | Mesh-wide mTLS settings for the workload |
| istio.peerAuthentication.portLevelMtls | object | `{}` | Per-port mTLS overrides (port number -> { mode }) |
| istio.requestAuthentication.annotations | object | `{}` | Annotations for the RequestAuthentication |
| istio.requestAuthentication.enabled | bool | `false` | Enable Istio RequestAuthentication (JWT validation) |
| istio.requestAuthentication.jwtRules | list | `[]` | JWT rules (issuer, jwksUri, etc.) |
| istio.sidecar.annotations | object | `{}` | Annotations for the Sidecar |
| istio.sidecar.egress | list | `[]` | Egress hosts. When empty, defaults to this namespace plus istio-system. |
| istio.sidecar.enabled | bool | `false` | Enable Istio Sidecar resource |
| istio.sidecar.ingress | list | `[]` | Inbound listeners |
| istio.sidecar.outboundTrafficPolicy | object | `{}` | Outbound traffic policy (e.g. mode: REGISTRY_ONLY) |
| istio.sidecar.workloadSelector | bool | `true` | Scope the Sidecar to this app's pods via a workloadSelector (false = namespace-wide) |
| istio.virtualService.annotations | object | `{}` | Annotations for the VirtualService |
| istio.virtualService.enabled | bool | `false` | Enable Istio VirtualService |
| istio.virtualService.gateways | list | `[]` | Gateways to bind (empty = mesh-internal only) |
| istio.virtualService.hosts | list | `[]` | Hosts to match (empty = the Service short name) |
| istio.virtualService.http | list | `[]` | HTTP routes. When empty, a default route to the Service is generated. |
| istio.virtualService.tcp | list | `[]` | TCP routes |
| istio.virtualService.tls | list | `[]` | TLS routes |
| lifecycle | object | `{}` | Container lifecycle hooks (e.g. preStop, postStart) |
| listenAddrs | list | `[]` | Varnish listen addresses (repeatable). Each entry is passed as --listen-addr value. Example: ["http=:8080,HTTP", "https=:8443,PROXY"] For native frontend TLS (Varnish 9+), add an https listener and configure tlsCerts below, e.g. ["http=:8080,http", "https=:8443,https"]. Default (empty list): the application default "http=:8080,HTTP" is used. |
| livenessProbe | object | `{"failureThreshold":3,"httpGet":{"path":"/healthz","port":"http-m"},"periodSeconds":1}` | Liveness probe configuration (only used when metrics.enabled is true) |
| logging.format | string | `""` | Log format: text, json (empty = app default text) |
| logging.level | string | `""` | Log level: DEBUG, INFO, WARN, ERROR (empty = app default INFO) |
| metrics.addr | string | `""` | Listen address for the metrics server (empty = app default ":9101") |
| metrics.enabled | bool | `true` |  |
| metrics.readHeaderTimeout | string | `""` | Max time to read request headers on metrics server (empty = app default 10s) |
| metrics.scrapeAnnotations | bool | `false` | Add the de-facto prometheus.io/{scrape,path,port} annotations to the pods for annotation-based Prometheus discovery. Leave false when using the ServiceMonitor or PodMonitor to avoid double scraping. |
| metrics.varnishstatExport | bool | `false` | Enable varnishstat Prometheus exporter |
| metrics.varnishstatExportFilter | string | `""` | Counter groups to export (empty = all). Only effective when varnishstatExport is true. |
| minReadySeconds | string | `""` | Minimum seconds a new pod must be ready before it is considered available (empty = omit) |
| nameOverride | string | `""` | Override the chart name |
| namespace | string | `""` |  |
| networkPolicy.allowDNS | bool | `true` | Allow DNS egress (UDP/TCP 53). Egress to the Kubernetes API server and to backends is cluster-specific and must be added via networkPolicy.egress. |
| networkPolicy.annotations | object | `{}` | Annotations for the NetworkPolicy |
| networkPolicy.egress | list | `[]` | Additional egress rules (appended after the DNS rule when allowDNS is true) |
| networkPolicy.enabled | bool | `false` | Enable NetworkPolicy |
| networkPolicy.ingress | list | `[]` | Ingress rules. When empty, a default rule allows traffic to the exposed ports (http, plus https/broadcast/metrics when enabled) from any source. |
| networkPolicy.policyTypes | list | `["Ingress","Egress"]` | Policy types to enforce |
| nodeSelector | object | `{}` | Node selector |
| podAnnotations | object | `{}` | Annotations to add to pods |
| podDisruptionBudget.enabled | bool | `false` | Enable PDB |
| podDisruptionBudget.maxUnavailable | string | `""` | Maximum unavailable pods |
| podDisruptionBudget.minAvailable | string | `""` | Minimum available pods |
| podLabels | object | `{}` | Labels to add to pods |
| podMonitor.enabled | bool | `false` | Enable Prometheus PodMonitor |
| podMonitor.interval | string | `""` | Scrape interval |
| podMonitor.labels | object | `{}` | Additional labels for the PodMonitor |
| podMonitor.namespace | string | `""` | Namespace for the PodMonitor (defaults to release namespace) |
| podMonitor.relabelings | list | `[]` | Relabelings |
| podMonitor.scrapeTimeout | string | `""` | Scrape timeout |
| podSecurityContext | object | `{}` | Pod-level security context |
| priorityClassName | string | `""` | Priority class name for pod scheduling priority |
| prometheusRule.defaultRules | bool | `true` | Include the built-in default alert rules (varnishd down, no ready backend endpoints, VCL render errors, VCL rollbacks) |
| prometheusRule.enabled | bool | `false` | Enable Prometheus PrometheusRule (alerting/recording rules) |
| prometheusRule.labels | object | `{}` | Additional labels for the PrometheusRule (e.g. to match the Prometheus ruleSelector) |
| prometheusRule.namespace | string | `""` | Namespace for the PrometheusRule (defaults to release namespace) |
| prometheusRule.rules | list | `[]` | Extra rule groups (passthrough, appended after the default group) |
| rbac.create | bool | `true` | Create RBAC resources (Role, RoleBinding) |
| rbac.createClusterRole | string | `"auto"` | Create ClusterRole for node access (zone auto-detection). "auto" creates it when template.zone is empty; set true/false to override. |
| readinessProbe | object | `{"failureThreshold":1,"httpGet":{"path":"/readyz","port":"http-m"},"periodSeconds":1}` | Readiness probe configuration (only used when metrics.enabled is true) |
| referenceGrant.annotations | object | `{}` | Annotations for the ReferenceGrant |
| referenceGrant.enabled | bool | `false` | Enable a ReferenceGrant allowing cross-namespace references to the Service |
| referenceGrant.from | list | `[]` | Allowed referents (list of { group, kind, namespace }), e.g. HTTPRoutes in another namespace. |
| referenceGrant.to | list | `[]` | Targets the grant permits references to (empty = this chart's Service) |
| replicaCount | int | `1` | Number of replicas (ignored when autoscaling.enabled is true) |
| resources | object | `{}` | Resource requests and limits |
| revisionHistoryLimit | string | `""` | Number of old ReplicaSets to retain for rollback |
| runtimeClassName | string | `""` | Runtime class name (e.g. gVisor, Kata Containers) |
| schedulerName | string | `""` | Scheduler name for the pods |
| secrets | list | `[]` | Secrets to watch for template values (repeatable). Each entry: { name, secret } Generates --secrets=name:secret |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"privileged":false,"readOnlyRootFilesystem":true,"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000}` | Container-level security context |
| selectorLabels | object | `{}` | Extra labels added to selector matchLabels (and thus to pod labels and all label selectors). WARNING: changing these on an existing release will cause a new Deployment to be created and the old ReplicaSet to be orphaned. |
| service.annotations | object | `{}` | Annotations for the Service |
| service.httpBroadcastPort | int | `8088` | Boardcast port exposed by the Service |
| service.httpMetricsPort | int | `9101` | Metrics port exposed by the Service |
| service.httpPort | int | `80` | HTTP port exposed by the Service |
| service.httpsPort | int | `443` | HTTPS port exposed by the Service (only when tlsCerts is set). |
| service.type | string | `"ClusterIP"` | Service type |
| serviceAccount.annotations | object | `{}` | Annotations for the ServiceAccount |
| serviceAccount.automountServiceAccountToken | bool | `true` | Automount API credentials |
| serviceAccount.create | bool | `true` | Create a ServiceAccount |
| serviceAccount.name | string | `""` | ServiceAccount name (defaults to chart fullname) |
| serviceMonitor.enabled | bool | `false` | Enable Prometheus ServiceMonitor |
| serviceMonitor.interval | string | `""` | Scrape interval |
| serviceMonitor.labels | object | `{}` | Additional labels for the ServiceMonitor |
| serviceMonitor.namespace | string | `""` | Namespace for the ServiceMonitor (defaults to release namespace) |
| serviceMonitor.relabelings | list | `[]` | Relabelings |
| serviceMonitor.scrapeTimeout | string | `""` | Scrape timeout |
| serviceName | string | `""` |  |
| startupProbe | object | `{"failureThreshold":30,"httpGet":{"path":"/ready","port":"http"},"periodSeconds":1}` | Startup probe configuration |
| strategy | object | `{"rollingUpdate":{"maxSurge":1,"maxUnavailable":0},"type":"RollingUpdate"}` | Deployment strategy (ignored when argoRollouts.enabled is true) |
| template.delims | string | `""` | Template delimiters (empty = app default "<< >>") |
| template.funcs | string | `""` | Template function library: "sprig" (default) or "sprout" (empty = app default sprig) |
| template.zone | string | `""` | Topology zone override (empty = auto-detect from NODE_NAME) |
| terminationGracePeriodSeconds | int | `90` | Termination grace period in seconds |
| tlsCerts | list | `[]` | kubernetes.io/tls Secrets to install as frontend TLS certificates (Varnish 9+, repeatable). Each entry: { name, secret } where secret references a Secret with tls.crt/tls.key (e.g. produced by cert-manager). Generates --tls-cert=name:secret. Requires an https listener in listenAddrs (e.g. "https=:8443,https"). Certificates are hot-reloaded on rotation without restarting Varnish; multiple entries are selected by SNI. The 'name' is a logical/SNI label. |
| tolerations | list | `[]` | Tolerations |
| topologySpreadConstraints | list | `[]` | Topology spread constraints |
| values | list | `[]` | ConfigMaps to watch for template values (repeatable). Each entry: { name, configmap } Generates --values=name:configmap |
| valuesDirPollInterval | string | `""` |  |
| valuesDirs | list | `[]` | Directories to poll for YAML template values (repeatable). Each entry: { name, path, configMap (optional — creates a volume from this ConfigMap) } Generates --values-dir=name:path |
| varnish.adminTimeout | string | `""` | Max time to wait for the cache admin CLI to become ready (empty = app default 30s) |
| varnish.varnishadmPath | string | `""` | Path to varnishadm binary (empty = auto-detect) |
| varnish.varnishdPath | string | `""` | Path to varnishd binary (empty = auto-detect) |
| varnish.varnishstatPath | string | `""` | Path to varnishstat binary (empty = auto-detect) |
| varnishdExtraArgs | list | `[]` |  |
| varnishncsa.backend | bool | `false` | Log backend requests instead of client requests |
| varnishncsa.enabled | bool | `false` | Enable varnishncsa access logging subprocess |
| varnishncsa.format | string | `""` | Custom log format string (empty = app default) |
| varnishncsa.output | string | `""` | Output file path (empty = stdout) |
| varnishncsa.path | string | `""` | Path to varnishncsa binary (empty = app default "varnishncsa") |
| varnishncsa.prefix | string | `""` | Prefix for each access log line on stdout (empty = app default "[access] ") |
| varnishncsa.query | string | `""` | VSL query expression (empty = none) |
| vcl.fileWatch | string | `""` | Watch VCL template and values-dir paths for changes. Empty string = omit flag (app default true). Set to true or false explicitly to emit the flag. |
| vcl.kept | string | `""` | Number of old VCL objects to retain after reload (empty = app default 0) |
| vcl.reloadRetries | string | `""` | Max retry attempts for vcl.load failures (empty = app default 3) |
| vcl.reloadRetryInterval | string | `""` | Wait between vcl.load retry attempts (empty = app default 2s) |
| vcl.shutdownTimeout | string | `""` | Time to wait for varnishd to exit before SIGKILL (empty = app default 30s) |
| vcl.templateWatchInterval | string | `""` | Poll interval for VCL template file changes (empty = app default 5s) |
| vclTemplate | string | `"/etc/k8s-httpcache/vcl.tmpl"` | Path to the VCL template inside the container |
| vclTemplateContent | string | a round-robin VCL template (see values.yaml) | VCL template rendered into the ConfigMap. This default provides a simple round-robin setup that works out of the box. |
| verticalPodAutoscaler.annotations | object | `{}` | Annotations for the VPA |
| verticalPodAutoscaler.enabled | bool | `false` | Enable VerticalPodAutoscaler. Do not combine with autoscaling (HPA) on the same CPU/memory resource. |
| verticalPodAutoscaler.resourcePolicy | object | `{}` | Per-container resource policy (minAllowed/maxAllowed/controlledResources) |
| verticalPodAutoscaler.updateMode | string | `"Off"` | Update mode: Off, Initial, Recreate, Auto |
| vinyl.vinyladmPath | string | `""` | Path to vinyladm binary (Vinyl Cache 9+; takes precedence over varnish paths) |
| vinyl.vinyldPath | string | `""` | Path to vinyld binary (Vinyl Cache 9+; takes precedence over varnish paths) |
| vinyl.vinylncsaPath | string | `""` | Path to vinylncsa binary (Vinyl Cache 9+; takes precedence over varnishncsa.path) |
| vinyl.vinylstatPath | string | `""` | Path to vinylstat binary (Vinyl Cache 9+; takes precedence over varnish paths) |

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| HBT Hamburger Berater Team GmbH |  | <https://www.hbt.de/> |

## Source Code

* <https://github.com/HBTGmbH/k8s-httpcache>
