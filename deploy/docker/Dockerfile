# cub-panel (master) container image.
# Multi-stage: build the static binary, then ship it on a tiny Alpine runtime.
# Build context is the repository root:
#   docker build -f deploy/docker/Dockerfile -t cub-panel .

# ---- build ----
FROM golang:1.25-alpine AS build
WORKDIR /src
# Cache modules first.
COPY src/go.mod src/go.sum ./
RUN go mod download
COPY src/ ./
# CGO off keeps the binary static (SQLite/pg/mysql drivers are all pure Go).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cub-panel ./cmd/panel

# ---- runtime ----
FROM alpine:3.22
# ca-certificates: the panel talks HTTPS to payment gateways and image servers.
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/cub-panel /usr/local/bin/cub-panel

# Defaults; override via -e / compose. SQLite lives on the mounted volume.
ENV CUB_PANEL_LISTEN=0.0.0.0:8080 \
    CUB_PANEL_DB=/data/panel.db \
    CUB_PANEL_DB_DRIVER=sqlite \
    CUB_PANEL_ALLOW_SIGNUP=1
VOLUME /data
# Informational only — host network mode ignores published ports.
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/cub-panel"]
