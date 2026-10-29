#!/usr/bin/env bash
# Build bd with the same beads source as this checkout's native library.
set -euo pipefail

if [[ $# != 1 ]]; then
	echo "usage: build-pinned-bd.sh OUTPUT_DIRECTORY" >&2
	exit 2
fi
repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
mkdir -p "$1"
bin_dir="$(cd -- "$1" && pwd)"
module=github.com/steveyegge/beads

# Resolve in this checkout, including replacements, and populate a cold cache.
# Disable ambient workspaces/flags so the library pin is the only source choice.
export GOWORK=off GOFLAGS=
go -C "$repo_root" mod download "$module"
replacement="$(go -C "$repo_root" list -m -f '{{if .Replace}}{{.Replace.Path}}@{{.Replace.Version}}{{end}}' "$module")"
if [[ -z "$replacement" ]]; then
	version="$(go -C "$repo_root" list -m -f '{{.Version}}' "$module")"
	GOBIN="$bin_dir" go install -tags gms_pure_go "$module/cmd/bd@$version"
	exit 0
fi

source_dir="$(go -C "$repo_root" list -m -f '{{.Dir}}' "$module")"
if [[ -z "$source_dir" || ! -d "$source_dir" ]]; then
	echo "build-pinned-bd: no unpacked source for $replacement" >&2
	exit 1
fi
# The fork still declares the upstream module path. Building its unpacked
# source avoids go install's path-mismatch error and uses cmd/bd's own full
# dependency graph. Read-only mode keeps the shared module cache immutable.
# A source build has module version (devel); retain its actual pin in Build.
echo "build-pinned-bd: building $replacement from $source_dir"
go -C "$source_dir" build -mod=readonly -buildvcs=false -tags gms_pure_go \
	-ldflags "-X main.Build=$replacement" -o "$bin_dir/bd" ./cmd/bd
