# k8s-httpcache

[![Test and Build](https://github.com/HBTGmbH/k8s-httpcache/actions/workflows/test-and-build.yml/badge.svg)](https://github.com/HBTGmbH/k8s-httpcache/actions/workflows/test-and-build.yml)
[![CodeQL](https://github.com/HBTGmbH/k8s-httpcache/actions/workflows/codeql.yml/badge.svg)](https://github.com/HBTGmbH/k8s-httpcache/actions/workflows/codeql.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/HBTGmbH/k8s-httpcache)](https://goreportcard.com/report/github.com/HBTGmbH/k8s-httpcache)
[![License](https://img.shields.io/github/license/HBTGmbH/k8s-httpcache)](https://github.com/HBTGmbH/k8s-httpcache/blob/main/LICENSE)

A replacement for [kube-httpcache](https://github.com/mittwald/kube-httpcache) with many more additional features (see list below).

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
- Automatic retry of transient `vcl.load` failures (e.g. during Varnish child process restarts), configurable via `--vcl-reload-retries` and `--vcl-reload-retry-interval`
  - https://github.com/mittwald/kube-httpcache/issues/138
- Supports multiple backend groups
  - https://github.com/mittwald/kube-httpcache/issues/133
- Supports multiple listen addresses with the full Varnish `-a` syntax, including PROXY protocol
  - https://github.com/mittwald/kube-httpcache/issues/206
- Uses `<< ... >>` as default template delimiters to not clash with Helm templating (configurable via `--template-delims`)
- Graceful connection draining on shutdown with active session polling via varnishstat
- [Broadcast server](#broadcast-server) that fans out requests (e.g. PURGE) to all Varnish frontend pods
- Prometheus metrics for VCL reloads, endpoint counts, broadcast stats, and more
- [ExternalName service](#externalname-services) support for hostname-based backends
  - https://github.com/mittwald/kube-httpcache/issues/39
- Template values from Kubernetes ConfigMaps ([`--values`](#values-specification)), Secrets ([`--secrets`](#secrets-specification)), or mounted directories ([`--values-dir`](#values-from-directories)), dynamically reloaded on changes
- Secret values are automatically [redacted](#security-considerations) from varnishd process output, varnishadm responses, and error logs
- Cross-namespace backends and values via `namespace/service` syntax
- All [Sprig](http://masterminds.github.io/sprig/) template functions available in VCL templates (including [`env`](http://masterminds.github.io/sprig/os.html) for environment variables)
  - https://github.com/mittwald/kube-httpcache/issues/53
- Automatic Varnish version detection with support for Varnish 6, 7, 8, and trunk builds
- Structured logging with configurable format (text/json) and log level
- [Kubernetes Events](#kubernetes-events) for VCL reloads, template changes, rollbacks, drain lifecycle, and varnishd crashes — visible via `kubectl describe pod` and `kubectl get events`
- [Discarding old VCL objects](#vcl-retention) after reload and keep the most recent `N` object (`--vcl-kept=N`), to avoid unbounded Varnish memory usage with every successive VCL reload
- [Endpoint change debouncing](#tuning-debounce) to avoid rapid VCL reload cycles, with independent timers for frontend and backend changes (`--frontend-debounce`, `--backend-debounce`)
  - https://github.com/mittwald/kube-httpcache/issues/66
- JSON [status endpoint](#status-endpoint) (`/status`) on the metrics server providing runtime state (version, uptime, endpoint counts, reload metrics, varnishd process status)
- [Topology-aware routing](#topology-aware-routing): endpoint zone, node name, and routing hints are exposed in templates, allowing weighted directors that prefer same-zone backends
- [Dynamic backend discovery](#dynamic-backend-discovery) via label selectors (`--backend-selector`), automatically adding and removing backends as matching Services appear or disappear
- Optional [access logging](#access-logging) via a managed `varnishncsa` subprocess with auto-restart, configurable log format, VSL query filtering, and stdout line prefixing

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
- **ClusterRole** + **ClusterRoleBinding** — permissions to read node objects for [topology zone detection](#topology-aware-routing) (not needed when using `--zone`)
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
| `--service-name`, `-s` | Kubernetes Service to watch for frontends: `[namespace/]service` |
| `--namespace`, `-n` | Kubernetes namespace (also used as default for services without a `namespace/` prefix) |
| `--vcl-template`, `-t` | Path to VCL Go template file |

### Listen, backend, and values flags

| Flag | Default | Description |
|------|---------|-------------|
| `--listen-addr`, `-l` | `http=:8080,HTTP` | Varnish listen address (repeatable). See [Listen address specification](#listen-address-specification). |
| `--backend`, `-b` | | Backend service (repeatable). See [Backend specification](#backend-specification). |
| `--values` | | ConfigMap to watch for template values (repeatable). See [Values specification](#values-specification). |
| `--secrets` | | Secret to watch for template values (repeatable). See [Secrets specification](#secrets-specification). |
| `--values-dir` | | Directory to poll for YAML template values (repeatable). See [Values from directories](#values-from-directories). |
| `--values-dir-poll-interval` | `5s` | Poll interval for `--values-dir` directories (only effective when `--file-watch` is enabled) |
| `--exclude-annotations` | | Annotation keys or prefixes to exclude from backend annotations (repeatable; trailing `*` for prefix match, e.g. `kubectl.kubernetes.io/*`). `kubectl.kubernetes.io/last-applied-configuration` is always excluded by default. |

### Varnish paths

| Flag | Default | Description |
|------|---------|-------------|
| `--varnishd-path` | `varnishd` | Path to varnishd binary |
| `--varnishadm-path` | `varnishadm` | Path to varnishadm binary |
| `--varnishstat-path` | `varnishstat` | Path to varnishstat binary |
| `--admin-timeout` | `30s` | Max time to wait for the varnish admin CLI to become ready |

### Broadcast flags

| Flag | Default | Description |
|------|---------|-------------|
| `--broadcast-addr` | `:8088` | Broadcast server listen address (`none` to disable) |
| `--broadcast-target-listen-addr` | *(first `--listen-addr`)* | Name of the `--listen-addr` to target for fan-out (only effective when broadcast is enabled) |
| `--broadcast-drain-timeout` | `30s` | Time to wait for broadcast connections to drain before shutting down (only effective when broadcast is enabled) |
| `--broadcast-shutdown-timeout` | `5s` | Time to wait for in-flight broadcast requests to finish after draining (only effective when broadcast is enabled) |
| `--broadcast-server-idle-timeout` | `120s` | Max idle time for client keep-alive connections to the broadcast server (only effective when broadcast is enabled) |
| `--broadcast-read-header-timeout` | `10s` | Max time to read request headers on the broadcast server (only effective when broadcast is enabled) |
| `--broadcast-client-idle-timeout` | `4s` | Max idle time for connections to Varnish pods in the broadcast client pool (only effective when broadcast is enabled) |
| `--broadcast-client-timeout` | `3s` | Timeout for each fan-out request to a Varnish pod (only effective when broadcast is enabled) |

### Metrics flags

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-addr` | `:9101` | Listen address for Prometheus metrics (`none` to disable) |
| `--metrics-read-header-timeout` | `10s` | Max time to read request headers on the metrics server |
| `--varnishstat-export` | `false` | Enable varnishstat Prometheus exporter on `/metrics` |
| `--varnishstat-export-filter` | *(all)* | Counter groups to export (e.g. `MAIN,SMA,VBE`); empty exports all |

The metrics endpoint exposes the standard Go runtime and process metrics (`go_*`, `process_*`) plus the following application metrics, all prefixed with `k8s_httpcache_`:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vcl_reloads_total` | Counter | `result` | VCL reload attempts (`success` or `error`) |
| `vcl_reload_retries_total` | Counter | | VCL reload retry attempts |
| `vcl_render_errors_total` | Counter | | VCL template render failures |
| `vcl_template_changes_total` | Counter | | VCL template file changes detected on disk |
| `vcl_template_parse_errors_total` | Counter | | VCL template parse failures |
| `vcl_rollbacks_total` | Counter | | Template rollbacks to previous known-good version |
| `endpoint_updates_total` | Counter | `role`, `service` | EndpointSlice updates received (`frontend` or `backend`) |
| `values_updates_total` | Counter | `configmap` | ConfigMap value updates received |
| `secrets_updates_total` | Counter | `secret` | Secret value updates received |
| `endpoints` | Gauge | `role`, `service` | Current ready endpoint count per service |
| `varnishd_up` | Gauge | | Whether the varnishd process is running (1/0) |
| `broadcast_requests_total` | Counter | `method`, `status` | Broadcast HTTP requests |
| `broadcast_fanout_targets` | Gauge | | Number of frontend pods targeted by the last broadcast |
| `build_info` | Gauge | `version`, `goversion` | Build metadata (always 1) |
| `debounce_events_total` | Counter | `group` | Events received per debounce group |
| `debounce_fires_total` | Counter | `group` | Debounce timer fires per group |
| `debounce_max_enforcements_total` | Counter | `group` | Reloads forced by the debounce-max deadline |
| `debounce_latency_seconds` | Histogram | `group` | Wall-clock time from first event in a debounce burst to the reload |
| `vcl_render_duration_seconds` | Histogram | | Time to render the VCL template to a temporary file |
| `vcl_reload_duration_seconds` | Histogram | | Time for varnishd VCL reload (`vcl.load` + `vcl.use`), including retries |
| `broadcast_duration_seconds` | Histogram | | Total wall-clock time for broadcast fan-out to all frontend pods |

The `group` label is either `frontend` (`--service-name` endpoint changes) or `backend` (`--backend`, `--values`, `--secrets`, `--values-dir`, and VCL template changes).

#### Varnishstat exporter

When `--varnishstat-export` is enabled, native Varnish counters from `varnishstat` are exported as `varnish_*` Prometheus metrics on the same `/metrics` endpoint. Use `--varnishstat-export-filter` to limit which counter groups are exported.

The exporter always registers these meta-metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `varnish_up` | Gauge | Whether the last varnishstat scrape succeeded (1/0) |
| `varnish_scrape_duration_seconds` | Gauge | Duration of the varnishstat scrape |
| `varnish_exporter_total_scrapes` | Counter | Cumulative varnishstat scrape count |
| `varnish_exporter_json_parse_failures_total` | Counter | JSON parse failures during scrapes |

Each varnishstat counter group maps to a metric prefix:

| Group | Metric prefix | Labels | Notes |
|-------|--------------|--------|-------|
| `MAIN` | `varnish_main_*` | | Core cache, session, and worker stats |
| `VBE` | `varnish_backend_*` | `backend`, `server` | Only the newest VCL reload is exported |
| `SMA` | `varnish_sma_*` | `type` | Storage memory allocator |
| `SMF` | `varnish_smf_*` | `type` | Storage memory file |
| `LCK` | `varnish_lock_*` | `target` | Renamed from `lck_*` for readability |
| `MEMPOOL` | `varnish_mempool_*` | `id` | Per-pool memory statistics |
| `MGT` | `varnish_mgt_*` | | Management process stats |

Special behaviors:

- `varnish_backend_up` (0/1 gauge) is derived from the `happy` health-probe bitmap
- Zero-value counters are omitted to reduce scrape size
- When multiple VCL reloads exist, only backends from the newest reload are exported
- `MAIN.fetch_*` → `varnish_main_fetch{type=...}` with a `_total` rollup; same pattern for sessions and worker threads
- `LCK` counters are renamed: `colls` → `lock_collisions`, `creat` → `lock_created`, `destroy` → `lock_destroyed`, `locks` → `lock_operations`

#### Status endpoint

The metrics server also serves a `/status` JSON endpoint that returns a snapshot of the current runtime state. This is useful for quick operator inspection without querying Prometheus:

```bash
curl -s http://localhost:9101/status | jq .
```

```json
{
  "version": "v0.1.0",
  "goVersion": "go1.26.0",
  "varnishMajorVersion": 8,
  "serviceName": "k8s-httpcache",
  "serviceNamespace": "default",
  "drainEnabled": true,
  "broadcastEnabled": true,
  "startedAt": "2025-01-15T10:30:00Z",
  "uptimeSeconds": 3600.5,
  "frontendCount": 3,
  "backendCounts": {
    "nginx": 2,
    "api": 4
  },
  "valuesCount": 1,
  "lastReloadAt": "2025-01-15T11:29:55Z",
  "reloadCount": 42,
  "varnishdUp": true
}
```

`lastReloadAt` is `null` before the first VCL reload. The endpoint only accepts `GET` requests.

#### Health endpoints

The metrics server exposes `/healthz` (liveness) and `/readyz` (readiness) endpoints that report whether varnishd is running. `/healthz` checks that the varnishd master process is alive. `/readyz` additionally verifies that the Varnish listen port is accepting TCP connections, which detects the window where the master is alive but the child worker has crashed and is restarting. Both return `200 ok` when healthy and `503` when not:

```bash
curl http://localhost:9101/healthz   # ok
curl http://localhost:9101/readyz    # ok
```

Use these in your pod spec to let Kubernetes detect and restart a stuck varnishd process (`livenessProbe`) or stop routing traffic before varnishd is ready (`readinessProbe`):

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 9101
  periodSeconds: 10
  failureThreshold: 3
readinessProbe:
  httpGet:
    path: /readyz
    port: 9101
  periodSeconds: 5
  failureThreshold: 1
```

Note that `/readyz` cannot verify whether the loaded VCL is correct and Varnish would actually serve traffic as expected. If you need that level of confidence, implement custom readiness logic directly in VCL (e.g. handle a health-check path in `vcl_recv`) and point your `readinessProbe` at Varnish itself.

Both endpoints are available whenever the metrics server is enabled (the default). They are disabled when `--metrics-addr=none`.

### Access logging flags

| Flag | Default | Description |
|------|---------|-------------|
| `--varnishncsa-enabled` | `false` | Enable managed `varnishncsa` access logging subprocess (see [Access logging](#access-logging)) |
| `--varnishncsa-path` | `varnishncsa` | Path to `varnishncsa` binary |
| `--varnishncsa-format` | *(Combined)* | Custom log format string (passed as `-F`); see `varnishncsa(1)` |
| `--varnishncsa-query` | *(all)* | VSL query expression (passed as `-q`) to filter which requests are logged |
| `--varnishncsa-backend` | `false` | Log backend (fetch) requests instead of client requests (passes `-b`) |
| `--varnishncsa-output` | *(stdout)* | Output file path (passed as `-w`); default writes to stdout |
| `--varnishncsa-prefix` | `[access]` + space | Prefix prepended to each log line on stdout (ignored when `--varnishncsa-output` is set) |

### Kubernetes Events

When the `POD_NAME` environment variable is set, k8s-httpcache emits Kubernetes Events visible via `kubectl describe pod` and `kubectl get events`. Set `POD_NAME` using the Downward API:

```yaml
env:
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
```

The following events are emitted:

| Reason | Type | Description |
|--------|------|-------------|
| `VCLReloaded` | Normal | VCL was loaded/reloaded successfully |
| `VCLTemplateChanged` | Normal | VCL template file change detected on disk |
| `VCLTemplateParseFailed` | Warning | VCL template failed to parse |
| `VCLRenderFailed` | Warning | VCL template render failed |
| `VCLReloadFailed` | Warning | varnishd rejected the new VCL |
| `VCLRolledBack` | Warning | Template rolled back to previous known-good version |
| `VarnishdExited` | Warning | varnishd process exited unexpectedly |
| `DrainStarted` | Normal | Graceful drain started after receiving a termination signal |
| `DrainCompleted` | Normal | All active connections have drained |
| `DrainTimeout` | Warning | Drain timeout reached, proceeding with forced shutdown |
| `VarnishncsaStartFailed` | Warning | `varnishncsa` failed to start (will retry) |
| `VarnishncsaExited` | Warning | `varnishncsa` exited unexpectedly (will restart) |
| `VarnishncsaRestarted` | Normal | `varnishncsa` restarted after unexpected exit |
| `VarnishncsaCrashLoop` | Warning | `varnishncsa` exceeded maximum consecutive crashes, giving up |

Events require RBAC permission to `create` and `patch` the `events` resource (see [RBAC](#rbac)). If the permission is missing, a single warning is logged and the service continues without event recording.

### Drain flags

| Flag | Default | Description |
|------|---------|-------------|
| `--drain` | `false` | Enable graceful connection draining on shutdown (see [Graceful shutdown](#graceful-shutdown--zero-downtime-deploys)) |
| `--drain-delay` | `15s` | Delay after marking backend sick before polling for active sessions (only effective when `--drain` is enabled) |
| `--drain-poll-interval` | `1s` | Poll interval for active sessions during graceful drain (only effective when `--drain` is enabled) |
| `--drain-timeout` | `0` | Max time to wait for active sessions to reach 0 (only effective when `--drain` is enabled). Default `0` skips session polling. Set to a positive duration (e.g. `30s`) to poll and wait for connections to close. |

### Template flags

| Flag | Default | Description |
|------|---------|-------------|
| `--template-delims` | `<< >>` | Template delimiters as a space-separated pair (e.g. `"<< >>"` or `"{{ }}"`) |
| `--zone` | | Topology zone of this Varnish pod (overrides auto-detection from `NODE_NAME`). See [Topology-aware routing](#topology-aware-routing). |

### Timing and logging flags

| Flag | Default | Description |
|------|---------|-------------|
| `--debounce` | `2s` | Debounce duration for endpoint changes |
| `--debounce-max` | `0` | Maximum debounce duration before a reload is forced (`0` disables; only effective when events arrive within the `--debounce` window) |
| `--frontend-debounce` | *(uses `--debounce`)* | Debounce duration for frontend (`--service-name`) changes; overrides `--debounce` for the frontend group |
| `--frontend-debounce-max` | *(uses `--debounce-max`)* | Maximum debounce duration for frontend changes; overrides `--debounce-max` for the frontend group |
| `--backend-debounce` | *(uses `--debounce`)* | Debounce duration for backend (`--backend`, `--values`, `--values-dir`, template) changes; overrides `--debounce` for the backend group |
| `--backend-debounce-max` | *(uses `--debounce-max`)* | Maximum debounce duration for backend changes; overrides `--debounce-max` for the backend group |
| `--shutdown-timeout` | `30s` | Time to wait for varnishd to exit before sending SIGKILL |
| `--vcl-template-watch-interval` | `5s` | Poll interval for VCL template file changes (only effective when `--file-watch` is enabled) |
| `--file-watch` | `true` | Watch VCL template and `--values-dir` paths for changes (disable with `--file-watch=false`) |
| `--vcl-reload-retries` | `3` | Max retry attempts for `vcl.load` failures (`0` disables retries) |
| `--vcl-reload-retry-interval` | `2s` | Wait between `vcl.load` retry attempts |
| `--vcl-kept` | `0` | Number of old VCL objects to retain after reload (`0` discards all) |
| `--debounce-latency-buckets` | `0.01,0.05,0.1,0.25,0.5,1,2.5,5,10` | Comma-separated histogram bucket boundaries (seconds) for `debounce_latency_seconds` |
| `--log-level` | `INFO` | Log level: `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `--log-format` | `text` | Log format: `text`, `json` |

### Tuning debounce

In a typical Kubernetes deployment, endpoint changes arrive in bursts — for example when a `Deployment` scales up/down or a rolling update replaces pods. The `--debounce` and `--debounce-max` flags control how these bursts are batched into VCL reloads.

**Why two settings?** `--debounce` alone handles short bursts well: it waits for a quiet period before reloading, so a flurry of near-simultaneous changes produces a single reload. But during a sustained stream of events — e.g. a rolling update replacing pods one at a time every few seconds — there is never a quiet period long enough for the debounce timer to fire. `--debounce-max` solves this by putting an upper bound on how long the system will wait. Together, the two settings give you the best of both worlds: short bursts are coalesced into one reload (thanks to `--debounce`), while prolonged activity still triggers periodic reloads (thanks to `--debounce-max`). Setting both to the same value would effectively disable the coalescing benefit — every burst would trigger a reload after exactly that duration, even if waiting just a little longer would have captured all changes in a single reload.

**Recommended starting point:**

```
--debounce=2s --debounce-max=10s
```

- `--debounce=2s` (the default) coalesces rapid-fire endpoint updates into a single reload. Most rolling updates emit several changes within a few seconds, so 2s is a good balance between responsiveness and avoiding unnecessary reloads.
- `--debounce-max=10s` caps the total wait. Without it, a long rolling update (e.g. replacing 100 pods one by one) can keep resetting the debounce timer indefinitely, leaving Varnish with a stale backend list for the entire rollout. With `--debounce-max=10s`, a reload is forced at least every 10 seconds during sustained activity.

**When to adjust:**

| Situation | Suggestion                                                                                     |
|-----------|------------------------------------------------------------------------------------------------|
| Small clusters with few pods | Lower both values (e.g. `--debounce=1s --debounce-max=5s`) for faster convergence              |
| Large clusters or slow rollouts (100+ pods) | Raise `--debounce-max` (e.g. `5s`–`10s`) to batch more changes per reload and reduce VCL churn |
| Near-instant convergence required | `--debounce=500ms --debounce-max=2s`, at the cost of more frequent reloads                     |

**Caution:** Setting either value too high can cause Varnish to keep routing requests to pods that have already been terminated or are no longer accepting traffic. During a rolling update, Kubernetes removes old pods while adding new ones — if the VCL reload is delayed too long, Varnish's backend list becomes stale and requests to removed pods will fail with backend connection errors. Ideally, keep `--debounce-max` shorter than any `preStop` sleep or graceful shutdown timeout configured on your backend pods — this ensures Varnish updates its backend list before the old pods actually stop accepting connections.

**Per-source overrides:** By default, all event sources share the same debounce settings. If your frontend and backend update patterns differ — e.g. frontends are stable while backends scale frequently — or the preStop / graceful shutdown timeouts are different between the frontend pods (k8s-httpcache/Varnish) and the backends, you can set independent timers with `--frontend-debounce` / `--frontend-debounce-max` and `--backend-debounce` / `--backend-debounce-max`. The frontend group covers `--service-name` endpoint changes; the backend group covers `--backend`, `--values`, `--values-dir`, and VCL template changes. When either timer fires, VCL reloads atomically with all latest state, clearing both timers. When not set, these flags inherit from `--debounce` / `--debounce-max`.

```
--debounce=2s --debounce-max=10s \
--backend-debounce=500ms --backend-debounce-max=2s
```

This gives the backend group a faster 500ms debounce while the frontend group keeps the global 2s/10s settings. The frontend debounce settings must then be synchronized with the preStop / graceful shutdown durations of the k8s-httpcache/Varnish pods, and the backend debounce settings need to fit the backends' preStop / graceful timeouts.

### VCL retention

Every time Varnish reloads, a new VCL object is created (e.g. `kv_reload_1`, `kv_reload_2`, …). The previously active VCL moves to the "available" state. Over time these old objects accumulate and consume memory inside Varnish.

The `--vcl-kept` flag controls how many old VCL objects are retained after each reload:

| Value | Behaviour |
|-------|-----------|
| `0` (default) | All old available VCL objects are discarded immediately after reload |
| `N` (e.g. `3`) | The `N` most recent available VCL objects are kept; older ones are discarded |

Cleanup runs in the background after every successful reload and is best-effort — a failed discard is logged as a warning but does not affect the reload itself.

**When to increase `--vcl-kept`:** Keeping one or more old VCL objects allows Varnish to finish processing in-flight requests that reference the previous VCL before it is discarded. In practice the default of `0` works well because Varnish internally holds a reference to any VCL that has active requests, preventing it from being freed until those requests complete. Increasing `--vcl-kept` can be useful for debugging (e.g. inspecting previous VCL versions via `varnishadm vcl.show`) or as extra safety margin in high-traffic environments.

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

## Dynamic backend discovery

The `--backend-selector` flag enables automatic backend discovery using Kubernetes label selectors. Instead of listing each backend explicitly with `--backend`, you specify a label selector and k8s-httpcache watches for matching Services, automatically adding and removing backends as Services appear or disappear.

Format: `[namespace//]selector[:port]`

| Part | Required | Description |
|------|----------|-------------|
| `namespace//` | no | Kubernetes namespace to watch. Use `*//` to watch all namespaces. Defaults to `--namespace` when omitted. |
| `selector` | yes | Kubernetes label selector (e.g. `app=myapp`, `tier=backend,env=prod`). Standard `key=value` and set-based syntax are supported. |
| `:port` | no | Port override applied to all discovered Services: numeric (e.g. `8080`) or named (e.g. `http`). When omitted, the first EndpointSlice port is used. |

### How discovery works

Discovered Services are merged into `.Backends` keyed by their Service name. For example, a Service named `api-v2` matching the selector appears in templates as `.Backends.api-v2`, exactly like an explicit `--backend=api-v2:api-v2` would.

Explicit `--backend` names take priority — if a discovered Service has the same name as an explicit backend, the discovered Service is skipped. This lets you pin specific backends while still discovering the rest dynamically.

Service labels and annotations are available on each `BackendGroup` via `.Labels` and `.Annotations`. This enables conditional VCL logic based on Service metadata:

```vcl
<< range $name, $bg := .Backends >>
  << if eq (index $bg.Labels "tier") "premium" >>
    # premium backend logic
  << end >>
<< end >>
```

Annotations work the same way:

```vcl
<< range $name, $bg := .Backends >>
  << if index $bg.Annotations "example.com/cache-ttl" >>
    # annotation-based routing logic
  << end >>
<< end >>
```

Both explicit `--backend` and discovered `--backend-selector` backends have their Service labels and annotations available on the `BackendGroup` (`.Labels` and `.Annotations`).

### Lifecycle

- **Startup**: All Services matching the selector at startup are discovered during the initial reconciliation and included in the first VCL render.
- **New Services**: When a new matching Service appears, a child EndpointSlice watcher is spawned and a VCL reload is triggered once its endpoints are known.
- **Removed Services**: When a matching Service is deleted or its labels no longer match the selector, its `BackendGroup` is removed from `.Backends` and a VCL reload is triggered.
- **Label/annotation changes**: When a Service's labels or annotations change (but still match the selector), the `BackendGroup`'s `.Labels` and `.Annotations` are updated and a VCL reload is triggered.

ExternalName services discovered via `--backend-selector` follow the same rules as explicit ExternalName backends: the `externalName` hostname is used directly, and a numeric `--backend-selector` port override is required (otherwise port 80 is used with a warning).

### Examples

```
# Discover backends in the default namespace matching app=myapp
--backend-selector=app=myapp

# Discover backends in a specific namespace with a port override
--backend-selector=staging//tier=backend:8080

# Discover backends across all namespaces
--backend-selector=*//app.kubernetes.io/part-of=my-system

# Multiple selectors can be combined
--backend-selector=app=api --backend-selector=app=worker
```

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

## Secrets specification

Format: `name:[namespace/]secret`

| Part | Required | Description |
|------|----------|-------------|
| `name` | yes | Template key, accessible as `.Secrets.<name>.<key>` |
| `namespace/` | no | Kubernetes namespace; defaults to `--namespace` if omitted |
| `secret` | yes | Kubernetes Secret name to watch |

Each Secret `data` value (which Kubernetes stores as `[]byte`) is converted to a string and YAML-parsed into its natural type, just like `--values`. The parsed data is exposed under `.Secrets.<name>`. When the Secret is updated, the VCL template is re-rendered and Varnish is reloaded (after debounce). If the Secret does not exist, an empty map is used.

### Examples

```
--secrets=auth:my-api-keys              # default namespace
--secrets=creds:staging/origin-tokens   # cross-namespace
```

Using secrets in a VCL template:

```vcl
sub vcl_backend_fetch {
  set bereq.http.Authorization = "Bearer << index .Secrets.auth "api-key" >>";
}
```

### Cross-namespace secrets

Use the `namespace/secret` syntax to reference a Secret in another namespace. This requires a Role and RoleBinding granting `list` and `watch` on `secrets` in the target namespace. See [RBAC](#rbac).

### Security considerations

Secret values are rendered directly into VCL and passed to Varnish, so it is important to understand where they can appear:

- **Rendered VCL on disk.** The rendered VCL is written to a temporary file that Varnish reads during `vcl.load`. The file is deleted immediately after use. In a Kubernetes deployment, the `/tmp` directory should be an `emptyDir` volume with `medium: Memory` (see [Security context](#security-context)) so that rendered VCL is never written to persistent storage.
- **Request and response headers.** If your VCL template sets a secret value as an HTTP header (e.g. `bereq.http.Authorization`), that header is visible to backend services and may appear in their access logs. Similarly, secrets placed on response headers (e.g. `resp.http.*`) are visible to clients and any intermediary proxies that log headers. Only set secrets on headers that are strictly necessary, and ensure that both upstream and downstream services treat those headers as sensitive.
- **Varnish process output and error messages.** When VCL compilation fails, `varnishd` and `varnishadm` may include VCL source lines — containing rendered secrets — in their error output. k8s-httpcache automatically redacts all known secret values (strings of 6 characters or longer) from process output, command responses, log messages, and Kubernetes Events. This protects against accidental exposure in logs, but the redactor only covers values it knows about — secrets must be loaded via `--secrets` to be redacted.
- **Varnish shared memory log (VSL).** Varnish logs request and response headers to its shared memory log, accessible via `varnishlog` / `varnishncsa`. If a secret is set on a header, it will appear in VSL. This is outside the control of k8s-httpcache. If you expose VSL tooling, be aware that header values may contain secrets.

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

The VCL template is a standard Go [`text/template`](https://pkg.go.dev/text/template) with custom delimiters `<<` and `>>` by default (instead of `{{ }}`) to avoid clashes with Helm templating. Use `--template-delims` to change the delimiters (e.g. `--template-delims="{{ }}"` for standard Go template syntax).

### Data model

The template receives the following data:

| Field | Type | Description |
|-------|------|-------------|
| `.Frontends` | `[]Frontend` | Varnish peer pods from the watched service EndpointSlice |
| `.Backends` | `map[string]BackendGroup` | Named backend groups keyed by the `name` from `--backend` or by Service name for `--backend-selector` discovered backends |
| `.Values` | `map[string]map[string]any` | Template values keyed by the `name` from `--values` or `--values-dir`. Each value is YAML-parsed, so structured data (maps, lists, numbers) is accessible. |
| `.Secrets` | `map[string]map[string]any` | Secret values keyed by the `name` from `--secrets`. Each value is YAML-parsed like `.Values`. |
| `.LocalZone` | `string` | Topology zone of the Varnish pod (empty if `NODE_NAME` is not set or zone detection fails). See [Topology-aware routing](#topology-aware-routing). |

Each `BackendGroup` has:

| Field | Type | Description |
|-------|------|-------------|
| `.Endpoints` | `[]Endpoint` | All ready endpoints for this backend |
| `.Labels` | `map[string]string` | Kubernetes Service labels. Updated dynamically when Service labels change. Useful for conditional VCL logic based on Service metadata. See [Dynamic backend discovery](#dynamic-backend-discovery). |
| `.Annotations` | `map[string]string` | Kubernetes Service annotations. Updated dynamically when Service annotations change. `kubectl.kubernetes.io/last-applied-configuration` is excluded by default; use `--exclude-annotations` to exclude additional keys. See [Dynamic backend discovery](#dynamic-backend-discovery). |
| `.LocalEndpoints` | `[]Endpoint` | Same-zone endpoints (where `.Zone == .LocalZone` or `.ForZones` contains `.LocalZone`). Empty when `LocalZone` is empty. See [Topology-aware routing](#topology-aware-routing). |
| `.RemoteEndpoints` | `[]Endpoint` | Other-zone endpoints (all remaining). Endpoints with unknown zone and no matching `.ForZones` hint are included here. Empty when `LocalZone` is empty. See [Topology-aware routing](#topology-aware-routing). |

Each `Frontend` / `Endpoint` has:

| Field | Type | Description |
|-------|------|-------------|
| `.IP` | `string` | Pod IP address (or hostname for ExternalName) |
| `.Port` | `int32` | Resolved port number |
| `.Name` | `string` | Pod name |
| `.Zone` | `string` | Topology zone from `topology.kubernetes.io/zone` (EndpointSlice) |
| `.NodeName` | `string` | Node hosting this endpoint |
| `.ForZones` | `[]string` | Zone names from `endpoint.hints.forZones` ([Topology Aware Routing](https://kubernetes.io/docs/concepts/services-networking/topology-aware-routing/)) |

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
| `keys` | `keys .Backends` — list of map keys |
| `hasKey` | `hasKey .Backends "api"` — check if a key exists |
| `get` | `get .Backends "api"` — get a value by key (`""` if missing) |
| `values` | `values .Backends` — list of map values |
| `pick` | `pick .Backends "api" "worker"` — subset of a map by key names |
| `omit` | `omit .Backends "internal"` — map without the named keys |

See the [full Sprig function reference](https://masterminds.github.io/sprig/) for the complete list.


### Runtime reload and rollback

The VCL template file is watched for changes. When a change is detected, k8s-httpcache re-renders the template and reloads Varnish. If the new template fails to compile, the previous working template is restored automatically so that endpoint updates continue to work.

### Reference VCL template

The following template (based on [`.github/test/manifest.yaml`](.github/test/manifest.yaml)) demonstrates shard-based routing, multiple backend groups, and PURGE handling. Note that drain VCL is **not** included here — when `--drain` is enabled, k8s-httpcache automatically injects the necessary VCL (see [Graceful shutdown](#graceful-shutdown--zero-downtime-deploys)).

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

<<- range $name, $bg := .Backends >>
<<- range $bg.Endpoints >>
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

  <<- range $name, $bg := .Backends >>
  new backend_<< $name >> = directors.round_robin();
  <<- range $bg.Endpoints >>
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
  }
  <<- end >>
}

sub vcl_purge {
  return (synth(200, "Purged"));
}
```

## Topology-aware routing

In multi-zone Kubernetes clusters, you may want Varnish to prefer backends in the same zone to reduce latency and cross-zone data transfer costs. k8s-httpcache exposes topology information from EndpointSlices so you can write zone-aware VCL templates.

### Setup

There are two ways to set the local topology zone. Choose one:

**Option A: Explicit `--zone` flag** (no extra RBAC needed)

If you deploy one instance per zone and already know the zone, pass it directly:

```yaml
args: ["--zone", "europe-west3-a", ...]
```

**Option B: Auto-detect from `NODE_NAME`** (requires node read access)

1. **Set the `NODE_NAME` environment variable** via the downward API so k8s-httpcache can detect the local zone:

   ```yaml
   env:
   - name: NODE_NAME
     valueFrom:
       fieldRef:
         fieldPath: spec.nodeName
   ```

2. **Grant node read access** — see [Node access for zone auto-detection](#node-access-for-zone-auto-detection) in the RBAC section.

When `--zone` is set, it takes precedence and `NODE_NAME` / node RBAC are not needed. If neither `--zone` nor `NODE_NAME` is set, the ClusterRole is missing, or the node has no `topology.kubernetes.io/zone` label, zone detection fails gracefully: `.LocalZone` will be empty, each `BackendGroup`'s `.LocalEndpoints` and `.RemoteEndpoints` will both be empty, and templates should fall back to `.Endpoints` (see the `if .LocalZone` guard in the [fallback director example](#example-fallback-director-preferring-same-zone-backends)).

   Zone-aware routing also requires that the **backend pods' nodes** have the `topology.kubernetes.io/zone` label. Kubernetes populates `.Zone` on each endpoint from the node hosting that pod. If the backend nodes lack zone labels, all endpoints will have an empty `.Zone` and land in `.RemoteEndpoints` even when `.LocalZone` is correctly detected — the local director will always be empty and the fallback director will only use the remote round-robin.

   [ExternalName service](#externalname-services) backends resolve to a DNS hostname rather than pod IPs, so their endpoints have no associated node. As a result, `.Zone`, `.NodeName`, and `.ForZones` will always be empty for ExternalName backends. They are included in `.Backends` as usual, but when zone splitting is active they will always land in `.RemoteEndpoints` (never in `.LocalEndpoints`).

### Available template fields

Top-level fields:

| Field | Description |
|-------|-------------|
| `.LocalZone` | Zone of the Varnish pod (from `--zone` flag, or auto-detected from the node's `topology.kubernetes.io/zone` label) |

Per-`BackendGroup` fields (on each group in `.Backends`):

| Field | Description |
|-------|-------------|
| `.LocalEndpoints` | Same-zone endpoints (where `.Zone == .LocalZone` or `.ForZones` contains `.LocalZone`). Empty when `.LocalZone` is empty. |
| `.RemoteEndpoints` | Other-zone endpoints. Endpoints with unknown zone and no matching `.ForZones` hint land here. Empty when `.LocalZone` is empty. |

Per-endpoint fields (on each `Frontend` / `Endpoint`):

| Field | Description |
|-------|-------------|
| `.Zone` | Topology zone from `topology.kubernetes.io/zone` (EndpointSlice) |
| `.NodeName` | Node hosting the endpoint |
| `.ForZones` | Zone hints from Kubernetes Topology Aware Routing (`service.kubernetes.io/topology-mode: Auto`) |

### Example: weighted director preferring same-zone backends

```vcl
sub vcl_init {
  new lb = directors.random();
  <<- range .Backends.myapp.Endpoints >>
  lb.add_backend(<< .Name >>_myapp,
    << if eq .Zone $.LocalZone >>10<< else >>1<< end >>);
  <<- end >>
}
```

This gives backends in the same zone a weight of 10, while remote backends get a weight of 1. If `NODE_NAME` is not set or zone detection fails, `.LocalZone` is empty and all backends get the same weight (the `eq` never matches).

### Example: fallback director preferring same-zone backends

Each `BackendGroup` has `.LocalEndpoints` and `.RemoteEndpoints` — pre-filtered views of `.Endpoints` split by zone — so you can build separate directors without inline conditionals. An endpoint is considered local if its `.Zone` matches `.LocalZone` or if its `.ForZones` hints include `.LocalZone` (Kubernetes Topology Aware Routing). When `.LocalZone` is empty, both lists are empty and you should fall back to `.Endpoints`.

The pattern works with any number of `--backend` groups. For each group, a local and remote [round-robin director](https://varnish-cache.org/docs/trunk/reference/vmod_directors.html) can be created, then combined via a [fallback director](https://varnish-cache.org/docs/trunk/reference/vmod_directors.html) that prefers the local director. The backends are declared from `.Endpoints` (which contains all endpoints regardless of zone), so every pod is reachable; only the director routing favors same-zone pods.

When `.LocalZone` is empty (e.g. `NODE_NAME` not set), both `.LocalEndpoints` and `.RemoteEndpoints` are empty. A round-robin director with zero backends returns `NULL` for every request, so the template must guard the fallback pattern with `if .LocalZone` and fall back to a plain round-robin over `.Endpoints`:

```vcl
vcl 4.1;

import directors;

<<- range $name, $bg := .Backends >>
<<- range $bg.Endpoints >>
backend << .Name >>_<< $name >> {
  .host = "<< .IP >>";
  .port = "<< .Port >>";
}
<<- end >>
<<- end >>

sub vcl_init {
  <<- range $name, $bg := .Backends >>
  <<- if $.LocalZone >>

  new local_<< $name >> = directors.round_robin();
  <<- range $bg.LocalEndpoints >>
  local_<< $name >>.add_backend(<< .Name >>_<< $name >>);
  <<- end >>

  new remote_<< $name >> = directors.round_robin();
  <<- range $bg.RemoteEndpoints >>
  remote_<< $name >>.add_backend(<< .Name >>_<< $name >>);
  <<- end >>

  new backend_<< $name >> = directors.fallback();
  backend_<< $name >>.add_backend(local_<< $name >>.backend());
  backend_<< $name >>.add_backend(remote_<< $name >>.backend());

  <<- else >>

  new backend_<< $name >> = directors.round_robin();
  <<- range $bg.Endpoints >>
  backend_<< $name >>.add_backend(<< .Name >>_<< $name >>);
  <<- end >>

  <<- end >>
  <<- end >>
}

sub vcl_recv {
  <<- range $name, $_ := .Backends >>
  if (req.url ~ "^/<< $name >>/") {
    set req.backend_hint = backend_<< $name >>.backend();
  }
  <<- end >>
}
```

With `--backend api=... --backend web=...` and pods spread across two zones, this creates `local_api`, `remote_api`, `backend_api` (fallback), `local_web`, `remote_web`, `backend_web` (fallback). Each `backend_*` director prefers same-zone pods via round-robin and falls back to the remote round-robin only when all local pods are unhealthy. When `.LocalZone` is empty, it degrades gracefully to a plain round-robin over all endpoints.

> **Note on shard routing:** When [frontend sharding](#reference-vcl-template) is combined with zone-aware backend directors, shard routing can still cause cross-zone traffic. The shard director selects the owning Varnish pod by URL hash — if the selected pod is in a different zone, the request is forwarded there before the backend director runs. The zone-aware fallback director then picks a backend local to *that* pod, not the pod that originally received the request. This is by design (it preserves cache efficiency), but means that cross-zone hops between Varnish pods are not eliminated by zone-aware backend routing alone.

### Kubernetes Topology Aware Routing hints

When the Service has `service.kubernetes.io/topology-mode: Auto`, the kube-proxy allocates zone hints on each endpoint. These are available as `.ForZones` (a list of zone names, from `endpoint.hints.forZones`). You can use them to filter endpoints that Kubernetes recommends for your zone:

```vcl
<<- range .Backends.myapp.Endpoints >>
<<- if has $.LocalZone .ForZones >>
backend << .Name >>_myapp { .host = "<< .IP >>"; .port = "<< .Port >>"; }
<<- end >>
<<- end >>
```

## Broadcast server

The broadcast server fans out incoming HTTP requests (e.g. PURGE) to all Varnish frontend pods and returns an aggregated JSON response.

Default listen address: `:8088`. Disable with `--broadcast-addr=none`.

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

## Access logging

k8s-httpcache can run a managed `varnishncsa` subprocess alongside `varnishd` to stream HTTP access logs. This is a convenience option for simple setups. For production deployments, consider running `varnishncsa` as a separate sidecar container instead — this lets log tailers use the container name to distinguish access logs from k8s-httpcache/varnishd output without relying on line prefixes.

Enable it with `--varnishncsa-enabled`:

```
k8s-httpcache --varnishncsa-enabled ...
```

By default, access logs are written to stdout using Varnish's Combined log format (equivalent to Apache's `%h %l %u %t "%r" %s %b "%{Referer}i" "%{User-agent}i"`). Each line is prefixed with `[access]` (plus a trailing space) so you can distinguish access log lines from application log lines:

```
[access] 10.244.0.5 - - [27/Feb/2026:14:30:01 +0000] "GET /api/v1/data HTTP/1.1" 200 1234 "https://example.com" "curl/8.5.0"
2026/02/27 14:30:01 INFO VCL reloaded
```

### Lifecycle

- The subprocess shares Varnish's working directory (`-n`) automatically
- If `varnishncsa` exits unexpectedly, it is automatically restarted after a 5-second delay
- Kubernetes Events are emitted for start failures, unexpected exits, and restarts (see [Kubernetes Events](#kubernetes-events))
- If `varnishncsa` crashes 3 times consecutively (start failures or unexpected exits), k8s-httpcache shuts down with a non-zero exit code so Kubernetes can restart the pod
- On shutdown, `varnishncsa` receives SIGTERM before `varnishd` is stopped

### Examples

Log only 5xx responses:

```
k8s-httpcache --varnishncsa-enabled --varnishncsa-query='RespStatus >= 500'
```

Use a custom format showing cache hit/miss:

```
k8s-httpcache --varnishncsa-enabled --varnishncsa-format='%h %s %{Varnish:hitmiss}x %U'
```

Log backend (fetch) requests instead of client requests:

```
k8s-httpcache --varnishncsa-enabled --varnishncsa-backend
```

Write to a file instead of stdout (prefix is ignored when writing to a file):

```
k8s-httpcache --varnishncsa-enabled --varnishncsa-output=/var/log/varnish/access.log
```

See [Access logging flags](#access-logging-flags) for the full flag reference.

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

k8s-httpcache needs `list` and `watch` on `services` and `endpointslices` in its own namespace. If using `--values`, it also needs `list` and `watch` on `configmaps`. If using `--secrets`, it also needs `list` and `watch` on `secrets`:

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
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["list", "watch"]
```

To enable [Kubernetes Events](#kubernetes-events), add:

```yaml
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
```

The `pods` `get` permission allows the event recorder to look up the pod UID so that events appear in `kubectl describe pod`. The `events` permissions are needed to actually create the events. Both are optional — if omitted, a single warning is logged and the service continues without event recording.

### Node access for zone auto-detection

To auto-detect the topology zone from `NODE_NAME` (see [Topology-aware routing](#topology-aware-routing)), add a ClusterRole and ClusterRoleBinding with `get` on `nodes`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: k8s-httpcache-nodes
rules:
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: k8s-httpcache-nodes
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: k8s-httpcache-nodes
subjects:
- kind: ServiceAccount
  name: k8s-httpcache
  namespace: default
```

This is **not needed** when using `--zone` to set the topology zone explicitly.

### Cross-namespace

To watch services, ConfigMaps, or Secrets in other namespaces (for cross-namespace backends, values, or secrets), create a Role and RoleBinding in each target namespace. The RoleBinding must reference the ServiceAccount from the k8s-httpcache namespace:

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
- apiGroups: [""]
  resources: ["secrets"]
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
