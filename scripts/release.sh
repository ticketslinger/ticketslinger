#!/usr/bin/env bash
# Builds the release tarball for one tag: scripts/release.sh <tag> <outdir>.
# The archive's root is the release layout itself (./bin, ./storefront, ./dashboard,
# ./VERSION), so it unpacks straight into releases/<tag>/ on the server.
set -euo pipefail

tag=${1:?usage: scripts/release.sh <tag> <outdir>}
mkdir -p "${2:?usage: scripts/release.sh <tag> <outdir>}"
out=$(cd "$2" && pwd)
root=$(cd "$(dirname "$0")/.." && pwd)
name="ticketslinger-$tag-linux-amd64"
dir="$out/$name"
rm -rf "$dir" "$out/$name.tar.gz"
mkdir -p "$dir/bin"

(cd "$root/api" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=$tag" -o "$dir/bin/ticketslinger" ./cmd/ticketslinger)

(cd "$root/web/storefront" && bun build.ts "$dir/storefront/dist")
bun build "$root/web/storefront/server/index.ts" --target=bun --minify --outfile "$dir/storefront/server.js"

(cd "$root/web/dashboard" && bunx vite build --outDir "$dir/dashboard/dist" --emptyOutDir)
bun build "$root/web/dashboard/server/index.ts" --target=bun --minify --outfile "$dir/dashboard/server.js"

echo "$tag" >"$dir/VERSION"
# Owned by root and stamped with the commit time (GNU tar), so the archive carries
# nothing of the machine that built it.
tar --owner=0 --group=0 --numeric-owner --sort=name --mtime="@$(git -C "$root" log -1 --format=%ct)" \
  -czf "$out/$name.tar.gz" -C "$dir" .
(cd "$out" && sha256sum "$name.tar.gz" >SHA256SUMS)
