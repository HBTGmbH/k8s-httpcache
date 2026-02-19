FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.26.0-alpine3.23@sha256:d4c4845f5d60c6a974c6000ce58ae079328d03ab7f721a0734277e69905473e5 AS modules
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

FROM modules AS builder
ARG TARGETOS
ARG TARGETARCH
ADD . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM64="v9.0" \
    go build -trimpath -ldflags '-extldflags "-static" -buildid=' -o k8s-httpcache .

FROM scratch
COPY --from=builder /build/k8s-httpcache /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
