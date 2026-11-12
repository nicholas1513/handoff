"""Tests for setup.py. Run: python3 -m unittest discover -s scripts"""

import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import setup
from setup import DENY_READ, QUEUE_ENTRY, Setup, SetupError, add_allow_write, add_deny_read


class AddAllowWriteTest(unittest.TestCase):
    home = Path("/home/u")

    def test_adds_and_keeps_order(self):
        s = {"model": "x", "sandbox": {"enabled": True, "filesystem": {"allowWrite": ["~/.gradle"]}}, "voice": False}
        self.assertEqual(add_allow_write(s, QUEUE_ENTRY, self.home), (True, []))
        self.assertEqual(list(s), ["model", "sandbox", "voice"])
        self.assertEqual(s["sandbox"]["filesystem"]["allowWrite"], ["~/.gradle", QUEUE_ENTRY])

    def test_idempotent_including_expanded_form(self):
        for present in (QUEUE_ENTRY, "/home/u/.handoff/queue"):
            s = {"sandbox": {"filesystem": {"allowWrite": [present]}}}
            self.assertEqual(add_allow_write(s, QUEUE_ENTRY, self.home), (False, []))

    def test_reports_broad_entries(self):
        s = {"sandbox": {"filesystem": {"allowWrite": ["~/.handoff", "/home/u"]}}}
        _, broad = add_allow_write(s, QUEUE_ENTRY, self.home)
        self.assertEqual(broad, ["~/.handoff", "/home/u"])

    def test_creates_missing_sections(self):
        s = {}
        add_allow_write(s, QUEUE_ENTRY, self.home)
        self.assertEqual(s, {"sandbox": {"filesystem": {"allowWrite": [QUEUE_ENTRY]}}})


def fake_run(outputs):
    """A subprocess.run stand-in that records calls and returns canned stdout per command prefix."""
    calls = []

    def run(argv, **kw):
        calls.append(argv)
        for prefix, out in outputs.items():
            if tuple(argv[: len(prefix)]) == prefix:
                return subprocess.CompletedProcess(argv, 0, stdout=out, stderr="")
        return subprocess.CompletedProcess(argv, 0, stdout="", stderr="")

    return run, calls


class AddDenyReadTest(unittest.TestCase):
    home = Path("/home/u")

    def test_adds_once_including_expanded_form(self):
        s = {"sandbox": {"filesystem": {"denyRead": ["~/.ssh", "/home/u/.handoff/snap"]}}}
        self.assertTrue(add_deny_read(s, self.home))
        self.assertEqual(s["sandbox"]["filesystem"]["denyRead"], ["~/.ssh", "/home/u/.handoff/snap", "~/.handoff/review"])
        self.assertEqual(s["permissions"]["deny"], ["Read(~/.handoff/snap/**)", "Read(~/.handoff/review/**)"])
        self.assertFalse(add_deny_read(s, self.home))

    def test_read_tool_rule_added_even_when_sandbox_entries_exist(self):
        # denyRead stops sandboxed commands only; the Read tool follows permission rules
        s = {"sandbox": {"filesystem": {"denyRead": list(DENY_READ)}}, "permissions": {"deny": ["Read(~/.ssh/**)"]}}
        self.assertTrue(add_deny_read(s, self.home))
        self.assertEqual(s["permissions"]["deny"], ["Read(~/.ssh/**)", "Read(~/.handoff/snap/**)", "Read(~/.handoff/review/**)"])


class SetupStepsTest(unittest.TestCase):
    def setUp(self):
        self.home = Path(tempfile.mkdtemp())
        self.settings = self.home / ".claude" / "settings.json"
        self.out = []

    def make(self, answer="y", run=None, yes=False):
        return Setup("/repo", self.home, assume_yes=yes, run=run or fake_run({})[0],
                     ask=lambda _: answer, out=self.out.append)

    def test_settings_applied_with_backup(self):
        self.settings.parent.mkdir()
        self.settings.write_text('{"model": "x"}')
        self.make().allow_queue_writes()
        fs = json.loads(self.settings.read_text())["sandbox"]["filesystem"]
        self.assertEqual((fs["allowWrite"], fs["denyRead"]), ([QUEUE_ENTRY], list(DENY_READ)))
        self.assertEqual(json.loads(self.settings.read_text())["permissions"]["deny"],
                         ["Read(~/.handoff/snap/**)", "Read(~/.handoff/review/**)"])
        self.assertEqual(len(list(self.settings.parent.glob("settings.json.bak.*"))), 1)
        self.assertEqual(list(self.settings.parent.glob("*.handoff-tmp")), [])

    def test_decline_leaves_file_untouched(self):
        self.settings.parent.mkdir()
        self.settings.write_text('{"model": "x"}')
        with self.assertRaises(SetupError):
            self.make(answer="n").allow_queue_writes()
        self.assertEqual(self.settings.read_text(), '{"model": "x"}')
        self.assertEqual(list(self.settings.parent.glob("settings.json.*")), [])

    def test_rerun_is_noop(self):
        self.make(yes=True).allow_queue_writes()
        first = self.settings.read_text()
        self.make(answer="n").allow_queue_writes()  # would raise if it asked again
        self.assertEqual(self.settings.read_text(), first)

    def test_missing_settings_file_created(self):
        self.make(yes=True).allow_queue_writes()
        self.assertIn(QUEUE_ENTRY, self.settings.read_text())

    def test_make_queue_is_private(self):
        self.make().make_queue()
        self.assertEqual((self.home / ".handoff" / "queue").stat().st_mode & 0o777, 0o700)

    def test_plugin_installed_when_absent(self):
        run, calls = fake_run({("claude", "plugin", "marketplace", "list"): "  ❯ other-market\n"})
        with mock.patch.object(setup.shutil, "which", return_value="/bin/claude"):
            self.make(run=run).install_plugin()
        self.assertIn(["claude", "plugin", "marketplace", "add", "/repo"], calls)
        self.assertIn(["claude", "plugin", "install", "handoff@handoff", "--scope", "user"], calls)

    def test_plugin_skipped_when_present(self):
        run, calls = fake_run({
            ("claude", "plugin", "marketplace", "list"): "Configured marketplaces:\n\n  ❯ handoff\n    Source: Directory\n",
            ("claude", "plugin", "list"): "  ❯ handoff@handoff\n    Status: ✔ enabled\n",
        })
        with mock.patch.object(setup.shutil, "which", return_value="/bin/claude"):
            self.make(run=run).install_plugin()
        self.assertEqual([c for c in calls if "install" in c or "add" in c], [])


class WritableTest(unittest.TestCase):
    def test_unwritable_dir_gives_sandbox_hint(self):
        d = Path(tempfile.mkdtemp())
        d.chmod(0o500)
        self.addCleanup(d.chmod, 0o700)
        with self.assertRaises(SetupError) as cm:
            setup.require_writable(d)
        self.assertIn("regular terminal", str(cm.exception))

    def test_writable_dir_passes_and_leaves_nothing(self):
        d = Path(tempfile.mkdtemp())
        setup.require_writable(d)
        self.assertEqual(list(d.iterdir()), [])


class EnsureOnPathTest(unittest.TestCase):
    def setUp(self):
        self.home = Path(tempfile.mkdtemp())
        self.bindir = str(self.home / "go" / "bin")
        self.out = []

    def run_it(self, shell="/bin/zsh", path="/usr/bin", answer="y", yes=False):
        s = Setup("/repo", self.home, assume_yes=yes, ask=lambda _: answer, out=self.out.append)
        with mock.patch.dict(setup.os.environ, {"SHELL": shell, "PATH": path}):
            s.ensure_on_path(self.bindir)

    def test_already_on_path_does_nothing(self):
        self.run_it(path="/usr/bin:" + self.bindir, answer="n")
        self.assertFalse((self.home / ".zshrc").exists())

    def test_appends_to_zshrc_once(self):
        (self.home / ".zshrc").write_text("export FOO=1\n")
        self.run_it()
        self.run_it(answer="n")  # second run must not ask or append again
        text = (self.home / ".zshrc").read_text()
        self.assertTrue(text.startswith("export FOO=1\n"))
        self.assertEqual(text.count('export PATH="$HOME/go/bin:$PATH"'), 1)

    def test_bash_uses_bash_profile_on_macos(self):
        with mock.patch.object(setup.sys, "platform", "darwin"):
            self.run_it(shell="/bin/bash", yes=True)
        self.assertIn("$HOME/go/bin", (self.home / ".bash_profile").read_text())

    def test_decline_leaves_profile_alone(self):
        self.run_it(answer="n")
        self.assertFalse((self.home / ".zshrc").exists())
        self.assertTrue(any("not on PATH" in line for line in self.out))

    def test_unknown_shell_prints_instructions(self):
        self.run_it(shell="/usr/bin/fish", yes=True)
        self.assertEqual(list(self.home.iterdir()), [])
        self.assertTrue(any("PATH" in line for line in self.out))


if __name__ == "__main__":
    unittest.main()
