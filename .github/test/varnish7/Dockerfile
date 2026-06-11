FROM varnish:7.7.3-alpine@sha256:8aa05c32b87720c21201f23664c14e58218e6a8bf2d240192d61ddf7cbda1543
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
