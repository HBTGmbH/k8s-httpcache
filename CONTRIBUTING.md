# Contributing to k8s-httpcache

Thanks for your interest in contributing! This document covers everything you need to get started.

## Prerequisites

- [Go 1.26+](https://go.dev/dl/)
- [Docker](https://docs.docker.com/get-docker/)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [golangci-lint](https://golangci-lint.run/welcome/install/)
- [hurl](https://hurl.dev/) for E2E test assertions
- A Kubernetes cluster for E2E testing (the CI uses [kind](https://kind.sigs.k8s.io/))
- [curl](https://curl.se/) for HTTP assertions in E2E tests
- [jq](https://jqlang.github.io/jq/) for JSON assertions in E2E tests
- [oha](https://github.com/hatoo/oha) (optional, only needed for the zero-downtime rollout test)

## Local development setup

Clone the repository and build:

```bash
git clone https://github.com/HBTGmbH/k8s-httpcache.git
cd k8s-httpcache
go build .
```

Build a static Linux binary (matching the CI):

```bash
CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags '-s -w -extldflags "-static" -buildid=' -o k8s-httpcache .
```

## Testing

### Unit tests

```bash
go test -race ./...
```

The CI uses [gotestsum](https://github.com/gotestyourself/gotestsum) for nicer output and JUnit reports:

```bash
gotestsum --format testdox -- -race ./...
```

### Linting

Run all linting checks (aborts on first failure):

```bash
.github/test/lint-all.sh
```

The project uses [golangci-lint](https://golangci-lint.run/) with an extensive rule set (see [`.golangci.yml`](.golangci.yml)). Run it locally:

```bash
golangci-lint run
```

YAML files are linted with [yamllint](https://yamllint.readthedocs.io/) (config in [`.yamllint.yml`](.yamllint.yml)), shell scripts with [ShellCheck](https://www.shellcheck.net/), and Markdown files with [markdownlint-cli2](https://github.com/DavidAnson/markdownlint-cli2) (config in [`.markdownlint-cli2.yaml`](.markdownlint-cli2.yaml)):

```bash
yamllint --strict .
shellcheck .github/test/*.sh
npx --yes markdownlint-cli2 "**/*.md"
```

The CI also runs [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) and [deadcode](https://pkg.go.dev/golang.org/x/tools/cmd/deadcode) detection:

```bash
go install golang.org/x/vuln/cmd/govulncheck@latest
govulncheck ./...

go install golang.org/x/tools/cmd/deadcode@latest
deadcode -test ./...
```

When modifying GitHub Actions workflows, run [actionlint](https://github.com/rhysd/actionlint) locally before pushing:

```bash
actionlint
```

### E2E tests

The CI runs E2E tests against a kind cluster. To run them locally:

1. Create a kind cluster:

   ```bash
   kind create cluster --name test --config .github/test/kind-config.yaml
   ```

2. Build and load the test image:

   ```bash
   CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags '-s -w -extldflags "-static" -buildid=' -o k8s-httpcache .
   mkdir -p .docker-context
   cp k8s-httpcache .docker-context/
   docker build -f .github/test/varnish8/Dockerfile -t k8s-httpcache:test .docker-context
   kind load docker-image k8s-httpcache:test --name test
   ```

3. Install ingress-nginx and deploy:

   ```bash
   kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/main/deploy/static/provider/kind/deploy.yaml
   kubectl patch configmap -n ingress-nginx ingress-nginx-controller --type merge -p '{"data":{"upstream-keepalive-timeout":"5"}}'
   kubectl wait -n ingress-nginx --for=condition=ready pod --selector=app.kubernetes.io/component=controller --timeout=120s
   kubectl apply -f .github/test/manifest.yaml
   kubectl rollout status deployment/k8s-httpcache --timeout=120s
   ```

4. Run all E2E tests (aborts on first failure):

   ```bash
   .github/test/test-all.sh
   ```

   Or run individual tests:

   ```bash
   .github/test/smoke-test.sh               # HTTP proxying, shard consistency
   .github/test/metrics-test.sh             # Prometheus metrics, broadcast fan-out
   .github/test/debounce-test.sh            # debounce coalescing and debounce-max
   .github/test/shard-test.sh               # shard distribution across pods
   .github/test/values-update-test.sh       # ConfigMap values live update
   .github/test/values-dir-update-test.sh   # values-dir (mounted volume) live update
   .github/test/vcl-update-test.sh          # VCL template live reload, retry & rollback
   .github/test/file-watch-disable-test.sh  # file-watch disable verification
   .github/test/drain-sessions-test.sh      # drain timing verification
   .github/test/drain-test.sh               # connection draining
   .github/test/topology-test.sh            # topology-aware routing
   .github/test/rollout-test.sh             # zero-downtime rollout (requires oha)
   ```

   `metrics-test.sh` automatically sets up `kubectl port-forward` for the
   metrics (`:9101`) and broadcast (`:8088`) ports if they are not already
   reachable, and cleans them up on exit.

5. Quick rebuild cycle (no cluster recreation needed):

   ```bash
   mkdir -p .docker-context \
     && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags '-s -w -extldflags "-static" -buildid=' -o k8s-httpcache . \
     && cp k8s-httpcache .docker-context/ \
     && docker build -t k8s-httpcache:test .docker-context -f .github/test/varnish8/Dockerfile \
     && kind load docker-image k8s-httpcache:test --name test \
     && kubectl rollout restart deployment/k8s-httpcache \
     && kubectl rollout status deployment/k8s-httpcache --timeout=120s
   ```

6. Clean up:

   ```bash
   kind delete cluster --name test
   ```

## Pull request workflow

1. **Fork and branch** — Create a feature branch from `main`. Use a descriptive name (e.g. `fix-backend-port-resolution`, `add-health-check-endpoint`).

2. **Make your changes** — Keep commits focused. Each commit should compile and pass tests.

3. **Run checks locally** before pushing:

   ```bash
   go test -race ./...
   golangci-lint run
   ```

4. **Open a pull request** against `main`. The PR description should explain *what* changed and *why*. Link any related issues.

5. **CI must pass** — The [Test and Build](.github/workflows/test-and-build.yml) workflow runs unit tests, linting, govulncheck, deadcode analysis, a full build, and E2E tests. All checks must be green before merging.

6. **Review** — A maintainer will review your PR. Please address feedback and keep the PR up to date with `main`.

## Code style

- Follow standard Go conventions ([Effective Go](https://go.dev/doc/effective_go), [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)).
- The `.golangci.yml` enforces the project's style rules — if the linter is happy, the style is fine.
- Use `gofmt` for formatting (enforced by CI).
- VCL templates use `<<` / `>>` delimiters by default (configurable via `--template-delims`).

## Releasing

Releases are automated. When a tag matching `v*` is pushed to `main`, the CI builds multi-arch binaries and container images, creates checksums, and publishes a GitHub release with auto-generated release notes.
