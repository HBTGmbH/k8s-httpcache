FROM varnish:9.0.3@sha256:7db93cafd7d610c3a646166bf8ff782da7e4fd0155a175d7beec28c796d469d3
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
