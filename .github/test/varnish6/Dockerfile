FROM varnish:6.0.16@sha256:855d6c2d3b99ab239649829f016c29217dc4fcb7e0372a2555e236036eae5195
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
