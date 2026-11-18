---
name: verify
description: "Check a change before committing and run the full fast suite before handoff."
---

# Verify a change

Run the fenced commands from the repository root. The [Makefile](../../../Makefile) selects Go 1.26.6 from [go.mod](../../../go.mod) and may download the toolchain. On macOS it discovers ICU and sets the CGO include and linker flags. Raw `go test` and `go build` bypass that setup. Use the Make targets below as the commands of record.

## Before committing

Check hook ownership first. This reads the configuration and takes less than a minute. If it fails, report the hooks path it found.

```sh
make check-hooks
```

Check formatting (3 minutes), then lint (9 minutes). Both can install the pinned linter in the Go tool cache directory under GOPATH if absent. Formatting prints the diff. Lint populates analysis caches.

```sh
make fmt-check
```

```sh
make lint
```

Vet uses the Go build cache (less than a minute).

```sh
make vet
```

Run the package target once per changed Go package (1 minute for this example). Skip this step for changes outside Go packages. The fast test targets set read-only module resolution and `GC_FAST_UNIT=1`. Their `env -i` allowlist preserves HOME and selected provider variables. Keep session variables such as `GC_CITY`, `GC_HOME`, `GC_SESSION_ID`, `BEADS_DIR`, `GC_DOLT_PORT` and `BEADS_DOLT_SERVER_PORT` out of `EXTRA_TEST_ENV`. The package target writes a test log and an isolated Git configuration in temporary storage.

```sh
make test-pkg PKG=./test/docsync
```

For documentation and verification skill changes, run the sync tests (1 minute, Go cache only). Hosted preflight runs this command on the prospective merge commit, including skill-only PRs. The [docs verification guide](../gascity-docs/references/verification.md) covers authoring tasks separately. Leave preview servers and diagram rendering out of unattended verification. The [testing policy](../../../TESTING.md) describes the separate hosted Bazel suite.

```sh
make check-docs
```

For API, OpenAPI or dashboard changes, use an already provisioned oapi-codegen, Node 22.13.0 or later, and npm. Run the schema check first (less than a minute). It regenerates tracked OpenAPI and event schemas, their text mirrors and the Go client, then checks for drift. The Makefile installs oapi-codegen onto PATH if missing. Stop and report the missing tool before running the target in that case.

```sh
make spec-ci
```

Then run the dashboard check (2 minutes). It installs workspace dependencies, rewrites the embedded dashboard and TypeScript client, typechecks, runs Go tests and checks for drift. Before the next inventory check or commit, move the dashboard's newly installed node_modules directory outside the checkout and restore the original dependency state. Otherwise Gazelle discovers Go packages inside the installed modules.

```sh
make dashboard-ci
```

## Build and check the CLI

The build and two CLI reads together took 1 minute. The build uses the caller's environment and Go caches. On macOS its signing step reads keychain identities and can sign the temporary executable. The two CLI reads run in an empty environment with a scratch home and working directory, away from repository credentials. Both exit without opening a port. The trap removes the binary and scratch directory.

```sh
(
  set -eu
  scratch=$(mktemp -d /var/tmp/gc-verify.XXXXXXXX)
  trap 'rm -rf "$scratch"' EXIT
  make build BUILD_DIR="$scratch/bin"
  env -i -C "$scratch" HOME="$scratch" ./bin/gc version
  env -i -C "$scratch" HOME="$scratch" ./bin/gc --help
)
```

## Before handoff

Run the sharded fast suite once. The measured push including this suite took 13 minutes. The runner writes per-shard logs and uses Go caches. A normal push that runs the suite supplies this result.

```sh
make test-fast-parallel
```

When a test fails in a package you did not change, run that one test alone five times. Use the package target's `RUN` and `COUNT` arguments and record the test, package, machine load and all five results. If it passes five of five, record the load-only failure in the handoff and continue. If it fails even once alone, stop and report the base failure. If a package exceeds its time limit, apply the same rule to its slowest test. Never rerun the suite until it passes. Investigate failures in a changed package before handoff.

## Other Make targets

The fenced Make targets above are the commands of record. **NEVER** run the following targets marked NEVER as verification. The harmless targets are unnecessary for this checklist.

| Class | Targets | Effect |
| --- | --- | --- |
| NEVER | `install` | Replaces the gc binary on PATH and updates its user symlink. |
| NEVER | `setup` | Installs tools and changes the shared Git hooks path. |
| NEVER | `install-tools`, `install-oapi-codegen`, `ensure-golangci-lint`, `install-buildx`, `setup-worker-inference` | Install developer, Docker or provider tooling. |
| NEVER | `fmt`, `fmt-staged-go`, `lint-fix` | Rewrite source or the index. |
| NEVER | `generate`, `check-schema`, `complexity-update`, `bazel-sync`, `clean` | Rewrite generated files or the complexity baseline, or delete the built binary. |
| NEVER | `docs-dev`, `dashboard-dev`, `dashboard-smoke`, `dashboard-e2e-go` | Start a preview server or supervisor. |
| NEVER | `diagrams-excalidraw`, `dashboard-build`, `dashboard-check`, `dashboard-e2e-play`, `dashboard-e2e`, `test-openclaw-bridge` | Download tooling or dependencies, rewrite build outputs or run a browser and server. |
| NEVER | `update-bundled-gastown-pack`, `sync-bd-corpus` | Fetch and rewrite a pack pin or vendored release corpus. |
| NEVER | `test-herdr-live`, `test-pack-registry-live` | Drive a live terminal server or contact the registry. |
| NEVER | `test-docker`, `docker-base`, `docker-agent`, `docker-controller`, `test-k8s`, `k8s-secret` | Use a daemon, build or load cluster images, or write cluster credentials. |
| NEVER | `test-chaos-dolt`, `test-mail-wisp-insert`, `test-mcp-mail` | Induce database failures, write mail through backends or start a mail server. |
| NEVER | `test-cmd-gc-process`, `test-cmd-gc-process-shard`, `test-cmd-gc-process-parallel`, `test-productmetrics-testhook` | Launch process-backed CLI or telemetry fixtures. |
| NEVER | `test-worker-core-phase2-real-transport`, `test-worker-core-phase2-all`, `test-worker-inference`, `test-worker-inference-phase3` | Exercise real transports or inference providers. |
| NEVER | `test-acceptance`, `test-beads-topology-matrix`, `test-bd-cli-contract`, `test-bd-conditional-release-contract`, `test-acceptance-b`, `test-acceptance-c`, `test-acceptance-all` | Launch command, database, lifecycle or inference fixtures. |
| NEVER | `test-tutorial-goldens`, `test-tutorial-regression`, `test-tutorial` | Use tmux, Dolt, bd and authenticated inference. |
| NEVER | `check-all`, `test-local-full-parallel`, `test-integration`, `test-integration-huma`, `test-integration-shards`, `test-integration-shards-parallel`, `test-integration-shards-cover` | Run integration infrastructure or start a supervisor. |
| NEVER | `test-integration-packages`, `test-integration-packages-cover`, `test-integration-bdstore`, `test-integration-bdstore-cover` | Run integration packages or store conformance. |
| NEVER | `test-integration-review-formulas`, `test-integration-review-formulas-cover`, `test-integration-review-formulas-basic`, `test-integration-review-formulas-basic-cover`, `test-integration-review-formulas-retries`, `test-integration-review-formulas-retries-cover`, `test-integration-review-formulas-recovery`, `test-integration-review-formulas-recovery-cover` | Drive process-backed workflow fixtures. |
| NEVER | `test-integration-rest`, `test-integration-rest-cover`, `test-integration-rest-smoke`, `test-integration-rest-smoke-cover`, `test-integration-rest-full`, `test-integration-rest-full-cover` | Run remaining process-backed integration shards. |
| Harmless | `check`, `test`, `test-mac`, `test-fsys-darwin-compile` | Duplicate unit gates or cross-compile a fixture. The serial suite exceeded its package deadline in measurement. |
| Harmless | `lint-full`, `lint-new`, `lint-changed`, `lint-affected`, `fmt-check-changed` | Alternate scopes of the formatting and lint checks, with the same linter prerequisite. |
| Harmless | `check-release-dist-ignore`, `check-routed-test-rows`, `check-split-topology-rows`, `check-inventories`, `check-residency-boundary` | Check repository inventories and boundaries. |
| Harmless | `test-ci-policy`, `check-gomod-replace`, `check-native-dependency-surface`, `check-eventexport-isolation`, `check-core-boundary`, `test-native-doltlite-beads` | Additional CI policy, dependency and native-store checks. |
| Harmless | `check-bd`, `check-docker`, `check-dolt`, `check-version-tag`, `print-cgo-env`, `check-icu`, `help` | Inspect local tool availability, environment or metadata. |
| Harmless | `complexity`, `complexity-diff`, `complexity-check`, `check-bd-parent-grammar` | Inspect complexity or check a pinned dependency's grammar. |
| Harmless | `test-worker-core`, `test-worker-core-phase2` | Run deterministic transcript and continuation tests. |
| NEVER | `test-cover`, `cover` | Delete previous CLI coverage shards and replace the merged report. |
| Harmless | `test-cover-mac`, `test-cover-noncmdgc`, `test-cover-cmdgc-shard` | Run additional suites and write local coverage profiles. |
| NEVER | `check-self-contained` | Builds the default binary in the checkout before checking linkage. |

## Operational scripts

Never run these scripts as verification. Paths are relative to the repository root.

- Container and session management uses `contrib/session-scripts/gc-controller-k8s`, `contrib/session-scripts/gc-session-k8s`, `scripts/gc-session-docker`, `contrib/session-scripts/gc-session-screen`, `contrib/beads-scripts/gc-beads-k8s` and `contrib/events-scripts/gc-events-k8s`.
- Messaging and city demos use `contrib/mail-scripts/gc-mail-mcp-agent-mail`, `contrib/openclaw-bridge/bridge.mjs`, `contrib/openclaw-bridge/telegram-bridge.mjs`, `contrib/openclaw-bridge/demo.sh`, `contrib/openclaw-bridge/demo-telegram.sh`, `contrib/mayor-chat/chat.mjs` and `contrib/demo/demo-01.sh`.
- The [core pack](../../../internal/bootstrap/packs/core/assets/scripts) can update the ledger, signal sessions and send mail through `internal/bootstrap/packs/core/assets/scripts/cascade-nudge-on-blocker-close.sh`, `internal/bootstrap/packs/core/assets/scripts/nudge-on-route.sh`, `internal/bootstrap/packs/core/assets/scripts/notify-on-human-gate-creation.sh`, `internal/bootstrap/packs/core/assets/scripts/renudge-stale-human-gates.sh`, `internal/bootstrap/packs/core/assets/scripts/escalate.sh`, `internal/bootstrap/packs/core/assets/scripts/gate-sweep.sh`, `internal/bootstrap/packs/core/assets/scripts/orphan-sweep.sh`, `internal/bootstrap/packs/core/assets/scripts/reaper.sh`, `internal/bootstrap/packs/core/assets/scripts/wisp-compact.sh`.
- The [bd pack's Dolt commands](../../../examples/bd/dolt/commands), `examples/bd/assets/scripts/gc-beads-bd.sh`, `contrib/beads-scripts/gc-beads-br`, `scripts/coordstore-soak/setup-isolated-dolt.sh` and `scripts/coordstore-soak/launch-dolt-baseline.sh` manage ledgers and database processes.
- `scripts/smoke-macos.sh`, `scripts/test-docker-session` and `scripts/worker_conformance_grid.py` initialize a sandbox city or exercise provider infrastructure. `scripts/docs-autofix-push.sh` and `scripts/bump-version.sh` can push branches or tags.
