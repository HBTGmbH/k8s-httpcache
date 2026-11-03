FROM varnish:9.1.0@sha256:4c6066279af88edcdd97d35a45579c77f363e551cd9cb5597644ac856d8ebbdf
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
