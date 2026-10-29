#!/bin/sh
# Run in the build's environment, with its go list flags and packages.
# Package discovery does not compile CGO, so missing headers cannot hide which
# selected dependencies need ICU. No arguments requests an explicit ICU probe.
set -eu

cgo_enabled=$(go env CGO_ENABLED)
[ "$cgo_enabled" != 0 ] || exit 0

# Ask Go for the effective flags: exported values override persisted GOENV
# settings, exactly as they do for the build that follows this check.
effective_flags=$(go env CGO_CPPFLAGS CGO_CFLAGS CGO_CXXFLAGS CGO_LDFLAGS)

# A caller can supply ICU without a discoverable installation prefix. xargs
# tokenizes quoted flag values without evaluating shell code in those values.
# The sentinel also makes BSD xargs run the probe when the flags are empty.
header_dirs() {
  # shellcheck disable=SC2016 # Variables belong to the child shell.
  printf '%s\n' -- "$1" | xargs sh -c '
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
    if [ -f "$include/unicode/regex.h" ]; then
      include=$(CDPATH= cd -P -- "$include" && pwd -P) || exit 1
      printf "%s\n" "$include"
    fi
  done
' check-cgo-include
}

cpp_dirs=$(header_dirs "$(printf '%s\n' "$effective_flags" | sed -n '1p')")
[ -z "$cpp_dirs" ] || exit 0

# The C preamble and C++ source must use the same ICU headers. CPPFLAGS
# reaches both compiles; otherwise require a directory in both flag sets.
c_dirs=$(header_dirs "$(printf '%s\n' "$effective_flags" | sed -n '2p')")
cxx_dirs=$(header_dirs "$(printf '%s\n' "$effective_flags" | sed -n '3p')")
if printf '%s\n' "$c_dirs" | (
  while IFS= read -r include; do
    [ -n "$include" ] || continue
    if printf '%s\n' "$cxx_dirs" | grep -F -x -e "$include" >/dev/null; then
      exit 0
    fi
  done
  exit 1
); then
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
