#!/usr/bin/env bash
# =============================================================================
# Mole Build Script
# Builds moled and/or mole for multiple platforms.
#
# Usage:
#   bash build.sh [server|client|all]   (default: all)
#
# Output binaries are placed in ./dist/
#
# Cross-compilation targets:
#   Linux   amd64 / arm64
#   macOS   amd64 / arm64 (Apple Silicon)
#   Windows amd64
# =============================================================================
set -euo pipefail

GOFLAGS="-mod=mod"
GONOSUMDB="*"
export GOFLAGS GONOSUMDB

DIST="./dist"
mkdir -p "${DIST}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; NC='\033[0m'
info()  { echo -e "${CYAN}[BUILD]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
fatal() { echo -e "${RED}[ERR]${NC}   $*" >&2; exit 1; }

TARGET="${1:-all}"

build_binary() {
    local pkg="$1" name="$2" os="$3" arch="$4"
    local ext=""
    [[ "${os}" == "windows" ]] && ext=".exe"

    local out="${DIST}/${name}_${os}_${arch}${ext}"
    info "Building ${out}…"

    GOOS="${os}" GOARCH="${arch}" go build \
        -trimpath \
        -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo 'dev')" \
        -o "${out}" \
        "${pkg}"

    ok "Built: ${out}  ($(du -sh "${out}" | cut -f1))"
}

build_server() {
    info "=== Building moled ==="
    build_binary ./cmd/server moled linux   amd64
    build_binary ./cmd/server moled linux   arm64
    # macOS and Windows builds of the server are supported but not typical.
    # Uncomment if needed:
    # build_binary ./cmd/server moled darwin  amd64
    # build_binary ./cmd/server moled darwin  arm64
}

build_client() {
    info "=== Building mole ==="
    build_binary ./cmd/client mole linux   amd64
    build_binary ./cmd/client mole linux   arm64
    build_binary ./cmd/client mole darwin  amd64
    build_binary ./cmd/client mole darwin  arm64
    build_binary ./cmd/client mole windows amd64
}

echo -e "\n${CYAN}╔══════════════════════════════════════╗"
echo -e "║   Mole Build Script            ║"
echo -e "╚══════════════════════════════════════╝${NC}\n"

go version

case "${TARGET}" in
    server) build_server ;;
    client) build_client ;;
    all)    build_server; build_client ;;
    *)      fatal "Unknown target '${TARGET}'. Use: server | client | all" ;;
esac

echo ""
info "All binaries:"
ls -lh "${DIST}/"
echo ""
ok "Build complete. Binaries are in ${DIST}/"
echo ""
echo -e "  ${YELLOW}Server deployment:${NC}"
echo -e "    1. Copy ${DIST}/moled_linux_amd64 to your server as moled"
echo -e "    2. Run: sudo bash scripts/moled-install.sh\n"
echo -e "  ${YELLOW}Client deployment:${NC}"
echo -e "    1. Copy the appropriate mole binary to each client machine"
echo -e "    2. Copy config.yaml.example to config.yaml and edit it"
echo -e "    3. Run: sudo ./mole --config config.yaml\n"
