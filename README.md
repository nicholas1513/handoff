# handoff

A review-and-run queue for commands an agent can't run from its sandbox. The agent stages the
exact command; you review it in a terminal viewer (what it does, which session and prompt it came
from, which Claude rule applies) and run it as yourself. The agent gets the result back.

handoff never reads, stores or passes on credentials. It runs CLIs you're already logged into.

## First-time setup

Install Go (1.23 or newer, e.g. `brew install go`) and have `python3` (3.8 or newer; macOS ships it
with the Xcode command-line tools), then run the setup script **yourself, from a regular terminal**. Claude's sandbox blocks what it
does: building into `~/go/bin`, creating `~/.handoff`, and editing `~/.claude/settings.json`.
Running it with `!` from a Claude prompt isn't reliable, because `!` commands can also be
sandboxed. If that happens, the script stops before changing anything and says so.

```sh
./scripts/setup.py
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

If the session shows **⚠ unverified**, the SessionStart hook didn't run for that session. Usually
that means `handoff` isn't on Claude Code's PATH (check `which handoff` in a new terminal), the
plugin isn't loaded, or the session started before setup (restart Claude Code itself, not just
the conversation). `~/.handoff/hook.log` has one line per hook call. A missing line means the
hook never ran, and an `error` line shows why it failed and which input fields it received.

## Use

- **Agent:** `handoff add --wait --desc "…" [--reason sandbox-blocked] [--file name=path] -- <argv…>`,
  run in the background. `handoff revoke <id>…` withdraws the session's own items that haven't run
  yet (e.g. one a newer item replaces); a waiting `add --wait` then exits with state `revoked`. Attachments are referred to in argv as `{file:name}`. See
  `plugin/skills/handoff/SKILL.md`.
- **Agent, file edits:** `handoff add --wait --desc "…" --edit <target>=<proposed>` stages a whole-file
  replacement. The viewer shows a unified diff of the target against the proposal (taken when you
  open the item) instead of the file, and Run refuses with `modified` if the target changed since.
  The diff sits in a scrolling box (half the window at most). Click a `[+] ↓` / `[+] ↑` row to reveal
  10 more unchanged lines below the hunk above / above the hunk below (`[+] ↕` reveals a small gap
  whole), or press `+` to widen every gap. `s` (or clicking `s settings` in the footer) opens
  settings, saved in `~/.handoff/viewer.json`: diff word wrap and attachment word wrap, each on or
  off on its own. An unwrapped box is framed and pans: `<` `>` move the selected one (or the first
  that pans), a sideways scroll moves the one under the pointer, and `‹` `›` mark lines that
  continue off-screen.
- **Agent, commits:** commits made in a scratch clone are handed over as a patch: written with
  `git format-patch --stdout <base>..HEAD`, attached with `--file commits.patch=<file>`, and staged
  as exactly `git -C <repo> am {file:commits.patch}`. The viewer doesn't parse the mail. When you
  open such an item it builds a **review copy** in `~/.handoff/review/<id>-<n>`: a fresh clone of the
  repo at its current HEAD, in which `git am` applies the same reviewed copy of the patch the run
  will apply, with the repo's hooks and config left out (only its `am.*`, `mailinfo.*`, `i18n.*`,
  `apply.*` and `core.whitespace` settings are copied, since they change what `git am` does). It
  then draws git's own log of what came out: a summary (commits, files, lines, and a warning for
  binary files), each commit's author, message and diff, under diff word wrap like the edit diff.
  If `git am` stops there, you see the commits it made before it stopped (the run makes them too),
  git's error, which the run would hit too, and the raw patch. The item doesn't run until its
  review copy is drawn. The run's `git am` runs no hooks either (the repo's or yours), nor the repo's
  fsmonitor, and signs no commit. The repo and the HEAD it was reviewed on are pinned: if the repo
  moves on, or another repo is put at its path, the item shows as changed and Run refuses with
  `modified`; `o` reviews it again. A repo with no commit yet gets no review copy, and the run is
  refused (so is every such item with git older than 2.44, which can't turn lazy fetches off). A
  `git pull` or `git fetch` from a local path gets a warning instead: its changes aren't shown.
- **Commit review setting:** `Commit review` in settings picks where those commits are reviewed:
  in the viewer, or in **VS Code**. Left on auto it is VS Code when the `code` command is on PATH
  (or, on macOS, the app is in `/Applications` or `~/Applications`), else the viewer; auto is
  resolved at each start and only saved once you pick a value (picking the one auto would pick
  makes it auto again). In VS Code mode the item shows the summary and an **Open in VS Code**
  button (or `v`), which opens the review copy with its HEAD back on the reviewed base, so the
  whole change sits staged in Source Control (the commits stay on its `reviewed` branch). VS Code
  opens each review copy in Restricted Mode, where Source Control doesn't run, so you have to trust
  that one folder to see the change. Trusting it lets VS Code and its extensions run code from the
  repo and the agent's patch: tasks, workspace settings, linter and test configs. Trust only
  that folder, `~/.handoff/review/<id>-<n>`, never `~/.handoff/review` itself (leave "trust the
  parent folder" unticked), and if the patch may touch such files, look at the changed files in
  the viewer first (`s` → Commit review) or review it there instead. VS Code trusts a folder by
  its path, so every build of a review copy gets a new folder, which VS Code asks you to trust
  again: after a changed patch, a moved HEAD, or a restart of the viewer.
- **You:** run `handoff` (or `! handoff` from the Claude prompt). Sessions | items | detail.
  Press `space` twice to run the selected item (any other key cancels), or click **Run** (then
  confirm). `r` is an alias for the first `space`. `x` or **Reject**, `o` re-open after a change,
  `q` quit. After a run or reject, the selection jumps to the oldest pending item in the same
  session, and `space` is ignored for a second after the jump, so a stray key can't run it.
  The sessions pane shows each session's `/resume` title (a custom `/rename` title wins), its
  folder, age and pending count. The detail pane is in sections: status chips, description,
  warnings, Command (one flag per line, every word verbatim), Why, attached files, Integrity and
  Result.

## Security model

| Threat | Defence |
|---|---|
| Agent forges which prompt or session asked for it | Hooks run outside the sandbox and write `~/.handoff/sessions/`, which the sandbox can't. The viewer reads provenance from there and marks sessions with no hook record **unverified**. |
| Shell injection | argv is executed directly, never through a shell. Interpreters (`bash -c`, `python -c`, `env`, `xargs`, …) are flagged. |
| Item or attachment changed after review | The viewer hashes what it showed and copies attachments to `~/.handoff/snap/`. Run re-verifies the hash, refuses with `modified` if it changed, and passes the snapshot paths, not the live files. |
| Handed-over commits drawn differently from what `git am` applies | The viewer doesn't parse the patch: it draws git's own result of `git am` on the reviewed copy, in a review copy of the repo with no hooks, no filters and none of its config but `am.*`, `mailinfo.*`, `i18n.*`, `apply.*` and `core.whitespace`, overriding attributes and submodule settings that hide a change from the diff. Run refuses with `modified` if the repo's HEAD moved since or another repo is at its path. It pins HEAD and runs `git am` as the review copy does: with replace refs, hooks, the fsmonitor, commit signing and lazy fetches off. It reports `failed` if `git am` made any commit the review copy didn't (tree, author, date or message) or any object its commits added doesn't hash to its id, also where `git am` stopped or failed in the review copy (an object file or setting changed since). |
| Edit diff shows one file but writes another | An edit item's argv must be exactly `/bin/cp {file:proposed} <target>` for its own absolute target, or it fails to load. Run also refuses if the target's hash differs from when the diff was taken. |
| Run twice / run after reject | A claim file is created exclusively before running or rejecting. |
| Terminal tricks in displayed text | Control characters, ANSI escapes and invisible/bidi formatting characters are shown escaped. |
| Stale items | Items expire 24h after creation; stretched expiry in `item.json` is rejected. |
| Agent revokes another session's item | `handoff revoke` refuses items from other sessions. The marker sits in the sandbox-writable `queue/<id>/revoked`, so an agent writing it by hand isn't blocked, but a marker can only stop an item, never run one, and a claimed (running) item ignores it. |
| Cancel signals an unrelated process | A stale run's pid is signalled only if the kernel still reports the item's program for it, so a reused pid is left alone. |

Remaining gap: an agent can name another real session's id. The viewer then shows that session's
real prompt and directory, so a mismatch is visible but not blocked.

## Layout

```
~/.handoff/
  queue/<id>/item.json, files/<name>        agent (sandbox-writable)
  queue/<id>/revoked                        agent: `handoff revoke`
  sessions/<sid>/session.json               SessionStart hook
  sessions/<sid>/prompts/<ts>.json          UserPromptSubmit hook, newest 20 kept
  sessions/<sid>/results/<id>.json|.claim   viewer (.claim holds the run's pid once started)
  snap/<id>/<name>                          viewer: reviewed attachments
  review/<id>-<n>/                          viewer: review copies of git am items, a new one per build, removed at startup once finished
  hook.log                                  hooks: one line per call (no prompt text)
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

Deferred to v2: batches, fsnotify (the viewer re-reads every 2s), `init`, `gc` (old
directories; stuck `running` items are ended with `c c` in the viewer), Homebrew formula, remembered auto-run rules.
