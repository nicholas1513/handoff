---
name: handoff
description: Use when a command you need fails only because of the sandbox (blocked network host, blocked write, blocked credential file), or needs the user's own CLI login (az, gh) that you can't use. Stage it with `handoff add` for the user to review and run instead of asking them to copy and paste it.
---

# Handoff

Stage the exact command the user would otherwise type. They review it in the `handoff` viewer and
run it as themselves. You get the result back.

**Only for what you can't do.** If your own tool call would work (normal permission prompt), use it.
Never stage something to get around a permission rule that denies it to you.

## Stage

```
handoff add --wait --desc "<what it does and why, one or two lines>" \
  --reason sandbox-blocked \
  --file body.json=./body.json \
  -- az rest --method post --url "https://…" --body "@{file:body.json}"
```

Run it with `run_in_background: true`. It exits when the user runs, rejects or cancels it, when
you revoke it, or when it expires (24h), and prints the result as JSON. Exit 0 only if the command
succeeded. It prints `staged <id>` first; keep the id.

- `--` then the argv. **No shell:** no pipes, `&&`, `$()`, redirects or globs. One command per item.
  Don't wrap in `bash -c`; the viewer flags interpreters and the user will likely reject it.
- Put request bodies in files and attach with `--file name=path`. Refer to them in argv as
  `{file:name}` (e.g. `@{file:body.json}`); the runner substitutes the reviewed copy.
- `--reason`: `sandbox-blocked` when the command failed only on the sandbox, `needs-login` when it
  needs the user's CLI login.
- Never put credentials or tokens in argv or files. The user's CLI already has their login.

## Propose a file edit

To change a file you can't write (sandbox or a deny rule on it), write the complete new contents to
a scratch file and stage:

```
handoff add --wait --desc "<what changes and why>" --reason sandbox-blocked \
  --edit <target>=<scratch file with the full new contents>
```

No `--` and no argv: the command is always `/bin/cp {file:proposed} <target>`. The user sees a
unified diff against the target as it is when they open the item, not the whole file, and the run
is refused (`modified`) if the target changes after they opened it. A missing target is created.
This is the one case where staging for a file denied to you is fine, and only when the user asked for
that change: they review the exact diff before anything is written. One file per item.

## Hand over commits

To land commits you made in a scratch clone in the user's repo, write them to a patch file yourself,
then stage `git am` of it, exactly in this form and with no other options:

```
git -C <scratch> format-patch --stdout <base>..HEAD > <file>.patch
handoff add --wait --desc "<what the commits do>" --reason sandbox-blocked \
  --file commits.patch=<file>.patch -- git -C <repo> am {file:commits.patch}
```

The viewer applies the patch with `git am` in a copy of the repo and shows the user each commit's
author, message and diff as git made them; the run applies the same reviewed copy. Never stage a
`git pull` or `git fetch` from a scratch path: the user sees only the path, not the changes, and the
clone can change before they run it.

The repo's HEAD is pinned when the user reviews the item. If the repo moves on before it runs, the
run is refused (`modified`): rebase your commits onto the repo's current HEAD and stage a new patch.
The repo needs at least one commit: `git am` into a repo with none is refused.

If `git am` fails (a commit doesn't apply), the repo is left mid-am and any further `git am` fails
too. Don't stage another one: tell the user, who either resolves it and runs `git am --continue` or
runs `git am --abort`. Then rebase your commits onto the repo's current HEAD and stage a new patch.

## Revoke what you no longer want

If an item hasn't run yet and you no longer want it (you staged a corrected or combined version,
the plan changed, or the user said not to), revoke it **before** staging a replacement:

```
handoff revoke <id>
```

Only your own session's items, and only ones not yet run: once the user starts a run it's theirs to
cancel. Never leave two items queued that would both do the job (e.g. two `git commit -a`); whichever
the user approves first runs, and the other runs later with stale content.

## Chain

When a later command needs an earlier one's output (e.g. a query ID), stage the first, wait for
its result, then stage the next with the real value. Don't guess IDs.

If the background command never wakes you, use Monitor on it, or read
`~/.handoff/sessions/$HANDOFF_SESSION_ID/results/<id>.json`.

## Errors

- `no session id`: you are not running inside Claude Code's Bash tool. Tell the user.
- Write denied under `~/.handoff/queue`: setup isn't finished. Ask the user to run
  `scripts/setup.py` from the handoff repo themselves, in a regular terminal (not with `!`, which
  can be sandboxed), then restart Claude Code. Don't edit settings or try to create `~/.handoff` yourself.
