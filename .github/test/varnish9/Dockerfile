FROM varnish:9.0.3@sha256:47c2533928bb366d10c103e1c6c59ae124c5467414ec4ffae8187023c9f8754f
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
