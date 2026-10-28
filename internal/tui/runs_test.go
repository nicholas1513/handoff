package tui

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/queue"
)

// runWorld is setup plus "slow" and "long", which sleep until canceled, with the clock fixed and the selection on
// slow. In list order: first, second, slow, long.
func runWorld(t *testing.T) (*queue.Queue, model) {
	t.Helper()
	q, m := setup(t)
	for _, d := range []string{"slow", "long"} {
		addItem(t, q, queue.Item{Description: d, SessionID: sid, Argv: []string{"/bin/sleep", "30"}}, nil)
	}
	clock := time.Now()
	m.now = func() time.Time { return clock }
	return q, pick(t, tickM(m), "slow")
}

// runSelected approves the selection with space space and runs the command off the event loop, as the program does;
// its runDone arrives on the channel. Cleanup stops the run and waits for it, so none outlives the test's files.
func runSelected(t *testing.T, m model) (model, <-chan tea.Msg) {
	t.Helper()
	m, _ = press(m, " ")
	m, cmd := press(m, " ")
	if cmd == nil {
		t.Fatalf("space space did not start a run on %q", m.item().Description)
	}
	stop, c, done := m.runs[m.item().ID].stop, make(chan tea.Msg, 1), make(chan struct{})
	go func() { c <- cmd(); close(done) }()
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("a run did not stop")
		}
	})
	return m, c
}

// await applies the runDone a run sends when it ends.
func await(t *testing.T, m model, c <-chan tea.Msg) model {
	t.Helper()
	select {
	case msg := <-c:
		next, _ := m.Update(msg)
		return next.(model)
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end")
	}
	return m
}

// named is the listed item with that description.
func named(t *testing.T, m model, desc string) queue.Item {
	t.Helper()
	for _, r := range m.rows {
		for _, it := range r.items {
			if it.Description == desc {
				return it
			}
		}
	}
	t.Fatalf("no item %q", desc)
	return queue.Item{}
}

// started waits until its run has started its process, and returns the process id.
func started(t *testing.T, q *queue.Queue, it queue.Item) int {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if pid, ok := q.ClaimPID(it); ok {
			return pid
		}
	}
	t.Fatalf("%s did not start", it.Description)
	return 0
}

func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// TestKeysWorkDuringARun: with a run in flight, keys move to and open other items, open the settings, and move back.
func TestKeysWorkDuringARun(t *testing.T) {
	_, m := runWorld(t)
	m, _ = runSelected(t, m)
	m.focus = 1
	m, _ = press(m, "k")
	if d := drawn(m, 2); rowWith(d, "second") == "" || rowWith(d, "[ Run ]") == "" {
		t.Fatalf("k during a run did not open second:\n%s", strings.Join(d, "\n"))
	}
	if rowWith(drawn(m, 1), "slow", "…") == "" {
		t.Fatalf("the run is no longer drawn:\n%s", strings.Join(drawn(m, 1), "\n"))
	}
	if m, _ = press(m, "s"); !strings.Contains(ansi.Strip(m.View()), "Diff word wrap") {
		t.Fatal("s did not open the settings during a run")
	}
	m, _ = press(m, "esc")
	m, _ = press(m, "j")
	if d := drawn(m, 2); rowWith(d, "slow") == "" || rowWith(d, "RUNNING") == "" || rowWith(d, "[ Cancel run ]") == "" {
		t.Fatalf("j did not open the run again:\n%s", strings.Join(d, "\n"))
	}
}

// TestReopeningARunKeepsItsFiles: moving back onto a run shows what was reviewed and leaves the snapshot files it
// runs with alone, though the agent has changed the item since.
func TestReopeningARunKeepsItsFiles(t *testing.T) {
	q, m := setup(t)
	it := addItem(t, q, queue.Item{Description: "att", SessionID: sid, Argv: []string{"/bin/sleep", "30"}},
		map[string]string{"notes.txt": tempFile(t, "notes.txt", "REVIEWED-TEXT\n")})
	m, _ = runSelected(t, pick(t, tickM(m), "att"))
	started(t, q, it)
	if err := os.WriteFile(filepath.Join(q.Root, "queue", it.ID, "files", "notes.txt"), []byte("SWAPPED-TEXT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.focus = 1
	m, _ = press(m, "k")
	m, _ = press(m, "j")
	if m.item().ID != it.ID {
		t.Fatalf("setup: selected %q", m.item().Description)
	}
	if b, _ := os.ReadFile(filepath.Join(q.Root, "snap", it.ID, "notes.txt")); string(b) != "REVIEWED-TEXT\n" {
		t.Errorf("reopening the run rewrote the file it was given: %q", b)
	}
	if d := strings.Join(drawn(m, 2), "\n"); !strings.Contains(d, "REVIEWED-TEXT") || strings.Contains(d, "SWAPPED-TEXT") {
		t.Errorf("reopening the run shows something other than what was reviewed:\n%s", d)
	}
}

// TestRunsInParallel: a second run starts while the first is in flight and both are drawn running. The second ends
// first and updates only itself: the first is still drawn running and the selection, on the first, stays.
func TestRunsInParallel(t *testing.T) {
	trueColor(t)
	q, m := runWorld(t)
	m, slow := runSelected(t, m)
	m.focus = 1
	m, _ = press(m, "k")
	m, second := runSelected(t, m)
	if items := drawn(m, 1); rowWith(items, "slow", "…") == "" || rowWith(items, "second", "…") == "" || rowWith(items, "first", "●") == "" {
		t.Fatalf("both runs are not drawn running:\n%s", strings.Join(items, "\n"))
	}
	if d := drawn(m, 2); rowWith(d, "RUNNING") == "" || rowWith(d, "[ Cancel run ]") == "" {
		t.Fatalf("the second run's detail does not show it running:\n%s", strings.Join(d, "\n"))
	}
	sameAsFresh(t, m, "a second run starting")
	m, _ = press(m, "j") // back onto slow
	m = await(t, m, second)
	if s := q.State(named(t, m, "second"), time.Now()); s != queue.Succeeded {
		t.Fatalf("second is %s", s)
	}
	if items := drawn(m, 1); rowWith(items, "second", "✓") == "" || rowWith(items, "slow", "…") == "" {
		t.Errorf("the run that ended is not drawn ended, or the other not running:\n%s", strings.Join(items, "\n"))
	}
	if d := drawn(m, 2); m.item().Description != "slow" || rowWith(d, "RUNNING") == "" || rowWith(d, "[ Cancel run ]") == "" {
		t.Errorf("the selection left slow, or slow is not drawn running:\n%s", strings.Join(d, "\n"))
	}
	if l := statusLine(m); !strings.Contains(l, "finished: second") {
		t.Errorf("the status does not name the run that ended: %q", l)
	}
	select {
	case <-slow:
		t.Error("slow ended with second")
	default:
	}
	sameAsFresh(t, m, "the second run ending")
}

// TestCancelOnlyTheSelectedRun: c c and the Cancel run button stop the selected run and no other, and a prompt raised
// on one run answers nothing once the wheel has moved the selection to another.
func TestCancelOnlyTheSelectedRun(t *testing.T) {
	key := func(k string) func(model) model { return func(m model) model { m, _ = press(m, k); return m } }
	at := func(x int) func(model) model {
		return func(m model) model { sw, iw, _ := columns(m.w); m, _ = click(m, sw+iw+x+1, buttonY); return m }
	}
	for what, way := range map[string][2]func(model) model{
		"c c":                                 {key("c"), key("c")},
		"[ Cancel run ] then [ Yes, cancel ]": {at(cancelRunX), at(yesCancelX)},
	} {
		t.Run(what, func(t *testing.T) {
			q, m := runWorld(t)
			sw, _, _ := columns(m.w)
			m, slowC := runSelected(t, m)
			m.focus = 1
			m, _ = press(m, "j")
			m, longC := runSelected(t, m)
			slow, long := named(t, m, "slow"), named(t, m, "long")
			started(t, q, slow)
			started(t, q, long)

			m = way[0](m) // raised on long
			m = mouseAt(m, sw+3, listTop+1, tea.MouseButtonWheelUp)
			if m = way[1](m); m.item().ID != slow.ID || strings.Contains(statusLine(m), "canceling") {
				t.Fatalf("a prompt raised on long canceled slow (selected %q)", m.item().Description)
			}
			m = mouseAt(m, sw+3, listTop+1, tea.MouseButtonWheelDown) // back onto long
			m = way[1](way[0](m))
			m = await(t, m, longC)
			if s := q.State(long, time.Now()); s != queue.Canceled {
				t.Errorf("long is %s, want canceled", s)
			}
			if s := q.State(slow, time.Now()); s != queue.Running || rowWith(drawn(m, 1), "slow", "…") == "" {
				t.Errorf("canceling long touched slow: %s\n%s", s, strings.Join(drawn(m, 1), "\n"))
			}
			select {
			case <-slowC:
				t.Error("slow ended with long")
			default:
			}
		})
	}
}

// TestQuitAsksAgainWhileRunsAreInFlight: with runs in flight q or ctrl+c asks first and a second one quits; any other
// key withdraws the question and is not acted on, and so does the last run ending. With none in flight q quits at once.
func TestQuitAsksAgainWhileRunsAreInFlight(t *testing.T) {
	q, m := runWorld(t)
	m, slowC := runSelected(t, m)
	m.focus = 1
	m, cmd := press(m, "q")
	if quits(cmd) || !strings.Contains(statusLine(m), "1 run in progress: q again to quit") {
		t.Fatalf("q with a run in flight quit or did not ask: %q", statusLine(m))
	}
	if m, cmd = press(m, "k"); quits(cmd) || m.item().Description != "slow" || strings.Contains(statusLine(m), "q again") {
		t.Fatalf("k after q was acted on, or left the question up (selected %q): %q", m.item().Description, statusLine(m))
	}
	m, _ = press(m, "k")
	m, secondC := runSelected(t, m)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if m = next.(model); quits(cmd) || !strings.Contains(statusLine(m), "2 runs in progress: q again to quit") {
		t.Fatalf("ctrl+c with two runs in flight quit or did not ask: %q", statusLine(m))
	}
	if _, cmd = press(m, "q"); !quits(cmd) {
		t.Fatal("a second q did not quit")
	}

	m = await(t, m, secondC) // still asking, for the one left
	if l := statusLine(m); !strings.Contains(l, "1 run in progress: q again to quit") {
		t.Fatalf("the question does not count the run left: %q", l)
	}
	if err := syscall.Kill(-started(t, q, named(t, m, "slow")), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	m = await(t, m, slowC) // ends on its own: the question goes with it
	if strings.Contains(statusLine(m), "q again") {
		t.Fatalf("the question is still drawn with no runs in flight: %q", statusLine(m))
	}
	at := m.item().Description
	if m, _ = press(m, "j"); m.item().Description == at {
		t.Fatal("the withdrawn question took the next key")
	}
	if _, cmd = press(m, "q"); !quits(cmd) {
		t.Fatal("q with no runs in flight did not quit at once")
	}
}

// TestFastTapsCannotRunAnUnseenItem: taps after space space land on the running item, where they do nothing; once it
// ends and the selection advances, the advance guard holds them for a second.
func TestFastTapsCannotRunAnUnseenItem(t *testing.T) {
	q, m := setup(t)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	first := named(t, m, "first")
	m, done := runSelected(t, m) // second
	taps := func(when string) {
		t.Helper()
		for range 4 {
			var cmd tea.Cmd
			if m, cmd = press(m, " "); cmd != nil || m.armed {
				t.Fatalf("%s: a tap armed or started a run on %q", when, m.item().Description)
			}
		}
	}
	taps("on the running item")
	m = await(t, m, done)
	if m.item().ID != first.ID {
		t.Fatalf("the run's end did not advance to first (selected %q)", m.item().Description)
	}
	taps("right after the advance")
	if s := q.State(first, time.Now()); s != queue.Pending {
		t.Fatalf("first is %s", s)
	}
	clock = clock.Add(1100 * time.Millisecond)
	if m, _ = press(m, " "); !m.armed {
		t.Fatal("space after the guard did not arm")
	}
}

// TestRejectDuringAnotherRun: x rejects a pending item while another runs, and the advance that follows passes over
// the run, even before its claim lands.
func TestRejectDuringAnotherRun(t *testing.T) {
	q, m := runWorld(t)
	m, _ = press(pick(t, m, "first"), " ")
	m, cmd := press(m, " ") // held, so its claim lands only after the reject
	if cmd == nil {
		t.Fatal("space space did not start a run on first")
	}
	m.focus = 1
	m, _ = press(m, "j")
	m, _ = press(m, "x")
	if s := q.State(named(t, m, "second"), time.Now()); s != queue.Rejected {
		t.Fatalf("second is %s, want rejected", s)
	}
	if m.item().Description != "slow" {
		t.Fatalf("the advance after the reject selected %q, want slow (first runs)", m.item().Description)
	}
	if items := drawn(m, 1); rowWith(items, "first", "…") == "" || rowWith(items, "second", "⊘") == "" {
		t.Fatalf("the run or the reject is not drawn:\n%s", strings.Join(items, "\n"))
	}
	next, _ := m.Update(cmd()) // the run, start to end
	if m = next.(model); rowWith(drawn(m, 1), "first", "✓") == "" || m.item().Description != "slow" {
		t.Fatalf("after first ended (selected %q):\n%s", m.item().Description, strings.Join(drawn(m, 1), "\n"))
	}
}

// TestALoadBeforeARunsDoneKeepsTheSelection: a load that reads a run's result before its runDone is applied still
// draws the run as running and doesn't pull the selection onto a new item; the runDone then moves it, guarded.
func TestALoadBeforeARunsDoneKeepsTheSelection(t *testing.T) {
	q, m := setup(t)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	finished(t, q, named(t, m, "first"), queue.Succeeded, "")
	m, _ = press(pick(t, tickM(m), "second"), " ")
	m, cmd := press(m, " ")
	if cmd == nil {
		t.Fatal("space space did not start a run on second")
	}
	msg := cmd() // the run, start to end: its result is on disk and its runDone not yet applied
	addItem(t, q, queue.Item{Description: "arrived", SessionID: sid}, nil)
	if m = tickM(m); m.item().Description != "second" || rowWith(drawn(m, 2), "[ Cancel run ]") == "" {
		t.Fatalf("a load before the runDone pulled the selection off the run (selected %q):\n%s",
			m.item().Description, strings.Join(drawn(m, 2), "\n"))
	}
	next, _ := m.Update(msg)
	if m = next.(model); m.item().Description != "arrived" {
		t.Fatalf("the runDone did not advance to arrived (selected %q)", m.item().Description)
	}
	if m, cmd = press(m, " "); cmd != nil || m.armed {
		t.Fatal("space right after the advance armed or ran arrived")
	}
}

// TestCancelingAStaleRunSparesOwnRuns: a run left behind by a viewer that went away recorded a pid the OS has since
// given to one of this viewer's runs, running the same program. Canceling the stale run ends it and signals nothing.
func TestCancelingAStaleRunSparesOwnRuns(t *testing.T) {
	q, m := runWorld(t)
	m, slowC := runSelected(t, m)
	slow := named(t, m, "slow")
	stale := addItem(t, q, queue.Item{Description: "stale", SessionID: sid, Argv: []string{"/bin/sleep", "30"}}, nil)
	if err := q.Claim(stale); err != nil {
		t.Fatal(err)
	}
	if err := q.RecordPID(stale, started(t, q, slow)); err != nil {
		t.Fatal(err)
	}
	m = pick(t, tickM(m), "stale")
	m.focus = 1
	m, _ = press(m, "c")
	m, _ = press(m, "c")
	if res, _ := q.ReadResult(stale); res.State != queue.Canceled || !strings.Contains(res.Stderr, "nothing was stopped") {
		t.Fatalf("stale's result = %+v", res)
	}
	select {
	case <-slowC:
		t.Fatal("canceling the stale run stopped this viewer's run of slow")
	case <-time.After(time.Second):
	}
	if s := q.State(slow, time.Now()); s != queue.Running || rowWith(drawn(m, 1), "slow", "…") == "" {
		t.Errorf("slow is %s, or not drawn running:\n%s", s, strings.Join(drawn(m, 1), "\n"))
	}
}

// TestStatusAfterARunKeepsTheKeyHints: the status naming a run that ended leaves the key hints on the line, also
// after moving on. Descriptions are often a line or two long.
func TestStatusAfterARunKeepsTheKeyHints(t *testing.T) {
	q, m := setup(t)
	desc := "Push the release branch to origin and open the pull request against main, so CI can run the integration suite before the cut."
	addItem(t, q, queue.Item{Description: desc, SessionID: sid}, nil)
	m, _ = press(pick(t, tickM(m), desc), " ")
	m, cmd := press(m, " ")
	if cmd == nil {
		t.Fatal("space space did not start a run")
	}
	next, _ := m.Update(cmd())
	m = next.(model)
	m.focus = 1
	for _, k := range []string{"k", "j"} {
		if l := ansi.Strip(ansi.Truncate(statusLine(m), m.w, "")); !strings.Contains(l, "finished: Push") ||
			!strings.Contains(l, "s settings") || !strings.Contains(l, "q quit") {
			t.Fatalf("before %s, the status line as cut at the window's width: %q", k, l)
		}
		m, _ = press(m, k)
	}
}

// TestOneAmIntoARepositoryAtATime: a second git am into the repository a run is applying to would pass its base pin
// before the first moves HEAD, then apply on top of the first's commits, not on what was reviewed.
func TestOneAmIntoARepositoryAtATime(t *testing.T) {
	needGit(t)
	files := map[string]string{"f": "hello\n", "g": "x\n"}
	repo := mkRepo(t, files)
	_, other := onePatch(t, files, map[string]string{"g": "y\n"})
	_, m := setup(t)
	m = built(amModel(t, m, repo, fixture(t, "rv-patchsec__plain"), 160, 40))
	m = built(amModel(t, m, repo, other, 160, 40))
	m.focus, m.advancedAt = 2, time.Time{}
	m, _ = press(m, " ")
	m, first := press(m, " ")
	if first == nil {
		t.Fatal("the first git am did not start")
	}
	m.focus = 1
	m, _ = press(m, "k")
	m = built(m)
	m.focus, m.advancedAt = 2, time.Time{}
	m, _ = press(m, " ")
	if m, cmd := press(m, " "); cmd != nil || !strings.Contains(statusLine(m), "still running") {
		t.Fatalf("a second git am into the same repository started while the first runs: status %q", statusLine(m))
	}
	next, _ := m.Update(first())
	if m = tickM(next.(model)); rowWith(drawn(m, 2), "Changed on disk") == "" {
		t.Fatalf("after the first git am, the second is not drawn as changed:\n%s", strings.Join(drawn(m, 2), "\n"))
	}
}
