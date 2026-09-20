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

Green/package/review/gate results are recorded in their named transcripts and summarized before handoff.
The upstream twin remains held; this branch targets a draft fork PR only.
