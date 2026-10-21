# handoff

A review-and-run queue for commands an agent can't run from its sandbox. The agent stages the
exact command; you review it in a terminal viewer (what it does, which session and prompt it came
from, which Claude rule applies) and run it as yourself. The agent gets the result back.

handoff never reads, stores or passes on credentials. It runs CLIs you're already logged into.

## First-time setup

Install Go (1.23 or newer, e.g. `brew install go`) and have `python3` (3.8 or newer; macOS ships it
with the Xcode command-line tools), then run the setup script **yourself, not
through Claude**. Claude's sandbox blocks what it does: building into `~/go/bin`, creating
`~/.handoff`, and editing `~/.claude/settings.json`. From inside a Claude Code session, run it with
`!`, which runs outside the sandbox:

```sh
./scripts/setup.py          # or: ! ./scripts/setup.py
```

It does the following, and is safe to re-run:
1. Builds `handoff` into `$(go env GOPATH)/bin`. If that folder isn't on PATH, it offers to add
   `export PATH="$HOME/go/bin:$PATH"` to `~/.zshrc` (zsh), or to `~/.bash_profile` (bash on macOS)
   or `~/.bashrc` (bash elsewhere). For other shells it prints what to add. The plugin hooks call
   `handoff` by name, so after adding it, open a new terminal and restart Claude Code from it.
2. Creates `~/.handoff/queue/`. The sandbox may only write *inside* that folder, so it can't create
   it.
3. Adds `"~/.handoff/queue"` to `sandbox.filesystem.allowWrite` in `~/.claude/settings.json`. It
   shows a diff and asks first, and keeps a timestamped backup (`--yes` skips the question). It
   warns if `allowWrite` already includes all of `~/.handoff`. That would let the agent write the
   hook-owned `sessions/` and `snap/` folders, which is what keeps provenance and reviewed files
   trustworthy.
4. Installs the Claude Code plugin at user scope. It adds this repo as a local marketplace (from
   `.claude-plugin/marketplace.json`) and runs `claude plugin install handoff@handoff`. The
   marketplace points at this checkout, so edits to `plugin/` take effect without reinstalling,
   but moving the folder breaks it (re-run the script).

To install the plugin by hand instead, or to try it for one session only:

```sh
claude plugin marketplace add /path/to/handoff && claude plugin install handoff@handoff --scope user
claude --plugin-dir /path/to/handoff/plugin      # one session, nothing installed
```

**Check it works.** Start a new session, since the hooks run at session start. Ask Claude to
stage `az account show` (or any harmless command) with `handoff add --wait`, then run
`! handoff`:
- The item's session shows **✓ verified**, with the prompt you just typed.
- Running it wakes Claude with the output.
- Asking Claude to write anywhere under `~/.handoff/sessions/` is blocked by the sandbox.

If the session shows **⚠ unverified** or `add` says `HANDOFF_SESSION_ID is not set`, the hooks
didn't run. Usually that means `handoff` isn't on Claude Code's PATH (check `which handoff` in a
new terminal), or the plugin isn't loaded.

## Use

- **Agent:** `handoff add --wait --desc "…" [--reason sandbox-blocked] [--file name=path] -- <argv…>`,
  run in the background. Attachments are referred to in argv as `{file:name}`. See
  `plugin/skills/handoff/SKILL.md`.
- **You:** run `handoff` (or `! handoff` from the Claude prompt). Sessions | items | detail.
  `r` or click **Run** (then confirm), `x` or **Reject**, `o` re-open after a change, `q` quit.

## Security model

| Threat | Defence |
|---|---|
| Agent forges which prompt or session asked for it | Hooks run outside the sandbox and write `~/.handoff/sessions/`, which the sandbox can't. The viewer reads provenance from there and marks sessions with no hook record **unverified**. |
| Shell injection | argv is executed directly, never through a shell. Interpreters (`bash -c`, `python -c`, `env`, `xargs`, …) are flagged. |
| Item or attachment changed after review | The viewer hashes what it showed and copies attachments to `~/.handoff/snap/`. Run re-verifies the hash, refuses with `modified` if it changed, and passes the snapshot paths, not the live files. |
| Run twice / run after reject | A claim file is created exclusively before running or rejecting. |
| Terminal tricks in displayed text | Control characters, ANSI escapes and invisible/bidi formatting characters are shown escaped. |
| Stale items | Items expire 24h after creation; stretched expiry in `item.json` is rejected. |

Remaining gap: an agent can name another real session's id. The viewer then shows that session's
real prompt and directory, so a mismatch is visible but not blocked.

## Layout

```
~/.handoff/
  queue/<id>/item.json, files/<name>        agent (sandbox-writable)
  sessions/<sid>/session.json               SessionStart hook
  sessions/<sid>/prompts/<ts>.json          UserPromptSubmit hook, newest 20 kept
  sessions/<sid>/results/<id>.json|.claim   viewer
  snap/<id>/<name>                          viewer: reviewed attachments
```

## Develop

```sh
go test -race ./...
python3 -m unittest discover -s scripts   # setup script
```

Inside the Claude Code sandbox, point Go's caches somewhere writable first:
`export GOCACHE=$TMPDIR/gocache GOPATH=$TMPDIR/gopath`. Fetching modules also needs
`proxy.golang.org`, `sum.golang.org` and `storage.googleapis.com` reachable through the
sandbox's network allowlist. The sandbox also blocks writes to `.git/config` and `.git/hooks`, so
run `git init` and similar commands yourself.

Deferred to v2: batches, fsnotify (the viewer re-reads every 2s), `init`, `gc` (clears stuck
`running` items and old directories), Homebrew formula, remembered auto-run rules.
