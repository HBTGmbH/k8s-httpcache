FROM varnish:8.0.2-alpine@sha256:1b39ddb86b460d9c80fbe7a92de6167ebafd4bb58d5b098a02f963d6efd7902e
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
