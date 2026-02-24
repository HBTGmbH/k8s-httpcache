FROM varnish:6.0.16@sha256:1ab688fc4ebddba39ef6a74e99216684e746eff0652450de0735b6bd65440a30
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
