#!/usr/bin/env bash
#
# test-push-gate-lock.sh — unit tests for the pre-push gate's cross-invocation
# concurrency bound (ga-owh20p) plus static assertions that it's wired into
# scripts/test-local-parallel correctly.
#
# The slot mechanics live in scripts/push-gate-lock-lib.sh and are exercised
# directly here — no real city, no real test suite run. Cross-process denial
# is tested deterministically with flock(1) probes, so there is no reliance
# on timing EXCEPT the one case that inherently requires wall-clock: the
# wait-then-timeout path, which uses second-scale overrides
# (PUSH_GATE_MAX_WAIT_SECONDS / PUSH_GATE_POLL_SECONDS) to stay fast.
#
# Coverage: acquire/hold/deny/release, the bounded wait and its return code,
# FD inheritance (a detached descendant must not pin a slot), dead-holder
# diagnostics, the missing-flock degrade path, malformed tunables, the
# GC_PUSH_GATE_NO_CAP escape hatch, both city-root resolution modes, the
# slots-dir fallback, and static assertions that scripts/test-local-parallel
# wires all of it up — including closing the gate FD before the fan-out.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="$TEST_DIR/push-gate-lock-lib.sh"
LOCAL_PARALLEL="$TEST_DIR/test-local-parallel"
WRAPPER="$TEST_DIR/with-push-gate-slot"

# shellcheck source=./push-gate-lock-lib.sh disable=SC1091
. "$LIB"

pass=0; fail=0
record_pass() { echo "  ok   $1"; pass=$((pass + 1)); }
record_fail() { echo "  FAIL $1 — $2"; fail=$((fail + 1)); }

assert_eq() {
    local name="$1" got="$2" want="$3"
    if [[ "$got" == "$want" ]]; then record_pass "$name"
    else record_fail "$name" "got '$got', want '$want'"; fi
}
assert_true()  { if "${@:2}"; then record_pass "$1"; else record_fail "$1" "expected true"; fi; }
assert_false() { if "${@:2}"; then record_fail "$1" "expected false"; else record_pass "$1"; fi; }
wait_for_match() {
    local file="$1" pattern="$2" attempt
    for ((attempt = 0; attempt < 100; attempt++)); do
        if grep -q "$pattern" "$file" 2>/dev/null; then return 0; fi
        sleep 0.05
    done
    return 1
}
assert_contains() {
    local name="$1" haystack="$2" needle="$3"
    if [[ "$haystack" == *"$needle"* ]]; then record_pass "$name"
    else record_fail "$name" "missing '$needle' in: $haystack"; fi
}

WORK="$(mktemp -d "${TMPDIR:-/var/tmp}/gc-gate-test.XXXXXX")"
WORK="$(cd "$WORK" && pwd -P)"
trap 'rm -rf "$WORK"' EXIT
SLOTS="$WORK/gate-slots"

# ---------------- acquire / hold (two slots) ----------------
export PUSH_GATE_MAX_CONCURRENT=2
FD0=""
if push_gate_acquire_slot "$SLOTS" FD0 "holder-A"; then
    record_pass "acquire.first_succeeds"
else
    record_fail "acquire.first_succeeds" "expected return 0"
fi
assert_true "acquire.fd_var_set" test -n "$FD0"

FD1=""
if push_gate_acquire_slot "$SLOTS" FD1 "holder-B"; then
    record_pass "acquire.second_succeeds_distinct_slot"
else
    record_fail "acquire.second_succeeds_distinct_slot" "expected return 0 (max=2)"
fi
assert_true "acquire.second_fd_differs" test "$FD0" != "$FD1"

HOLDERS="$(push_gate_describe_slots "$SLOTS" 2)"
assert_contains "describe.has_pid_a"   "$HOLDERS" "$$"
assert_contains "describe.has_label_a" "$HOLDERS" "holder-A"
assert_contains "describe.has_label_b" "$HOLDERS" "holder-B"

# ---------------- denial while both slots held (cross-process, deterministic) ----------------
if flock -n "$SLOTS/slot-0.lock" -c 'exit 0'; then
    record_fail "deny.flock_probe_slot0" "child acquired a slot we hold"
else
    record_pass "deny.flock_probe_slot0"
fi
if flock -n "$SLOTS/slot-1.lock" -c 'exit 0'; then
    record_fail "deny.flock_probe_slot1" "child acquired a slot we hold"
else
    record_pass "deny.flock_probe_slot1"
fi
CHILD_HELD="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=0 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z holder-C; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "deny.lib_from_child_zero_wait" "$CHILD_HELD" "DENIED"

# ---------------- all slots busy: waits, prints diagnostics, then times out with rc 1 ----------------
# The child exits with push_gate_acquire_slot's own status so CHILD_RC asserts
# the LIBRARY's return code, not the child shell's last echo.
START="$(date +%s)"
CHILD_OUT="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=2 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; push_gate_acquire_slot "$DIR" z holder-D; rc=$?; if [[ "$rc" -eq 0 ]]; then echo ACQUIRED; else echo "DENIED rc=$rc"; fi; exit "$rc"' 2>&1)"
CHILD_RC=$?
ELAPSED=$(( $(date +%s) - START ))
assert_contains "wait.busy_message_immediate" "$CHILD_OUT" "busy"
assert_contains "wait.reports_holder_a"        "$CHILD_OUT" "holder-A"
assert_contains "wait.timeout_message"         "$CHILD_OUT" "timed out"
assert_contains "wait.eventually_denied"       "$CHILD_OUT" "DENIED"
assert_true     "wait.elapsed_at_least_bound"  test "$ELAPSED" -ge 2
assert_eq       "wait.library_returns_1_on_timeout" "$CHILD_RC" "1"

# ---------------- release -> reacquire ----------------
push_gate_release_slot "$FD0"
if flock -n "$SLOTS/slot-0.lock" -c 'exit 0'; then
    record_pass "release.flock_probe_succeeds"
else
    record_fail "release.flock_probe_succeeds" "could not acquire slot-0 after release"
fi
CHILD_FREE="$(LIB="$LIB" DIR="$SLOTS" PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=1 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z holder-E; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "release.lib_from_child" "$CHILD_FREE" "ACQUIRED"
push_gate_release_slot "$FD1"

# The public wrapper must share the library's slots, preserve argv and command
# failures, and keep its lock FD out of the command's descendants.
WRAP_CITY="$WORK/wrapper-city"
mkdir -p "$WRAP_CITY/.gc"
WRAP_SLOTS="$WRAP_CITY/.gc/gate-slots"
LOCAL_TEST_LOG_DIR="$WORK/retained logs" push_gate_acquire_slot "$WRAP_SLOTS" FD0 "wrapper-holder-A"
push_gate_acquire_slot "$WRAP_SLOTS" FD1 "wrapper-holder-B"
CHILD_OUT="$(GC_CITY_PATH="$WRAP_CITY" PUSH_GATE_MAX_WAIT_SECONDS=1 PUSH_GATE_POLL_SECONDS=1 bash "$WRAPPER" -- touch "$WORK/ran-while-busy" 2>&1)"
CHILD_RC=$?
assert_eq "wrapper.contention_exits_75" "$CHILD_RC" "75"
assert_contains "wrapper.names_first_holder" "$CHILD_OUT" "wrapper-holder-A"
assert_contains "wrapper.names_second_holder" "$CHILD_OUT" "wrapper-holder-B"
assert_contains "wrapper.names_retained_logs" "$CHILD_OUT" "logs=$WORK/retained logs"
assert_false "wrapper.does_not_run_command_while_busy" test -e "$WORK/ran-while-busy"
push_gate_release_slot "$FD0"
push_gate_release_slot "$FD1"
CHILD_OUT="$(GC_CITY_PATH="$WRAP_CITY" bash "$WRAPPER" -- bash -c 'printf "<%s>\n" "$@"; exit 23' _ 'a b' '*' 2>&1)"
CHILD_RC=$?
assert_eq "wrapper.preserves_command_exit" "$CHILD_RC" "23"
assert_contains "wrapper.preserves_argv" "$CHILD_OUT" $'<a b>\n<*>'
assert_true "wrapper.releases_after_failure" flock -n "$WRAP_SLOTS/slot-0.lock" -c 'exit 0'
CHILD_OUT="$(GC_CITY_PATH="$WRAP_CITY" PUSH_GATE_MAX_CONCURRENT=1 bash "$WRAPPER" -- bash -c '
    if ( true <&200 ) 2>/dev/null; then echo FD_LEAKED; exit 1; fi
    if flock -n "$1/slot-0.lock" -c "exit 0"; then echo SLOT_NOT_HELD; exit 1; fi
    echo HELD_WITHOUT_INHERITED_FD
' _ "$WRAP_SLOTS" 2>&1)"
CHILD_RC=$?
assert_eq "wrapper.command_runs_under_slot" "$CHILD_RC" "0"
assert_contains "wrapper.severs_fd_inheritance" "$CHILD_OUT" "HELD_WITHOUT_INHERITED_FD"

push_gate_acquire_slot "$WRAP_SLOTS" FD0 "wrapper-wait-holder-A"
push_gate_acquire_slot "$WRAP_SLOTS" FD1 "wrapper-wait-holder-B"
GC_CITY_PATH="$WRAP_CITY" PUSH_GATE_MAX_WAIT_SECONDS=10 PUSH_GATE_POLL_SECONDS=1 \
    bash "$WRAPPER" -- printf '%s\n' AFTER_RELEASE >"$WORK/wrapper-wait.log" 2>&1 &
WAITER=$!
assert_true "wrapper.waits_before_release" wait_for_match "$WORK/wrapper-wait.log" 'all 2 slot(s) busy'
assert_false "wrapper.command_waits_for_release" grep -q AFTER_RELEASE "$WORK/wrapper-wait.log"
push_gate_release_slot "$FD0"
wait "$WAITER"
assert_eq "wrapper.succeeds_after_release" "$?" "0"
assert_contains "wrapper.reports_acquisition" "$(cat "$WORK/wrapper-wait.log")" 'acquired after wait'
assert_contains "wrapper.runs_after_release" "$(cat "$WORK/wrapper-wait.log")" AFTER_RELEASE
push_gate_release_slot "$FD1"

mkfifo "$WORK/command-block"
GC_CITY_PATH="$WRAP_CITY" bash "$WRAPPER" -- bash -c \
    'printf "%s\n" "$$" >"$1/command-pid"; read -r line <"$1/command-block"' _ "$WORK" \
    >"$WORK/wrapper-signal.log" 2>&1 &
SIGNALLED=$!
assert_true "wrapper.command_started_before_signal" wait_for_match "$WORK/command-pid" '^[0-9]'
kill -TERM "$SIGNALLED"
wait "$SIGNALLED" 2>/dev/null
assert_eq "wrapper.preserves_signal_status" "$?" "143"
assert_true "wrapper.signal_releases_slot" flock -n "$WRAP_SLOTS/slot-0.lock" -c 'exit 0'
if [[ -s "$WORK/command-pid" ]]; then
    assert_false "wrapper.signal_drains_command" kill -0 "$(cat "$WORK/command-pid")"
fi

# A command can itself supervise an isolated test group. Its drain must finish
# before the wrapper escalates against the supervisor. Six seconds here models
# a five-second shard drain plus scheduling overhead, with a TERM-ignoring test.
cat >"$WORK/nested-supervisor" <<'SH'
#!/usr/bin/env bash
source "$1"
on_term() {
    gc_harness_terminate_group "$gc_harness_supervised_pgid" 30
    trap - TERM
    kill -TERM $$
}
trap on_term TERM
trap 'gc_harness_terminate_group "$gc_harness_supervised_pgid" 30' EXIT
gc_harness_run_supervised nested "" 0 -- bash -c \
    'trap "" TERM; printf "%s\n" "$$" >"$1/nested-pid"; read -r line <"$1/command-block"' _ "$2"
SH
GC_CITY_PATH="$WRAP_CITY" bash "$WRAPPER" -- bash "$WORK/nested-supervisor" \
    "$TEST_DIR/lib/harness-reap.sh" "$WORK" >"$WORK/wrapper-nested.log" 2>&1 &
NESTED_WRAPPER=$!
assert_true "wrapper.nested_group_started" wait_for_match "$WORK/nested-pid" '^[0-9]'
kill -TERM "$NESTED_WRAPPER"
wait "$NESTED_WRAPPER" 2>/dev/null
assert_eq "wrapper.nested_signal_status" "$?" "143"
if [[ -s "$WORK/nested-pid" ]]; then
    NESTED_PID="$(cat "$WORK/nested-pid")"
    NESTED_STATE="$(ps -o stat= -p "$NESTED_PID" 2>/dev/null || true)"
    if [[ -z "$NESTED_STATE" || "$NESTED_STATE" == *Z* ]]; then
        record_pass "wrapper.nested_group_drained_before_release"
    else
        record_fail "wrapper.nested_group_drained_before_release" "test process $NESTED_PID survived in state $NESTED_STATE"
        # Only this fixture's isolated group; never leave the red repro running.
        kill -KILL -- -"$NESTED_PID" 2>/dev/null || true
    fi
fi
assert_true "wrapper.nested_signal_releases_slot" flock -n "$WRAP_SLOTS/slot-0.lock" -c 'exit 0'

# ---------------- FD inheritance: a live detached descendant must not pin a slot ----------------
# scripts/test-local-parallel closes the gate FD before the fan-out so jobs
# never inherit it (asserted statically below). This asserts the library half
# of that contract: releasing unlocks the open-file-description itself, so
# even a descendant that did inherit a copy — a daemonized tmux server, a
# dolt sql-server, an escaped `gc` — stops holding the slot the moment the
# gate releases it.
INHERIT_SLOTS="$WORK/inherit-slots"
FD_INH=""
if push_gate_acquire_slot "$INHERIT_SLOTS" FD_INH "holder-inherit"; then
    if command -v setsid >/dev/null 2>&1; then
        setsid bash -c 'sleep 3' &
    else
        bash -c 'sleep 3' &
    fi
    INHERIT_CHILD=$!
    push_gate_release_slot "$FD_INH"
    if flock -n "$INHERIT_SLOTS/slot-0.lock" -c 'exit 0'; then
        record_pass "inherit.detached_descendant_does_not_pin_slot"
    else
        record_fail "inherit.detached_descendant_does_not_pin_slot" \
            "slot still held after release while a detached descendant is alive"
    fi
    kill "$INHERIT_CHILD" 2>/dev/null || true
    wait "$INHERIT_CHILD" 2>/dev/null || true
else
    record_fail "inherit.detached_descendant_does_not_pin_slot" "could not acquire a slot to set up the case"
fi

# ---------------- an occupied descriptor must not consume a slot (ga-rjj9lm) ----------------
# The slot index used to be hardwired to a descriptor number (slot i always
# took FD PUSH_GATE_FD_BASE + i), and a descriptor already open in the CALLING
# process — inherited from any ancestor — made the acquire loop `continue` past
# that slot without ever opening or flocking its lock file. A FREE slot was
# counted as busy, silently: the caller burned its entire wait bound and then
# reported "all N slot(s) busy" while describe_slots printed nothing for the
# skipped slot, because that slot's lock file had never been created. That
# self-contradicting output is the fingerprint. In production it became a false
# FIX-IS-BAD push denial on gastownhall/gascity#5783 (47 passed / 4 failed
# against slots that were in fact free, after a 600s stall).
# Occupancy of a DESCRIPTOR must never be read as occupancy of a SLOT.
FDBASE_SLOTS="$WORK/fdbase-slots"
FDBASE_OUT="$(LIB="$LIB" DIR="$FDBASE_SLOTS" ANCHOR="$WORK/fdbase-anchor" \
    PUSH_GATE_MAX_CONCURRENT=2 PUSH_GATE_MAX_WAIT_SECONDS=0 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '
        . "$LIB"
        : >"$ANCHOR"
        # Occupy exactly the descriptor slot-0 would have claimed.
        eval "exec ${PUSH_GATE_FD_BASE}<>\"\$ANCHOR\"" || { echo SETUP_FAILED; exit 0; }
        a=""; b=""
        push_gate_acquire_slot "$DIR" a holder-A || { echo FIRST_DENIED; exit 0; }
        push_gate_acquire_slot "$DIR" b holder-B || { echo SECOND_DENIED; exit 0; }
        if [[ "$a" == "$b" ]]; then echo SAME_FD; else echo BOTH; fi
    ')"
assert_eq "fd_base.occupied_descriptor_does_not_consume_a_slot" "$FDBASE_OUT" "BOTH"

# ---------------- descriptor span exhausted: degrade best-effort, never misreport ----------------
# Scanning for a free descriptor introduces a third way to fail before any
# slot is even probed: every number in [PUSH_GATE_FD_BASE, +PUSH_GATE_FD_SPAN)
# already open in the calling shell. No amount of waiting fixes that, so it
# must degrade exactly as the missing-flock and unwritable-dir paths do —
# diagnostic, empty fd, return 0 — and must never surface as a timeout, which
# would send operators chasing contention that does not exist (ga-rjj9lm).
FDSPAN_OUT="$(LIB="$LIB" DIR="$WORK/fdspan-slots" ANCHOR="$WORK/fdspan-anchor" \
    PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=5 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '
        . "$LIB"
        : >"$ANCHOR"
        for (( n = PUSH_GATE_FD_BASE; n < PUSH_GATE_FD_BASE + PUSH_GATE_FD_SPAN; n++ )); do
            eval "exec $n<>\"\$ANCHOR\"" || { echo SETUP_FAILED; exit 0; }
        done
        z=preset
        push_gate_acquire_slot "$DIR" z holder-H
        echo "rc=$? fd=[$z]"
    ' 2>&1)"
assert_contains "fd_span.warns_no_free_descriptor" "$FDSPAN_OUT" "no free descriptor"
assert_contains "fd_span.returns_zero_empty_fd"    "$FDSPAN_OUT" "rc=0 fd=[]"
case "$FDSPAN_OUT" in
    *"timed out"*)
        record_fail "fd_span.never_reports_timeout" "found 'timed out' in output: $FDSPAN_OUT" ;;
    *)
        record_pass "fd_span.never_reports_timeout" ;;
esac

# ---------------- describe: a held slot whose recorded PID is gone is flagged ----------------
# Hold a slot for real, then overwrite its holder line with a PID that cannot
# exist — the exact shape a leaked descendant leaves behind: lock genuinely
# held, recorded holder long gone.
DEAD_SLOTS="$WORK/dead-slots"
FD_DEAD=""
if push_gate_acquire_slot "$DEAD_SLOTS" FD_DEAD "holder-dead"; then
    printf '%s %s %s %s\n' "999999999" "1970-01-01T00:00:00Z" "holder-dead" "somehost" >"$DEAD_SLOTS/slot-0.lock"
    DEAD_OUT="$(push_gate_describe_slots "$DEAD_SLOTS" 1)"
    assert_contains "describe.flags_dead_holder" "$DEAD_OUT" "holder pid dead"
    push_gate_release_slot "$FD_DEAD"
else
    record_fail "describe.flags_dead_holder" "could not acquire a slot to set up the case"
fi

# ---------------- missing flock(1): degrade best-effort, never block ----------------
# PATH is stripped of every directory so `command -v flock` fails; the library
# must warn and return 0 with an empty FD rather than burn the wait bound.
mkdir -p "$WORK/empty-bin"
# shellcheck disable=SC2016  # $? and $z are the child shell's, evaluated there
NOFLOCK_OUT="$(LIB="$LIB" DIR="$WORK/noflock-slots" PATH="$WORK/empty-bin" \
    PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=0 PUSH_GATE_POLL_SECONDS=1 \
    "$BASH" -c '. "$LIB"; z=preset; push_gate_acquire_slot "$DIR" z holder-F; echo "rc=$? fd=[$z]"' 2>&1)"
assert_contains "no_flock.warns_and_names_flock" "$NOFLOCK_OUT" "flock(1) not found"
assert_contains "no_flock.returns_zero_empty_fd" "$NOFLOCK_OUT" "rc=0 fd=[]"

# ---------------- mkdir failure: degrade best-effort, never misreport as timeout ----------------
# The original bug (ga-5enlx8): a linked worktree's .git is a FILE, so the
# slots-dir fallback resolved under it and mkdir -p could never succeed. The
# old code mapped that mkdir failure to the same `return 1` as a real
# wait-bound timeout, so operators chased fleet contention that did not
# exist. A blocked FILE (not a permission bit, so this holds even as root)
# stands in for that unwritable-parent case.
BLOCKED_PARENT="$WORK/blocked-parent"
: >"$BLOCKED_PARENT"
MKDIRFAIL_OUT="$(LIB="$LIB" DIR="$BLOCKED_PARENT/gate-slots" \
    PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=5 PUSH_GATE_POLL_SECONDS=1 \
    bash -c '. "$LIB"; z=preset; push_gate_acquire_slot "$DIR" z holder-G; echo "rc=$? fd=[$z]"' 2>&1)"
assert_contains "mkdir_fail.warns_cannot_create_slot_dir" "$MKDIRFAIL_OUT" "cannot create slot dir"
assert_contains "mkdir_fail.returns_zero_empty_fd"        "$MKDIRFAIL_OUT" "rc=0 fd=[]"
# The misreporting was the actual harm, so assert the absence of the timeout
# message directly rather than relying on rc=0 to imply it.
case "$MKDIRFAIL_OUT" in
    *"timed out"*)
        record_fail "mkdir_fail.never_reports_timeout" "found 'timed out' in output: $MKDIRFAIL_OUT" ;;
    *)
        record_pass "mkdir_fail.never_reports_timeout" ;;
esac

# ---------------- malformed tunables fall back to their documented defaults ----------------
# Each bad value must be rejected by name and replaced, never fed to
# arithmetic (`-1`, `abc`) or turned into a busy loop / zero-slot sweep (`0`).
for bad_case in "empty:" "zero:0" "negative:-1" "nonnumeric:abc"; do
    bad_name="${bad_case%%:*}"
    bad_val="${bad_case#*:}"
    TUNE_OUT="$(LIB="$LIB" DIR="$WORK/tunables-max-$bad_name" \
        PUSH_GATE_MAX_CONCURRENT="$bad_val" PUSH_GATE_MAX_WAIT_SECONDS=1 PUSH_GATE_POLL_SECONDS=1 \
        bash -c '. "$LIB"; push_gate_acquire_slot "$DIR" z holder-T; echo "rc=$?"' 2>&1)"
    assert_contains "tunables.max_concurrent_${bad_name}_still_acquires" "$TUNE_OUT" "rc=0"
    # An empty value is already covered by the `${VAR:-default}` expansion, so
    # only the values that actually reach validation emit the warning.
    if [[ -n "$bad_val" ]]; then
        assert_contains "tunables.max_concurrent_${bad_name}_warns_by_name" "$TUNE_OUT" "PUSH_GATE_MAX_CONCURRENT"
    fi
done
for bad_tunable in PUSH_GATE_MAX_WAIT_SECONDS PUSH_GATE_POLL_SECONDS; do
    TUNE_OUT="$(LIB="$LIB" DIR="$WORK/tunables-$bad_tunable" BAD="$bad_tunable" \
        bash -c 'export "$BAD=abc"; . "$LIB"; push_gate_acquire_slot "$DIR" z holder-T; echo "rc=$?"' 2>&1)"
    assert_contains "tunables.${bad_tunable}_warns_by_name" "$TUNE_OUT" "$bad_tunable"
    assert_contains "tunables.${bad_tunable}_still_acquires" "$TUNE_OUT" "rc=0"
done

# ---------------- escape hatch: GC_PUSH_GATE_NO_CAP bypasses the cap entirely ----------------
NOCAP_OUT="$(LIB="$LIB" DIR="$SLOTS" GC_PUSH_GATE_NO_CAP=1 PUSH_GATE_MAX_CONCURRENT=1 PUSH_GATE_MAX_WAIT_SECONDS=0 \
    bash -c '. "$LIB"; if push_gate_acquire_slot "$DIR" z; then echo ACQUIRED; else echo DENIED; fi')"
assert_eq "no_cap.bypasses_full_slots" "$NOCAP_OUT" "ACQUIRED"

# ---------------- city-root resolution: env short-circuit ----------------
CITY_ENV="$WORK/city-env"
mkdir -p "$CITY_ENV"
: >"$CITY_ENV/city.toml"
RESOLVED_ENV="$(GC_CITY_PATH="$CITY_ENV" GC_CITY="" GC_CITY_ROOT="" push_gate_city_root)"
assert_eq "city_root.env_short_circuit" "$RESOLVED_ENV" "$CITY_ENV"

# An env var pointing at a directory with neither city.toml nor .gc/ must NOT
# be trusted verbatim — it should fall through to walk-up instead of
# returning garbage. Bounded entirely inside $WORK (HOME=$WORK as the
# ceiling) so this can't accidentally pick up a real city.toml somewhere in
# the ambient filesystem's actual ancestry.
BOGUS="$WORK/not-a-city"
mkdir -p "$BOGUS/nested"
if RESOLVED_BOGUS="$(cd "$BOGUS/nested" && GC_CITY_PATH="$BOGUS" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK" push_gate_city_root)"; then
    assert_true "city_root.rejects_unvalidated_env" test "$RESOLVED_BOGUS" != "$BOGUS"
else
    record_pass "city_root.rejects_unvalidated_env"
fi

# ---------------- city-root resolution: walk-up discovery ----------------
CITY_WALK="$WORK/city-walk"
mkdir -p "$CITY_WALK/rigs/proj/sub"
: >"$CITY_WALK/city.toml"
RESOLVED_WALK="$(cd "$CITY_WALK/rigs/proj/sub" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_city_root)"
assert_eq "city_root.walk_up_finds_ancestor" "$RESOLVED_WALK" "$CITY_WALK"

# ---------------- slots dir: derives from city root, falls back to repo-relative ----------------
SLOTS_FROM_CITY="$(GC_CITY_PATH="$CITY_ENV" GC_CITY="" GC_CITY_ROOT="" push_gate_slots_dir)"
assert_eq "slots_dir.under_city_root" "$SLOTS_FROM_CITY" "$CITY_ENV/.gc/gate-slots"

NOCITY="$WORK/no-city-repo"
mkdir -p "$NOCITY"
(cd "$NOCITY" && git init -q .) 2>/dev/null || true
SLOTS_FALLBACK="$(cd "$NOCITY" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_slots_dir)"
assert_eq "slots_dir.falls_back_to_repo_relative" "$SLOTS_FALLBACK" "$NOCITY/.git/gate-slots"

LINKED_REPO="$WORK/linked-repo"
LINKED_A="$WORK/linked-a"
LINKED_B="$WORK/linked-b"
mkdir -p "$LINKED_REPO"
git -C "$LINKED_REPO" init -q
git -C "$LINKED_REPO" config user.name "Push Gate Test"
git -C "$LINKED_REPO" config user.email "push-gate-test@example.invalid"
: >"$LINKED_REPO/tracked"
git -C "$LINKED_REPO" add tracked
git -C "$LINKED_REPO" commit -qm "seed linked worktree fixture"
git -C "$LINKED_REPO" worktree add -q --detach "$LINKED_A"
git -C "$LINKED_REPO" worktree add -q --detach "$LINKED_B"

SLOTS_LINKED_A="$(cd "$LINKED_A" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_slots_dir)"
SLOTS_LINKED_B="$(cd "$LINKED_B" && GC_CITY_PATH="" GC_CITY="" GC_CITY_ROOT="" HOME="$WORK/unrelated-home" push_gate_slots_dir)"
LINKED_COMMON="$(cd "$LINKED_REPO/.git" && pwd -P)"
SLOTS_LINKED_A_N="$(cd "$(dirname "$SLOTS_LINKED_A")" && pwd -P)/gate-slots"
assert_eq "slots_dir.linked_worktree_uses_common_git_dir" "$SLOTS_LINKED_A_N" "$LINKED_COMMON/gate-slots"
assert_eq "slots_dir.linked_worktrees_share_slots" "$SLOTS_LINKED_B" "$SLOTS_LINKED_A"

FD_LINKED=""
if push_gate_acquire_slot "$SLOTS_LINKED_A" FD_LINKED "linked-holder"; then
    record_pass "slots_dir.linked_worktree_can_acquire"
    push_gate_release_slot "$FD_LINKED"
else
    record_fail "slots_dir.linked_worktree_can_acquire" "resolved slot directory is not usable: $SLOTS_LINKED_A"
fi

# ---------------- static wiring assertions against test-local-parallel ----------------
assert_true "wiring.sources_lib"   grep -q 'push-gate-lock-lib.sh' "$LOCAL_PARALLEL"
assert_true "wiring.calls_acquire" grep -q 'push_gate_acquire_slot' "$LOCAL_PARALLEL"
assert_true "wiring.has_override"  grep -q 'GC_PUSH_GATE_NO_CAP'   "$LOCAL_PARALLEL"

acq_line="$(grep -n 'push_gate_acquire_slot ' "$LOCAL_PARALLEL" | head -1 | cut -d: -f1)"
if [[ -n "$acq_line" ]]; then
    LOSER_BLOCK="$(sed -n "${acq_line},$((acq_line + 10))p" "$LOCAL_PARALLEL")"
    assert_contains "wiring.timeout_exits_75" "$LOSER_BLOCK" "exit 75"
else
    record_fail "wiring.timeout_exits_75" "no push_gate_acquire_slot call found in $LOCAL_PARALLEL"
fi
# Must be the release call wired to an EXIT trap specifically — not just any
# trap and any release call existing independently somewhere in the file
# (e.g. an unrelated per-job cleanup trap for a temp dir).
assert_true "wiring.releases_slot_on_exit" grep -qE 'trap .*push_gate_release_slot.*EXIT' "$LOCAL_PARALLEL"

# The gate FD must be closed BEFORE the job fan-out, or every job — and every
# daemon a job leaks — inherits a copy and can pin the slot past this
# invocation's death. Line ordering is the assertion: a close that lands after
# the fan-out is worthless.
close_line="$(grep -nE 'exec \$\{gate_fd\}>&-' "$LOCAL_PARALLEL" | head -1 | cut -d: -f1)"
fanout_line="$(grep -n 'xargs -0' "$LOCAL_PARALLEL" | head -1 | cut -d: -f1)"
if [[ -n "$close_line" && -n "$fanout_line" && "$close_line" -lt "$fanout_line" ]]; then
    record_pass "wiring.closes_gate_fd_before_fanout"
else
    record_fail "wiring.closes_gate_fd_before_fanout" \
        "close at line '${close_line:-none}', fan-out at line '${fanout_line:-none}'"
fi

echo
echo "push-gate-lock tests: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
