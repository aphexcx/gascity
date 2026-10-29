#!/bin/sh
# Source this file, then call gc_go_toolchain with the module directory.
# Resolve before carrying GOROOT through an env -i boundary: an inherited root
# may belong to a different driver than the one currently on PATH.
gc_go_toolchain() {
  gc_go_version=$(awk '$1 == "go" { print $2; exit }' "$1/go.mod") || return
  if [ -z "$gc_go_version" ]; then
    echo "go-toolchain: missing go directive in $1/go.mod" >&2
    return 1
  fi
  case "${GOTOOLCHAIN:-auto}" in
    auto) GOTOOLCHAIN="go$gc_go_version" ;;
  esac
  export GOTOOLCHAIN
  gc_go_root=$(
    unset GOROOT
    go env GOROOT
  ) || return
  if [ ! -x "$gc_go_root/bin/go" ]; then
    echo "go-toolchain: resolved GOROOT has no executable bin/go: $gc_go_root" >&2
    return 1
  fi
  # The host go may be a shim that keeps compiler scratch and cache off tmpfs.
  # Capture its defaults before PATH selects the real binary; caller values win.
  if [ "${GOTMPDIR+x}" != x ]; then
    gc_go_tmpdir=$(unset GOROOT; go env GOTMPDIR) || return
    if [ -n "$gc_go_tmpdir" ]; then
      export GOTMPDIR="$gc_go_tmpdir"
    fi
  fi
  if [ "${GOCACHE+x}" != x ]; then
    gc_go_cache=$(unset GOROOT; go env GOCACHE) || return
    if [ -n "$gc_go_cache" ]; then
      export GOCACHE="$gc_go_cache"
    fi
  fi
  GOROOT="$gc_go_root"
  case "$PATH" in
    "$GOROOT/bin"|"$GOROOT/bin":*) ;;
    *) PATH="$GOROOT/bin:$PATH" ;;
  esac
  export GOROOT PATH
}
