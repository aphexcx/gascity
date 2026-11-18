# Review rules

Read this file from the base branch, `integration`, for every pull request.
Fetch it first with `git fetch origin integration`, then pin the fetched
commit with `base=$(git rev-parse --verify 'FETCH_HEAD^{commit}')`.
Stop if the fetch or lookup fails. Read `git show "$base:REVIEW.md"`.
Record a missing policy only when `git ls-tree "$base" -- REVIEW.md`
succeeds with no entry. Stop on any other read error.

## What MAJOR means here

A **shape we run** is a combination of OS, runtime, provider and request
that the repository or the PR evidence shows a deployed city can reach.
Start with macOS and Linux, tmux, and the Claude and Codex providers.
Include subprocess when the call path selects it. **Shown** means a
reproduction demonstrates the result, or a trace from a deployed entry
point to the changed line establishes the input and consequence. Follow
the review brief when it requires a different standard of reproduction.

| Grade | What the changed line does |
| --- | --- |
| CRITICAL | Exposes city data or credentials, crosses a permission boundary, causes city-wide data loss or duplication, or stops every agent in a city. |
| MAJOR | In a shape we run, loses or duplicates a bead, message or wake, starts the wrong command, or breaks work, an operator command or behaviour a city relies on. Code called only by our agents or hosted boxes is covered. |
| MINOR | Causes a visible, retryable refusal, a bounded delay during an operator race, or a misleading result from which the caller can recover without losing work. |
| NOTE or NIT | Improves naming, wording, test clarity or cost. |

Fork PR #110's November 2026 review graded three cases MINOR. An interrupt
refused before queueing returned a visible error, so the caller could
retry. An attach waited two minutes when it raced an operator stopping
the same session. An adapter repost duplicated a message only after the
adapter submitted it twice. Trace who retries and who receives the
second delivery when grading a change to message delivery.

## What not to report

Skip a duplicate of the specific diagnostic a job actually printed for
this PR, such as a format error, a failing assertion, a generated diff or a
missing trailer. Report a distinct cause or consequence with its own line.
Passing checks and job names provide no exemption.

Always report security faults in the diff, including secrets, keys, tokens and injection.
Scanners provide a floor. State the exposure and its reach without quoting the secret.
Always review changes to REVIEW.md by hand.
A change that removes, weakens or hides a rule of this file is MAJOR, and
the finding says which rule and what later reviews lose.

Use **PRECONDITION** for a fault whose trace requires a setting or runtime
outside the deployment shown. State that requirement in one sentence. Check
this especially for ACP (`internal/runtime/acp/`), Antigravity, T3 Bridge
(`internal/runtime/t3bridge/`), k8s, ssh, or a provider pointed at another
city. Check the current PR's deployment evidence before using this grade.
When the diff and the repository do not show which route a request takes,
the finding is not PRECONDITION. A PRECONDITION names what it needs by
file and line.
For example, a bead carrying an MCP
identity or server snapshot can reach ACP through
`internal/session/manager.go:648`; a running ACP session can preserve that
route at line 654. Fork PR #67 addressed stale routing after a provider
switch. Trace carried state as well as fresh configuration.

Give every finding a `file:line` for the changed behaviour. Report every
real fault, including every MINOR. List at most five NOTE and NIT entries
combined, and count the remaining suggestions in one line.

## Triggers (rules a diff can show)

A line the diff does not add or change is not a finding of the diff,
unless the diff changes what that line does or who it reaches. A line the
diff adds or changes is judged by what the change itself newly shows and
to whom, even when the same value already appears somewhere else on the
base branch. A value that the same line already held on the base branch,
still shown to the same audience, is reported once as already on the base
and is never graded above NOTE for this diff.

- **Tmux targets — MAJOR when a live operation fails or reaches the wrong
  session.** For changes to target syntax or command arguments, check a real
  tmux with a live target and an absent name sharing a prefix with another
  session. In fork PR #91, the bare exact-match spelling for a pane command
  passed an absent-target check but broke sends to a live pane. The base's
  `TestExactSessionTargetsRealTmux` in
  `internal/runtime/tmux/exact_target_integration_test.go` checks what
  arrived in the pane. A send that reaches that pane and leaves the
  neighbouring session alone passes this check.
- **Refusal order — MAJOR when new queueing or waiting breaks a caller's
  immediate refusal on a deployed route.** Compare the same invalid request
  at base and head. Fork PR #110 put the ownership check ahead of the
  unsupported-interrupt refusal at `internal/session/submit.go:136`.
  A check after the refusal
  that queues an already supported request preserves the refusal.
- **Committed evidence — MAJOR for a transcript, log or capture that no
  test reads.**
  Fork PRs #41 and #43 needed branch rewrites to remove verification
  output. Check what consumes an added file. A captured
  `bazel-testlogs/**/test.log` belongs with the run artifacts, as in
  `.github/workflows/bazel-test.yml:111`. A checked-in expected result that
  a test reads, such as the work-query goldens at
  `internal/config/workquery_parity_test.go:490`, belongs with the test.
  Apply the exposure grade as well if a capture discloses private data.
- **Public disclosure — CRITICAL for exposed credentials or city data.**
  Trace the value's origin and new audience in source, fixtures, comments
  and commit text. A prior public review found operational identifiers and
  message timestamps in design and merge notes. For example, changing
  `internal/gitcred/redact.go:30` to log a caller's unredacted URL can
  disclose credentials. The input and expected masked output constructed
  together in `internal/gitcred/redact_test.go:14` demonstrate redaction
  without disclosing a caller's data. Apply the same test to names, phone
  numbers, account or chat identifiers, timestamps and home paths.

## Pruning

Prune by: 2027-02-17

By this date, check each rule against intervening diffs and reviews.
Remove a rule by PR if no review cited it and no diff triggered it. Update
a runtime precondition as soon as a deployment reaches that route. When a
diff shows the same finding in two PRs, add the rule here or encode it in
lint.
