#!/bin/sh
# gc dolt pull — Pull Dolt databases from their configured remotes.
#
# Uses the live Dolt SQL server when reachable so pull does not contend with
# active databases. Falls back to CLI mode only when no server is running.
# Pulls the configured remote's `main` branch in both SQL and CLI modes.
#
# Conflicts: a bd store shared between cities (pushed and pulled through a
# hub) has the same automatic sweep run by every city — bd's defer wake flips
# an expired deferred `issues` row to open on every store that reads the
# ready front, and each store writes its own fresh row_lock and updated_at.
# The change is the same on both sides; only those two columns differ, and
# dolt reports the row as a conflict on the next pull (hw-ynz1w, 2026-09-10).
# The pull resolves exactly that class and nothing else: a conflicted
# `issues` row whose EVERY other column is equal on both sides takes the
# remote's row_lock and updated_at (so the other city's next pull of this
# merge is a fast-forward), inside one transaction dolt refuses to commit
# while any conflict remains. A row that differs in any other column, a
# conflict in any other table, or a schema change under the merge leaves the
# store exactly as it was and the pull fails with the rows listed for manual
# resolution. See resolve_benign_conflicts.
#
# Environment: GC_CITY_PATH, GC_DOLT_PORT, GC_DOLT_USER, GC_DOLT_PASSWORD,
#   GC_DOLT_PULL_TIMEOUT_SECS (default: 120) — wall-clock bound for the
#   SQL-mode DOLT_PULL; increase for a slow link or a large first pull.
#
# One server-side pull per database at a time (gp-f2yq): a CALL DOLT_PULL
# runs inside the sql-server (it fetches first) and outlives a client the
# bound killed, so before issuing one the script asks the server whether a
# DOLT_PULL / DOLT_FETCH is already in flight on the server (skipped when
# one is), and when the bound expires it KILLs the server-side session the
# pull printed about itself and proves it gone from the processlist. The pull
# statement also takes the server's session lock for the database (GET_LOCK,
# timeout 0) in the same batch, so two runners that both read "nothing in
# flight" cannot both pull. See the "Server-side remote operations" helpers
# in assets/scripts/runtime.sh.
set -e

: "${GC_DOLT_USER:=root}"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}"
. "$PACK_DIR/assets/scripts/runtime.sh"
# This run's lock nonce (see runtime.sh, "Server-side single-flight gate"):
# one random value per script run, computed here and not at source time; no
# random bytes = no run (before any database is touched).
REMOTE_OP_RUN_NONCE=$(remote_op_new_run_nonce) || {
  echo "gc dolt pull: no run nonce — refusing to run (see the line above)" >&2
  exit 2
}

db_filter=""
data_dir="$DOLT_DATA_DIR"

while [ $# -gt 0 ]; do
  case "$1" in
    --db) db_filter="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: gc dolt pull [--db NAME]"
      echo ""
      echo "Pull Dolt databases from their configured remotes."
      echo ""
      echo "Flags:"
      echo "  --db NAME   Pull only the named database"
      echo ""
      echo "Conflicts:"
      echo "  A conflicted bd issues row that differs from the remote ONLY in"
      echo "  row_lock and updated_at (the same automatic change written by two"
      echo "  cities, e.g. bd's defer wake) takes the remote's values and the pull"
      echo "  completes. Any other conflict leaves the database exactly as it was,"
      echo "  prints the conflicted rows, and fails the pull for manual resolution."
      echo ""
      echo "Environment:"
      echo "  GC_DOLT_PULL_TIMEOUT_SECS  SQL-mode pull bound (default 120)"
      exit 0
      ;;
    *) echo "gc dolt pull: unknown flag: $1" >&2; exit 1 ;;
  esac
done

case "$(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | tr '[:upper:]' '[:lower:]')" in
  information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe)
  echo "gc dolt pull: reserved Dolt database name: $(printf '%s' "$db_filter" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//') (used internally by Dolt or gc)" >&2
  exit 1
  ;;
esac

# Wall-clock bound for the SQL-mode DOLT_PULL (seconds). Defaults to 120s (the
# prior fixed ceiling). Validated the way the sync bounds are: an empty /
# non-numeric / all-zero value is rejected before any database is touched —
# GNU `timeout 0` disables the timeout, i.e. an unbounded pull, the exact
# anti-hang outcome this bound exists to prevent.
pull_timeout="${GC_DOLT_PULL_TIMEOUT_SECS-120}"
case "$pull_timeout" in
  ''|*[!0-9]*) pull_timeout_valid=false ;;
  *[1-9]*)     pull_timeout_valid=true ;;
  *)           pull_timeout_valid=false ;;
esac
if [ "$pull_timeout_valid" != true ]; then
  printf 'gc dolt pull: invalid GC_DOLT_PULL_TIMEOUT_SECS=%s (must be a positive integer)\n' \
    "$pull_timeout" >&2
  exit 2
fi
# Canonical decimal: leading zeros dropped (validated non-zero, so never empty)
# so the value prints and compares as the integer it is.
pull_timeout=$(printf '%s' "$pull_timeout" | sed 's/^0*//')

is_running() {
  managed_runtime_tcp_reachable "$GC_DOLT_PORT"
}

valid_database_name() {
  case "$1" in
    [A-Za-z0-9_]*)
      case "$1" in *[!A-Za-z0-9_-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

valid_remote_name() {
  case "$1" in
    [A-Za-z0-9_.-]*)
      case "$1" in *[!A-Za-z0-9_.-]*) return 1 ;; *) return 0 ;; esac
      ;;
    *) return 1 ;;
  esac
}

# dolt_sql QUERY [TIMEOUT_SECS] [USE_DB] — run a SQL query against the live
# server under a wall-clock bound (dolt_sql_csv, runtime.sh); 120s by default,
# sized for metadata queries. The pull passes its own bound and its database
# (--use-db, so the server attributes the session in its processlist).
dolt_sql() {
  dolt_sql_csv "${2:-120}" "${3:-}" "$1"
}

# --- Conflict resolution -----------------------------------------------------
#
# run_db_sql NAME DIR QUERY — run QUERY against database NAME (SQL mode, on
# the live server) or the database checkout at DIR (CLI mode), CSV on stdout.
run_db_sql() {
  if [ "$server_running" = true ]; then
    dolt_sql "USE \`$1\`; $3"
  else
    (cd "$2" && run_bounded 120 dolt sql --result-format csv -q "$3")
  fi
}

# valid_column_name — a bd column name is a plain identifier; anything else
# is never spliced into SQL.
valid_column_name() {
  case "$1" in
    ''|*[!A-Za-z0-9_]*) return 1 ;;
    *) return 0 ;;
  esac
}

# benign_conflict_sql NAME DIR — set CONFLICT_PREDICATE and CONFLICT_DIFFERING
# from the `issues` schema of the database. Returns 1 (and sets neither) when
# the schema cannot be read, has a column name that is not a plain
# identifier, or is not a bd issues table (no row_lock or no updated_at):
# then nothing is ever auto-resolved for this database.
#
# CONFLICT_PREDICATE selects a row of dolt_conflicts_issues that is a benign
# conflict: both sides modified the row, every column other than row_lock
# and updated_at is equal on both sides — compared as bytes (BINARY), so a
# case-insensitive collation cannot make "Fix API" and "fix api" equal, and
# NULL-safe (<=>) — and the `issues` schema the merge produced is still the
# schema this predicate was built from (a schema change under the merge
# would have added a column the predicate does not compare, so the
# fingerprint makes the predicate match nothing and the transaction fails
# closed).
#
# CONFLICT_DIFFERING is the comma-separated list of columns that differ on a
# conflicted row, for the report.
benign_conflict_sql() {
  CONFLICT_PREDICATE=""
  CONFLICT_DIFFERING=""
  cols_csv=$(run_db_sql "$1" "$2" "SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issues' ORDER BY ordinal_position" 2>/dev/null) || return 1
  cols=$(printf '%s\n' "$cols_csv" | awk 'NR > 1 && $0 != "" {print}' | tr -d '\r"')
  [ -n "$cols" ] || return 1
  fingerprint=""
  predicate="our_diff_type = 'modified' AND their_diff_type = 'modified'"
  differing=""
  has_row_lock=false
  has_updated_at=false
  for col in $cols; do
    valid_column_name "$col" || return 1
    fingerprint="${fingerprint:+$fingerprint,}$col"
    differing="${differing:+$differing, }IF(BINARY \`our_$col\` <=> BINARY \`their_$col\`, NULL, '$col')"
    case "$col" in
      row_lock) has_row_lock=true; continue ;;
      updated_at) has_updated_at=true; continue ;;
    esac
    predicate="$predicate AND BINARY \`our_$col\` <=> BINARY \`their_$col\`"
  done
  [ "$has_row_lock" = true ] && [ "$has_updated_at" = true ] || return 1
  # The schema guard: the merged table must still have exactly the columns
  # the predicate compared. GROUP_CONCAT obeys group_concat_max_len and
  # truncates silently, so the count travels beside the names (a column
  # appended past a truncation point changes the count) and every session
  # that evaluates the predicate raises the limit (see session_prelude).
  ncols=$(printf '%s\n' "$cols" | grep -c '.')
  predicate="$predicate AND (SELECT CONCAT(COUNT(*), ':', GROUP_CONCAT(column_name ORDER BY ordinal_position)) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'issues') = '$ncols:$fingerprint'"
  CONFLICT_PREDICATE="$predicate"
  CONFLICT_DIFFERING="CONCAT_WS(',', $differing)"
  return 0
}

# abort_cli_merge NAME DIR — CLI mode holds a refused merge in the working
# set on disk; put the database back at its pre-pull head. SQL mode never
# wrote anything to abort. An abort that fails is reported, not hidden.
abort_cli_merge() {
  [ "$server_running" = true ] && return 0
  if ! (cd "$2" && dolt merge --abort >/dev/null 2>&1); then
    echo "  $1: WARNING: dolt merge --abort failed; the conflicted merge is still in the working set (dolt conflicts cat issues)" >&2
  fi
}

# resolve_benign_conflicts NAME DIR REMOTE URL — the conflicted pull, retried
# inside one transaction that resolves the benign rows and commits only when
# no conflict is left. Prints the pull result line and returns 0 when the
# merge landed; otherwise prints the conflicted rows and the error and
# returns 1 with the database exactly as it was before the pull (in CLI
# mode the on-disk merge is aborted).
#
# SQL mode pulls again inside the transaction (the failed autocommit pull
# rolled its merge back); CLI mode already holds the merge in the working
# set. Either way the statements are the same: report every conflicted
# table and every conflicted `issues` row with its differing columns; give
# each benign row the remote's row_lock and updated_at and clear its
# conflict marker; report what is left; stage and commit. DOLT_COMMIT
# refuses a working set with any conflict left, and a transaction that never
# reaches COMMIT is discarded — the fail-closed operand is dolt's own, not a
# check of ours. "nothing to commit" on the retry means the pull found no
# conflict this time (a fast-forward or a clean merge, both durable before
# COMMIT), so it is reported as a plain pull.
resolve_benign_conflicts() {
  name="$1"; dir="$2"; remote_name="$3"; remote_url="$4"
  if ! benign_conflict_sql "$name" "$dir"; then
    abort_cli_merge "$name" "$dir"
    echo "  $name: ERROR: pull failed: merge conflict, and the issues table is not a bd store (no row_lock/updated_at) — resolve manually" >&2
    return 1
  fi
  p="$CONFLICT_PREDICATE"
  sql="SET @@autocommit = 0; SET @@session.group_concat_max_len = 1048576;"
  if [ "$server_running" = true ]; then
    sql="$sql CALL DOLT_PULL('$remote_name', 'main');"
  fi
  # Report rows put the count BEFORE the table name and fold a row's id and
  # differing columns into one field, so a table name with a comma, a quote
  # or a space cannot shift the parsed count (CSV quoting is undone on the
  # displayed name only).
  sql="$sql SELECT 'conflict' AS k, num_conflicts AS n, \`table\` AS t FROM dolt_conflicts;"
  sql="$sql SELECT 'schema' AS k, COUNT(*) AS n FROM dolt_schema_conflicts;"
  # A delete/modify conflict has no our_id (our side removed the row); the
  # row is still named from whichever side has it.
  sql="$sql SELECT 'row' AS k, CONCAT(COALESCE(our_id, their_id, base_id), ': ', $CONFLICT_DIFFERING) AS detail FROM dolt_conflicts_issues;"
  sql="$sql UPDATE issues SET row_lock = (SELECT c.their_row_lock FROM dolt_conflicts_issues c WHERE c.our_id = issues.id AND $p), updated_at = (SELECT c.their_updated_at FROM dolt_conflicts_issues c WHERE c.our_id = issues.id AND $p) WHERE id IN (SELECT c.our_id FROM dolt_conflicts_issues c WHERE $p);"
  sql="$sql DELETE FROM dolt_conflicts_issues WHERE $p;"
  sql="$sql SELECT 'remaining' AS k, num_conflicts AS n, \`table\` AS t FROM dolt_conflicts;"
  sql="$sql CALL DOLT_ADD('-A');"
  sql="$sql CALL DOLT_COMMIT('-m', 'gc dolt pull: merge $remote_name/main (row_lock/updated_at-only conflicts in issues resolved to the remote)', '--author', 'gc dolt pull <gc-dolt-pull@gascity.local>');"
  sql="$sql COMMIT;"
  resolve_rc=0
  out=$(run_db_sql "$name" "$dir" "$sql" 2>&1) || resolve_rc=$?
  rows=$(printf '%s\n' "$out" | grep '^row,' | sed 's/^row,//; s/^"//; s/"$//; s/""/"/g' || true)
  row_count=$(printf '%s\n' "$rows" | grep -c '.' || true)
  if [ "$resolve_rc" -eq 0 ]; then
    ids=$(printf '%s\n' "$rows" | sed 's/:.*//' | tr '\n' ' ' | sed 's/ $//')
    echo "  $name: pulled from $remote_url (resolved $row_count row_lock/updated_at-only conflict(s) in issues: $ids)"
    return 0
  fi
  reported=$(printf '%s\n' "$out" | grep -c '^conflict,' || true)
  schema_conflicts=$(printf '%s\n' "$out" | awk -F, '$1 == "schema" {n += $2} END {print n + 0}')
  # The retried pull found no conflict (a fast-forward or a clean merge,
  # both durable before COMMIT) exactly when the session reported no
  # conflicted table, no schema conflict, and then DOLT_COMMIT itself had
  # nothing to commit. The error line is matched by its own shape — result
  # rows (a table could be named anything) never count.
  if [ "$reported" -eq 0 ] && [ "$schema_conflicts" -eq 0 ] && printf '%s\n' "$out" | grep -q '^error on line [0-9]* for query CALL DOLT_COMMIT(.*nothing to commit'; then
    echo "  $name: pulled from $remote_url"
    return 0
  fi
  remaining=$(printf '%s\n' "$out" | awk -F, '$1 == "remaining" {n += $2} END {print n + 0}')
  if [ "$remaining" -eq 0 ]; then
    remaining=$(printf '%s\n' "$out" | awk -F, '$1 == "conflict" {n += $2} END {print n + 0}')
  fi
  tables=$(printf '%s\n' "$out" | grep '^conflict,' | sed 's/^conflict,[0-9]*,//; s/^"//; s/"$//; s/""/"/g' | sort -u | tr '\n' ' ' | sed 's/ $//')
  if [ -n "$rows" ]; then
    printf '%s\n' "$rows" | sed "s/^/  $name: conflict /" >&2
  fi
  abort_cli_merge "$name" "$dir"
  if [ "$schema_conflicts" -gt 0 ]; then
    echo "  $name: ERROR: pull failed: $schema_conflicts schema conflict(s) and $remaining row conflict(s) in ${tables:-no table} need manual resolution; nothing was written" >&2
  else
    echo "  $name: ERROR: pull failed: $remaining conflict(s) in ${tables:-unknown table(s)} need manual resolution; nothing was written" >&2
  fi
  case "$out" in
    *"nothing to commit"*|*"in conflict"*|*"unresolved conflicts"*) ;;
    *) printf '%s\n' "$out" | grep -i 'error' | head -3 | sed "s/^/  $name: /" >&2 || true ;;
  esac
  return 1
}

find_remote_sql() {
  db="$1"
  remote_csv=$(dolt_sql "USE \`$db\`; SELECT name, url FROM dolt_remotes LIMIT 1") || return 1
  printf '%s\n' "$remote_csv" | awk -F, 'NR > 1 && $1 != "" {print $1 "|" $2; exit}'
}

pull_database_sql() {
  name="$1"
  if ! valid_database_name "$name"; then
    echo "  $name: ERROR: invalid database name" >&2
    return 1
  fi

  remote_pair=$(find_remote_sql "$name") || {
    echo "  $name: ERROR: failed to query remotes" >&2
    return 1
  }
  if [ -z "$remote_pair" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  remote_name=${remote_pair%%|*}
  remote_url=${remote_pair#*|}
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  pull_err_tmp=$(mktemp) || {
    echo "  $name: ERROR: cannot create temp file for pull diagnostics" >&2
    return 1
  }
  # gp-f2yq: ONE server-side remote operation per database at a time. A CALL
  # DOLT_PULL runs inside the sql-server and outlives a client the bound
  # killed, so ask the server first and skip when a pull or fetch is already
  # in flight for this database. Fail closed: a processlist query that fails,
  # or answers with anything but a processlist, skips too.
  inflight_rc=0
  inflight=$(remote_op_sessions "$name" 120 "$pull_err_tmp") || inflight_rc=$?
  if [ "$inflight_rc" -ne 0 ]; then
    echo "  $name: ERROR: processlist query failed (exit $inflight_rc) — skipped" >&2
    remote_op_replay_stderr "$name" "$pull_err_tmp"
    rm -f "$pull_err_tmp"
    return 1
  fi
  inflight_oldest=$(printf '%s\n' "$inflight" | remote_op_sessions_oldest)
  if [ -n "$inflight_oldest" ]; then
    rm -f "$pull_err_tmp"
    echo "  $name: pull already in flight for ${inflight_oldest#* }s (session ${inflight_oldest%% *}) — skipped" >&2
    return 1
  fi
  pull_out_tmp=$(mktemp) || {
    echo "  $name: ERROR: cannot create temp file for the pull session id" >&2
    rm -f "$pull_err_tmp"
    return 1
  }
  pull_rc=0
  # The server-side gate (remote_op_gate_sql) is the batch's first statement
  # after USE: the session takes this database's lock and this run's lock and
  # prints its OWN connection id in that same statement, or the batch stops
  # here, before the CALL. The id and the locks are one statement, so the id
  # the KILL targets when the bound expires is the lock holder by construction
  # (a separate id statement before the gate could record a session that a
  # pooled-client reconnect left lockless while the reconnected one pulled on
  # — the mayor's gate r1); --use-db attributes the session to this database.
  # The CALL's own first argument re-proves that THIS session still holds the
  # run lock (remote_op_owned_arg), so a reconnected client never pulls on a
  # lockless session.
  dolt_sql "USE \`$name\`; $(remote_op_gate_sql "$name"); CALL DOLT_PULL($(remote_op_owned_arg "$name" "$remote_name"), 'main')" "$pull_timeout" "$name" \
    >"$pull_out_tmp" 2>"$pull_err_tmp" || pull_rc=$?
  pull_session_id=$(remote_op_session_id "$pull_out_tmp")
  rm -f "$pull_out_tmp"
  if [ "$pull_rc" -eq 0 ]; then
    rm -f "$pull_err_tmp"
    echo "  $name: pulled from $remote_url"
    return 0
  fi
  # The bound's verdict outranks anything the client printed.
  if bound_expired "$pull_rc"; then
    echo "  $name: pull timed out after ${pull_timeout}s (GC_DOLT_PULL_TIMEOUT_SECS; client exit $pull_rc)" >&2
    # The client is dead (124: the bound; 137: the bound's SIGKILL escalation);
    # the server-side pull is not. End it and prove it ended (the outcome is
    # reported on its own line).
    kill_remote_op_session pull "$name" "$pull_session_id" || true
  elif remote_op_gate_refused "$pull_err_tmp"; then
    rm -f "$pull_err_tmp"
    echo "  $name: pull already in flight — the server refused a second one (session lock $(remote_op_lock_name "$name") held) — skipped" >&2
    return 1
  elif remote_op_gate_lost "$pull_err_tmp"; then
    rm -f "$pull_err_tmp"
    echo "  $name: pull not sent — this session lost the run lock between the gate and the CALL (client reconnected) — skipped" >&2
    return 1
  elif grep -qi 'conflict' "$pull_err_tmp"; then
    # A failed autocommit pull with conflicts rolled its merge back and wrote
    # nothing; retry it inside the resolving transaction (gp-c04p).
    rm -f "$pull_err_tmp"
    resolve_benign_conflicts "$name" "$data_dir/$name" "$remote_name" "$remote_url"
    return $?
  else
    echo "  $name: ERROR: pull failed (exit $pull_rc)" >&2
  fi
  remote_op_replay_stderr "$name" "$pull_err_tmp"
  rm -f "$pull_err_tmp"
  return 1
}

pull_database_cli() {
  d="$1"
  name="$2"

  remote_name=""
  remote_url=""
  if [ -f "$d/.dolt/remotes.json" ]; then
    remote_name=$(grep -o '"name":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"name":"//;s/"//' || true)
    remote_url=$(grep -o '"url":"[^"]*"' "$d/.dolt/remotes.json" 2>/dev/null | head -1 | sed 's/"url":"//;s/"//' || true)
  fi
  [ -z "$remote_name" ] && remote_name="origin"

  if [ -z "$remote_url" ]; then
    echo "  $name: skipped (no remote)"
    return 0
  fi
  if ! valid_remote_name "$remote_name"; then
    echo "  $name: ERROR: invalid remote name: $remote_name" >&2
    return 1
  fi

  # A merge already in progress belongs to whoever started it (possibly
  # with resolutions half done); the pull must neither build on it nor
  # abort it. Read the state first — a failed read is a failure, never "no
  # merge" — and touch nothing when a merge is found.
  if ! pre_status=$(run_db_sql "$name" "$d" "SELECT is_merging FROM dolt_merge_status" 2>&1); then
    echo "  $name: ERROR: the merge state could not be read before pulling ($(printf '%s\n' "$pre_status" | head -1)); nothing was done" >&2
    return 1
  fi
  case "$(printf '%s\n' "$pre_status" | awk 'NR == 2 {print $1}')" in
    true|1)
      echo "  $name: ERROR: a merge is already in progress; resolve it or abort it (dolt merge --abort) before pulling; nothing was done" >&2
      return 1
      ;;
  esac

  if (cd "$d" && dolt pull "$remote_name" main 2>&1); then
    echo "  $name: pulled from $remote_url"
    return 0
  fi

  # CLI mode leaves a failed merge in the working set. Read the merge state
  # first — a failed read is a failure, never "no merge" — and put the
  # database back on every path that does not complete the merge.
  if ! merge_status=$(run_db_sql "$name" "$d" "SELECT is_merging FROM dolt_merge_status" 2>&1); then
    abort_cli_merge "$name" "$d"
    echo "  $name: ERROR: pull failed, and the merge state could not be read ($(printf '%s\n' "$merge_status" | head -1)); any merge in progress was aborted" >&2
    return 1
  fi
  case "$(printf '%s\n' "$merge_status" | awk 'NR == 2 {print $1}')" in
    true|1) ;;
    *)
      echo "  $name: ERROR: pull failed" >&2
      return 1
      ;;
  esac
  if ! schema_out=$(run_db_sql "$name" "$d" "SELECT COUNT(*) FROM dolt_schema_conflicts" 2>&1); then
    abort_cli_merge "$name" "$d"
    echo "  $name: ERROR: pull failed, and the schema conflicts could not be read ($(printf '%s\n' "$schema_out" | head -1)); the merge was aborted" >&2
    return 1
  fi
  schema_conflicts=$(printf '%s\n' "$schema_out" | awk 'NR == 2 {print $1 + 0}')
  if [ "${schema_conflicts:-0}" -gt 0 ]; then
    abort_cli_merge "$name" "$d"
    echo "  $name: ERROR: pull failed: $schema_conflicts schema conflict(s) need manual resolution; the merge was aborted, nothing was written" >&2
    return 1
  fi
  if ! conflicts_out=$(run_db_sql "$name" "$d" "SELECT COUNT(*) FROM dolt_conflicts" 2>&1); then
    abort_cli_merge "$name" "$d"
    echo "  $name: ERROR: pull failed, and the conflicts could not be read ($(printf '%s\n' "$conflicts_out" | head -1)); the merge was aborted" >&2
    return 1
  fi
  conflicts=$(printf '%s\n' "$conflicts_out" | awk 'NR == 2 {print $1 + 0}')
  if [ "${conflicts:-0}" -gt 0 ]; then
    resolve_benign_conflicts "$name" "$d" "$remote_name" "$remote_url"
    return $?
  fi

  abort_cli_merge "$name" "$d"
  echo "  $name: ERROR: pull failed: the merge did not complete and reported no conflict; the merge was aborted" >&2
  return 1
}

exit_code=0
server_running=false
is_running && server_running=true
if [ -d "$data_dir" ]; then
  for d in "$data_dir"/*/; do
    [ ! -d "$d/.dolt" ] && continue
    name="$(basename "$d")"
    case "$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]')" in information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe) continue ;; esac
    [ -n "$db_filter" ] && [ "$name" != "$db_filter" ] && continue
    if [ -f "$d/.no-sync" ]; then
      echo "  $name: skipped (.no-sync)"
      continue
    fi

    if [ "$server_running" = true ]; then
      pull_database_sql "$name" || exit_code=1
    else
      pull_database_cli "$d" "$name" || exit_code=1
    fi
  done
fi

exit $exit_code
