#!/usr/bin/env python3
"""First-time setup for handoff.

Run it yourself from a regular terminal, not through Claude: its sandbox blocks these writes,
and `!` commands can be sandboxed too. Every step is safe to re-run.

    --yes   make changes (shell profile PATH line, ~/.claude/settings.json) without asking
"""

import difflib
import json
import os
import shutil
import subprocess
import sys
import tempfile
from datetime import datetime
from pathlib import Path

QUEUE_ENTRY = "~/.handoff/queue"
# allowWrite entries that would also expose the hook-owned sessions/ and snap/ folders.
BROAD_ENTRIES = ("~", "~/", "~/.handoff", "~/.handoff/")
# Viewer-only folders the agent must not read either: snap/ holds copies of files the viewer read for review (an edit's
# target, attachments), review/ checkouts of repos the agent may not be able to read.
DENY_READ = ("~/.handoff/snap", "~/.handoff/review")


class SetupError(Exception):
    pass


SANDBOX_HINT = ("If you ran this from Claude Code with `!`, Claude's sandbox may have blocked it. "
                "Run it from a regular terminal instead.")


def require_writable(path):
    """Fail early, with a useful message, instead of halfway through a build."""
    try:
        tempfile.TemporaryFile(dir=path).close()
    except OSError as e:
        raise SetupError(f"can't write to {path} ({e.strerror}). {SANDBOX_HINT}") from e


def add_allow_write(settings, entry, home):
    """Add entry to sandbox.filesystem.allowWrite in place.

    Returns (changed, broad): whether entry was added, and any over-broad entries already present.
    """
    allow = settings.setdefault("sandbox", {}).setdefault("filesystem", {}).setdefault("allowWrite", [])
    expanded = entry.replace("~", str(home), 1)
    broad_forms = set(BROAD_ENTRIES) | {str(home), str(home / ".handoff")}
    broad = [e for e in allow if e in broad_forms]
    if entry in allow or expanded in allow:
        return False, broad
    allow.append(entry)
    return True, broad


def add_deny_read(settings, home):
    """Add DENY_READ to sandbox.filesystem.denyRead and as Read(...) deny rules in place: denyRead stops sandboxed
    commands only, and Claude's Read tool follows permission rules. Returns whether any entry was added."""
    deny = settings.setdefault("sandbox", {}).setdefault("filesystem", {}).setdefault("denyRead", [])
    added = [e for e in DENY_READ if e not in deny and e.replace("~", str(home), 1) not in deny]
    deny.extend(added)
    rules = settings.setdefault("permissions", {}).setdefault("deny", [])
    new_rules = [f"Read({e}/**)" for e in DENY_READ if f"Read({e}/**)" not in rules]
    rules.extend(new_rules)
    return bool(added or new_rules)


class Setup:
    def __init__(self, repo, home, assume_yes=False, run=subprocess.run, ask=input, out=None):
        self.repo = Path(repo)
        self.home = Path(home)
        self.settings_path = self.home / ".claude" / "settings.json"
        self.assume_yes = assume_yes
        self._run = run
        self._ask = ask
        # Flush so our lines stay in order with the output of the commands we run.
        self._out = out or (lambda text: print(text, flush=True))
        self._color = sys.stdout.isatty()

    # -- output helpers

    def _style(self, code, text):
        return f"\033[{code}m{text}\033[0m" if self._color else text

    def step(self, title):
        self._out("\n" + self._style("1", title))

    def warn(self, text):
        self._out(self._style("33", "warning: ") + text)

    def cmd(self, *argv, capture=True):
        return self._run(list(argv), check=True, text=True, capture_output=capture)

    # -- steps

    def run(self):
        self.check_go()
        self.build()
        self.make_queue()
        self.allow_queue_writes()
        self.install_plugin()
        self.print_check()

    def check_go(self):
        self.step("1. Go")
        if not shutil.which("go"):
            raise SetupError("Go isn't installed. Install it (e.g. 'brew install go') and re-run.")
        self._out(self.cmd("go", "version").stdout.strip())

    def build(self):
        self.step("2. Build the binary")
        bindir = self.cmd("go", "env", "GOBIN").stdout.strip() or str(Path(self.cmd("go", "env", "GOPATH").stdout.strip()) / "bin")
        Path(bindir).mkdir(parents=True, exist_ok=True)
        require_writable(bindir)
        self._run(["go", "build", "-o", str(Path(bindir) / "handoff"), "./cmd/handoff"], check=True, cwd=self.repo)
        self._out(f"installed {bindir}/handoff")
        self.ensure_on_path(bindir)

    def ensure_on_path(self, bindir):
        """Offer to add bindir to the shell profile; the hooks call `handoff` by name."""
        if bindir in os.environ.get("PATH", "").split(os.pathsep):
            return
        shown = "$HOME" + bindir[len(str(self.home)):] if bindir.startswith(str(self.home) + "/") else bindir
        line = f'export PATH="{shown}:$PATH"'
        restart = "Open a new terminal and restart Claude Code from it, so the hooks see the new PATH."
        shell = Path(os.environ.get("SHELL", "")).name
        profile = {"zsh": ".zshrc", "bash": ".bash_profile" if sys.platform == "darwin" else ".bashrc"}.get(shell)
        if profile is None:
            self.warn(f"{bindir} is not on PATH. Add it in your shell's startup file, then: {restart}")
            return
        profile = self.home / profile
        if profile.exists() and line in profile.read_text():
            self._out(f"PATH line already in {profile}. {restart}")
            return
        if not self.assume_yes and not self._ask(f"{bindir} is not on PATH. Add `{line}` to {profile}? [y/N] ").strip().lower().startswith("y"):
            self.warn(f"{bindir} is not on PATH. Add `{line}` to {profile} yourself, then: {restart}")
            return
        with profile.open("a") as f:
            f.write(f"\n# added by handoff setup\n{line}\n")
        self._out(f"added `{line}` to {profile}. {restart}")

    def make_queue(self):
        self.step("3. Queue directory")
        queue = self.home / ".handoff" / "queue"
        queue.mkdir(parents=True, exist_ok=True)
        for d in (queue.parent, queue):
            d.chmod(0o700)
        self._out("created ~/.handoff/queue")

    def allow_queue_writes(self):
        self.step("4. Agent access: write only the queue, read neither snap/ nor review/")
        path = self.settings_path
        before = path.read_text() if path.exists() else ""
        settings = json.loads(before) if before.strip() else {}
        changed, broad = add_allow_write(settings, QUEUE_ENTRY, self.home)
        changed = add_deny_read(settings, self.home) or changed
        if broad:
            self.warn(
                f"allowWrite contains {', '.join(broad)}. That lets the agent write the hook-owned\n"
                f"  sessions/ and snap/ folders and forge provenance. Narrow it to {QUEUE_ENTRY}."
            )
        if not changed:
            self._out(f"{QUEUE_ENTRY} is already in allowWrite, and {' and '.join(DENY_READ)} are denied reads")
            return

        # json keeps key order, so the diff shows only the added entry (plus any reformatting).
        after = json.dumps(settings, indent=2) + "\n"
        if before:
            self._out("".join(difflib.unified_diff(
                before.splitlines(keepends=True), after.splitlines(keepends=True), str(path), str(path) + " (new)")))
        if not self.assume_yes and not self._ask(f"Apply this change to {path}? [y/N] ").strip().lower().startswith("y"):
            raise SetupError(f'not changed. Add "{QUEUE_ENTRY}" to sandbox.filesystem.allowWrite and '
                             f'{json.dumps(list(DENY_READ))} to sandbox.filesystem.denyRead (and Read(<path>/**) to permissions.deny) yourself.')

        path.parent.mkdir(parents=True, exist_ok=True)
        if path.exists():
            backup = path.with_name(f"{path.name}.bak.{datetime.now():%Y%m%d%H%M%S}")
            shutil.copy2(path, backup)
            self._out(f"backup: {backup}")
        tmp = path.with_name(path.name + ".handoff-tmp")
        tmp.write_text(after)
        os.replace(tmp, path)  # atomic, so a crash can't leave half a settings file
        self._out(f"saved: {QUEUE_ENTRY} writable, {' and '.join(DENY_READ)} unreadable")

    def install_plugin(self):
        self.step("5. Claude Code plugin")
        manual = f"claude plugin marketplace add {self.repo} && claude plugin install handoff@handoff --scope user"
        if not shutil.which("claude"):
            self._out(f"claude isn't on PATH. Install the plugin later with:\n  {manual}")
            return
        marketplaces = self.cmd("claude", "plugin", "marketplace", "list").stdout
        if any(line.strip().split()[-1:] == ["handoff"] for line in marketplaces.splitlines()):
            self._out("marketplace 'handoff' already added")
        else:
            self.cmd("claude", "plugin", "marketplace", "add", str(self.repo), capture=False)
        if "handoff@handoff" in self.cmd("claude", "plugin", "list").stdout:
            self._out("plugin handoff@handoff already installed")
        else:
            self.cmd("claude", "plugin", "install", "handoff@handoff", "--scope", "user", capture=False)

    def print_check(self):
        self.step("6. Check it works")
        self._out(
            "Start a new Claude Code session (the plugin's hooks run at session start). Ask Claude to stage a\n"
            "harmless command, e.g.\n"
            '  handoff add --wait --desc "check login" -- gh auth status\n'
            "and run `! handoff`. The item's session should show \"✓ verified\" with your prompt, and running\n"
            "it should wake Claude with the output. \"⚠ unverified\" means the SessionStart hook didn't\n"
            "run: check that `which handoff` works in a new terminal, and restart Claude Code itself."
        )


def main(argv):
    if any(a not in ("--yes",) for a in argv):
        print(__doc__.strip(), file=sys.stderr)
        return 2
    setup = Setup(repo=Path(__file__).resolve().parent.parent, home=Path.home(), assume_yes="--yes" in argv)
    try:
        setup.run()
    except SetupError as e:
        print(setup._style("31", "error: ") + str(e), file=sys.stderr)
        return 1
    except PermissionError as e:
        print(setup._style("31", "error: ") + f"{e}. {SANDBOX_HINT}", file=sys.stderr)
        return 1
    except subprocess.CalledProcessError as e:
        print(setup._style("31", "error: ") + f"{' '.join(e.cmd)} failed ({e.returncode})", file=sys.stderr)
        if e.stderr:
            print(e.stderr, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
