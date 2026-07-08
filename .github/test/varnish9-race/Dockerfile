# Race-detector E2E image. The race detector needs cgo, so the binary is
# dynamically linked and must match the runtime image's glibc. Compile it in the
# same Debian bookworm as the varnish:9 base, then run it there. Built only by
# the race leg of the integration-test matrix in test-and-build.yml.
FROM golang:1.26.5-bookworm@sha256:18aedc16aa19b3fd7ded7245fc14b109e054d65d22ed53c355c899582bbb2113 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=1 go build -race -trimpath -o /k8s-httpcache .

FROM varnish:9.0.3@sha256:69773370657fcc3e7226dffc0bb2f836d222961c50558143b814e301cf4283e2
COPY --from=build --chmod=755 /k8s-httpcache /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
