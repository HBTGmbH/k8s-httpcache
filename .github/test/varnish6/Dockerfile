FROM varnish:6.0.18@sha256:9c0a40c3343f4f4634383a3a335390969b7390f5b4ad3b31917790fadc552dbd
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
