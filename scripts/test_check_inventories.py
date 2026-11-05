"""Small proofs for inventory drift, using synthetic lists and BUILD copies."""

import importlib.util
import os
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "check_inventories", Path(__file__).with_name("check-inventories.py")
)
check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check)


class ShardInventoryTests(unittest.TestCase):
    def setUp(self):
        self.arrays = {
            "formula_tests": ["TestBasic", "TestRetry", "TestRecovery"],
            "review_formulas_basic_tests": ["TestBasic"],
            "review_formulas_retry_tests": ["TestRetry"],
            "review_formulas_recovery_tests": ["TestRecovery"],
            "rest_smoke_tests": ["TestSmoke"],
            "cmd_gc_integration_tests": ["TestTagged", "TestLinuxOnly"],
        }
        self.integration = {
            "TestBasic", "TestRetry", "TestRecovery", "TestSmoke", "TestDynamic",
            "TestBdStoreConformance", "TestBdStoreMailWispInsert",
        }

    def drift(self):
        return check.shard_drift(
            self.arrays, self.integration, {"TestUnit", "TestTagged"}, {"TestUnit"}
        )

    def test_dynamic_rest_and_platform_specific_manifest_are_valid(self):
        self.assertEqual(self.drift(), [])

    def test_missing_tagged_test_names_its_inventory(self):
        self.arrays["cmd_gc_integration_tests"].remove("TestTagged")
        self.assertIn("unassigned cmd/gc integration test: TestTagged", self.drift())

    def test_formula_partition_rejects_missing_duplicate_and_stale_names(self):
        for replacement, diagnostic in (
            ([], "unassigned formula test: TestBasic"),
            (["TestBasic", "TestRetry"], "duplicate formula route: TestRetry"),
            (["TestGone"], "stale review_formulas_basic_tests entry: TestGone"),
        ):
            with self.subTest(replacement=replacement):
                self.arrays["review_formulas_basic_tests"] = replacement
                self.assertIn(diagnostic, self.drift())

    def test_duplicate_smoke_and_formula_route_fails(self):
        self.arrays["rest_smoke_tests"].append("TestBasic")
        self.assertIn("duplicate integration route: TestBasic", self.drift())

    def test_deleted_smoke_test_fails(self):
        self.integration.remove("TestSmoke")
        self.assertIn("stale rest_smoke_tests entry: TestSmoke", self.drift())

    def test_duplicate_manifest_entry_fails(self):
        self.arrays["cmd_gc_integration_tests"].append("TestTagged")
        self.assertIn("duplicate cmd_gc_integration_tests entry: TestTagged", self.drift())

    def test_array_parser_rejects_missing_or_executable_entries(self):
        source = "\n".join(f"{name}=(\n  {' '.join(names)}\n)" for name, names in self.arrays.items())
        self.assertEqual(check.parse_arrays(source), self.arrays)
        for broken in ("", source.replace("TestTagged", '"$(false)"')):
            with self.subTest(broken=broken), self.assertRaises(check.InventoryError):
                check.parse_arrays(broken)


class BuildInventoryTests(unittest.TestCase):
    def test_combined_generator_checks_copies_and_names_drift(self):
        with tempfile.TemporaryDirectory(prefix="inventory-test-", dir="/var/tmp") as directory:
            root = Path(directory)
            (root / "BUILD.bazel").write_text("original\n")
            (root / "MODULE.bazel").write_text("")

            def generate(command, cwd, **kwargs):
                if command[:2] == ["git", "ls-files"]:
                    return mock.Mock(stdout="BUILD.bazel\0", returncode=0)
                if command[0] == "bazel":
                    return mock.Mock(stdout="", returncode=0)
                if command[0] == check.sys.executable:
                    self.assertNotEqual(cwd, root)
                    (cwd / "BUILD.bazel").write_text("generated\n")
                    return mock.Mock(stdout="", returncode=0)
                self.fail(f"unexpected command: {command}")

            with mock.patch.object(check.shutil, "which", return_value="bazel"), mock.patch.object(check, "run", side_effect=generate):
                with self.assertRaisesRegex(check.InventoryError, "BUILD.bazel"):
                    check.check_build(root)
            self.assertEqual((root / "BUILD.bazel").read_text(), "original\n")

    def test_failed_discovery_never_becomes_an_empty_inventory(self):
        with self.assertRaises(check.InventoryError):
            check.run([check.sys.executable, "-c", "raise SystemExit(7)"], Path.cwd())

    def test_timeout_reaps_the_discovery_process_group(self):
        process = mock.Mock(pid=123)
        process.communicate.side_effect = [subprocess.TimeoutExpired("go", 60), ("partial output", None)]
        with mock.patch.object(check.subprocess, "Popen", return_value=process) as start, mock.patch.object(check.os, "killpg") as kill:
            with self.assertRaisesRegex(check.InventoryError, "(?s)timed out.*partial output"):
                check.run(["go", "test", "-list", "^Test"], Path.cwd())
            self.assertTrue(start.call_args.kwargs["start_new_session"])
            kill.assert_called_once_with(123, check.signal.SIGKILL)
            self.assertEqual(process.communicate.call_count, 2)

    def test_absent_bazel_is_explicit_and_generator_still_runs(self):
        with tempfile.TemporaryDirectory(prefix="inventory-test-", dir="/var/tmp") as directory:
            root = Path(directory)
            (root / "BUILD.bazel").write_text("original\n")
            (root / "MODULE.bazel").write_text("")
            with mock.patch.object(check.shutil, "which", return_value=None), mock.patch.object(check, "run", return_value=mock.Mock(stdout="BUILD.bazel\0", returncode=0)) as run:
                self.assertIn("bazel absent: BUILD sync left to CI", check.check_build(root))
                self.assertEqual(run.call_args.args[0], [check.sys.executable, str(root / "tools/bazel/repo_tree.py")])

    def test_bazel_failure_without_patch_cannot_pass(self):
        with tempfile.TemporaryDirectory(prefix="inventory-test-", dir="/var/tmp") as directory:
            root = Path(directory)
            (root / "BUILD.bazel").write_text("original\n")
            (root / "MODULE.bazel").write_text("")
            with mock.patch.object(check.shutil, "which", return_value="bazel"), mock.patch.object(check, "run", side_effect=[mock.Mock(stdout="BUILD.bazel\0"), mock.Mock(stdout="failed to build", returncode=1)]):
                with self.assertRaisesRegex(check.InventoryError, "Gazelle failed without a diff"):
                    check.check_build(root)


class HookInventoryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="inventory-hook-", dir="/var/tmp")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = Path(__file__).resolve().parent.parent
        self.env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        self.env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL="/dev/null")
        self.command(["git", "init", "-q", "--template="])
        self.write(".githooks/lib/beads-chain.sh", "#!/bin/sh\nexit 0\n", executable=True)
        self.write("scripts/push-ownership-guard.sh", "assert_bead_still_claimed() { return 0; }\n")
        self.write("bin/make", '#!/bin/sh\nprintf "%s\\n" "$*" >> make.calls\ncase "$*" in *check-inventories*) exit 42;; esac\n', executable=True)
        self.env["PATH"] = str(self.root / "bin") + os.pathsep + self.env["PATH"]

    def command(self, command, **kwargs):
        return subprocess.run(command, cwd=self.root, env=self.env, text=True, capture_output=True, check=True, **kwargs).stdout

    def write(self, name, contents="fixture\n", executable=False):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(contents)
        if executable:
            path.chmod(0o755)

    def commit(self):
        self.command(["git", "-c", "user.name=Inventory Test", "-c", "user.email=inventory@example.invalid", "commit", "-qm", "fixture"])
        return self.command(["git", "rev-parse", "HEAD"]).strip()

    def hook(self, name, input=None):
        return subprocess.run(["bash", str(self.repo / ".githooks" / name)], cwd=self.root, env=self.env, text=True, capture_output=True, input=input, check=False)

    def test_precommit_inventory_paths_fail_before_expensive_gates(self):
        for name in ("pkg/new_test.go", "pkg/BUILD.bazel", "scripts/test-integration-shard", "test/test-resources.toml", "TESTING.md"):
            with self.subTest(name=name):
                self.write(name)
                self.command(["git", "add", name])
                result = self.hook("pre-commit")
                self.assertEqual(result.returncode, 42, result.stdout + result.stderr)
                self.assertEqual((self.root / "make.calls").read_text().splitlines(), ["--no-print-directory check-inventories"])
                (self.root / "make.calls").unlink()
                self.command(["git", "rm", "--cached", name])

    def test_precommit_deleted_test_still_checks_inventory(self):
        self.write("pkg/old_test.go")
        self.command(["git", "add", "pkg/old_test.go"])
        self.commit()
        self.command(["git", "rm", "pkg/old_test.go"])
        self.assertEqual(self.hook("pre-commit").returncode, 42)

    def test_precommit_unrelated_file_skips_inventory(self):
        self.write("unrelated.txt")
        self.command(["git", "add", "unrelated.txt"])
        self.assertEqual(self.hook("pre-commit").returncode, 0)
        self.assertFalse((self.root / "make.calls").exists())

    def test_push_checks_inventory_even_without_go_changes(self):
        self.write("unrelated.txt")
        self.command(["git", "add", "unrelated.txt"])
        base = self.commit()
        self.write("unrelated.txt", "changed\n")
        self.command(["git", "add", "unrelated.txt"])
        head = self.commit()
        result = self.hook("pre-push", f"refs/heads/topic {head} refs/heads/topic {base}\n")
        self.assertEqual(result.returncode, 42, result.stdout + result.stderr)
        self.assertEqual((self.root / "make.calls").read_text().splitlines(), ["--no-print-directory check-inventories"])


if __name__ == "__main__":
    unittest.main()
