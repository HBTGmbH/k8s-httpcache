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
- Supports multiple backend groups
- Supports multiple listen addresses with the full Varnish `-a` syntax, including PROXY protocol
  - https://github.com/mittwald/kube-httpcache/issues/206
- Uses "<< ... >>" as Template delimiter to not clash with Helm templating

## Container image

The published image at `ghcr.io/hbtgmbh/k8s-httpcache` is a distroless (`FROM scratch`) binary-only image (linux/amd64, linux/arm64). It contains only the statically linked k8s-httpcache binary and no Varnish installation. The binary works with any Varnish distribution image (Alpine, Debian, Ubuntu, etc.).

Build your own image by copying the binary into a Varnish base image of your choice:

```dockerfile
FROM varnish:8.0.0-alpine
COPY --from=ghcr.io/hbtgmbh/k8s-httpcache:<version> /usr/local/bin/k8s-httpcache /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
```

## Quick start

[`deploy/kube-manifest.yaml`](deploy/kube-manifest.yaml) contains a complete working example. It creates the following resources:

- **ServiceAccount** — identity for the k8s-httpcache pod
- **Role** — permissions to list/watch services and endpointslices
- **RoleBinding** — binds the Role to the ServiceAccount
- **Deployment** — runs k8s-httpcache with Varnish (3 replicas, preStop hook for graceful shutdown)
- **Service** — exposes HTTP (port 80) and the broadcast server (port 8088)
- **ConfigMap** — holds the VCL template

Apply it with:

```bash
kubectl apply -f deploy/kube-manifest.yaml
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

### Listen and backend flags

| Flag | Default | Description |
|------|---------|-------------|
| `--listen-addr` | `http=:8080,HTTP` | Varnish listen address (repeatable). See [Listen address specification](#listen-address-specification). |
| `--backend` | | Backend service (repeatable). See [Backend specification](#backend-specification). |

### Varnish paths

| Flag | Default | Description |
|------|---------|-------------|
| `--varnishd-path` | `varnishd` | Path to varnishd binary |
| `--varnishadm-path` | `varnishadm` | Path to varnishadm binary |
| `--admin-addr` | `127.0.0.1:6082` | Varnish admin listen address |
| `--secret-path` | *(auto-generated)* | Path to write the varnishadm secret file |

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

### Timing and logging flags

| Flag | Default | Description |
|------|---------|-------------|
| `--debounce` | `2s` | Debounce duration for endpoint changes |
| `--shutdown-timeout` | `30s` | Time to wait for varnishd to exit before sending SIGKILL |
| `--log-level` | `INFO` | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR` |

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

## VCL template

The VCL template is a standard Go [`text/template`](https://pkg.go.dev/text/template) with custom delimiters `<<` and `>>` (instead of `{{ }}`) to avoid clashes with Helm templating.

### Data model

The template receives the following data:

| Field | Type | Description |
|-------|------|-------------|
| `.Frontends` | `[]Frontend` | Varnish peer pods from the watched service EndpointSlice |
| `.Backends` | `map[string][]Endpoint` | Named backend groups keyed by the `name` from `--backend` |

Each `Frontend` / `Endpoint` has:

| Field | Type | Description |
|-------|------|-------------|
| `.IP` | `string` | Pod IP address (or hostname for ExternalName) |
| `.Port` | `int32` | Resolved port number |
| `.Name` | `string` | Pod name |

### Template functions

| Function | Description |
|----------|-------------|
| `replace` | `strings.ReplaceAll` — e.g. `<< replace .Name "-" "_" >>` to sanitize pod names for VCL identifiers |

### Runtime reload and rollback

The VCL template file is watched for changes. When a change is detected, k8s-httpcache re-renders the template and reloads Varnish. If the new template fails to compile, the previous working template is restored automatically so that endpoint updates continue to work.

### Reference VCL template

The following template from [`deploy/kube-manifest.yaml`](deploy/kube-manifest.yaml) demonstrates shard-based routing, multiple backend groups, PURGE handling, and graceful draining:

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

# Dummy backend to use as a drain flag. The preStop hook will set this backend to sick to trigger draining
# via varnishadm.
backend drain_flag {
  .host = "127.0.0.1"; # <- any IP ist fine
  .port = "9"; # <- any port ist fine
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

sub handle_readiness {
  if (req.url == "/ready") {
    # Return 200 if not draining, else 503.
    if (std.healthy(drain_flag)) {
      return (synth(200));
    } else {
      return (synth(503));
    }
  }
}

sub vcl_recv {
  call handle_readiness;

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

sub handle_draining {
  # During draining, which is activated via varnishadm setting the backend health to sick,
  # we respond with 'Connection: close' to inform clients not to reuse connections.
  # This will eventually lead to all connections being closed and no new requests being accepted.
  if (!std.healthy(drain_flag)) {
    set resp.http.Connection = "close";
  }
}

sub vcl_purge {
  return (synth(200, "Purged"));
}

sub vcl_deliver {
  # Check whether we are draining and adjust the Connection header in client responses accordingly.
  call handle_draining;
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

k8s-httpcache supports zero-downtime deploys via a preStop lifecycle hook. The sequence is:

1. **Mark as draining** — `varnishadm backend.set_health drain_flag sick` marks the pod as unhealthy. The readiness probe starts returning 503, and `Connection: close` is sent on all responses.
2. **Wait for traffic to stop** — sleep to allow the Service endpoint to be removed from load balancers.
3. **Wait for connections to drain** — poll `varnishstat` for active sessions until all connections are closed or a deadline is reached.
4. **Shutdown** — Varnish exits.

The reference preStop hook from [`deploy/kube-manifest.yaml`](deploy/kube-manifest.yaml):

```yaml
lifecycle:
  preStop:
    exec:
      command:
      - sh
      - -c
      - |
        varnishadm 2>/proc/1/fd/2 -T 127.0.0.1:6082 -S /var/lib/varnish/secrets/secret backend.set_health drain_flag sick
        echo > /proc/1/fd/1 "preStop: Waiting 15s for new connections to stop coming in..."
        sleep 15
        if [ "30" -gt 0 ]; then
          deadline=$(( $(date +%s) + 30 ))
        fi
        echo > /proc/1/fd/1 "preStop: Waiting at most 30s for all connections to be drained..."
        while :; do
          val=$(varnishstat 2>/proc/1/fd/2 -1 \
                | awk '/MEMPOOL\.sess[0-9]+\.live/ {a+=$2} END {print a+0}')
          if [ "$val" -eq 0 ]; then
            echo > /proc/1/fd/1 "preStop: All connections are gone. Telling Varnish to shut down now."
            break
          elif [ "30" -gt 0 ] && [ "$(date +%s)" -ge "$deadline" ]; then
            echo > /proc/1/fd/1 "preStop: Deadline reached while there are still connections. Telling Varnish to shut down now anyway."
            break
          fi
          echo > /proc/1/fd/1 "preStop: There are still $val client connections. Continue waiting..."
          sleep 1
        done
```

Set `terminationGracePeriodSeconds` on the pod spec to accommodate the sleep + drain time (e.g. `90` seconds).

## RBAC

### Minimum permissions

k8s-httpcache needs `list` and `watch` on `services` and `endpointslices` in its own namespace:

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
```

### Cross-namespace

To watch services in other namespaces (for cross-namespace backends), create a Role and RoleBinding in each target namespace. The RoleBinding must reference the ServiceAccount from the k8s-httpcache namespace:

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
