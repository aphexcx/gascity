# Review rules

The codex gate and a second model's read take this file from the base
branch, integration, never from the pull request's own head. A reviewer
that reads a working tree reads the pull request's copy and must
compare it with integration's. This file says what our cities run, what
failure looks like there, what not to report, and the few rules a diff
can show. A REVIEW.md in a pull request is under review itself, by the
rules of integration's copy.

## What MAJOR means here

This fork is the gc our own cities run.

Our cities run:

- macOS and Linux.
- Sessions under tmux, the default session runtime (an empty
  `[session] provider`, `internal/config/config.go`).
- The Claude and Codex providers.

No city of ours runs:

- The ACP transport (`internal/runtime/acp/`). No agent of ours sets
  `session = "acp"`, and a Claude or Codex session starts under tmux
  unless its template asks for ACP (`internal/config/provider.go`).
- The T3 Bridge runtime (`internal/runtime/t3bridge/`).
- A remote-city setting, `GC_CITY_URL` or `GC_CITY_CONTEXT`, in a
  provider's or the workspace's environment
  (`internal/api/session_runtime.go`).

The grades:

- CRITICAL: a fault in what our cities run that exposes a city's data or
  a credential, crosses a permission boundary, loses or duplicates data
  across a city, or stops every agent in a city.
- MAJOR: a fault that one ordinary action, or one ordinary fault, reaches
  in what our cities run, and that loses or duplicates a bead, a message
  or a wake, starts the wrong command, or breaks work, an operator's
  command or behaviour a city relies on. Code only our own agents call
  is covered.
- MINOR: a real fault whose sequence needs a write paused or failed at a
  chosen instant, a lock held past a deadline, a process killed between
  two chosen lines, or two unlikely things at the same moment; or one
  the caller sees at once and can retry, a bounded delay, or a
  misleading result the caller recovers from without losing work.
- NOTE: naming, wording, a test's shape, a cheaper way.

How narrow a sequence is never lowers two kinds of fault:

- One an outsider can trigger. An outsider is anyone but the city's own
  operator and its agents: a chat sender, a webhook, a caller of a port
  the city listens on.
- One that loses or exposes data. Data is what a city keeps: beads,
  mail, a session's transcript, files in a rig, credentials. A message
  or a wake still in delivery is graded by how its loss is reached.

A fault this file gives no other grade takes the grade of its effect
under these four, and a security fault this file gives no other grade is
MAJOR at least.

## What not to report

- A fault that needs something no city of ours runs (the list above) is
  reported at MINOR with the mark PRECONDITION and the precondition in
  one sentence, never as MAJOR.
- A fault is not a PRECONDITION when state a session already carries
  takes it there. A session whose bead holds an MCP identity or an MCP
  server snapshot is routed to ACP by that state only when its
  configured transport is ACP or its process already runs on ACP
  (`transportForBead`, `internal/session/manager.go`). A finding that
  traces a tmux session of ours into the code that handles that state
  keeps its grade; one that needs either of those two is a
  PRECONDITION.
- What a CI job itself prints when it runs on this pull request: a
  format or lint error, a failing test, a generated file out of date.
  The fault the diff shows is still reported when a job's output is its
  symptom, and so is a fault no job printed.
- Always reported: a secret, a key, a token, an injection or any other
  security fault in the diff, whatever a scanner says.
- Always reviewed by hand: a change to REVIEW.md itself. A change that
  removes, weakens or hides a rule of this file is MAJOR, and the finding
  says which rule and what later reviews lose. A removal the Pruning
  section calls for, made in a pull request that says which review of
  the rule found it unused, is not a weakening and is not MAJOR; every
  other removal keeps its grade.
- At most five NOTE (or NIT) findings in one review: report the five
  that matter most and count the rest in one line. A MINOR is a real
  fault, never capped and never dropped.
- A claim that names neither a file:line nor a commit and a line of
  its message is not reported. Every finding names the file and line
  it rests on, or, when it rests on a commit message, the commit and
  the line of its message.

## Triggers (rules a diff can show)

A line the diff does not add or change is not a finding of the diff,
unless the diff changes what that line does or who it reaches.

- Nothing private in any file, test fixture, comment or commit message:
  no customer, no person, no phone number, no account, chat or channel
  id, no message timestamp, no private host, no home-directory path.
  This repository is public. A private value the diff adds is MAJOR, and
  CRITICAL when it is a key's or a token's value.
- No evidence on the pull request branch. A transcript, a log or a
  capture that no test reads stays outside this repository; a diff that
  adds one is MAJOR.

## Pruning

Prune by: 2027-02-17

On or before that date each rule above is read against the reviews
since it was written. A rule no review cited and no diff triggered is
removed in a pull request; a thing this file names as one no city of
ours runs is removed the day a city starts running it; a finding that
repeats in two pull requests and is readable from a diff is added here
or as a lint rule.
