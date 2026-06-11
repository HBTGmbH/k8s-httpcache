FROM varnish:8.0.2-alpine@sha256:1e7ed3f804951e43ed90c300bfabb5715b26a4514d119fe6d8dd43f82394adb8
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
