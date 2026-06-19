FROM varnish:6.0.18@sha256:57d9f4ddc7d67bba82f6ac513bf8bd69ef07b15cfbbbb90110ff5c3782de2650
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
