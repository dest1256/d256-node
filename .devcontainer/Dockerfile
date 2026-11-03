# Dev/CI toolchain. Nothing is installed on the host: every make target runs in this image.
FROM golang:1.27

RUN apt-get update && apt-get install -y --no-install-recommends make git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV GOBIN=/usr/local/bin
RUN go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 \
 && go install honnef.co/go/tools/cmd/staticcheck@latest \
 && go install golang.org/x/vuln/cmd/govulncheck@latest \
 && go clean -cache -modcache

ENV D256_IN_DEV=1 \
    GOFLAGS=-buildvcs=false \
    GOCACHE=/cache/go-build \
    GOMODCACHE=/cache/go-mod
WORKDIR /src/d256-node
