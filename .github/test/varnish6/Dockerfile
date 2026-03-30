FROM varnish:6.0.17@sha256:0d15a35db7861a1405c663c3698661ab48d44c62033d1de01f4b179bd67bfea5
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
