#!/bin/bash
# Source from the project root; all build artifacts stay beneath installer-data.
CODEX_BG_BUILD_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
export GOPATH="$CODEX_BG_BUILD_ROOT/installer-data/cache/gopath"
export GOMODCACHE="$CODEX_BG_BUILD_ROOT/installer-data/cache/gomod"
export GOCACHE="$CODEX_BG_BUILD_ROOT/installer-data/cache/gobuild"
export GOTMPDIR="$CODEX_BG_BUILD_ROOT/installer-data/tmp"
export TMPDIR="$GOTMPDIR"
export GOTOOLCHAIN=local
export PATH="$CODEX_BG_BUILD_ROOT/installer-data/cache/toolchain/go/bin:$PATH"
mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOTMPDIR"
