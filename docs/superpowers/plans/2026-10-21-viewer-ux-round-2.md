# Viewer UX Round 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the `handoff` viewer faster to review with, and no less careful: a detail pane you can
scan, session titles like `/resume`, auto-advance to the next pending item, and `space space` to run.

**Architecture:** Every change is in `internal/tui`. `tui.go` keeps the model, keys, mouse and
layout. The detail rendering moves to a new `detail.go`, and the transcript title reader goes in a
new `title.go`. Nothing in `queue`, `runner`, `rules`, `hook` or `cmd` changes.

**Tech Stack:** Go 1.23, Bubble Tea v1.3.4, lipgloss v1.1.0, `github.com/charmbracelet/x/ansi`
(already in `go.sum` as an indirect dependency; `go mod tidy` promotes it to direct, and no new
module is added).

**Spec:** `docs/superpowers/specs/2026-10-21-viewer-ux-round-2-design.md`. Read it first. Its
anti-goals and its "Calls made after the builder check" section are requirements.

## Global Constraints

- Run every test with `go test -race ./...`. Inside the Claude sandbox, prefix it with `GOCACHE=$TMPDIR/gocache GOPATH=$TMPDIR/gopath`.
- Every string from `queue/`, the transcript or a result goes through `sanitize` before it's displayed.
- Never hide an argv element: each element's sanitized text must appear verbatim once ANSI codes and line wraps are removed.
- A single keypress never runs an item. The mouse keeps Run → `[ Yes, run ]` at `yesX`, which is not where Run is.
- Use ANSI palette colors only (`lipgloss.Color("0")`–`("15")`), as `tui.go` does now.
- Mouse geometry constants (`listTop`, `buttonY`, `runX`, `rejectX`, `cancelX`, `yesX`) keep their values, and the button row stays the detail pane's first line.
- Out of scope: approve-once rules, cwd warnings, and any change outside `internal/tui`.

## Relation to the v2 roadmap (`~/.claude/plans/handoff-tool.md`, deferred items in `handoff-tool-v1.md`)

This round builds none of the v2 items. It's checked against them so they still fit afterwards:
- **Batches (`b` key, one confirm after scrolling every item, stop at the first failure):** `b` stays unbound. Auto-advance and `space space` are single-item. A later batch run should reuse the keyboard-arm state (`armed`) rather than add a third confirm path.
- **Remembered auto-rules (`~/.config/handoff/rules.toml`, written only from viewer clicks; tabled again 2026-10-21):** the chips line in Task 3 is where an "approved by rule" chip would go. Nothing here reads or writes rules.
- **fsnotify instead of the 2s tick:** auto-advance relies only on `refresh` keeping the selection by item ID, which holds whatever triggers the refresh. The title cache keys on mtime and size, so it works either way.
- **`gc`:** unaffected. The "drop keys while running" guard is the in-memory `m.running`, not the on-disk `running` state that `gc` would clear.
- **Configurable expiry and Homebrew:** unaffected.

## Review Focus

1. **A transcript that's mid-write or malformed** (a truncated last line, non-JSON lines) must give the last good title, not an error. Tested in Task 1.
2. **A title with newlines, ANSI escapes or bidi characters** must render on one sanitized line. Tested in Task 1.
3. **Argv with no flags, a lone `-` value (`--body -`), or `--flag=value`** must still show every element verbatim. Tested in Task 2.
4. **A narrow terminal** (`WindowSizeMsg{Width: 60}`) must render without panicking, with every argv element still present. Tested in Task 3.
5. **The 2s refresh tick right after an auto-advance** must not move the selection back. Tested in Task 5.

---

### Task 1: Session titles from the transcript

**Files:**
- Create: `internal/tui/title.go`
- Modify: `internal/tui/tui.go` (`sessionLabel`, its caller in `View`, and the `model` struct)
- Test: `internal/tui/title_test.go`

**Interfaces:**
- Produces: `func transcriptTitle(path string) string` returns the last `custom-title`'s `customTitle`; if there's none, the last `ai-title`'s `aiTitle`; otherwise `""`. Lines that aren't valid JSON are skipped.
- Produces: `type titleCache map[string]titleEntry` with `func (c titleCache) get(path string) string`, re-reading only when the file's mtime or size changes. `titleEntry{mod time.Time; size int64; title string}`.
- Produces: `func sessionLabel(r sessionRow, title string, pending int, now time.Time) string`. The model gets a `titles titleCache` field, initialized in `newModel`.

- [ ] **Step 1: Write the failing tests**

```go
func TestTranscriptTitle(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, lines ...string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(strings.Join(lines, "\n")), 0o600)
		return p
	}
	ai := `{"type":"ai-title","aiTitle":"%s","sessionId":"x"}`
	custom := `{"type":"custom-title","customTitle":"%s","sessionId":"x"}`
	cases := map[string]struct{ path, want string }{
		"last ai wins":      {write("a", fmt.Sprintf(ai, "old"), fmt.Sprintf(ai, "new")), "new"},
		"custom beats ai":   {write("b", fmt.Sprintf(custom, "mine"), fmt.Sprintf(ai, "auto")), "mine"},
		"truncated tail":    {write("c", fmt.Sprintf(ai, "good"), `{"type":"ai-title","aiTi`), "good"},
		"junk lines":        {write("d", "not json", fmt.Sprintf(ai, "ok"), "{}"), "ok"},
		"missing file":      {filepath.Join(dir, "nope"), ""},
	}
	for name, c := range cases {
		if got := transcriptTitle(c.path); got != c.want {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
	}
}

func TestSessionLabelSanitizesTitle(t *testing.T) {
	r := sessionRow{id: sid, sess: &queue.Session{SessionID: sid, Cwd: "/x/source"}}
	got := sessionLabel(r, "evil\x1b[31m\nsecond line", 2, time.Now())
	if strings.Contains(got, "\x1b[31m") || strings.Contains(got, "\n") {
		t.Fatalf("title not sanitized to one line: %q", got)
	}
	for _, want := range []string{"2", "source"} {
		if !strings.Contains(ansi.Strip(got), want) {
			t.Errorf("label %q missing %q", got, want)
		}
	}
}
```

Add one more test, `TestSessionLabelFallsBackToShortID`: with `title == ""`, the label contains `sid[:8]`. With `sess == nil`, it still starts with `⚠` and contains `sid[:8]`.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test -race ./internal/tui -run 'Title|SessionLabel'`
Expected: a compile failure (`transcriptTitle` undefined).

- [ ] **Step 3: Implement it**

Scan with `bufio.Scanner`, using a buffer max of 16 MB, since transcript lines can be large. Only `json.Unmarshal` lines that contain `"-title"`, so a 3 MB file stays cheap. The label format is `<● N >title · <cwd base> · <age>`, where `●` and the count appear only when `pending > 0`. Age: `<1m` → `now`, under an hour → `Nm ago`, under a day → `Nh ago`, otherwise `Nd ago`, measured from the newest item's `Created`. Pass the result through `oneLine(...)`. Unverified rows keep `warn.Render("⚠ "+short)`. In `View`, count pending items with `m.q.State(it, now) == queue.Pending`.

- [ ] **Step 4: Run them and confirm they pass**

Run: `go test -race ./internal/tui`
Expected: PASS (including the existing tests).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/title.go internal/tui/title_test.go internal/tui/tui.go go.mod go.sum
git commit -m "Viewer: show /resume titles in the sessions pane"
```

---

### Task 2: Command layout

**Files:**
- Create: `internal/tui/detail.go` (this task adds only the command helpers)
- Test: `internal/tui/detail_test.go`

**Interfaces:**
- Produces: `func commandLines(argv []string, sizes map[string]int) []string` returns styled lines. Line 1 is the program and its leading non-flag words (cyan). Then one line per flag: the flag (purple), padded to the longest flag, then its value. Then one line per positional argument. `sizes` maps attachment name → bytes, for the `📎` decoration.
- Produces: `func methodOf(argv []string) string` returns the upper-cased value after `--method`, `-X` or `--request`, or `--method=…`; `""` if absent.
- Produces: `func flatten(s string) string`, a helper in `detail.go` used by the tests: `ansi.Strip`, then remove every `\n` together with the whitespace that follows it.

- [ ] **Step 1: Write the failing tests**

```go
func TestCommandLinesShowEveryElementVerbatim(t *testing.T) {
	cases := [][]string{
		{"curl", "--request", "PUT", "--user-agent", "handoff-test/1.0",
			"--url", "https://api.example.com/v1/organizations/example-org/projects/example-project/dashboards/3c9a1f2e-7b4d-4e8a-9f61-2d5c8b0e7a43?api-version=2024-01-01",
			"--data", "@{file:body.json}"},
		{"/usr/bin/true"},
		{"curl", "--data", "-", "-X", "POST", "https://x.test/a"},
		{"gh", "api", "--method=get", "--hostname=x.test", "repos/o/r"},
		{"bash", "-c", "echo hi; rm -rf ~/x"},
	}
	for _, argv := range cases {
		got := flatten(strings.Join(commandLines(argv, map[string]int{"body.json": 6100}), "\n"))
		for _, a := range argv {
			if !strings.Contains(got, sanitize(a)) {
				t.Errorf("argv %q: element %q missing from %q", argv, a, got)
			}
		}
	}
}

func TestMethodOf(t *testing.T) {
	for want, argv := range map[string][]string{
		"PUT": {"gh", "api", "--method", "put"}, "GET": {"gh", "api", "--method=get"},
		"POST": {"curl", "-X", "POST"}, "": {"gh", "auth", "status"},
	} {
		if got := methodOf(argv); got != want {
			t.Errorf("methodOf(%q) = %q want %q", argv, got, want)
		}
	}
}
```

Add `TestAttachmentDecoration`: for `@{file:body.json}`, the flattened output contains both `@{file:body.json}` and `📎`, plus `6.1 KB`.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test -race ./internal/tui -run 'CommandLines|MethodOf|Attachment'`
Expected: a compile failure.

- [ ] **Step 3: Implement it**

Pairing rule: a word starting with `-` is a flag. The next word is its value unless that word starts with `--`; a lone `-` counts as a value. `--flag=value` stays one word. If a flag is followed by another flag, it gets its own line with no value. URLs (values starting with `http://` or `https://`): break them at `/` into continuation lines, indented under the value column, never cutting a segment. Host and last segment in color 6, the middle faint. `{file:name}` gets `📎 ` in front and ` (attached, N.N KB, see below)` after it, in color 3, with the original text unchanged. Sanitize every word before styling.

- [ ] **Step 4: Run them and confirm they pass**

Run: `go test -race ./internal/tui`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/detail.go internal/tui/detail_test.go go.mod go.sum
git commit -m "Viewer: lay out commands by flag, decorate URLs and attachments"
```

---

### Task 3: Detail pane in sections

**Files:**
- Modify: `internal/tui/detail.go` (move `detail()` here from `tui.go` and rewrite it)
- Modify: `internal/tui/tui.go` (delete the old `detail()`)
- Test: `internal/tui/detail_test.go`

**Interfaces:**
- Consumes: `commandLines`, `methodOf`, `flatten` (Task 2).
- Produces: `func (m model) detail() string`, same signature as now.

- [ ] **Step 1: Write the failing tests**

```go
func TestDetailOrderAndTrustedSession(t *testing.T) {
	_, m := setup(t)
	out := ansi.Strip(m.detail())
	order := []string{"PENDING", "second", "── Command", "/usr/bin/true", "── Why", "do the thing", sid, "── Integrity"}
	last := -1
	for _, s := range order {
		i := strings.Index(out, s)
		if i <= last {
			t.Fatalf("%q missing or out of order in:\n%s", s, out)
		}
		last = i
	}
	if !strings.Contains(out, "verified") {
		t.Fatal("verified marker missing")
	}
}
```

Add these three tests:
- `TestDetailWarningsAboveCommand`: an item with argv `{"bash","-c","echo"}` puts `runs an interpreter` before `── Command`.
- `TestDetailNarrowTerminal`: after `m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})`, `flatten(m.View())` contains every argv element of a long `curl … --url https://…` item, and nothing panics.
- `TestDetailPrettyPrintsJSONAttachment`: a `{file:body.json}` attachment containing `{"a":1}` renders with `"a": 1` under a `── 📎 body.json` heading.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test -race ./internal/tui -run Detail`
Expected: FAIL (the headings don't exist yet).

- [ ] **Step 3: Implement it**

Follow the order in the spec's section 1:
1. Chips line: the state upper-cased (color 3 when pending, 2 when succeeded, 1 when failed or rejected), the method chip (GET in color 2; PUT, POST, PATCH and DELETE in color 1; anything else plain), verified or `⚠ unverified session`, and the time left.
2. The description in bold.
3. The warnings (`changed`, `unverified`, `interpreter`, `settings problems`).
4. `── Command ──`, then `commandLines`.
5. `── Why ──`: the prompt time and text; the verified session's full id and its hook cwd; one faint line with the reason · the rule badge · the item cwd, with `~` in place of `m.home`.
6. One `── 📎 name ──` per file: `json.Indent` when it parses, otherwise the raw content.
7. `── Integrity ──`, then the hash, faint.
8. `── Result ──` when there is one.

Write the section heading helper as `func heading(title string) string`, rendering `── ` + title + ` ──` faint, with the title in color 4.

- [ ] **Step 4: Run them and confirm they pass**

Run: `go test -race ./internal/tui`
Expected: PASS, including `TestClickSelectsAndRunNeedsConfirm` (it checks for `do the thing` and `verified`).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/detail.go internal/tui/detail_test.go internal/tui/tui.go
git commit -m "Viewer: detail pane in sections with chips, headings and pretty attachments"
```

---

### Task 4: `space space` to run, separate from the mouse confirm

**Files:**
- Modify: `internal/tui/tui.go` (`model`, `key`, `mouse`, `View` header, the status help line)
- Test: `internal/tui/tui_test.go`

**Interfaces:**
- Produces: a model field `armed bool` (the keyboard arm), kept separate from `confirm bool` (the mouse confirm). Produces the constant `armedBtn = "space again to run · any other key cancels"`.

- [ ] **Step 1: Write the failing tests**

```go
func press(m model, k string) (model, tea.Cmd) {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	if k == " " {
		msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	}
	next, cmd := m.Update(msg)
	return next.(model), cmd
}

func TestSpaceSpaceRuns(t *testing.T) {
	q, m := setup(t)
	m, cmd := press(m, " ")
	if !m.armed || cmd != nil || q.State(*m.item(), time.Now()) != queue.Pending {
		t.Fatal("one space must only arm")
	}
	if !strings.Contains(m.View(), "space again to run") {
		t.Fatal("armed prompt not shown")
	}
	m, cmd = press(m, " ")
	if cmd == nil {
		t.Fatal("second space did not start the run")
	}
}

func TestArmedOtherKeyCancelsAndIsNotActedOn(t *testing.T) {
	q, m := setup(t)
	m, _ = press(m, " ")
	m, _ = press(m, "x")
	if m.armed || q.State(*m.item(), time.Now()) != queue.Pending {
		t.Fatal("x while armed must cancel, not reject")
	}
}

func TestClickAtYesWhileKeyArmedDoesNotRun(t *testing.T) {
	_, m := setup(t)
	sw, iw, _ := columns(m.w)
	m, _ = press(m, " ")
	m, cmd := click(m, sw+iw+yesX+1, buttonY)
	if cmd != nil || m.armed {
		t.Fatal("a click must cancel a key arm, never run")
	}
}
```

Add `TestKeysDroppedWhileRunning`: set `m.running = true`, press `" "` twice, and confirm `m.armed == false` and `cmd == nil`.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test -race ./internal/tui -run 'Space|Armed|KeyArmed|Running'`
Expected: FAIL.

- [ ] **Step 3: Implement it**

In `key`:
1. If `m.running`, drop every key except `q` and `ctrl+c`.
2. If `m.armed`: `" "` → `startRun()`; anything else → `armed = false` and return.
3. If `m.confirm` (the mouse confirm): any key cancels it.
4. Otherwise, `" "` sets `armed = m.pending()`.

Keep `r` as an alias that arms exactly like `" "`. Remove `y`. In `mouse`, any left press cancels `armed` before it's handled. When armed, the header renders `warn.Render(armedBtn)`, with no clickable region. Status help: `↑↓ move · tab/click pane · space space run · x reject · o re-open · q quit`.

- [ ] **Step 4: Run them and confirm they pass**

Run: `go test -race ./internal/tui`
Expected: PASS, including the existing mouse test.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/tui.go internal/tui/tui_test.go
git commit -m "Viewer: space space runs; keyboard arm separate from mouse confirm"
```

---

### Task 5: Auto-advance to the oldest pending item

**Files:**
- Modify: `internal/tui/tui.go` (`model`, `runDone` handling, `reject`, `refresh`, the space guard)
- Test: `internal/tui/tui_test.go`

**Interfaces:**
- Consumes: `armed` and the `key` flow (Task 4).
- Produces: model fields `now func() time.Time` (`time.Now` in `newModel`; tests replace it), and `advancedAt time.Time`. Produces `func (m *model) advance(fromID string)`, which runs only if the selected item's ID equals `fromID`, and selects the session's pending item with the earliest `Created`.
- Produces the constant `advanceGuard = time.Second`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRejectAdvancesToOldestPending(t *testing.T) {
	_, m := setup(t) // "second" (newest) is selected, "first" is older
	m, _ = press(m, "x")
	if m.item().Description != "first" {
		t.Fatalf("after reject selected %q, want oldest pending %q", m.item().Description, "first")
	}
}

func TestAdvanceSurvivesRefresh(t *testing.T) {
	_, m := setup(t)
	m, _ = press(m, "x")
	next, _ := m.Update(tickMsg(time.Now()))
	if next.(model).item().Description != "first" {
		t.Fatal("refresh tick moved the selection back")
	}
}

func TestNoAdvanceIfUserMovedAway(t *testing.T) {
	_, m := setup(t)
	ran := m.item().ID
	m.focus = 1
	m.move(1) // user moves to "first" while the run is in flight
	m.advance(ran)
	if m.item().Description != "first" {
		t.Fatal("advance must not fire when the selection moved off the item that ran")
	}
}

func TestSpaceIgnoredRightAfterAdvance(t *testing.T) {
	_, m := setup(t)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	m, _ = press(m, "x") // advances
	m, cmd := press(m, " ")
	if m.armed || cmd != nil {
		t.Fatal("space within 1s of an auto-advance must be ignored")
	}
	clock = clock.Add(1100 * time.Millisecond)
	m, _ = press(m, " ")
	if !m.armed {
		t.Fatal("space after the guard should arm")
	}
}
```

Add `TestNothingPendingStays`: reject both items; the selection stays on the last-rejected item, and `advancedAt` is unchanged by the final reject.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test -race ./internal/tui -run 'Advance|Pending|IgnoredRightAfter'`
Expected: FAIL.

- [ ] **Step 3: Implement it**

1. `reject()` calls `m.advance(id)` after writing its result.
2. The `runDone` case calls `m.refresh()` and then `m.advance(msg.id)`.
3. When `advance` moves the selection, it sets `m.advancedAt = m.now()`, `m.reviewed = nil` (so `open` snapshots the new item) and `m.open()`.
4. `refresh` is unchanged: it already keeps the selection by the selected item's ID. The jump survives the tick because `advance` moves the selection before the tick arrives (`TestAdvanceSurvivesRefresh` checks this).
5. In `key`, `" "` and `r` are ignored while `m.now().Sub(m.advancedAt) < advanceGuard`.

- [ ] **Step 4: Run them and confirm they pass**

Run: `go test -race ./...`
Expected: PASS in all packages.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/tui.go internal/tui/tui_test.go
git commit -m "Viewer: auto-advance to the oldest pending item, with a 1s space guard"
```

---

### Task 6: Docs and a manual check

**Files:**
- Modify: `README.md` (keys and the viewer description)
- Modify: `~/.claude/plans/handoff-tool-v1.md` ("Viewer (v1)" keys line and the round-2 status)

- [ ] **Step 1: Update the README key list** to `space space` run, `x` reject, `o` re-open and `q` quit, and describe the sectioned detail pane and the session titles in one sentence each.
- [ ] **Step 2: Rebuild and run the viewer** with `go build -o $TMPDIR/handoff ./cmd/handoff && $TMPDIR/handoff`. The user runs the real viewer from their own terminal after `scripts/setup.py`. Check:
  - the session row shows this session's `/resume` title;
  - a pending item shows the chips, the headings and the full command;
  - `space space` runs it, and the selection jumps to the next pending item.
- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "Docs: viewer round 2 keys and layout"
```
