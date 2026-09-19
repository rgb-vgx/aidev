#!/bin/sh
# Install aidev from its GitHub releases:
#
#   curl -fsSL https://raw.githubusercontent.com/rgb-vgx/aidev/main/install.sh | sh
#
# Picks the archive for this machine (Linux or macOS, amd64 or arm64), checks it
# against the release's SHA256SUMS, and installs aidev into ~/.local/bin. Set
# AIDEV_VERSION=v0.1.0 to pin a release and AIDEV_INSTALL_DIR to install
# elsewhere. Everything runs inside main, called on the last line, so a
# truncated download runs nothing.

# download URL FILE, with curl or wget; fails naming the URL.
download() {
  if [ "$downloader" = curl ]; then
    curl -fsSL -o "$2" "$1" && return 0
  else
    wget -qO "$2" "$1" && return 0
  fi
  echo "install.sh: failed to download $1" >&2
  exit 1
}

main() {
  set -eu
  download_base=${AIDEV_DOWNLOAD_BASE:-https://github.com/rgb-vgx/aidev/releases}
  version=${AIDEV_VERSION:-}
  install_dir=${AIDEV_INSTALL_DIR:-$HOME/.local/bin}
  system=$(uname -s)
  case "$system" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) echo "install.sh: unsupported system $system: aidev runs on Linux and macOS" >&2; exit 1 ;;
  esac
  machine=$(uname -m)
  case "$machine" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "install.sh: unsupported architecture $machine: aidev runs on amd64 and arm64" >&2; exit 1 ;;
  esac
  asset="aidev_${os}_${arch}.tar.gz"
  if [ -z "$version" ]; then
    base_dir="$download_base/latest/download"
  else
    base_dir="$download_base/download/$version"
  fi
  asset_url="$base_dir/$asset"
  sums_url="$base_dir/SHA256SUMS"
  if command -v curl >/dev/null 2>&1; then
    downloader=curl
  elif command -v wget >/dev/null 2>&1; then
    downloader=wget
  else
    echo "install.sh: need curl or wget to download $asset_url" >&2
    exit 1
  fi
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT
  asset_file="$tmpdir/$asset"
  sums_file="$tmpdir/SHA256SUMS"
  download "$asset_url" "$asset_file"
  download "$sums_url" "$sums_file"
  expected=$(awk -v want="$asset" '$2 == want {print $1; exit}' "$sums_file")
  if [ -z "$expected" ]; then
    echo "install.sh: checksum entry missing for $asset" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "$asset_file" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "$asset_file" | awk '{print $1}')
  else
    echo "install.sh: need sha256sum or shasum to verify checksum for $asset" >&2
    exit 1
  fi
  if [ "$actual" != "$expected" ]; then
    echo "install.sh: checksum mismatch for $asset" >&2
    exit 1
  fi
  tar -xzf "$asset_file" -C "$tmpdir"
  mkdir -p "$install_dir"
  cp "$tmpdir/aidev" "$install_dir/.aidev.new.$$"
  chmod 755 "$install_dir/.aidev.new.$$"
  mv "$install_dir/.aidev.new.$$" "$install_dir/aidev"
  version_output=$("$install_dir/aidev" version)
  echo "Installed $version_output to $install_dir/aidev"
  case ":$PATH:" in
    *":$install_dir:"*) ;;
    *)
      echo "To use aidev, add the install directory to your PATH:"
      echo "export PATH=\"$install_dir:\$PATH\""
      echo "Add that line to ~/.bashrc or ~/.zshrc, then restart your shell."
      ;;
  esac
  if ! command -v opencode >/dev/null 2>&1; then
    echo "aidev needs OpenCode for its agent: install opencode from https://opencode.ai"
  else
    echo "aidev will run its agent with opencode at $(command -v opencode)."
  fi
  echo "Next, run:"
  echo "  aidev setup"
  echo "It starts PostgreSQL in Docker or uses --database-url, and writes the configuration."
  echo "For Claude Code:"
  echo "  claude plugin marketplace add rgb-vgx/aidev"
  echo "  claude plugin install aidev@aidev"
}
main "$@"
