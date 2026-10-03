#!/usr/bin/env bash
# Exercise the real runner and Makefile environment boundary with one cheap
# probe job in place of the Go suite. The worker shell and log I/O stay real.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch="$(mktemp -d "${TMPDIR:-/var/tmp}/gc-local-env-test.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/bin" "$scratch/user-tmp" "$scratch/caller tmp"

cat >"$scratch/bin/xargs" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
# Consume the original jobs, then exercise their unchanged worker with one
# command that exposes only the environment values this test owns.
cat >/dev/null
shift 4 # -0 -n1 -P <jobs>
printf 'probe_log_dir=%s\n' "$LOCAL_TEST_LOG_DIR"
"$@" 'environment::printf "probe_tmpdir=%s\nprobe_goflags=%s\n" "$TMPDIR" "$GOFLAGS"'
cat "$LOCAL_TEST_LOG_DIR/environment.log"
SH
cat >"$scratch/bin/uname" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$GC_ENV_TEST_OS"
SH
cat >"$scratch/bin/getconf" <<'SH'
#!/usr/bin/env bash
[[ "$1" == DARWIN_USER_TEMP_DIR ]]
printf '%s\n' "$GC_ENV_TEST_USER_TMP"
exit "${GC_ENV_TEST_GETCONF_STATUS:-0}"
SH
chmod +x "$scratch/bin/"*

export PATH="$scratch/bin:$PATH"
export GC_ENV_TEST_OS=Darwin GC_ENV_TEST_USER_TMP="$scratch/user-tmp/"
export LOCAL_TEST_JOBS=1 CMD_GC_PROCESS_TOTAL=1 GC_PUSH_GATE_NO_CAP=1 GC_TEST_NO_SLICE=1
unset TMPDIR LOCAL_TEST_LOG_DIR GC_TEST_INNER_P
cd "$repo_root"

assert_contains() {
  if [[ "$output" != *"$1"* ]]; then
    printf 'missing %s in:\n%s\n' "$1" "$output" >&2
    exit 1
  fi
}

output="$(LOCAL_TEST_LOG_DIR="$scratch/fresh/nested logs" bash scripts/test-local-parallel fast 2>&1)" || {
  printf '%s\n' "$output" >&2
  exit 1
}
assert_contains "probe_tmpdir=$scratch/user-tmp"
test -f "$scratch/fresh/nested logs/environment.log"
echo 'ok: creates and preserves caller log directory; Darwin per-user TMPDIR'

output="$(TMPDIR="$scratch/caller tmp" bash scripts/test-local-parallel fast 2>&1)"
assert_contains "probe_tmpdir=$scratch/caller tmp"
log_dir="$(printf '%s\n' "$output" | sed -n 's/^probe_log_dir=//p')"
[[ "$log_dir" == "$scratch/caller tmp/"* ]]
test ! -e "$log_dir"
echo 'ok: caller TMPDIR wins; default logs use it and are removed on success'

output="$(bash scripts/test-local-parallel fast 2>&1)"
assert_contains "probe_tmpdir=$scratch/user-tmp"
log_dir="$(printf '%s\n' "$output" | sed -n 's/^probe_log_dir=//p')"
[[ "$log_dir" == "$scratch/user-tmp/"* ]]
test ! -e "$log_dir"
echo 'ok: Darwin default logs and workers share the per-user TMPDIR'

output="$(GC_ENV_TEST_OS=Linux bash scripts/test-local-parallel fast 2>&1)"
assert_contains 'probe_tmpdir=/var/tmp'
echo 'ok: non-Darwin default remains /var/tmp'

for getconf_status in 0 1; do
  output="$(GC_ENV_TEST_USER_TMP='' GC_ENV_TEST_GETCONF_STATUS="$getconf_status" bash scripts/test-local-parallel fast 2>&1)"
  fallback_tmp="$(printf '%s\n' "$output" | sed -n 's/^probe_tmpdir=//p')"
  [[ "$fallback_tmp" == /var/tmp/gc-local-tmp.* ]]
  log_dir="$(printf '%s\n' "$output" | sed -n 's/^probe_log_dir=//p')"
  [[ "$log_dir" == "$fallback_tmp/"* ]]
  if [[ -e "$fallback_tmp" ]]; then
    rmdir "$fallback_tmp"
    echo 'runner left its fallback scratch directory behind' >&2
    exit 1
  fi
  echo "ok: missing Darwin user temp (getconf exit $getconf_status) uses one scratch root and cleans it"
done

touch "$scratch/not-a-directory"
if output="$(LOCAL_TEST_LOG_DIR="$scratch/not-a-directory/logs" bash scripts/test-local-parallel fast 2>&1)"; then
  echo 'uncreatable log directory unexpectedly succeeded' >&2
  exit 1
fi
assert_contains 'LOCAL_TEST_LOG_DIR'
[[ "$output" != *'Running '* ]]
echo 'ok: uncreatable caller log directory fails before fan-out'

# Make scrubs the fixture knobs, so use the real platform tools for this
# boundary. The xargs probe still comes from PATH and avoids running the suite.
rm -f "$scratch/bin/uname" "$scratch/bin/getconf"
expected_tmp=/var/tmp
if [[ "$(uname -s)" == Darwin ]]; then
  expected_tmp="$(getconf DARWIN_USER_TEMP_DIR 2>/dev/null || true)"
  expected_tmp="${expected_tmp%/}"
fi
output="$(GC_TEST_INNER_P=4 LOCAL_TEST_LOG_DIR="$scratch/make logs" make --no-print-directory test-fast-parallel 2>&1)"
assert_contains 'inner_p=4'
if [[ -n "$expected_tmp" ]]; then
  assert_contains "probe_tmpdir=$expected_tmp"
else
  assert_contains 'probe_tmpdir=/var/tmp/gc-local-tmp.'
fi
assert_contains 'probe_goflags='
assert_contains '-p=4'
test -f "$scratch/make logs/environment.log"
echo 'ok: Make preserves inner parallelism, caller logs and unset TMPDIR'

output="$(TMPDIR="$scratch/caller tmp" LOCAL_TEST_LOG_DIR="$scratch/make caller logs" make --no-print-directory test-fast-parallel 2>&1)"
assert_contains "probe_tmpdir=$scratch/caller tmp"
test -f "$scratch/make caller logs/environment.log"
echo 'ok: Make preserves caller TMPDIR'

echo 'local-environment tests: 9 passed'
