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
if printf '%s\n' -- "${CGO_CPPFLAGS-}" "${CGO_CXXFLAGS-}" | xargs sh -c '
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
  printf '%s\n' 'ICU headers not found: brew install icu4c, set ICU_PREFIX=/path/to/installed/icu, or set CGO_CPPFLAGS=-I/path/to/icu/include' >&2
  exit 1
}

[ "$#" -gt 0 ] || missing_icu

# Lint loads tests by default and accepts repeated build-tag flags. Translate
# only its load options; issue filtering and formatter flags do not affect ICU.
if [ "$1" = --lint ]; then
  shift
  lint_tests=true
  tags=
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --) shift; break ;;
      --build-tags)
        shift
        [ "$#" -gt 0 ] || { echo 'check-icu: --build-tags needs a value' >&2; exit 2; }
        tags="${tags:+$tags,}$1" ;;
      --build-tags=*) tags="${tags:+$tags,}${1#*=}" ;;
      --tests=false) lint_tests=false ;;
      --tests|--tests=true) lint_tests=true ;;
    esac
    shift
  done
  [ "$#" -gt 0 ] || exit 0
  [ -z "$tags" ] || set -- "-tags=$tags" "$@"
  [ "$lint_tests" = false ] || set -- -test "$@"
fi

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
