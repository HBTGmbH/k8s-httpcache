FROM varnish:6.0.18@sha256:baeffccf2143476540ceb3883388bc84ef6fc077de80ff80aa72dd81dbd977af
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
