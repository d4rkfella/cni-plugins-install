FROM golang:1.24.1-alpine3.21 AS builder


# renovate: depName=containernetworking/plugins
ARG CNI_PLUGINS_VERSION=v1.6.2

ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64

WORKDIR /app

COPY copyfiles.go .

RUN go build -o copyfiles copyfiles.go

RUN apk add --no-cache \
    curl \
    ca-certificates \
    tar \
    coreutils 

RUN curl -fsSLO https://github.com/containernetworking/plugins/releases/download/$CNI_PLUGINS_VERSION/cni-plugins-linux-amd64-$CNI_PLUGINS_VERSION.tgz{,.sha256} && \
    sha256sum --check --strict cni-plugins-linux-amd64-$CNI_PLUGINS_VERSION.tgz.sha256 && \
    mkdir -p /opt/cni/bin && \
    tar -xzvf cni-plugins-linux-amd64-$CNI_PLUGINS_VERSION.tgz -C /opt/cni/bin

FROM gcr.io/distroless/static

COPY --from=builder /app/copyfiles /usr/local/bin/copyfiles

COPY --from=builder /opt/cni/bin /opt/cni/bin

ENTRYPOINT ["/usr/local/bin/copyfiles"]
