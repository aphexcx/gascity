# gp-zbem evidence

Base: `5a85171d8a61df8a1460b487738bb396494bb94a`, the merged PR #30 squash on `integration`.
Scope: the reaper zero grammar/comment, two docs, maintenance table, and this evidence directory.

E5 compatibility exception: an optionally signed bare run of zero digits also disables stale closes,
so existing inputs such as `00` cannot fall through to the unchanged hours arm.
`ruling-e5.txt` records the mayor's correction of E1 and the two additional invalid rows.

The implementation accepts all-zero Go duration components with mandatory units, including `.0h`,
`0.h`, `0µs`, and `0μs`. A single leading sign is allowed. Bare fractional zeros and unit-less trailing
components produce the unchanged single warning and disable age-based closes.

## Reproduction

`run.py` records exact command arguments, ICU flags, combined output, exit status, and wall time.
`parser-cases.py` extracts and runs the real trim, conversion and selection code on each interpreter.
It preserves all 52 predecessor inputs, corrects four expected warning flags, adds five missing
requested inputs, and adds the E5 `.0`/`0.0` controls: 59 distinct cases per interpreter.

- `red.txt`: original nine new rows plus 31 existing rows on the unmodified base: 35 pass, 5 fail.
- `red-e5.txt`: two E5 rows on the same unmodified script: `.0` passes, `0.0` fails.
- The six observed failures are `.0h`, `0.h`, `0µs`, `0μs`, `0h0`, and `0.0`.
- `0h0.0m` already passed on the base, contrary to the original spec's predicted RED; its row remains.
- `parser-red.txt`: the matrix rejects the predecessor's warning for `0µs`.
- `go-duration-oracle.txt` and `parse-duration.go`: the local Go parser's actual edge behavior.
- `shell-syntax.txt`, `source.diff`, and `scope-check.txt`: syntax and exact scope checks.

Focused GREEN: 42/42 rows pass; parser matrix: 59/59 on each Bash; syntax: both pass.
Full package: 250 top-level / 392 including subtests pass, with one existing tmux skip.
Independent Codex QUICK r1: CLEAN. Source commit: fe71f5450b356a40c255e00f5d6127ef352ba0d9.
Pre-commit: lint, codegen, vet, and doc-sync pass. The first attempt failed only because
new Markdown evidence made assets an undocumented doc-tree root; evidence now uses .txt
without changing the doc-tree policy. Both attempt transcripts are retained.
Required ordinary push gate passed 10/10 jobs on 4cbfb639582d2936744cd15f411d191a3c68dc63, wall 1816.423s.
LOCAL_TEST_JOBS=2 and PUSH_GATE_MAX_CONCURRENT=2; no gate bypass or retry.
The subsequent commit records evidence only; reviewed source is unchanged.
The upstream twin remains held; this branch targets a draft fork PR only.

Captured pre-commit line-end whitespace is normalized for git diff --check; all messages and statuses are retained.
