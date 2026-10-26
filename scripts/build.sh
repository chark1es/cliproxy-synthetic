#!/usr/bin/env bash
# Build synthetic.so for the host architecture inside a Debian 12 (bookworm)
# Go container, so the library links against the same glibc generation as the
# official CLIProxyAPI image.
#
# usage: scripts/build.sh [version] [output-dir]
set -euo pipefail

version="${1:-dev}"
out="${2:-dist}"
image="${GO_IMAGE:-golang:1.26-bookworm}"

mkdir -p "$out"
docker run --rm \
  --user "$(id -u):$(id -g)" \
  -v "$PWD":/src -w /src \
  -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomod \
  -e CGO_ENABLED=1 -e GOFLAGS=-buildvcs=false \
  "$image" \
  go build -trimpath -buildmode=c-shared \
    -ldflags "-s -w -X main.version=${version}" \
    -o "$out/synthetic.so" .
rm -f "$out/synthetic.h"
echo "built $out/synthetic.so (version ${version})"
