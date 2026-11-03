# Race-detector E2E image. The race detector needs cgo, so the binary is
# dynamically linked and must match the runtime image's glibc. Compile it in the
# same Debian trixie as the varnish:9 base, then run it there. Built only by
# the race leg of the integration-test matrix in test-and-build.yml.
FROM golang:1.27.3-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=1 go build -race -trimpath -o /k8s-httpcache .

FROM varnish:9.1.0@sha256:842a2874f49d5b9eb7690a8fe0b6d3a66d8045b2d2a96154b4a804e2cbd8ac33
COPY --from=build --chmod=755 /k8s-httpcache /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
