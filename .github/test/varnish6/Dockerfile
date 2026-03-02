FROM varnish:6.0.16@sha256:b61dde4047098837f0b22b0855e57ce01797471f586e377cd559197c50627412
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
