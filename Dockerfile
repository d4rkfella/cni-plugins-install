FROM alpine:3.21@sha256:21dc6063fd678b478f57c0e13f47560d0ea4eeba26dfc947b2a4f81f686b9f45

# renovate: depName=containernetworking/plugins
ARG CNI_PLUGINS_VERSION=v1.6.1

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
