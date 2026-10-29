#!/bin/sh
# Print an installed ICU prefix. Homebrew metadata alone is not proof that
# headers exist; an explicit ICU_PREFIX is handled by the calling Makefile.
set -eu

if command -v brew >/dev/null 2>&1; then
  formulas=$(brew list --formula 2>/dev/null | sed -n '/^icu4c@[0-9][0-9]*$/p' | sort -t@ -k2,2nr)
  for formula in $formulas icu4c; do
    prefix=$(brew --prefix "$formula" 2>/dev/null) || continue
    if [ -n "$prefix" ] && [ -f "$prefix/include/unicode/regex.h" ]; then
      printf '%s\n' "$prefix"
      exit 0
    fi
  done
fi

if command -v pkg-config >/dev/null 2>&1; then
  prefix=$(pkg-config --variable=prefix icu-uc 2>/dev/null) || prefix=
  if [ -n "$prefix" ] && [ -f "$prefix/include/unicode/regex.h" ]; then
    printf '%s\n' "$prefix"
    exit 0
  fi
fi

# The Makefile emits the remedy once and stops before invoking the compiler.
exit 1
