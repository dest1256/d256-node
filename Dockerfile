# syntax=docker/dockerfile:1
# Build context is the parent directory (sibling checkouts); see `make image`.
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY d256-node/go.mod d256-node/go.sum* d256-node/
COPY --from=panel proto d256-panel/proto
WORKDIR /src/d256-node
RUN go mod download
COPY d256-node/ ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-w -s -X github.com/dest1256/d256-node/internal/version.Version=${VERSION}" -o /out/d256-node ./cmd/d256-node

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/d256-node /usr/local/bin/d256-node
USER nonroot:nonroot
VOLUME ["/var/lib/d256-node"]
ENTRYPOINT ["/usr/local/bin/d256-node"]
CMD ["run"]
