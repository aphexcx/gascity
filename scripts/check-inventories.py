#!/usr/bin/env python3
"""Check generated BUILD files and the hand-maintained Go test inventories."""

from collections import Counter
from pathlib import Path
import os
import re
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile


ARRAYS = (
    "formula_tests",
    "review_formulas_basic_tests",
    "review_formulas_retry_tests",
    "review_formulas_recovery_tests",
    "rest_smoke_tests",
    "cmd_gc_integration_tests",
)


class InventoryError(Exception):
    """An inventory differs from its source or cannot be checked."""


def run(command, cwd, allowed=(0,)):
    try:
        process = subprocess.Popen(
            command, cwd=cwd, text=True, stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT, start_new_session=True,
        )
    except OSError as error:
        raise InventoryError(f"{shlex.join(command)}: {error}") from error
    try:
        output, _ = process.communicate(timeout=60)
    except (subprocess.TimeoutExpired, KeyboardInterrupt):
        # Discovery can start a test binary and its TestMain children. Reap
        # this command's own process group rather than leaving those behind.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        output, _ = process.communicate()
        raise InventoryError(f"{shlex.join(command)} interrupted or timed out after 60s\n{output}") from None
    result = subprocess.CompletedProcess(command, process.returncode, output)
    if result.returncode not in allowed:
        raise InventoryError(
            f"{shlex.join(command)} exited {result.returncode}\n{result.stdout}"
        )
    return result


def parse_arrays(source):
    arrays = {}
    for name in ARRAYS:
        match = re.search(r"^" + name + r"=\(\n(.*?)^\)", source, re.M | re.S)
        if match is None:
            raise InventoryError(f"missing shard array: {name}")
        names = shlex.split(match[1], comments=True)
        if not names or any(not re.fullmatch(r"Test[A-Z_0-9]\w*", item) for item in names):
            raise InventoryError(f"invalid or empty shard array: {name}")
        arrays[name] = names
    return arrays


def shard_drift(arrays, integration, cmd_tagged, cmd_untagged):
    drift = []
    for name, names in arrays.items():
        for test, count in sorted(Counter(names).items()):
            if count > 1:
                drift.append(f"duplicate {name} entry: {test}")
        if name != "cmd_gc_integration_tests":
            drift.extend(f"stale {name} entry: {test}" for test in sorted(set(names) - integration))

    formulas = arrays["formula_tests"]
    routed = sum((arrays[name] for name in ARRAYS[1:4]), [])
    for test, count in sorted(Counter(routed).items()):
        if count > 1:
            drift.append(f"duplicate formula route: {test}")
    drift.extend(f"unassigned formula test: {test}" for test in sorted(set(formulas) - set(routed)))
    drift.extend(f"formula route absent from formula_tests: {test}" for test in sorted(set(routed) - set(formulas)))

    # Full-rest discovers the complement dynamically; only the explicit buckets
    # can omit or duplicate a route. The bdstore bucket is fixed in the runner.
    explicit = formulas + arrays["rest_smoke_tests"] + ["TestBdStoreConformance", "TestBdStoreMailWispInsert"]
    drift.extend(f"stale integration route: {test}" for test in sorted(set(explicit) - integration))
    for test, count in sorted(Counter(explicit).items()):
        if count > 1:
            drift.append(f"duplicate integration route: {test}")

    # Linux-only entries may be absent on this host. The canonical Linux AST
    # guard below checks stale entries; live discovery also catches host-only
    # tagged additions that are invisible to the Linux guard.
    missing = cmd_tagged - cmd_untagged - set(arrays["cmd_gc_integration_tests"])
    drift.extend(f"unassigned cmd/gc integration test: {test}" for test in sorted(missing))
    return drift


def check_build(root):
    listed = run(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", "*BUILD.bazel"], root).stdout
    paths = sorted(set(Path(name) for name in listed.split("\0") if name))
    original = {path: (root / path).read_bytes() for path in paths if (root / path).exists()}
    bazel = shutil.which("bazel")
    # Only BUILD files are copied. Gazelle reads the real sources in diff mode;
    # repo_tree operates on the projected BUILD tree, never on the checkout.
    with tempfile.TemporaryDirectory(prefix="gc-inventories-", dir="/var/tmp") as directory:
        projected = Path(directory)
        for path, content in original.items():
            (projected / path).parent.mkdir(parents=True, exist_ok=True)
            (projected / path).write_bytes(content)
        (projected / "MODULE.bazel").write_bytes((root / "MODULE.bazel").read_bytes())
        if bazel:
            patch = projected / "gazelle.patch"
            result = run([bazel, "run", "//:gazelle", "--", "-mode=diff", f"-patch={patch}"], root, allowed=(0, 1))
            has_patch = patch.exists() and patch.stat().st_size > 0
            if result.returncode and not has_patch:
                raise InventoryError(f"Gazelle failed without a diff:\n{result.stdout}")
            if has_patch:
                run(["git", "apply", "-p0", str(patch)], projected)
        run([sys.executable, str(root / "tools/bazel/repo_tree.py")], projected)
        generated = {path.relative_to(projected): path.read_bytes() for path in projected.rglob("BUILD.bazel")}
        drift = sorted(str(path) for path in original.keys() | generated.keys() if original.get(path) != generated.get(path))
        if drift:
            raise InventoryError("BUILD drift (run make bazel-sync):\n" + "\n".join(drift))
    if not bazel:
        return "skipped (bazel absent: BUILD sync left to CI; repo_tree ok)"
    return "ok"


def list_tests(root, package, tagged):
    command = ["go", "test", "-list", "^Test", "-run", "^$", "-count=1"]
    if tagged:
        command += ["-tags", "integration"]
    if package == "./test/integration":
        # Listing must work without tmux, gc, or any other runtime executable.
        # The Go driver still has its normal PATH for compilation and linking.
        command += ["-exec", "env PATH="]
    output = run(command + [package], root).stdout
    names = {line for line in output.splitlines() if re.fullmatch(r"Test\w+", line)}
    if not names:
        raise InventoryError(f"empty test inventory: {package}, integration={tagged}\n{output}")
    return names


def check_shards(root):
    arrays = parse_arrays((root / "scripts/test-integration-shard").read_text())
    drift = shard_drift(
        arrays, list_tests(root, "./test/integration", True),
        list_tests(root, "./cmd/gc", True), list_tests(root, "./cmd/gc", False),
    )
    if drift:
        raise InventoryError("shards drift:\n" + "\n".join(drift))
    run([
        "go", "test", "./scripts", "-count=1", "-timeout=30s", "-run",
        "^(TestCmdGCIntegrationManifestMatchesTaggedDeclarations|TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory)$",
    ], root)


def main():
    root = Path(__file__).resolve().parent.parent
    os.environ["GC_FAST_UNIT"] = "1"
    phase = "checker tests"
    try:
        run([sys.executable, "-m", "unittest", "discover", "-s", "scripts", "-p", "test_check_inventories.py"], root)
        phase = "BUILD"
        build = check_build(root)
        phase = "shards"
        check_shards(root)
        phase = "census"
        run([
            "go", "test", "./internal/testpolicy/resourcecensus", "-count=1", "-timeout=30s",
            "-run", "^TestRepositoryLedgerMatchesCensusAndDocumentation$",
        ], root)
    except (InventoryError, OSError, ValueError) as error:
        print(f"inventories: {phase} failed: {error}", file=sys.stderr)
        return 1
    print(f"inventories: BUILD {build}, shards ok, census ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
