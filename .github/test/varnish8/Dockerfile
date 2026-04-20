FROM varnish:8.0.1-alpine@sha256:0860b5e9f5535e373080f6921bdb68959dc93c300da569107cfedc45fe236e67
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
