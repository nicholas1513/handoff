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

Run it with `run_in_background: true`. It exits when the user runs, rejects or lets the item
expire (24h), and prints the result as JSON. Exit 0 only if the command succeeded.

- `--` then the argv. **No shell:** no pipes, `&&`, `$()`, redirects or globs. One command per item.
  Don't wrap in `bash -c`; the viewer flags interpreters and the user will likely reject it.
- Put request bodies in files and attach with `--file name=path`. Refer to them in argv as
  `{file:name}` (e.g. `@{file:body.json}`); the runner substitutes the reviewed copy.
- `--reason`: `sandbox-blocked` when the command failed only on the sandbox, `needs-login` when it
  needs the user's CLI login.
- Never put credentials or tokens in argv or files. The user's CLI already has their login.

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
