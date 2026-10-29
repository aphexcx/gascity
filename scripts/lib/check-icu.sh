#!/bin/sh
# Run in the build's environment, with its go list flags and packages.
# Package discovery does not compile CGO, so missing headers cannot hide which
# selected dependencies need ICU. No arguments requests an explicit ICU probe.
set -eu

cgo_enabled=$(go env CGO_ENABLED)
[ "$cgo_enabled" != 0 ] || exit 0

# A caller can supply ICU without a discoverable installation prefix. xargs
# tokenizes quoted flag values without evaluating shell code in those values.
# The sentinel also makes BSD xargs run the probe when the flags are empty.
# shellcheck disable=SC2016 # Variables belong to the child shell.
if printf '%s\n' -- "${CGO_CFLAGS-}" "${CGO_CPPFLAGS-}" | xargs sh -c '
  next_include=false
  for flag do
    if "$next_include"; then
      include=$flag
      next_include=false
    else
      case "$flag" in
        -I|-isystem) next_include=true; continue ;;
        -I*) include=${flag#-I} ;;
        *) continue ;;
      esac
    fi
    [ ! -f "$include/unicode/regex.h" ] || exit 0
  done
  exit 1
' check-cgo-include; then
  exit 0
fi

missing_icu() {
  printf '%s\n' 'ICU headers not found: brew install icu4c or set ICU_PREFIX=/path/to/installed/icu' >&2
  exit 1
}

[ "$#" -gt 0 ] || missing_icu

# shellcheck disable=SC2016 # $dir is a Go template variable.
sources=$(go list -deps -f '{{if .CgoFiles}}{{$dir := .Dir}}{{range .CgoFiles}}{{$dir}}/{{.}}{{"\n"}}{{end}}{{range .CFiles}}{{$dir}}/{{.}}{{"\n"}}{{end}}{{range .CXXFiles}}{{$dir}}/{{.}}{{"\n"}}{{end}}{{range .HFiles}}{{$dir}}/{{.}}{{"\n"}}{{end}}{{end}}' "$@")
printf '%s\n' "$sources" | while IFS= read -r source; do
  [ -n "$source" ] || continue
  if grep -Eq '#[[:space:]]*include[[:space:]]*[<"]unicode/(u?regex)\.h[>"]' "$source"; then
    missing_icu
  else
    status=$?
    [ "$status" -eq 1 ] || exit "$status"
  fi
done
