FROM varnish:9.0.3@sha256:69773370657fcc3e7226dffc0bb2f836d222961c50558143b814e301cf4283e2
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
