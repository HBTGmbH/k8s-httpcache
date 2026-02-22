ARG VARNISH_IMAGE=varnish:8.0.0-alpine
FROM ${VARNISH_IMAGE}
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
