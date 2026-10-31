# Upstream twin gates

A twin ports a fork change onto upstream's own base and toolchain. Use the
upstream checkout's pins and commit hook. A passing fork suite does not
establish that upstream's formatter or nogo accepted the port.

## Build the matching linter

From the fork checkout, this helper reads the supplied upstream checkout's
`Makefile` and `go.mod`, resolves its compiler, and prints the linter path:

```sh
lint_bin=$(scripts/ensure-upstream-golangci-lint "$UPSTREAM_CHECKOUT")
export PATH="$(dirname "$lint_bin"):$PATH"
```

The cache is outside both checkouts, under the first GOPATH entry at
`bin/gascity-tools/golangci-lint-v<VERSION>-<GOVERSION>/golangci-lint`.
The helper honors an explicit `toolchain` directive, validates the built
binary's module and compiler metadata, and reuses only an exact match.
A compiler mismatch stops before installation and names the required
`GOTOOLCHAIN` setting. It leaves the global linter untouched.

## Run the upstream hook on Linux

Use the existing assigned lane. The `bazel-test` workflow has an optional
dispatch path into `upstream-twin-gates`; ordinary fork runs retain their
existing build and test commands. The opt-in job uses Ubuntu 24.04, the
twin's Go version, the helper above, system ICU development files, and the
twin's own pinned `.github/actions/setup-bazel` bootstrap. It needs no RBE
credentials and cannot push a branch.

First publish the reviewed source snapshot and the recipe branch to the
fork through their required gates. The twin input may be an existing WIP
commit; its presence on a branch does not count as a passed commit hook.
Record the full immutable twin head and its reviewed upstream base. Both
must exist in the fork, the base must be an ancestor, and their delta must
be nonempty. Use the authorized GitHub profile for every fetch and push;
on Citadel it is `/Users/tailor512/.config/gh-aphexcx`.

```sh
gh workflow run bazel-test.yml --repo aphexcx/gascity \
  --ref "$RECIPE_BRANCH" \
  -f upstream_twin_head="$TWIN_HEAD" \
  -f upstream_twin_base="$TWIN_BASE"
```

Record the resulting run ID and check its recipe SHA and dispatched inputs.
The job copies the helper outside its disposable checkout, checks out the
exact twin, stages the complete base-to-head delta, and invokes `git commit`
with upstream's unchanged `.githooks/pre-commit`. That hook owns staged
formatting, `make lint-changed` / Bazel nogo, and generation. No step is
substituted by a success flag. `NOGO_BUILD_FLAGS` only limits concurrency
while retaining the hook's normal output group and failure behavior.

A successful job also proves that the committed tree exactly equals the
submitted tree and that the checkout is clean. A formatter or generator
change fails this check: review that change in a new input and rerun. The
job preserves the original author and replaces any obsolete `Hook-Skipped`
trailer with its run URL. This creates a new commit; it does not modify the
published twin branch.

Download the `upstream-twin-gate` artifact into the work item's external
evidence directory. It contains the hook source, linter version, hook log,
input/output tree IDs, accepted commit SHA, and a Git bundle. Failure logs
are uploaded too, so an artifact's existence alone does not establish a pass.

```sh
gh run watch "$RUN_ID" --repo aphexcx/gascity --exit-status
gh run download "$RUN_ID" --repo aphexcx/gascity \
  -n upstream-twin-gate -D "$EVIDENCE_DIR/linux-gate"
git bundle verify "$EVIDENCE_DIR/linux-gate/twin.bundle"
git fetch "$EVIDENCE_DIR/linux-gate/twin.bundle" refs/heads/upstream-gated-twin
test "$(git rev-parse FETCH_HEAD)" = "$(cat "$EVIDENCE_DIR/linux-gate/commit.txt")"
test "$(git rev-parse 'FETCH_HEAD^{tree}')" = "$(git rev-parse "$TWIN_HEAD^{tree}")"
```

Coordinate replacement of the source snapshot with the twin branch's owner.
Check that no other lane holds the branch, preserve its original SHA, and
use an explicit lease if replacing an already published commit. Never move
someone else's checkout or force-push over a changed remote. The publisher
opens a draft upstream PR and includes the run URL and external evidence
path. Full upstream tests and PR checks remain separate gates; a green
commit-hook job establishes only the hook and exact-tree contract.

## Citadel's known host failures

These measurements apply to upstream `6f488617b` on 2026-10-31. Recheck pins
when upstream changes. The full measurement table and command logs live
outside the repository in the work item's evidence directory.

| Gate | Citadel cause | What changes the result |
| --- | --- | --- |
| Staged formatter | Host golangci-lint 2.12.0 was built with Go 1.26.6; upstream pins 2.14.0 and Go 1.27.3. | Use the compiler-keyed helper, then put its directory first on PATH. |
| Bazel nogo / cgo link | Pinned LLVM 22.1.8 rejects SDK 27 TAPI `arm64e.x1`. An installed SDK 26.5 and target plus host compile/link flags still leave SDK 27 library search paths in the generated toolchain. `SDKROOT` does not override its absolute `xcrun --sdk macosx` probe. | Use the Linux hook job. A host Xcode change requires Afik's authorization. |
| ICU headers | Homebrew advertises ICU79 while the installed headers are under ICU78. Plain Go needs the installed include/library paths; Bazel actions do not inherit those Go environment flags. | Verify the headers before setting Go CGO flags. The Linux job uses declared native dependencies and upstream's Linux sysroot. |
| `TestInitFromPinsHostedDoltEndpoint` | Citadel Dolt 2.1.10 is below upstream's managed minimum 2.6.0. | Run on an isolated runner with upstream's Dolt pin, or wait for Afik to authorize the shared host upgrade. |
| `TestInitFromHostedPreservesRigSiteBindings` | Same host Dolt mismatch; recorded as a base failure before the twin. | Same authorization boundary. Do not rerun the full package merely to rediscover it. |

The two Dolt tests are prior base measurements, not waived passing tests.
Do not upgrade the shared Dolt process, which hosts beads stores, as part of
a twin. Upstream's macOS regression workflow provisions its own pinned
Dolt on macOS 15 runners; it does not establish compatibility with
Citadel's Xcode 27 installation. The fork's older `fork-verify` workflow
uses a different Dolt pin and narrow Go tests, so it cannot replace nogo.

PR 74's startup-overlay port and PR 75's prompt-suggestion port are separate
work items. Their owners should use this recipe after reviewing each port's
upstream-specific conflicts; the gate does not perform those ports.
