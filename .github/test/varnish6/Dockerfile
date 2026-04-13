FROM varnish:6.0.17@sha256:a327469c847044adad0f29d2912abe3ee2078efcc70fa723233a0e2ff033e602
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
