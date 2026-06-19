FROM varnish:8.0.2-alpine@sha256:edd2efd296a8bdb021841e8498a7cb61ded8bd5f3cb22ab542001a61cc750083
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
