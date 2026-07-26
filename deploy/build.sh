#!/bin/sh
# Rebuild both binaries from source into ../bin.
# Requires Go 1.25+. Produces static, cgo-free binaries.
#
# Cross-compile for other node architectures by overriding GOARCH; the output
# then gets an arch suffix so it never clobbers the native build:
#   GOARCH=arm64 ./build.sh   →  bin/cub-panel-arm64, bin/cub-agent-arm64
set -eu

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$SRC_DIR/src"

command -v go >/dev/null 2>&1 || {
	echo "go toolchain not found; install Go 1.25+ first" >&2
	exit 1
}

mkdir -p "$SRC_DIR/bin"

export CGO_ENABLED=0

HOST_ARCH="$(go env GOHOSTARCH)"
HOST_OS="$(go env GOHOSTOS)"
TARGET_ARCH="${GOARCH:-$HOST_ARCH}"
TARGET_OS="${GOOS:-$HOST_OS}"

SUFFIX=""
if [ "$TARGET_ARCH" != "$HOST_ARCH" ] || [ "$TARGET_OS" != "$HOST_OS" ]; then
	SUFFIX="-$TARGET_ARCH"
	[ "$TARGET_OS" != "linux" ] && SUFFIX="-$TARGET_OS$SUFFIX"
fi

# Stamp the release so both sides can report it (and the panel can warn about
# an agent that was never upgraded). Override with VERSION=v0.1.17 ./build.sh.
VERSION="${VERSION:-$(git -C "$SRC_DIR" describe --tags --always 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X cubpanel/internal/shared.Version=$VERSION"

echo "==> building cub-panel ($TARGET_OS/$TARGET_ARCH, $VERSION)"
go build -trimpath -ldflags="$LDFLAGS" -o "$SRC_DIR/bin/cub-panel$SUFFIX" ./cmd/panel

echo "==> building cub-agent ($TARGET_OS/$TARGET_ARCH, $VERSION)"
go build -trimpath -ldflags="$LDFLAGS" -o "$SRC_DIR/bin/cub-agent$SUFFIX" ./cmd/agent

chmod 0755 "$SRC_DIR/bin/cub-panel$SUFFIX" "$SRC_DIR/bin/cub-agent$SUFFIX"
ls -lh "$SRC_DIR/bin/"
