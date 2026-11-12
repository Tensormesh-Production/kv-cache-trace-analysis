#!/bin/sh
# tm-trace install script, for macOS and Linux.
#
# Usage:
#   curl -sSfL https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest/download/install.sh | sh
#   curl -sSfL https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest/download/install.sh | sh -s -- [--version v0.1.0] [--install-dir ~/bin]
#
# Environment variables (alternative to the options):
#   TM_TRACE_VERSION      Version to install, such as v0.1.0 (default: latest)
#   TM_TRACE_INSTALL_DIR  Installation folder
#   TM_TRACE_RELEASES     Releases URL, for a mirror (default: the GitHub releases)
#
# It downloads the release from GitHub, checks its SHA-256 checksum, and also
# checks the checksum file's signature when cosign is installed.

set -eu

REPO="Tensormesh-Production/kv-cache-trace-analysis"
BINARY="tm-trace"
BASE_URL="${TM_TRACE_RELEASES:-https://github.com/${REPO}/releases}"

info() { printf '[info]  %s\n' "$*"; }
warn() { printf '[warn]  %s\n' "$*" >&2; }
die()  { printf '[error] %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not found in PATH"; }

usage() {
  cat <<'EOF'
tm-trace install script

USAGE:
  curl -sSfL https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest/download/install.sh | sh
  curl -sSfL https://github.com/tensormesh-production/kv-cache-trace-analysis/releases/latest/download/install.sh | sh -s -- [OPTIONS]

OPTIONS:
  -v, --version <version>   Version to install, such as v0.1.0 (default: latest)
  -d, --install-dir <dir>   Installation folder (default: /usr/local/bin if writable, else ~/.local/bin)
  -h, --help                Show this help
EOF
}

VERSION="${TM_TRACE_VERSION:-latest}"
INSTALL_DIR="${TM_TRACE_INSTALL_DIR:-}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    -v|--version)     [ "$#" -ge 2 ] || die "$1 needs a value"; VERSION="$2";     shift 2 ;;
    -d|--install-dir) [ "$#" -ge 2 ] || die "$1 needs a value"; INSTALL_DIR="$2"; shift 2 ;;
    -h|--help)        usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

case "$(uname -s)" in
  Linux*)  OS=linux ;;
  Darwin*) OS=darwin ;;
  *) die "unsupported OS: $(uname -s). On Windows, download tm-trace_<version>_windows_<arch>.zip from ${BASE_URL}" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU: $(uname -m)" ;;
esac

need curl
need tar
if   command -v shasum    >/dev/null 2>&1; then SHA_CMD="shasum -a 256"
elif command -v sha256sum >/dev/null 2>&1; then SHA_CMD="sha256sum"
else die "need shasum or sha256sum to check the download"; fi

if [ "$VERSION" = latest ]; then
  VERSION=$(curl -sSfLI "${BASE_URL}/latest" | tr -d '\r' | grep -i '^location:' | tail -1 | sed 's|.*/tag/||')
  [ -n "$VERSION" ] || die "could not find the latest version at ${BASE_URL}/latest"
fi
info "installing ${BINARY} ${VERSION} for ${OS}/${ARCH}"

ARCHIVE="${BINARY}_${VERSION#v}_${OS}_${ARCH}.tar.gz"
URL="${BASE_URL}/download/${VERSION}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

curl -sSfL "${URL}/${ARCHIVE}" -o "${WORK_DIR}/${ARCHIVE}" || die "download failed: ${URL}/${ARCHIVE}"
curl -sSfL "${URL}/checksums.txt" -o "${WORK_DIR}/checksums.txt" || die "download failed: ${URL}/checksums.txt"

if command -v cosign >/dev/null 2>&1; then
  curl -sSfL "${URL}/checksums.txt.sigstore.json" -o "${WORK_DIR}/checksums.txt.sigstore.json" \
    || die "download failed: ${URL}/checksums.txt.sigstore.json"
  cosign verify-blob "${WORK_DIR}/checksums.txt" --bundle "${WORK_DIR}/checksums.txt.sigstore.json" \
    --certificate-identity "https://github.com/${REPO}/.github/workflows/release.yml@refs/tags/${VERSION}" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com >/dev/null 2>&1 \
    || die "the signature of checksums.txt does not match the ${REPO} release workflow"
  info "signature ok"
else
  info "cosign not found: skipped the signature check"
fi

(cd "$WORK_DIR" && grep "  ${ARCHIVE}\$" checksums.txt | $SHA_CMD -c - >/dev/null) \
  || die "checksum mismatch for ${ARCHIVE}: the download may be corrupt"
info "checksum ok"
tar -xzf "${WORK_DIR}/${ARCHIVE}" -C "$WORK_DIR" "$BINARY"

if [ -z "$INSTALL_DIR" ]; then
  if [ -w /usr/local/bin ]; then INSTALL_DIR=/usr/local/bin; else INSTALL_DIR="${HOME}/.local/bin"; fi
fi
mkdir -p "$INSTALL_DIR" || die "cannot create ${INSTALL_DIR}"
DEST="${INSTALL_DIR}/${BINARY}"
if [ -e "$DEST" ] && [ ! -w "$DEST" ]; then
  die "cannot write ${DEST}: use --install-dir with a writable folder"
fi
mv "${WORK_DIR}/${BINARY}" "$DEST"
chmod +x "$DEST"
info "installed ${DEST}"

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *) warn "${INSTALL_DIR} is not in PATH. Add it with: export PATH=\"${INSTALL_DIR}:\$PATH\"" ;;
esac
