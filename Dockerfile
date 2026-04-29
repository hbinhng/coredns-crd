# Builds a CoreDNS binary with the `crd` plugin baked in.
# The plugin is registered before `kubernetes` in plugin.cfg so it gets first
# crack at queries before the in-cluster service resolver.

ARG GO_VERSION=1.23
ARG COREDNS_VERSION=v1.12.0

FROM golang:${GO_VERSION}-alpine AS builder
ARG COREDNS_VERSION
RUN apk add --no-cache git make bash

WORKDIR /src/plugin
COPY . .

# Resolve our module path so `go get` from the CoreDNS tree picks it up locally.
RUN go mod download

WORKDIR /src
RUN git clone --depth 1 --branch ${COREDNS_VERSION} https://github.com/coredns/coredns.git
WORKDIR /src/coredns

# Insert "crd:github.com/hbinhng/coredns-crd/plugin/crd" before the kubernetes line.
RUN sed -i '/^kubernetes:kubernetes$/i crd:github.com/hbinhng/coredns-crd/plugin/crd' plugin.cfg

# Point Go at the local plugin source instead of pulling from a remote.
RUN go mod edit -require=github.com/hbinhng/coredns-crd@v0.0.0
RUN go mod edit -replace=github.com/hbinhng/coredns-crd=/src/plugin
RUN go generate
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/coredns

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/coredns /coredns
EXPOSE 53 53/udp 9153
USER nonroot:nonroot
ENTRYPOINT ["/coredns"]
