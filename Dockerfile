# syntax=docker/dockerfile:1
# Build context is this repository; the panel repository (it owns the proto module) is the named context `panel`:
#   docker build --build-context panel=../d256-panel .   (or panel=https://github.com/dest1256/d256-panel.git)
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
WORKDIR /src/d256-node
COPY go.mod go.sum* ./
COPY --from=panel proto /src/d256-panel/proto
RUN go mod download
COPY . .
ARG VERSION=dev
# Only the end-to-end test sets UPDATE_BASE_URL (a local release server); production images never do.
ARG UPDATE_BASE_URL=
RUN LD="-w -s -X github.com/dest1256/d256-node/internal/version.Version=${VERSION}"; \
    if [ -n "$UPDATE_BASE_URL" ]; then LD="$LD -X github.com/dest1256/d256-node/internal/update.DefaultBaseURL=$UPDATE_BASE_URL"; fi; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "$LD" -o /out/d256-node ./cmd/d256-node \
 && mkdir -p /out/state

FROM --platform=$BUILDPLATFORM alpine:3.22 AS geo
# The same geo files the 3x-ui routing presets refer to (ext:geosite_RU.dat:..., ext:geoip_IR.dat:...).
RUN apk add --no-cache curl ca-certificates \
 && mkdir -p /geo && cd /geo \
 && L=https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download \
 && I=https://github.com/chocolate4u/Iran-v2ray-rules/releases/latest/download \
 && R=https://github.com/runetfreedom/russia-v2ray-rules-dat/releases/latest/download \
 && curl -sfL -o geoip.dat $L/geoip.dat && curl -sfL -o geosite.dat $L/geosite.dat \
 && curl -sfL -o geoip_IR.dat $I/geoip.dat && curl -sfL -o geosite_IR.dat $I/geosite.dat \
 && curl -sfL -o geoip_RU.dat $R/geoip.dat && curl -sfL -o geosite_RU.dat $R/geosite.dat

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
