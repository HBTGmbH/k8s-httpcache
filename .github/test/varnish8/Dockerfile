FROM varnish:8.0.1-alpine@sha256:6951946bb82d7cd8e9e1c866ecfe25ae95aa4200c4a88ebd9c49f4dcdf3b26da
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
