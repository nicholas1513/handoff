# Viewer UX, round 2: design

Date: 2026-10-21. Source: the user's feedback after the first real use, about 40 items staged
across two sessions while building an Azure DevOps dashboard. The feedback log and the evidence
are in `~/.claude/plans/handoff-tool-v1.md`, under "UX feedback after real use".

## Intent

During real use the viewer worked, but it was slow to review with. Every item meant reading a
wall of same-colored text, finding the next pending item by hand, working out which session was
which from a UUID prefix, and pressing two keys to approve. The goal is to make reviewing fast
**without making it less careful**. The viewer is a security boundary: it's where a human decides
whether a command the agent wrote runs under their own login.

## Goals

1. **Detail pane you can scan.** The description first, then the command, then the context,
   separated and colored by meaning.
2. **Sessions you can recognize.** Each session row shows the same title `claude --resume` shows.
3. **Auto-advance.** After a run or reject, the selection moves to the next pending item in the
   same session.
4. **One-key approve.** `space` and then `space` runs the item, replacing `r` then `y`.

## Non-goals

- **Approve-once / auto-approval rules.** The user tabled these. The design sketch so far is in the
  plan file (decision 12). Don't build any part of it, and don't add hooks "for later".
- Changes to `handoff add`, the hooks, the queue format or the runner.
- **Any new working-directory behavior** (user decision). Keep showing the item's cwd, as now, in
  the Why line. Add no warnings for a cwd that differs from the session's cwd or sits inside
  sandbox-writable paths. That idea was raised and deferred: tools read config from cwd, so a
  cwd the agent can write to is the riskier case.
- Grouping two sessions into one thread. The titles are meant to make related sessions easy to
  spot, not to merge them.

## Anti-goals (a failure even if it works)

- **Anything that hides part of the command.** Every argv element stays visible in the detail pane,
  with no folding, no "+N more flags", and no truncating a value without a way to see it in full.
  A hidden flag is exactly where something malicious would go. Long values may wrap or be dimmed,
  but never be dropped.
- **Anything that makes running easier by accident.** A single keypress must never run an item.
  The mouse keeps its two-step confirm with Yes in a different spot from Run.
- **Showing agent-written or transcript text unsanitized.** Descriptions, argv, files, output and
  now session titles all go through `sanitize`.

## Constraints

- Go + Bubble Tea + lipgloss (`internal/tui/tui.go`), the existing three-pane layout, and the
  mouse support.
- Mouse hit-testing uses the fixed geometry constants at the top of `tui.go` (`buttonY`, `runX`, …).
  If the detail header changes shape, those must still match what's drawn. The existing test
  `TestClickSelectsAndRunNeedsConfirm` covers this and must keep passing.
- `go test -race ./...` passes. Inside the sandbox, use `GOCACHE=$TMPDIR/gocache GOPATH=$TMPDIR/gopath`.
- Terminal colors: use ANSI palette colors (as now), so the user's terminal theme applies.

## The design (decided with the user)

### 1. Detail pane: "sections with headings" (mockup option A)

Order, top to bottom:

1. **Button row** (as now, on `buttonY`), so mouse hit-testing doesn't move.
2. **Status chips** on one line: the state (`PENDING`, `SUCCEEDED`, …), the HTTP-ish method when
   argv has a `--method` / `-X` value (`GET` green; `PUT`/`POST`/`PATCH`/`DELETE` red; anything else
   neutral), then `✓ verified` or a red `⚠ unverified session`, then the time left before expiry.
3. **Description**, in bold.
4. **Warnings**, bold red, directly under the description and never folded: changed on disk,
   unverified session (with the short id), runs an interpreter, settings problems.
5. `── Command ──` heading. The program and subcommand (the non-flag words before the first flag)
   go on the first line in cyan. After that, each flag goes on its own line in purple, with its
   value aligned in a column. A value that starts with `-` is still a value if it follows a flag
   that takes one; when that's ambiguous, show each word on its own line rather than guess wrong.
   Positional arguments go on their own lines **in their original argv position** (changed by
   the final review: reordering misled the reader for `docker run … rm -rf /`). A word that is
   empty or contains whitespace or `"` is shown Go-quoted, so an embedded newline can't draw a
   fake flag line. The method chip uses the **last** `--method`/`-X`, as CLIs do. URLs: host and the last path
   segment in teal, the middle dimmed, wrapped onto continuation lines rather than truncated.
   `{file:name}` references show as `📎 name` in orange, with the size and "(attached, see
   below)".
6. `── Why ──`: the prompt time and the prompt text ("16:40 you said: …"), then one dim line with the
   reason (`needs-login`), the rule badge and the cwd (`~`-shortened).
7. One `── 📎 <name> ──` section per attached file, with its full content. JSON is pretty-printed
   when it parses; otherwise the content is shown as-is.
8. `── Integrity ──` (changed after the first real use, user decision: a bare hash gave no algorithm
   and couldn't be checked by hand):
   - The combined hash, labeled `SHA-256 (combined: item.json + attachments)`, dimmed. It's what
     the run-time `modified` check and `result.json`'s `sha256_reviewed` use; no standard tool
     reproduces it.
   - Then one line per file with its own plain SHA-256 (what `shasum -a 256 <path>` prints) and the
     exact path to check: `item.json` at `~/.handoff/queue/<id>/item.json`, and each attachment at
     its **snapshot** copy, `~/.handoff/snap/<id>/<name>` (outside the agent's write access, and the
     copy that actually runs). Paths are shown `~`-shortened, with the same home rule as the cwd.
   - The per-file hashes come from the same bytes the combined hash was computed from (one read),
     not from a second read of the files.
   - **Revised after use (user decision):** the per-file lines come **first**, then the combined line,
     labeled `combined (internal; not comparable to shasum)`, so the first hash the eye lands on is
     one that can be checked. The user compared the combined hash with `shasum` and read it as a mismatch.
   - A `c` copy-to-clipboard key (OSC 52, `shasum -c` format) was built, then **reverted at the user's
     request** as too complicated for now. To check by hand, run `shasum -a 256 <path>` on a listed path.
9. `── Result ──` when there's a result: the state colored by outcome, the exit code, the time,
   then stdout and stderr under sub-labels.

Scrolling works as now (detail focus plus up and down, or the mouse wheel).

### 2. Sessions pane: titles from `/resume`

Row format: `<pending-glyph+count> <title> · <cwd basename> · <age of newest item>`, for example
`● 2  Product Input dashboard · source · 3m ago`. With nothing pending, the glyph and count are
dimmed or left out.

The title comes from the session's transcript, at the `transcript_path` the SessionStart hook
recorded in `session.json` (Claude Code writes that file; the sandbox can't). Scan its JSONL lines
for:

- `{"type":"custom-title","customTitle":…}`, set by `/rename`. The **last** one wins.
- If there's none, `{"type":"ai-title","aiTitle":…}`. Again the last one wins.
- If neither exists or the file can't be read: the short session id.

Transcripts get large (3 MB seen), and the viewer refreshes every 2s, so cache the title per
session keyed on the transcript's mtime and size. Sanitize the title and cut it to one line.

Unverified sessions (no hook record) keep the red `⚠ <short id>` label. They have no transcript
path to read.

### 2b. Items pane in two sections (added after real use, user decision)

The items pane is split under two headers, each with a count. A header is hidden when its section is empty.
- **`Outstanding (N)`**: pending and running items, **oldest first** (by `Created`), so the list is worked top to bottom.
- **`Completed (N)`**: every other state (succeeded, failed, rejected, expired, modified), **newest first** by finish time:
  the result's `RanAt` when there is a result; otherwise (expired) the item's `Expires`. Rows stay dimmed.
- Every row starts with a timestamp, then the state glyph (●/…/✓/✗/⊘/⌛/!), then the description. Outstanding rows show the staged time
  (`Created`); Completed rows show the finish time. Local time: `15:04` if it's today, otherwise `Jan 2 15:04`.
- Headers can't be selected. Up and down move between items only, skipping headers, and a click on a header does nothing.
- The "invalid items" pseudo-session is unchanged.
- Because Outstanding is oldest-first, the oldest pending item is always the first item row. Session entry and auto-advance
  (sections 3 and below) keep picking it by `Created`, so they land on that row.

### 2c. Scrolling (added after real use, user decision)

- **Items pane:** the list scrolls to keep the selected item in view, for keyboard moves and automatic jumps alike. The mouse wheel
  still moves the selection. When rows are hidden, a dim `↑ N more` / `↓ N more` line shows at the top or bottom; these lines take
  a list row and can't be clicked. Headers scroll with the list (they aren't pinned). Clicks map through the scroll offset, so a
  click always selects exactly the drawn item.
- **Detail pane, Result output:** stdout and stderr together sit in a box capped at **40% of the window height**, with a dim
  `lines A–B of T` line when the output is longer. With the detail pane focused, `[` / `]` and PgUp / PgDn scroll that box by a page;
  ↑↓ still scroll the whole detail pane, and the mouse wheel over the box scrolls the box. Only result output is capped: the command,
  the description, the warnings and the attachments always show in full. The box scroll resets to the top on a different item.
- **Revised after real use (user decision):** for an item that has a result, the Result section (its heading, the exit line, the
  capped box and the footer) comes right after the chips, the description and the warnings, ahead of Command / Why / attachments /
  Integrity. Otherwise the box sat below the visible pane and the cap looked like it did nothing. Pending items keep the old order.
- **The box must look scrollable (user decision, after real use):** when the output is longer than the cap, the box is drawn as a
  container:
  - a rounded border;
  - a background visibly lighter than the terminal's (adaptive: lighter on dark terminals, slightly darker-tinted on light ones);
  - a one-column scrollbar inside the right edge (a track plus a thumb whose size and position show the visible fraction and offset);
  - the bottom border carrying `lines A–B of T · [ ] or PgUp/PgDn`;
  - `[ ] scroll output` on the status line while the detail pane is focused and the box has more to show.
  Output that fits is drawn plain, as before. The output is wrapped to the inner width (inside the border, padding and scrollbar), so
  nothing is cut off.
- **Whole-pane scroll is bounded:** ↑↓ on the detail pane stop when the last line reaches the pane's bottom, so content that fits
  doesn't scroll at all.

### 3. Auto-advance

After a **run finishes** or a **reject**, if the session still has pending items, select the
**oldest** pending one (by `created`). Agents stage chains in order, so oldest-first follows the
order they meant. If nothing is pending, the selection stays where it is. Focus doesn't change
panes. The 2s refresh must not undo the jump: today, `refresh` re-selects by the kept item id,
so the advance has to update that kept id.

**Added after the first real use (user decision):** a pending item that **arrives** also pulls the
selection when the selected item is finished (succeeded, failed, rejected, expired or modified,
meaning not pending and not running). On refresh, if the selected item isn't pending or running and
the same session has a pending item, select the oldest pending one, using the same rules as above
(set `advancedAt` so the 1s `space` guard applies). Never pull away from a pending or running item,
and never across sessions. In real use agents stage one item at a time, so without this rule the
item-to-item advance almost never fired.

### 4. Keys

- `space`: on a pending item, arms the run, and the button row shows a red "space again to
  run · any other key cancels". A second `space` runs it. Any other key cancels the arm and is
  **not** acted on (so `x` while armed cancels and doesn't reject).
- `x` rejects as now. Remove `r` and `y`, or keep them as hidden aliases (builder's call: keeping
  `r` arms the same way is harmless).
- Update the status-line help to match.
- Mouse: unchanged (Run, then "Yes, run" in a different spot).

## Calls made after the builder check (author's calls, not discussed with the user)

1. **Keyboard arm and mouse confirm are separate states.** `space` sets a key-arm state whose
   button row shows only the red "space again to run · any other key cancels" text, with no Yes
   button and no clickable target. Mouse Run sets the existing confirm state with
   `[ Cancel ] [ Yes, run ]`. A click at `yesX` runs only in the mouse-confirm state. Any mouse
   click cancels a key arm. `space` doesn't confirm a mouse arm; it cancels it like any other key.
2. **Fast taps can't run an item you haven't seen.** Keys that arrive while a run is in flight are
   dropped. After an auto-advance, `space` is ignored for 1 second, so `space space space space`
   can't carry over into the next item. A test covers this.
3. **Auto-advance fires only if the selection is still on the item that ran** (or that was
   rejected). If you've moved elsewhere by the time the run finishes, nothing jumps.
4. **"Every argv element is visible" means verbatim.** Each element's sanitized text appears in
   full once styling and line wrapping are removed. `📎` is decoration added next to `{file:name}`
   (and any `@` prefix stays); it never replaces the text. The test strips ANSI codes and wraps,
   then checks every element.
5. **The trusted session identity stays in the Why section:** `✓ verified`, the full session id and
   the session's hook-recorded cwd. The leftover risk under security fix A (an agent naming another
   real session) relies on the human seeing these.

## Left to the builder

- Exact colors within the ANSI palette, chip styling, and the spacing of the flag column.
- How to detect "takes a value" for flags. A simple rule is fine: the next word is the value unless
  it starts with `--`. Fall back to one word per line when that's unclear.
- Whether JSON pretty-printing applies only to `.json` attachments or to anything that parses.
- How the title cache is structured.
- Test shape. At minimum: a title-parsing test (custom title beats AI title, last one wins, a
  missing file falls back to the id), an auto-advance test (oldest pending is picked; nothing is
  pending, so the selection stays), a key test (one `space` doesn't run; `space` `space` does;
  `space` then `x` cancels and doesn't reject), and a test that every argv element appears in
  the rendered detail.
