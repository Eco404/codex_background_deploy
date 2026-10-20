#!/bin/bash
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
. scripts/go-env.sh
command -v go >/dev/null || { printf 'Install Go >=1.24 or place its toolchain in installer-data/cache/toolchain/go.\n' >&2; exit 1; }
go test ./internal/app ./cmd/codex-bg
mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags='-s -w' -o "dist/codex-bg-linux-$arch" ./cmd/codex-bg
done
(cd dist && sha256sum codex-bg-linux-* > SHA256SUMS)
case "$(uname -m)" in x86_64|amd64) build_host_arch=amd64;; aarch64|arm64) build_host_arch=arm64;; *) exit 1;; esac
cp "dist/codex-bg-linux-$build_host_arch" codex-bg
chmod 755 codex-bg
for arch in amd64 arm64; do
  stage="installer-data/tmp/package-$arch"
  mkdir -p "$stage/codex-background-deploy/dist"
  # Migration validates an installed v3 controller against this saved source.
  mkdir -p "$stage/codex-background-deploy/legacy/v3/payload"
  cp legacy/v3/payload/control.sh "$stage/codex-background-deploy/legacy/v3/payload/"
  cp install.sh README.md "$stage/codex-background-deploy/"
  cp "dist/codex-bg-linux-$arch" "$stage/codex-background-deploy/codex-bg"
  cp "dist/codex-bg-linux-$arch" dist/SHA256SUMS "$stage/codex-background-deploy/dist/"
  tar -czf "dist/codex-background-deploy-linux-$arch.tar.gz" -C "$stage" codex-background-deploy
  rm -rf -- "$stage"
done
