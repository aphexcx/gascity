---
name: verify
description: "Run before every commit: the exact build, lint, vet, test and boot commands for this repository."
---

# Verify a change

Run these commands from the repository root. The [Makefile](../../../Makefile) selects the Go version in [go.mod](../../../go.mod) (1.26.6) by default. On macOS it discovers the ICU prefix and sets the CGO include and linker flags. Use an installed ICU and the repository's pinned linter.

## Before committing

Check formatting, then lint and vet the Go packages.

```sh
make fmt-check
```

```sh
make lint
```

```sh
make vet
```

Run the affected package. Replace the package in this example with the one you changed. The fast test targets use read-only module resolution, set `GC_FAST_UNIT=1`, and run under an `env -i` allowlist. Keep session variables such as `GC_CITY`, `GC_HOME`, `GC_SESSION_ID`, `BEADS_DIR`, `GC_DOLT_PORT` and `BEADS_DOLT_SERVER_PORT` out of `EXTRA_TEST_ENV`.

```sh
make test-pkg PKG=./test/docsync
```

For documentation, run the sync tests and follow the [docs verification guide](../gascity-docs/references/verification.md) for diagrams, generated pages and preview. The hosted PR workflow runs this target on the prospective merge commit, including when only this skill changes.

```sh
make check-docs
```

For API, OpenAPI or dashboard changes, use Node 22.13.0 or later and npm. This target installs workspace dependencies, builds and typechecks the dashboard, runs its Go tests, regenerates the client and checks the generated diff.

```sh
make dashboard-ci
```

Run the full fast unit suite before handoff. If a case fails under load, capture the full-run failure and repeat the single case using the package target's `RUN` and `COUNT` arguments. A passing isolated run helps diagnose timing sensitivity.

```sh
make test
```

Where the Bazel toolchain works, use the package-level fast loop in the [testing policy](../../../TESTING.md). If the macOS C++ toolchain fails to compile Abseil, use the package command above.

## Build and check the CLI

Build into a temporary directory. Both CLI invocations start with an empty environment, a temporary home and a working directory outside the checkout. Version and help exit without opening a port.

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

## Live and integration coverage

Reserve these targets for their provisioned environments.

- `test-herdr-live` drives panes and restarts a real terminal server. `test-pack-registry-live` contacts the pack registry.
- `test-docker` drives a Docker daemon. `docker-base`, `docker-agent` and `docker-controller` build daemon images and the latter two can load a local cluster. `test-k8s` needs a Kubernetes cluster. `k8s-secret` writes cluster credentials.
- `test-chaos-dolt` exercises database failures. `test-mail-wisp-insert` exercises mail writes through the database backends. `test-mcp-mail` starts a mail server.
- `test-integration` and its shards exercise process-backed infrastructure. `test-acceptance-b` exercises lifecycle operations, and `test-acceptance-c` and `test-worker-inference` use real inference providers.
- `test-worker-core-phase2-real-transport` uses real transports. `test-tutorial-goldens` uses tmux, Dolt, bd and provider authentication. Their aggregate targets inherit those requirements.

Other entry points have operational effects even when run from a checkout.

| Entry points | Effect |
| --- | --- |
| `gc-controller-k8s`, `gc-session-k8s`, `gc-session-docker`, `gc-session-screen`, `gc-beads-k8s`, `gc-events-k8s` | Manage containers, sessions or cluster resources. |
| `gc-mail-mcp-agent-mail`, OpenClaw's `bridge.mjs`, `telegram-bridge.mjs`, `demo.sh`, `demo-telegram.sh`, and the `chat.mjs` and `demo-01.sh` launchers | Talk to messaging services or a running city. |
| [Core pack scripts](../../../internal/bootstrap/packs/core/assets/scripts), including `cascade-nudge-on-blocker-close.sh`, `nudge-on-route.sh`, `notify-on-human-gate-creation.sh`, `renudge-stale-human-gates.sh`, `escalate.sh`, `gate-sweep.sh`, `orphan-sweep.sh`, `reaper.sh` and `wisp-compact.sh` | Update the selected ledger, signal sessions or send mail. |
| The [bd pack's Dolt commands](../../../examples/bd/dolt/commands), `gc-beads-bd.sh`, `gc-beads-br`, `setup-isolated-dolt.sh` and `launch-dolt-baseline.sh` | Manage a ledger or start, maintain and query database processes. |
| `smoke-macos.sh`, `test-docker-session`, `worker_conformance_grid.py` | Download a release and initialize a sandbox city, or exercise provider infrastructure. |
| `docs-autofix-push.sh` and `bump-version.sh` | Can push branches or tags to a remote. |
