#!/bin/sh
# Download and install (or update) the ouvrier CLI from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/ArnaudGuiovanna/ouvrier/main/install.sh | sh
#
# Options (environment variables):
#   OUVRIER_VERSION   release tag to install (default: latest), e.g. v0.3.0
#   OUVRIER_BIN_DIR   install directory (default: /usr/local/bin, falling back
#                     to ~/.local/bin when /usr/local/bin is not writable)
#
# POSIX sh, no dependencies beyond curl (or wget), uname, and sha256 tooling.
set -eu

REPO="ArnaudGuiovanna/ouvrier"
VERSION="${OUVRIER_VERSION:-latest}"

err() { printf 'install: %s\n' "$1" >&2; exit 1; }

# ---- detect platform --------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux) os=linux ;;
  darwin) os=darwin ;;
  *) err "unsupported OS '$os' (linux and darwin only); build from source with: go install github.com/$REPO/cmd/ouvrier@latest" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) err "unsupported architecture '$arch' (amd64 and arm64 only)" ;;
esac

asset="ouvrier_${os}_${arch}"

# ---- pick a downloader ------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  dl() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  dl() { wget -qO "$2" "$1"; }
else
  err "need curl or wget"
fi

# ---- resolve the download base ----------------------------------------------
if [ "$VERSION" = latest ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

printf 'install: downloading %s (%s)...\n' "$asset" "$VERSION" >&2
dl "$base/$asset" "$tmp/$asset" || err "download failed — does release '$VERSION' have a $asset binary? (releases before v0.3.0 ship source only)"

# ---- verify checksum (best effort: skip with a warning if unavailable) ------
if dl "$base/SHA256SUMS" "$tmp/SHA256SUMS" 2>/dev/null; then
  want=$(grep " $asset\$" "$tmp/SHA256SUMS" | awk '{print $1}')
  [ -n "$want" ] || err "no checksum for $asset in SHA256SUMS"
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
  else
    got=""
  fi
  if [ -n "$got" ] && [ "$got" != "$want" ]; then
    err "checksum mismatch for $asset (want $want, got $got)"
  fi
  [ -n "$got" ] && printf 'install: checksum verified\n' >&2
else
  printf 'install: warning: no SHA256SUMS published for %s; skipping verification\n' "$VERSION" >&2
fi

chmod +x "$tmp/$asset"

# ---- choose an install dir --------------------------------------------------
if [ -n "${OUVRIER_BIN_DIR:-}" ]; then
  bindir="$OUVRIER_BIN_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then
  bindir=/usr/local/bin
else
  bindir="$HOME/.local/bin"
fi
mkdir -p "$bindir"

dest="$bindir/ouvrier"
if mv "$tmp/$asset" "$dest" 2>/dev/null; then
  :
elif command -v sudo >/dev/null 2>&1; then
  printf 'install: %s is not writable; using sudo\n' "$bindir" >&2
  sudo mv "$tmp/$asset" "$dest"
else
  err "cannot write to $bindir (set OUVRIER_BIN_DIR to a writable directory)"
fi

printf 'install: installed ouvrier to %s\n' "$dest" >&2
case ":$PATH:" in
  *":$bindir:"*) ;;
  *) printf 'install: note: %s is not on your PATH; add it to use `ouvrier` directly\n' "$bindir" >&2 ;;
esac

"$dest" version
