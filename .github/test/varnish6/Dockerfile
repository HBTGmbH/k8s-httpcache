FROM varnish:6.0.18@sha256:efe8efe67d8fa8be0a15ed23f8ea7c518e8a11060c5942ff014dcd5ee590c019
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
