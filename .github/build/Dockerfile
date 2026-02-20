FROM scratch
ARG TARGETARCH
COPY --chmod=755 k8s-httpcache-linux-${TARGETARCH} /usr/local/bin/k8s-httpcache
ENTRYPOINT ["/usr/local/bin/k8s-httpcache"]
CMD []
