FROM varnish:6.0.17@sha256:5a330531aed44556cb3945541e545ee72883e95c02004a90e0de9d9d4095c2e9
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
