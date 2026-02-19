ARG BASE_IMAGE
FROM ${BASE_IMAGE} AS binary

FROM varnish:8.0.0-alpine@sha256:a84e1d4adb58b1f0594efbc95b9854c37040aafbbb990fa4889d6520a2feea91
COPY --from=binary /usr/local/bin/k8s-httpcache /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
