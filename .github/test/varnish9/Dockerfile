FROM varnish:9.1.0@sha256:842a2874f49d5b9eb7690a8fe0b6d3a66d8045b2d2a96154b4a804e2cbd8ac33
ARG BINARY=k8s-httpcache
COPY --chmod=755 ${BINARY} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
