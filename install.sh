#!/bin/sh
# Install tmvp:  curl -fsSL https://raw.githubusercontent.com/muthuishere/tmvp/main/install.sh | sh
# Options (env): TMVP_VERSION=v0.1.0 (default latest)  TMVP_BIN_DIR=~/bin (default ~/.local/bin)
# One static binary is all tmvp needs; docs and the tmvp-discoverer agent skill are built in.
# The skill is opt-in: `tmvp skill install --discover`.
# Re-run to upgrade. Never uses sudo.
set -eu
repo="${TMVP_REPO:-muthuishere/tmvp}"
dest="${TMVP_BIN_DIR:-$HOME/.local/bin}"
ver="${TMVP_VERSION:-latest}"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in darwin|linux) ;; *) echo "tmvp: unsupported OS $os (Windows: use install.cmd)" >&2; exit 1 ;; esac
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "tmvp: unsupported CPU $arch" >&2; exit 1 ;; esac
if [ -n "${TMVP_BASE_URL:-}" ]; then base="$TMVP_BASE_URL"
elif [ "$ver" = latest ]; then base="https://github.com/$repo/releases/latest/download"
else base="https://github.com/$repo/releases/download/$ver"; fi
asset="tmvp_${os}_${arch}"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
want=$(awk -v f="$asset" '$2==f {print $1}' "$tmp/checksums.txt")
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}'); fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then echo "tmvp: checksum mismatch for $asset; not installed" >&2; exit 1; fi
mkdir -p "$dest"
install -m 0755 "$tmp/$asset" "$dest/tmvp"
"$dest/tmvp" version | head -1
case ":$PATH:" in *":$dest:"*) ;; *) echo "add $dest to PATH, e.g.: echo 'export PATH=\"$dest:\$PATH\"' >> ~/.zshrc" ;; esac
echo "next: tmvp skill install --discover, then in your product repo ask your agent: /tmvp-discoverer"
