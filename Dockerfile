FROM alpine:3.21@sha256:56fa17d2a7e7f168a043a2712e63aed1f8543aeafdcee47c58dcffe38ed51099

# renovate: depName=containernetworking/plugins
ARG CNI_PLUGINS_VERSION=v1.6.2

WORKDIR /tmp

RUN apk add --no-cache \
    curl \
    ca-certificates \
    bash \
    tar \
    coreutils \
    && rm -rf /var/cache/apk/*

RUN curl -fsSLO https://github.com/containernetworking/plugins/releases/download/$CNI_PLUGINS_VERSION/cni-plugins-linux-amd64-$CNI_PLUGINS_VERSION.tgz{,.sha256} && \
    sha256sum --check --strict cni-plugins-linux-amd64-$CNI_PLUGINS_VERSION.tgz.sha256 && \
    mkdir -p /opt/cni/bin && \
    tar -xzvf cni-plugins-linux-amd64-$CNI_PLUGINS_VERSION.tgz -C /opt/cni/bin && \
    rm -rf /root/.cache /tmp/*

ENTRYPOINT ["/bin/bash", "-c", "\
  mkdir -p /host/opt/cni/bin && \
  cp /opt/cni/bin/* /host/opt/cni/bin && \
  if [ $? -eq 0 ]; then echo 'CNI plugins copied successfully.'; exit 0; else echo 'Failed to copy CNI plugins.'; exit 1; fi"]
