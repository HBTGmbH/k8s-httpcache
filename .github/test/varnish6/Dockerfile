FROM varnish:6.0.18@sha256:e9d21a33fa558d5fd34e2474286a16a4bbe29aa38a687ffe34845e8911db6bf2
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
