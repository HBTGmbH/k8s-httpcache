# Race-detector E2E image. The race detector needs cgo, so the binary is
# dynamically linked and must match the runtime image's glibc. Compile it in the
# same Debian trixie as the varnish:9 base, then run it there. Built only by
# the race leg of the integration-test matrix in test-and-build.yml.
FROM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=1 go build -race -trimpath -o /k8s-httpcache .

FROM varnish:9.1.0@sha256:4c6066279af88edcdd97d35a45579c77f363e551cd9cb5597644ac856d8ebbdf
COPY --from=build --chmod=755 /k8s-httpcache /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
