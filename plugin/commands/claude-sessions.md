---
description: Save or restore the Claude sessions running in Ghostty (windows, tabs, folders)
argument-hint: save | restore
disable-model-invocation: true
---
The handoff plugin's prompt hook answers `/handoff:claude-sessions $ARGUMENTS` before Claude sees it, so this text only reaches you when that hook didn't run. Tell the user in one line that the handoff hook didn't handle the command (check that the `handoff` binary is current and that `~/.handoff/hook.log` shows the prompt hook), and do nothing else.
