# k8s-httpcache

[![Test and Build](https://github.com/HBTGmbH/k8s-httpcache/actions/workflows/test-and-build.yml/badge.svg)](https://github.com/HBTGmbH/k8s-httpcache/actions/workflows/test-and-build.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/HBTGmbH/k8s-httpcache)](https://goreportcard.com/report/github.com/HBTGmbH/k8s-httpcache)
[![License](https://img.shields.io/github/license/HBTGmbH/k8s-httpcache)](https://github.com/HBTGmbH/k8s-httpcache/blob/main/LICENSE)

Replacement for [kube-httpcache](https://github.com/mittwald/kube-httpcache).

## Features

- Supports startup/readiness probes without waiting for a non-empty frontend EndpointSlice
  - https://github.com/mittwald/kube-httpcache/issues/36
  - https://github.com/mittwald/kube-httpcache/issues/222
  - https://github.com/mittwald/kube-httpcache/issues/260
- Varnish does not hang on startup when endpoints are not yet available. Endpoints can become available at runtime and the VCL will update accordingly.
  - https://github.com/mittwald/kube-httpcache/issues/168
- Nicer CLI interface. varnishd cmd args can be directly specified after '--'
- Watching for changes of the VCL template in file system and dynamically reloading it at runtime
- Rollback for failed VCL template updates, such that frontend/backend changes still load fine after a failed VCL template was loaded
  - https://github.com/mittwald/kube-httpcache/issues/138
- Supports multiple backend groups
  - https://github.com/mittwald/kube-httpcache/issues/133
- Supports multiple listen addresses with the full Varnish `-a` syntax, including PROXY protocol
  - https://github.com/mittwald/kube-httpcache/issues/206
- Uses "<< ... >>" as Template delimiter to not clash with Helm templating
- Graceful connection draining on shutdown with active session polling via varnishstat
- Broadcast server that fans out requests (e.g. PURGE) to all Varnish frontend pods
- Prometheus metrics for VCL reloads, endpoint counts, broadcast stats, and more
- ExternalName service support for hostname-based backends
  - https://github.com/mittwald/kube-httpcache/issues/39
- Template values from Kubernetes ConfigMaps (`--values`) or mounted directories (`--values-dir`), dynamically reloaded on changes
- Cross-namespace backends and values via `namespace/service` syntax
- All [Sprig](http://masterminds.github.io/sprig/) template functions available in VCL templates (including [`env`](http://masterminds.github.io/sprig/os.html) for environment variables)
  - https://github.com/mittwald/kube-httpcache/issues/53
- Automatic Varnish version detection with support for Varnish 6, 7, 8, and trunk builds
- Structured logging with configurable format (text/json) and log level
- Endpoint change debouncing to avoid rapid VCL reload cycles
  - https://github.com/mittwald/kube-httpcache/issues/66

## Container image

The published image at `ghcr.io/hbtgmbh/k8s-httpcache` is a distroless (`FROM scratch`) binary-only image (linux/amd64, linux/arm64). It contains only the statically linked k8s-httpcache binary and no Varnish installation. The binary works with any Varnish distribution image (Alpine, Debian, Ubuntu, etc.).

Build your own image by copying the binary into a Varnish base image of your choice:

```dockerfile
FROM varnish:8.0.0-alpine
COPY --from=ghcr.io/hbtgmbh/k8s-httpcache:<version> /usr/local/bin/k8s-httpcache /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
```

## Quick start

[`.github/test/manifest.yaml`](.github/test/manifest.yaml) contains a complete working example. It creates the following resources:

- **ServiceAccount** — identity for the k8s-httpcache pod
- **Role** — permissions to list/watch services and endpointslices
- **RoleBinding** — binds the Role to the ServiceAccount
- **Deployment** — runs k8s-httpcache with Varnish (3 replicas, graceful connection draining)
- **Service** — exposes HTTP (port 80) and the broadcast server (port 8088)
- **ConfigMap** — holds the VCL template

Apply it with:

```bash
kubectl apply -f .github/test/manifest.yaml
```

## Configuration

All configuration is done via CLI flags. Arguments after `--` are passed directly to `varnishd`.

```
k8s-httpcache [flags] [-- varnishd-args...]
```

### Required flags

| Flag | Description |
|------|-------------|
| `--service-name` | Kubernetes Service to watch for frontends: `[namespace/]service` |
| `--namespace` | Kubernetes namespace (also used as default for services without a `namespace/` prefix) |
| `--vcl-template` | Path to VCL Go template file |

### Listen, backend, and values flags

| Flag | Default | Description |
|------|---------|-------------|
| `--listen-addr` | `http=:8080,HTTP` | Varnish listen address (repeatable). See [Listen address specification](#listen-address-specification). |
| `--backend` | | Backend service (repeatable). See [Backend specification](#backend-specification). |
| `--values` | | ConfigMap to watch for template values (repeatable). See [Values specification](#values-specification). |
| `--values-dir` | | Directory to poll for YAML template values (repeatable). See [Values from directories](#values-from-directories). |
| `--values-dir-poll-interval` | `5s` | Poll interval for `--values-dir` directories |

### Varnish paths

| Flag | Default | Description |
|------|---------|-------------|
| `--varnishd-path` | `varnishd` | Path to varnishd binary |
| `--varnishadm-path` | `varnishadm` | Path to varnishadm binary |
| `--varnishstat-path` | `varnishstat` | Path to varnishstat binary |
| `--admin-timeout` | `30s` | Max time to wait for the varnish admin port to become ready |

### Broadcast flags

| Flag | Default | Description |
|------|---------|-------------|
| `--broadcast-addr` | `:8088` | Broadcast server listen address (set to `""` to disable) |
| `--broadcast-target-listen-addr` | *(first `--listen-addr`)* | Name of the `--listen-addr` to target for fan-out |
| `--broadcast-drain-timeout` | `30s` | Time to wait for broadcast connections to drain before shutting down |
| `--broadcast-shutdown-timeout` | `5s` | Time to wait for in-flight broadcast requests to finish after draining |
| `--broadcast-server-idle-timeout` | `120s` | Max idle time for client keep-alive connections to the broadcast server |
| `--broadcast-read-header-timeout` | `10s` | Max time to read request headers on the broadcast server |
| `--broadcast-client-idle-timeout` | `4s` | Max idle time for connections to Varnish pods in the broadcast client pool |
| `--broadcast-client-timeout` | `3s` | Timeout for each fan-out request to a Varnish pod |

### Metrics flags

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-addr` | `:9101` | Listen address for Prometheus metrics (set to `""` to disable) |
| `--metrics-read-header-timeout` | `10s` | Max time to read request headers on the metrics server |

The metrics endpoint exposes the standard Go runtime and process metrics (`go_*`, `process_*`) plus the following application metrics, all prefixed with `k8s_httpcache_`:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vcl_reloads_total` | Counter | `result` | VCL reload attempts (`success` or `error`) |
| `vcl_render_errors_total` | Counter | | VCL template render failures |
| `vcl_template_changes_total` | Counter | | VCL template file changes detected on disk |
| `vcl_template_parse_errors_total` | Counter | | VCL template parse failures |
| `vcl_rollbacks_total` | Counter | | Template rollbacks to previous known-good version |
| `endpoint_updates_total` | Counter | `role`, `service` | EndpointSlice updates received (`frontend` or `backend`) |
| `values_updates_total` | Counter | `configmap` | ConfigMap value updates received |
| `endpoints` | Gauge | `role`, `service` | Current ready endpoint count per service |
| `varnishd_up` | Gauge | | Whether the varnishd process is running (1/0) |
| `broadcast_requests_total` | Counter | `method`, `status` | Broadcast HTTP requests |
| `broadcast_fanout_targets` | Gauge | | Number of frontend pods targeted by the last broadcast |
| `build_info` | Gauge | `version`, `goversion` | Build metadata (always 1) |

### Drain flags

| Flag | Default | Description |
|------|---------|-------------|
| `--drain` | `false` | Enable graceful connection draining on shutdown (see [Graceful shutdown](#graceful-shutdown--zero-downtime-deploys)) |
| `--drain-delay` | `15s` | Delay after marking backend sick before polling for active sessions |
| `--drain-poll-interval` | `1s` | Poll interval for active sessions during graceful drain |
| `--drain-timeout` | `0` | Max time to wait for active sessions to reach 0. Default `0` skips session polling. Set to a positive duration (e.g. `30s`) to poll and wait for connections to close. |

### Timing and logging flags

| Flag | Default | Description |
|------|---------|-------------|
| `--debounce` | `2s` | Debounce duration for endpoint changes |
| `--shutdown-timeout` | `30s` | Time to wait for varnishd to exit before sending SIGKILL |
| `--vcl-template-watch-interval` | `5s` | Poll interval for VCL template file changes |
| `--log-level` | `INFO` | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `--log-format` | `text` | Log format: `text`, `json` |

### Passing arguments to varnishd

Use the `--` separator to pass arguments directly to `varnishd`:

```bash
k8s-httpcache \
  --service-name=k8s-httpcache \
  --namespace=default \
  --vcl-template=/etc/k8s-httpcache/vcl.tmpl \
  --backend=nginx:nginx \
  -- \
  -s default,1M \
  -t 5s \
  -p default_grace=0s \
  -p timeout_idle=75s
```

### Varnish 6 notes

On Varnish 6, the default `varnishd` in `$PATH` is a wrapper script that always passes `-n`, which breaks `varnishd -V` (used to detect the Varnish version at startup). Use `--varnishd-path` to point to the real binary:

```
--varnishd-path=/usr/sbin/varnishd
```

Varnish 6 also does not default to `/var/lib/varnish` as working directory. You must pass `-n` explicitly via the `--` separator so that varnishd, varnishadm, and varnishstat all use the same working directory:

```
-- -n /var/lib/varnish
```

## Backend specification

Format: `name:[namespace/]service[:port]`

| Part | Required | Description |
|------|----------|-------------|
| `name` | yes | Template key used in VCL (e.g. `nginx`, `api`) |
| `namespace/` | no | Kubernetes namespace; defaults to `--namespace` if omitted |
| `service` | yes | Kubernetes Service name |
| `port` | no | Numeric port, named port, or omitted for first EndpointSlice port |

### Port resolution

- **Numeric port** (e.g. `:3000`) — used as-is
- **Named port** (e.g. `:http`) — looked up in the EndpointSlice
- **Omitted** — the first port from the EndpointSlice is used

### Examples

```
--backend=nginx:nginx              # first EndpointSlice port
--backend=api:my-svc:3000          # numeric port
--backend=api:my-svc:http          # named port
--backend=api:staging/my-svc:8080  # cross-namespace with numeric port
```

### Cross-namespace backends

Use the `namespace/service` syntax to reference a service in another namespace:

```
--backend=api:other-ns/my-service:8080
```

This requires a Role and RoleBinding in the target namespace granting list/watch on services and endpointslices to the k8s-httpcache ServiceAccount. See [RBAC](#rbac).

### ExternalName services

When a backend points to an ExternalName service, the hostname from the `externalName` field is used directly as the endpoint. Named ports are not supported for ExternalName services (there is no EndpointSlice to resolve them from). A numeric port is required; if omitted, port 80 is used with a warning.

## Listen address specification

Format: `[name=][host]:port[,proto...]`

| Part | Required | Description |
|------|----------|-------------|
| `name=` | no | Identifier for the listen address (used by `--broadcast-target-listen-addr`) |
| `host` | no | Bind IP; omit to listen on all interfaces |
| `port` | yes | Numeric port (1-65535) |
| `proto` | no | Comma-separated protocols passed to Varnish (e.g. `HTTP`, `PROXY`) |

### Examples

```
--listen-addr=http=:8080,HTTP                # named, all interfaces, HTTP protocol (default)
--listen-addr=:9090                           # unnamed, all interfaces
--listen-addr=admin=127.0.0.1:6082           # named, loopback only
--listen-addr=0.0.0.0:8080,HTTP,PROXY        # HTTP + PROXY protocol
```

## Values specification

Format: `name:[namespace/]configmap`

| Part | Required | Description |
|------|----------|-------------|
| `name` | yes | Template key, accessible as `.Values.<name>.<key>` |
| `namespace/` | no | Kubernetes namespace; defaults to `--namespace` if omitted |
| `configmap` | yes | Kubernetes ConfigMap name to watch |

Each ConfigMap `data` value (which Kubernetes stores as a string) is YAML-parsed into its natural type: plain strings stay strings, numbers become numbers, booleans become booleans, and structured YAML (maps, lists) becomes nested `map[string]any` / `[]any`. The parsed data is exposed under `.Values.<name>`. When the ConfigMap is updated, the VCL template is re-rendered and Varnish is reloaded (after debounce). If the ConfigMap does not exist, an empty map is used.

### Examples

```
--values=tuning:my-tuning-cm           # default namespace
--values=config:staging/app-config     # cross-namespace
```

Using values in a VCL template (flat string value):

```vcl
sub vcl_backend_response {
  set beresp.ttl = << index .Values.tuning "ttl" | default "120" >>s;
}
```

Structured YAML values are also supported. For example, given a ConfigMap:

```yaml
data:
  server: |
    host: example.com
    port: 8080
```

The nested fields are accessible in the template:

```vcl
set req.http.X-Origin = "<< .Values.config.server.host >>:<< .Values.config.server.port >>";
```

### Cross-namespace values

Use the `namespace/configmap` syntax to reference a ConfigMap in another namespace. This requires a Role and RoleBinding granting `list` and `watch` on `configmaps` in the target namespace. See [RBAC](#rbac).

## Values from directories

Format: `name:/path/to/dir`

| Part | Required | Description |
|------|----------|-------------|
| `name` | yes | Template key, accessible as `.Values.<name>.<key>` (must be unique across both `--values` and `--values-dir`) |
| `path` | yes | Filesystem directory path to poll for YAML files |

The `--values-dir` flag is an alternative to `--values` for cases where a ConfigMap is already mounted into the container's filesystem. Instead of watching the ConfigMap via the Kubernetes API, k8s-httpcache polls the mounted directory for `.yaml` and `.yml` files.

**Behavior:**
- Files must have a `.yaml` or `.yml` extension; other files are ignored
- Dotfiles (names starting with `.`) are skipped — Kubernetes mounts ConfigMaps with `..data` and `..version` symlinks
- The filename without extension becomes the key (e.g. `server.yaml` → key `"server"`)
- Each file is YAML-parsed into its natural type, identical to `--values` parsing
- The directory is polled at the `--values-dir-poll-interval` (default `5s`)
- The parsed data is exposed under `.Values.<name>`, same as `--values`

### Examples

```
--values-dir=tuning:/etc/config/tuning
--values-dir=config:/var/run/configmap
```

Given a ConfigMap mounted at `/etc/config/tuning` with files:

```
/etc/config/tuning/ttl.yaml     → contents: "300"
/etc/config/tuning/server.yaml  → contents: "host: example.com\nport: 8080"
```

The values are accessible in templates as `.Values.tuning.ttl` (300) and `.Values.tuning.server.host` ("example.com").

## VCL template

The VCL template is a standard Go [`text/template`](https://pkg.go.dev/text/template) with custom delimiters `<<` and `>>` (instead of `{{ }}`) to avoid clashes with Helm templating.

### Data model

The template receives the following data:

| Field | Type | Description |
|-------|------|-------------|
| `.Frontends` | `[]Frontend` | Varnish peer pods from the watched service EndpointSlice |
| `.Backends` | `map[string][]Endpoint` | Named backend groups keyed by the `name` from `--backend` |
| `.Values` | `map[string]map[string]any` | Template values keyed by the `name` from `--values` or `--values-dir`. Each value is YAML-parsed, so structured data (maps, lists, numbers) is accessible. |

Each `Frontend` / `Endpoint` has:

| Field | Type | Description |
|-------|------|-------------|
| `.IP` | `string` | Pod IP address (or hostname for ExternalName) |
| `.Port` | `int32` | Resolved port number |
| `.Name` | `string` | Pod name |

### Template functions

All [Sprig](https://masterminds.github.io/sprig/) template functions are available (the same library used by Helm). A few commonly useful ones for VCL templates:

| Function | Description |
|----------|-------------|
| `replace` | `replace old new src` — e.g. `<< replace "http" "https" .URL >>` |
| `upper` / `lower` | Convert to upper/lower case |
| `trimPrefix` / `trimSuffix` | Strip a prefix or suffix |
| `contains` / `hasPrefix` / `hasSuffix` | String predicates for `if` guards |
| `default` | `default "fallback" .Val` — use a default when a value is empty |
| `join` | `join ", " .List` — join a list with a separator |
| `quote` / `squote` | Wrap in double/single quotes |

See the [full Sprig function reference](https://masterminds.github.io/sprig/) for the complete list.

### Runtime reload and rollback

The VCL template file is watched for changes. When a change is detected, k8s-httpcache re-renders the template and reloads Varnish. If the new template fails to compile, the previous working template is restored automatically so that endpoint updates continue to work.

### Reference VCL template

The following template from [`.github/test/manifest.yaml`](.github/test/manifest.yaml) demonstrates shard-based routing, multiple backend groups, and PURGE handling. Note that drain VCL is **not** included here — when `--drain` is enabled, k8s-httpcache automatically injects the necessary VCL (see [Graceful shutdown](#graceful-shutdown--zero-downtime-deploys)).

```vcl
vcl 4.1;

import directors;
import std;

<<- range .Frontends >>
backend << .Name >> {
  .host = "<< .IP >>";
  .port = "<< .Port >>";
}
<<- end >>

<<- range $name, $eps := .Backends >>
<<- range $eps >>
backend << .Name >>_<< $name >> {
  .host = "<< .IP >>";
  .port = "<< .Port >>";
}
<<- end >>
<<- end >>

acl purge {
  "localhost";
  "127.0.0.1";
  "::1";
}

sub vcl_init {
  <<- if .Frontends >>
  new cluster = directors.shard();
  <<- range .Frontends >>
  cluster.add_backend(<< .Name >>);
  <<- end >>
  cluster.reconfigure();
  <<- end >>

  <<- range $name, $eps := .Backends >>
  new backend_<< $name >> = directors.round_robin();
  <<- range $eps >>
  backend_<< $name >>.add_backend(<< .Name >>_<< $name >>);
  <<- end >>
  <<- end >>
}

sub vcl_recv {
  if (req.method == "PURGE") {
    if (!client.ip ~ purge) {
      return (synth(405, "Not allowed"));
    }
    return (purge);
  }

  <<- if .Frontends >>
  if (!req.http.X-Shard-Routed) {
    set req.backend_hint = cluster.backend(by=URL);
    set req.http.x-shard = req.backend_hint;
    if (req.http.x-shard != server.identity) {
      set req.http.X-Shard-Routed = "true";
      return(pass);
    }
  }
  <<- end >>

  <<- range $name, $_ := .Backends >>
  if (req.url ~ "^/<< $name >>/") {
    set req.backend_hint = backend_<< $name >>.backend();
    set req.url = regsub(req.url, "^/<< $name >>/", "/");
  }
  <<- end >>
}

sub vcl_purge {
  return (synth(200, "Purged"));
}
```

## Broadcast server

The broadcast server fans out incoming HTTP requests (e.g. PURGE) to all Varnish frontend pods and returns an aggregated JSON response.

Default listen address: `:8088`. Disable with `--broadcast-addr=""`.

### Usage

Send a request to the broadcast server and it will forward it to every Varnish pod:

```bash
curl -X PURGE http://k8s-httpcache:8088/path/to/purge
```

### Response format

The response is a JSON object mapping pod names to their individual responses:

```json
{
  "pod-name-1": {
    "status": 200,
    "body": "Purged"
  },
  "pod-name-2": {
    "status": 200,
    "body": "Purged"
  }
}
```

If no frontends are available, the server returns HTTP 503 with:

```json
{
  "error": "no frontends available"
}
```

## Graceful shutdown / zero-downtime deploys

k8s-httpcache has built-in support for graceful connection draining. Enable it with `--drain`:

```
k8s-httpcache --drain --drain-delay=15s ...
```

### How it works

When `--drain` is enabled, k8s-httpcache automatically injects drain VCL into the rendered template output. The injected VCL adds a dummy backend that serves as a health flag, and a `sub vcl_deliver` that checks the backend's health status and sets `Connection: close` on all responses when draining is active, signalling clients to close their connections.

On SIGTERM (e.g. during a rolling update), the shutdown sequence is:

1. **Start draining** — the injected VCL backend is marked sick via `varnishadm`, causing Varnish to send `Connection: close` on all responses.
2. **Wait for endpoint removal** (`--drain-delay`, default `15s`) — sleep to allow Kubernetes to remove the pod from Service endpoints and load balancers. A second signal skips this wait.
3. **Shutdown** — SIGTERM is forwarded to varnishd.

### Waiting for connections to close

By default (`--drain-timeout=0`), session polling is skipped and shutdown proceeds immediately after the drain delay. This is the safe default because some clients hold long-lived connections (e.g. WebSockets, streaming) that may never close on their own.

To wait for all connections to close before shutting down, set `--drain-timeout` to a positive duration:

```
k8s-httpcache --drain --drain-delay=15s --drain-timeout=30s ...
```

This adds a step between 2 and 3: poll `varnishstat` for active sessions every second until all connections are closed or the timeout is reached. A second signal aborts this wait.

**Important:** When using `--drain-timeout`, ensure that your clients' keep-alive/idle timeout is lower than the configured drain timeout. This ensures clients close the connection (in response to `Connection: close`) before the server shuts down, avoiding a race condition where a client sends a new request just as the server is closing the connection on its side.

### Injected VCL details

The drain VCL is injected transparently around your template output:

- A `sub vcl_deliver` is **prepended** (before your `vcl_deliver`) to ensure the `Connection: close` header is always set during draining, even if your `vcl_deliver` returns early.
- A drain backend declaration is **appended** (after all your backends) so it never becomes Varnish's default backend.
- `import std;` is added automatically if not already present in your template.

### Pod spec requirements

Set `terminationGracePeriodSeconds` to accommodate the drain delay + drain timeout + shutdown timeout (e.g. `90` seconds):

```yaml
terminationGracePeriodSeconds: 90
```

## RBAC

### Minimum permissions

k8s-httpcache needs `list` and `watch` on `services` and `endpointslices` in its own namespace. If using `--values`, it also needs `list` and `watch` on `configmaps`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: k8s-httpcache
  namespace: default
rules:
- apiGroups: [""]
  resources: ["services"]
  verbs: ["list", "watch"]
- apiGroups: ["discovery.k8s.io"]
  resources: ["endpointslices"]
  verbs: ["list", "watch"]
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["list", "watch"]
```

### Cross-namespace

To watch services or ConfigMaps in other namespaces (for cross-namespace backends or values), create a Role and RoleBinding in each target namespace. The RoleBinding must reference the ServiceAccount from the k8s-httpcache namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: k8s-httpcache
  namespace: other-namespace
rules:
- apiGroups: [""]
  resources: ["services"]
  verbs: ["list", "watch"]
- apiGroups: ["discovery.k8s.io"]
  resources: ["endpointslices"]
  verbs: ["list", "watch"]
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: k8s-httpcache
  namespace: other-namespace
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: k8s-httpcache
subjects:
- kind: ServiceAccount
  name: k8s-httpcache
  namespace: default  # <-- namespace where k8s-httpcache runs
```

## Security context

The recommended security context runs as the `varnish` user (UID/GID 1000 in the official Varnish Alpine image), non-root, with a read-only root filesystem and all capabilities dropped:

```yaml
securityContext:
  allowPrivilegeEscalation: false
  privileged: false
  runAsUser: 1000
  runAsGroup: 1000
  runAsNonRoot: true
  readOnlyRootFilesystem: true
  capabilities:
    drop:
    - ALL
```

Writable directories (`/tmp` and `/var/lib/varnish`) are provided as `emptyDir` volumes backed by memory:

```yaml
volumes:
- name: tmp
  emptyDir:
    sizeLimit: 256Mi
    medium: Memory
- name: varlibvarnish
  emptyDir:
    sizeLimit: 512Mi
    medium: Memory
```
