FROM varnish:8.0.2-alpine@sha256:a44e33228e62b1da338b0de838e56ef0c49fc828e933b393a7ecf20a38121e60
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
