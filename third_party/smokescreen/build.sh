#!/usr/bin/env bash
# Builds the smokescreen egress proxy the fleet runs beside mirror-cache.
#
# The version comes from this directory's go.mod (tool directive), so a bump
# is one `go get -tool github.com/stripe/smokescreen@<rev>` away and the
# binary's `go version -m` output names the exact upstream commit. The build
# matches the mirror-cache goreleaser settings: static, cgo off, stripped.
#
# Usage: ./build.sh [GOOS GOARCH]   (default linux arm64)
# Output: dist/smokescreen-<GOOS>-<GOARCH>, sha256 printed on stdout.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

goos="${1:-linux}"
goarch="${2:-arm64}"
out="dist/smokescreen-${goos}-${goarch}"

mkdir -p dist
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
	go build -trimpath -ldflags='-s -w' -o "$out" github.com/stripe/smokescreen

shasum -a 256 "$out"
