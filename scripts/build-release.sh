#!/bin/sh
# Build the release archives: one static aidev binary per platform in
# dist/ plus a SHA256SUMS file. .github/workflows/release.yml runs it on a v* tag.
set -eu

if [ -z "${VERSION:-}" ]; then
	echo "build-release.sh: VERSION is required, for example VERSION=v0.1.0" >&2
	exit 1
fi

DIST=${DIST:-dist}

rm -rf "$DIST"
mkdir -p "$DIST"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for plat in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os=${plat%/*}
	arch=${plat#*/}
	mkdir -p "$tmp/${os}_${arch}"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$tmp/${os}_${arch}/aidev" ./cmd/aidev
	tar -C "$tmp/${os}_${arch}" -czf "$DIST/aidev_${os}_${arch}.tar.gz" aidev
	echo "$DIST/aidev_${os}_${arch}.tar.gz"
done

(
	cd "$DIST"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum aidev_linux_amd64.tar.gz aidev_linux_arm64.tar.gz aidev_darwin_amd64.tar.gz aidev_darwin_arm64.tar.gz > SHA256SUMS
	else
		shasum -a 256 aidev_linux_amd64.tar.gz aidev_linux_arm64.tar.gz aidev_darwin_amd64.tar.gz aidev_darwin_arm64.tar.gz > SHA256SUMS
	fi
)
