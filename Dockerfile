# syntax=docker/dockerfile:1
# Build context is this repository; the panel repository (it owns the proto module) is the named context `panel`:
#   docker build --build-context panel=../d256-panel .   (or panel=https://github.com/dest1256/d256-panel.git)
FROM golang:1.27-alpine AS builder
WORKDIR /src
WORKDIR /src/d256-node
COPY go.mod go.sum* ./
COPY --from=panel proto /src/d256-panel/proto
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-w -s -X github.com/dest1256/d256-node/internal/version.Version=${VERSION}" -o /out/d256-node ./cmd/d256-node \
 && mkdir -p /out/state

FROM alpine:3.22 AS geo
RUN apk add --no-cache curl ca-certificates  && mkdir -p /geo && cd /geo  && curl -sfLRO https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat  && curl -sfLRO https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=geo /geo /usr/share/xray
ENV XRAY_LOCATION_ASSET=/usr/share/xray
COPY --from=builder /out/d256-node /usr/local/bin/d256-node
COPY --from=builder --chown=65532:65532 /out/state /var/lib/d256-node
USER nonroot:nonroot
ENV D256_STATE_DIR=/var/lib/d256-node
VOLUME ["/var/lib/d256-node"]
ENTRYPOINT ["/usr/local/bin/d256-node"]
CMD ["run"]
