FROM varnish:6.0.17@sha256:ec99f528eb409789d6b2d01178a36388f21d5a19faab7f695e39ad5e23eca5ad
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
