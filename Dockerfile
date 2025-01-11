FROM golang:alpine3.21@sha256:c23339199a08b0e12032856908589a6d41a0dab141b8b3b21f156fc571a3f1d3 AS builder

# renovate: depName=containernetworking/plugins
ARG CNI_PLUGINS_VERSION=v1.6.2

WORKDIR /app

COPY copyfiles.go .

RUN go build -o copyfiles copyfiles.go

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

# Step 2: Use the distroless base image
FROM gcr.io/distroless/base

# Copy the compiled Go binary into the distroless image
COPY --from=builder /app/copyfiles /usr/local/bin/copyfiles

COPY --from=builder /opt/cni/bin /opt/cni/bin

# Set the entrypoint to the compiled Go binary
CMD ["/usr/local/bin/copyfiles"]
