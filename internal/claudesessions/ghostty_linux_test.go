package claudesessions

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeTabs is a Ghostty whose terminals take a title when written to.
type fakeTabs struct {
	nodes  []tabNode
	ttyOf  map[int]string // node index -> tty, for terminals that exist
	titles map[string]string
	frozen map[string]bool // a tty whose title never changes
	busy   map[string]bool // a tty that repaints its title after each read
	writes []string
}

func (f *fakeTabs) tabs() ([]tabNode, error) { return f.nodes, nil }
func (f *fakeTabs) close()                   {}
func (f *fakeTabs) title(n tabNode) (string, error) {
	for i, c := range f.nodes {
		if c.path != n.path {
			continue
		}
		tty, ok := f.ttyOf[i]
		if !ok {
			return n.title, nil
		}
		t := f.titles[tty]
		if f.busy[tty] {
			f.titles[tty] = "◐ busy"
		}
		return t, nil
	}
	return "", errors.New("no node")
}

func newFake(t *testing.T, titles ...string) *fakeTabs {
	f := &fakeTabs{ttyOf: map[int]string{}, titles: map[string]string{}, frozen: map[string]bool{}, busy: map[string]bool{}}
	for i, ttl := range titles {
		// Two windows: the last tab is alone in window 1.
		win, tab := 0, i
		if i == len(titles)-1 {
			win, tab = 1, 0
		}
		f.nodes = append(f.nodes, tabNode{win, tab, "b", "/" + ttl, ttl})
	}
	oldSet, oldSleep, oldSuffix := setTitle, sleepPoll, markSuffix
	setTitle = func(tty, title string) {
		f.writes = append(f.writes, tty+"="+title)
		if !f.frozen[tty] {
			f.titles[tty] = title
		}
	}
	sleepPoll = func(time.Duration) {}
	markSuffix = func() string { return "x" }
	t.Cleanup(func() { setTitle, sleepPoll, markSuffix = oldSet, oldSleep, oldSuffix })
	return f
}

func (f *fakeTabs) attach(node int, tty string) {
	f.ttyOf[node] = tty
	f.titles[tty] = f.nodes[node].title
}

func TestMatchTabsFindsTabsAndPutsTitlesBack(t *testing.T) {
	f := newFake(t, "one", "two", "three")
	f.attach(1, "/dev/pts/2")
	f.attach(2, "/dev/pts/3")
	f.attach(0, "/dev/pts/1")
	got, err := matchTabs(f, []string{"/dev/pts/3", "/dev/pts/1", "/dev/pts/2"})
	if err != nil {
		t.Fatal(err)
	}
	want := []terminal{{"0", "0", "/dev/pts/1"}, {"0", "1", "/dev/pts/2"}, {"1", "0", "/dev/pts/3"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for tty, title := range map[string]string{"/dev/pts/1": "one", "/dev/pts/2": "two", "/dev/pts/3": "three"} {
		if f.titles[tty] != title {
			t.Fatalf("%s title = %q, want %q", tty, f.titles[tty], title)
		}
	}
}

// A tab with no Claude in it is never written to, and one that won't take a title is left for the extra window.
func TestMatchTabsLeavesOutWhatItCannotPlace(t *testing.T) {
	f := newFake(t, "plain", "stuck", "ok")
	f.attach(1, "/dev/pts/2")
	f.attach(2, "/dev/pts/3")
	f.frozen["/dev/pts/2"] = true
	got, err := matchTabs(f, []string{"/dev/pts/2", "/dev/pts/3"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []terminal{{"1", "0", "/dev/pts/3"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if strings.Contains(strings.Join(f.writes, " "), "/dev/pts/1") {
		t.Fatalf("wrote to a terminal that isn't Claude's: %v", f.writes)
	}
}

// A busy Claude repaints its title: the marker is only there for the read right after a write.
func TestMatchTabsCatchesABusyTab(t *testing.T) {
	f := newFake(t, "busy", "other")
	f.attach(0, "/dev/pts/1")
	f.attach(1, "/dev/pts/2")
	f.busy["/dev/pts/1"] = true
	got, err := matchTabs(f, []string{"/dev/pts/1", "/dev/pts/2"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []terminal{{"0", "0", "/dev/pts/1"}, {"1", "0", "/dev/pts/2"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMatchTabsWithoutTabs(t *testing.T) {
	f := newFake(t)
	f.nodes = nil
	if _, err := matchTabs(f, []string{"/dev/pts/1"}); err == nil {
		t.Fatal("no tabs should be an error, so Save says why the layout is flat")
	}
}

func TestWriteTitleSanitizes(t *testing.T) {
	path := t.TempDir() + "/tty"
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTitle(path, "ok\x07\x1b]0;evil\u009d✳")
	if got, _ := os.ReadFile(path); string(got) != "\x1b]2;ok]0;evil✳\a" {
		t.Fatalf("wrote %q", got)
	}
}

func TestSurfaceOf(t *testing.T) {
	env := []byte("A=1\x00GHOSTTY_SURFACE_ID=0xabc\x00B=2\x00")
	if s, ok := surfaceOf(env); !ok || s != "0xabc" {
		t.Fatalf("surfaceOf = %q %v", s, ok)
	}
	if _, ok := surfaceOf([]byte("NOT_GHOSTTY_SURFACE_ID=1\x00")); ok {
		t.Fatal("matched another variable")
	}
}

func TestGhosttyOfWalksUp(t *testing.T) {
	fakeProcs(t, map[int]procInfo{30: {ppid: 20, name: "2.1.319"}, 20: {ppid: 10, name: "fish"}, 10: {ppid: 1, name: "ghostty"}})
	if pid, ok := ghosttyOf(30); !ok || pid != 10 {
		t.Fatalf("ghosttyOf = %d %v", pid, ok)
	}
	fakeProcs(t, map[int]procInfo{30: {ppid: 1, name: "konsole"}})
	if _, ok := ghosttyOf(30); ok {
		t.Fatal("found Ghostty above a Konsole session")
	}
}

func TestShellIn(t *testing.T) {
	const pw = "root:x:0:0::/root:/bin/bash\ndigit:x:1000:1000::/home/digit:/bin/fish\nodd:x:1:1::/:/bin/sh; rm -rf ~\n"
	for name, want := range map[string]string{"digit": "/bin/fish", "odd": "/bin/sh", "nobody": "/bin/sh"} {
		if got := shellIn(pw, name); got != want {
			t.Fatalf("shellIn(%s) = %q, want %q", name, got, want)
		}
	}
}

func TestOpenWindowsAddsTabsToTheirWindow(t *testing.T) {
	var runs [][]string
	oldRun, oldFind, oldShell, oldSleep := runGhostty, findSurface, shellPath, sleepPoll
	runGhostty = func(a ...string) error { runs = append(runs, a); return nil }
	findSurface = func(id string) (string, bool) { return "0x" + id[:1], true }
	shellPath = func() string { return "/bin/fish" }
	sleepPoll = func(time.Duration) {}
	t.Cleanup(func() { runGhostty, findSurface, shellPath, sleepPoll = oldRun, oldFind, oldShell, oldSleep })

	err := openWindows([][]Tab{{{SessionID: idA, Cwd: "/src/a b"}, {SessionID: idB, Cwd: "/src/b"}}, {{SessionID: idC, Cwd: "/src/c"}}})
	if err != nil {
		t.Fatal(err)
	}
	resume := func(id string) []string {
		return []string{"-e", "/bin/fish", "-l", "-c", "claude --resume " + id + "; exec /bin/fish -l"}
	}
	want := [][]string{
		append([]string{"+new-window", "--working-directory=/src/a b"}, resume(idA)...),
		append([]string{"+new-tab", "--surface-id=0xa", "--working-directory=/src/b"}, resume(idB)...),
		append([]string{"+new-window", "--working-directory=/src/c"}, resume(idC)...),
	}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("runs = %q\nwant %q", runs, want)
	}
}

// Only a session in the window's first tab can name it; without that the other tabs mustn't land in the wrong window.
func TestOpenWindowsStopsWhenTheWindowHasNoSurface(t *testing.T) {
	var runs int
	oldRun, oldFind, oldShell, oldSleep := runGhostty, findSurface, shellPath, sleepPoll
	runGhostty = func(a ...string) error { runs++; return nil }
	findSurface = func(string) (string, bool) { return "", false }
	shellPath = func() string { return "/bin/sh" }
	sleepPoll = func(time.Duration) { time.Sleep(time.Millisecond * 200) }
	t.Cleanup(func() { runGhostty, findSurface, shellPath, sleepPoll = oldRun, oldFind, oldShell, oldSleep })
	if testing.Short() {
		t.Skip("waits for the surface timeout")
	}
	err := openWindows([][]Tab{{{SessionID: idA}, {SessionID: idB}}})
	if err == nil || !strings.Contains(err.Error(), "1 tab") || runs != 1 {
		t.Fatalf("err = %v runs = %d", err, runs)
	}
}
