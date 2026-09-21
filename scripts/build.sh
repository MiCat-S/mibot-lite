#!/bin/bash
# Build the mibot-lite binary with its version baked in.
#
# The version is injected at link time; building with a bare `go build`
# leaves it empty, and then .version reports "dev" and .update cannot tell
# whether it is already on the release it just fetched.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
out=${1:-$root/mibot-lite}
version=${MIBOT_VERSION:-$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)}

cd "$root"
CGO_ENABLED=${CGO_ENABLED:-0} go build -trimpath \
  -ldflags "-s -w -X main.version=$version" \
  -o "$out" ./cmd/mibot-lite
printf 'built %s (version %s, %s/%s)\n' "$out" "$version" "$(go env GOOS)" "$(go env GOARCH)"
