import runpy
import tempfile
import unittest
from pathlib import Path
from unittest import mock


sync = runpy.run_path(str(Path(__file__).with_name("update-bundled-gastown-pack")))


class PinnedRegistryReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.registry = Path(self.temp.name) / "registry.toml"
        self.old = "a" * 40
        self.new = "b" * 40
        self.registry.write_text(
            '[[pack]]\n'
            + 'name = "gastown"\nsource = "https://example.com/packs/tree/main/gastown"\n'
            + self.release("0.1.0", self.old)
            + self.release("0.2.0", self.new),
            encoding="utf-8",
        )

    def release(self, version, commit, withdrawn=False):
        return (
            f'[[pack.release]]\nversion = "{version}"\ncommit = "{commit}"\n'
            f'ref = "main"\nhash = "sha256:{commit}"\n'
            f'withdrawn = {str(withdrawn).lower()}\n'
        )

    def select(self, pin=None):
        return sync["registry_release"](self.registry, pin=pin)

    def test_latest_mode_still_selects_newest_release(self):
        self.assertEqual(self.select()["commit"], self.new)

    def test_pinned_mode_does_not_follow_newer_release(self):
        self.assertEqual(self.select("sha:" + self.old)["version"], "0.1.0")

    def test_unknown_or_malformed_pin_fails_closed(self):
        for pin in ("", "main", "sha:abc", "sha:" + "c" * 40):
            with self.subTest(pin=pin), self.assertRaises(SystemExit):
                self.select(pin)

    def test_withdrawn_pin_is_rejected(self):
        self.registry.write_text(
            '[[pack]]\nname = "gastown"\nsource = "example"\n'
            + self.release("0.1.0", self.old, withdrawn=True)
            + self.release("0.2.0", self.new), encoding="utf-8",
        )
        with self.assertRaises(SystemExit):
            self.select("sha:" + self.old)

    def test_ambiguous_pin_is_rejected(self):
        with self.registry.open("a") as handle:
            handle.write(self.release("0.3.0", self.old))
        with self.assertRaises(SystemExit):
            self.select("sha:" + self.old)

    def test_check_modes_are_mutually_exclusive(self):
        with mock.patch("sys.argv", ["sync", "--check", "--check-pinned"]):
            with self.assertRaises(SystemExit) as error:
                sync["parse_args"]()
        self.assertEqual(error.exception.code, 2)


if __name__ == "__main__":
    unittest.main()
