FROM varnish:6.0.18@sha256:65de4db75b40ab26fe1b769b3fecafe12ecc0312310d2a8a557a1958683fa279
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
