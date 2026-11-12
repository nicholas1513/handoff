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
   trustworthy. It also adds `"~/.handoff/snap"` and `"~/.handoff/review"` to
   `sandbox.filesystem.denyRead`, which covers the agent's shell commands, and denies its Read tool
   the same folders with `Read(~/.handoff/snap/**)` and `Read(~/.handoff/review/**)` in
   `permissions.deny`: the viewer copies files there that the agent may not be able to read (an
   edit's current target, a repo's checkout). Re-run it to add these to an existing setup.
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
stage `gh auth status` (or any harmless command) with `handoff add --wait`, then run
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

The agent stages commands with `handoff add`. You review them in the viewer and run them as
yourself. The agent's side is also written up for Claude in `plugin/skills/handoff/SKILL.md`.

### Review and run

Run `handoff` in a terminal, or `! handoff` from a Claude prompt. The screen has three panes:

- **Sessions:** each Claude session's `/resume` title (a custom `/rename` title wins), its folder,
  age and pending count.
- **Items:** the selected session's items, under Outstanding and Completed.
- **Detail:** the selected item, in sections: status chips, description, warnings, Command (one
  flag per line, every word verbatim), Why, attached files, Integrity and Result.

| Key | Does |
|---|---|
| `space` `space` | Run the selected item, or click **Run** and confirm. `r` works as the first `space`, and any other key cancels. |
| `x` | Reject the selected item, or click **Reject**. |
| `c` `c` | Stop the selected run, or click **Cancel run**. |
| `o` | Open an item again after it changed. |
| `s` | Settings, also under `s settings` in the footer. Saved in `~/.handoff/viewer.json`. |
| `?` | A short manual, also the **Manual** row in settings. `↑` `↓` scroll it and `esc` closes it. |
| `ctrl+s` `ctrl+l` | Save or restore the running Claude sessions (see [below](#save-and-restore-claude-sessions)). |
| `q` | Quit. While anything is running it asks for a second `q`. |

A run carries on while you read, move around, and run or reject other items, so several can run
at once. After a run or reject, if the selection is still on that item, it jumps to the oldest
pending item in the same session. `space` is ignored for a second after the jump, so a stray key
can't run it. While the manual or a prompt is open, nothing behind it reacts.

### Stage a command

```sh
handoff add --wait --desc "Open an issue for the crash" --reason needs-login \
  --file body.json=./body.json \
  -- gh api --method POST repos/OWNER/REPO/issues --input "{file:body.json}"
```

- The agent runs it in the background. It prints the result when you run, reject or cancel the
  item.
- Everything after `--` is the argv, with no shell. `{file:name}` stands for an attached file, and
  the run gets the copy you reviewed.
- `--reason` is `sandbox-blocked` or `needs-login`.
- `handoff revoke <id>…` withdraws the session's own items that haven't run yet, such as one a
  newer item replaces. A waiting `add --wait` then exits with state `revoked`.

### Propose a file edit

```sh
handoff add --wait --desc "…" --edit <target>=<proposed>
```

This stages a whole-file replacement. Instead of the file, the viewer shows a unified diff of the
target, as it is when you open the item, against the proposal. Run refuses with `modified` if the
target has changed since.

The diff sits in a scrolling box, at most half the window:

- Click a `[+] ↓` or `[+] ↑` row to reveal 10 more unchanged lines below the hunk above or above
  the hunk below. `[+] ↕` reveals a small gap whole, and `+` widens every gap.
- Settings turn diff word wrap and attachment word wrap on or off, each on its own.
- An unwrapped box is framed and pans. `<` `>` move the selected one (or the first that pans), a
  sideways scroll moves the one under the pointer, and `‹` `›` mark lines that continue
  off-screen.

### Hand over commits

Commits made in a scratch clone are handed over as a patch, staged in exactly this form:

```sh
git -C <scratch> format-patch --stdout <base>..HEAD > commits.patch
handoff add --wait --desc "…" --file commits.patch=commits.patch -- git -C <repo> am {file:commits.patch}
```

The viewer doesn't parse the mail. When you open the item, it builds a **review copy** in
`~/.handoff/review/<id>-<n>`: a fresh clone of the repo at its current HEAD. There, `git am`
applies the same reviewed copy of the patch that the run will apply. The repo's hooks and config
are left out. Only its `am.*`, `mailinfo.*`, `i18n.*`, `apply.*` and `core.whitespace` settings
are copied, since they change what `git am` does. The viewer then draws git's own log of the
result:

- a summary of commits, files and lines, with a warning for binary files
- each commit's author, message and diff, under the diff word wrap setting

If `git am` stops, you see the commits it made before stopping (the run makes them too), git's
error (the run would hit it too), and the raw patch. The item can't run until its review copy is
drawn.

**When it runs:**
- Its `git am` runs no hooks (neither the repo's nor yours) and not the repo's fsmonitor, and it
  signs no commit. It can't turn off filter and merge drivers that the repo's config names, so
  those get a warning.
- The repo and the HEAD it was reviewed on are pinned. If the repo moves on, or another repo is
  put at its path, the item shows as changed and Run refuses with `modified`. `o` reviews it
  again.

**What's refused or only warned about:**
- A repo with no commit yet gets no review copy, and the run is refused. With git older than
  2.44, which can't turn lazy fetches off, every `git am` item is refused.
- A `git pull` or `git fetch` from a local path gets a warning, because its changes aren't shown.
- `git am` in any other form (another option, git named another way or as `git-am`, or run by
  another program such as `xcrun`) gets a warning and no review copy. Its raw patch can show
  something other than what git applies.

#### Reviewing commits in VS Code

The **Commit review** setting picks where those commits are reviewed: in the viewer or in VS Code.
Left on auto, it's VS Code when the `code` command is on PATH (or, on macOS, when the app is in
`/Applications` or `~/Applications`), and the viewer otherwise. Auto is worked out at each start
and only saved once you pick a value. Picking the value auto would pick makes it auto again.

In VS Code mode the item shows the summary and an **Open in VS Code** button (or `v`). It opens
the review copy with its HEAD back on the reviewed base, so the whole change sits staged in Source
Control. The commits stay on its `reviewed` branch.

> [!WARNING]
> VS Code opens each review copy in Restricted Mode, where Source Control doesn't run, so you have
> to trust that one folder to see the change. Trusting it lets VS Code and its extensions run code
> from the repo and from the agent's patch, such as tasks, workspace settings, and linter and test
> configs.
>
> - Trust only that folder, `~/.handoff/review/<id>-<n>`, never `~/.handoff/review` itself. Leave
>   "trust the parent folder" unticked.
> - If the patch may touch such files, look at the changed files in the viewer first
>   (`s` → Commit review), or review the whole change there instead.
>
> VS Code trusts a folder by its path, and every build of a review copy gets a new folder. So it
> asks again after a changed patch, a moved HEAD, or a restart of the viewer.

### Save and restore Claude sessions

This needs Ghostty 1.4 or newer, on macOS or on Linux with the GTK build. A save records the
running sessions with their Ghostty windows and tabs, and a restore reopens them after a reboot or
crash.

| | In the viewer | In any Claude tab |
|---|---|---|
| **Save** | `ctrl+s`, which then offers to close the saved tabs (defaults to **No**) | `/handoff:claude-sessions save` |
| **Restore** | `ctrl+l`, after asking first (defaults to **Yes**) | `/handoff:claude-sessions restore`, with no confirmation |

In the viewer's prompts, `←` `→` choose and `space` or `enter` selects.

- Closing hangs up each tab's shell and its Claude, so work in progress stops. A restore resumes
  them. Closing leaves out tabs that have moved on since the save, and keeps the viewer's own tab
  while a run is in progress.
- A restore opens one Ghostty window per saved window, each tab running `claude --resume <id>` in
  its folder. Sessions still running aren't opened again.
- A save refuses to run when nothing is running, so it can't overwrite the last save.
- The Sessions pane title shows the last save. `~/.handoff/claude-sessions.log` has a line for
  each save, restore and close, saying what it did and listing the sessions by window.
- The prompt hook answers the slash command, so Claude never sees it.
- Running sessions come from Claude Code's own list in `~/.claude/sessions/`, so they don't need
  the plugin. That format is undocumented, so the hook's records still cover any session the list
  leaves out or that can't be read. A session found only that way needs a restart or `/clear`
  after this version is installed.

**macOS:** the first save or restore makes macOS ask whether your terminal may control Ghostty. A
tab split into several Claude terminals comes back as one tab each.

**Linux:** Ghostty has no command that lists its tabs, so a save reads them from the
accessibility bus (AT-SPI). It tells the tabs apart by briefly setting a one-off title on each
Claude terminal, then putting the title back, so tab titles may flicker, but focus doesn't move.
Restore opens windows and tabs with `ghostty +new-window` and `+new-tab`, which need Ghostty's
D-Bus activation. Sessions it can't place go in one extra window: a busy tab that keeps repainting
its title, a split that isn't the focused one, or a tab with a fixed title. If the accessibility
bus isn't there, all of them do.

## Security model

| Threat | Defence |
|---|---|
| Agent forges which prompt or session asked for it | Hooks run outside the sandbox and write `~/.handoff/sessions/`, which the sandbox can't. The viewer reads provenance from there and marks sessions with no hook record **unverified**. |
| Shell injection | argv is executed directly, never through a shell. Interpreters (`bash -c`, `python -c`, `env`, `xargs`, …) are flagged. |
| Item or attachment changed after review | The viewer hashes what it showed and copies attachments to `~/.handoff/snap/`. Run re-verifies the hash, refuses with `modified` if it changed, and passes the snapshot paths, not the live files. |
| Agent has the viewer read a file for it | `item.json` and attachments are read only as plain files: a symlink anywhere under `queue/<id>`, a hard link or a FIFO makes the item invalid. Setup denies the agent reads of `snap/` and `review/`, where the viewer copies what it read for review. |
| Handed-over commits drawn differently from what `git am` applies | The viewer doesn't parse the patch: it draws git's own result of `git am` on the reviewed copy, in a review copy of the repo with no hooks, no filters and none of its config but `am.*`, `mailinfo.*`, `i18n.*`, `apply.*` and `core.whitespace`, overriding attributes and submodule settings that hide a change from the diff. Run refuses with `modified` if the repo's HEAD moved since or another repo is at its path. It pins HEAD and runs `git am` as the review copy does: with replace refs, hooks, the fsmonitor, commit signing and lazy fetches off; filter and merge drivers in the repo's config, which the run may still run, are drawn as a warning. It reports `failed` if `git am` made any commit the review copy didn't (tree, author, date or message) or any object its commits added doesn't hash to its id, also where `git am` stopped or failed in the review copy (an object file or setting changed since). `git am` in any other form gets no review copy and a warning. |
| Edit diff shows one file but writes another | An edit item's argv must be exactly `/bin/cp {file:proposed} <target>` for its own absolute target, or it fails to load. Run also refuses if the target's hash differs from when the diff was taken. |
| Run twice / run after reject | A claim file is created exclusively before running or rejecting. |
| Terminal tricks in displayed text | Control characters, ANSI escapes and invisible/bidi formatting characters are shown escaped. |
| Stale items | Items expire 24h after creation; stretched expiry in `item.json` is rejected. |
| Agent revokes another session's item | `handoff revoke` refuses items from other sessions. The marker sits in the sandbox-writable `queue/<id>/revoked`, so an agent writing it by hand isn't blocked, but a marker can only stop an item, never run one, and a claimed (running) item ignores it. |
| Agent plants sessions for restore to type | The save is in `~/.handoff/claude-sessions.json`, outside the sandbox-writable `queue/`. Restore runs only `claude --resume <id>` and only for an id shaped like a session id; the folder is passed to Ghostty as a setting, never typed or put in shell text. |
| A forged entry in Claude Code's session list | An entry counts only if its process is running, in a terminal, and started when the entry says; its id must be shaped like a session id, and its folder is only ever an argument. |
| A tab's title carries terminal escapes back into a tab | On Linux a save puts a tab's title back by writing it to that terminal. Control characters are dropped first, so a title can't end the sequence and start another. |
| Closing saved tabs signals an unrelated process | Close starts from what's running now, by session id, and hangs up a session leader only if it holds the same terminal as that session's Claude. It only runs after a prompt that defaults to No. |
| Cancel signals an unrelated process | A stale run's pid is signalled only if the kernel still reports the item's program for it, so a reused pid is left alone. |

Remaining gap: an agent can name another real session's id. The viewer then shows that session's
real prompt and directory, so a mismatch is visible but not blocked.

## Layout

```
~/.handoff/
  queue/<id>/item.json, files/<name>        agent (sandbox-writable)
  queue/<id>/revoked                        agent: `handoff revoke`
  sessions/<sid>/session.json               SessionStart hook (with the Claude process's pid and start time)
  sessions/<sid>/prompts/<ts>.json          UserPromptSubmit hook, newest 20 kept
  sessions/<sid>/results/<id>.json|.claim   viewer (.claim holds the run's pid once started)
  snap/<id>/<name>                          viewer: reviewed attachments (the agent can't read snap/ or review/)
  review/<id>-<n>/                          viewer: review copies of git am items, a new one per build, removed at startup once finished
  hook.log                                  hooks: one line per call (no prompt text)
  claude-sessions.json                      viewer or prompt hook: the last save of the running sessions
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
