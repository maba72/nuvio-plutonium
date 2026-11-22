#!/usr/bin/env bash
# Build Plutonium for the common desktop/server targets.
set -euo pipefail

cd "$(dirname "$0")/.."

version="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
out="${OUT:-dist}"
mkdir -p "$out"

targets=(
  "linux amd64"
  "linux arm64"
  "linux arm"
  "darwin amd64"
  "darwin arm64"
  "windows amd64"
  "windows arm64"
)

for target in "${targets[@]}"; do
  read -r goos goarch <<<"$target"
  name="plutonium-${goos}-${goarch}"
  if [ "$goos" = "windows" ]; then
    name="${name}.exe"
  fi
  echo "building $name"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.version=$version" \
    -o "$out/$name" ./cmd/plutonium
done

echo "wrote binaries to $out"
