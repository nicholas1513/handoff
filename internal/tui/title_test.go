package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/nicholas1513/handoff/internal/queue"
)

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
		"last ai wins":    {write("a", fmt.Sprintf(ai, "old"), fmt.Sprintf(ai, "new")), "new"},
		"custom beats ai": {write("b", fmt.Sprintf(custom, "mine"), fmt.Sprintf(ai, "auto")), "mine"},
		"truncated tail":  {write("c", fmt.Sprintf(ai, "good"), `{"type":"ai-title","aiTi`), "good"},
		"junk lines":      {write("d", "not json", fmt.Sprintf(ai, "ok"), "{}"), "ok"},
		"missing file":    {filepath.Join(dir, "nope"), ""},
	}
	for name, c := range cases {
		if got := transcriptTitle(c.path); got != c.want {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
	}
}

func TestTitleCacheRereadsOnChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t")
	os.WriteFile(p, []byte(`{"type":"ai-title","aiTitle":"one"}`), 0o600)
	c := titleCache{}
	if got := c.get(p); got != "one" {
		t.Fatalf("got %q", got)
	}
	os.WriteFile(p, []byte(`{"type":"ai-title","aiTitle":"three"}`), 0o600)
	if got := c.get(p); got != "three" {
		t.Fatalf("not re-read after change: %q", got)
	}
}

func TestSessionLabelSanitizesTitle(t *testing.T) {
	r := sessionRow{id: sid, sess: &queue.Session{SessionID: sid, Cwd: "/x/source"}}
	got := sessionLabel(r, "evil\x1b[31m\nsecond line", 2, time.Now())
	if strings.Contains(got, "\x1b[31m") || strings.Contains(got, "\n") {
		t.Fatalf("title not sanitized to one line: %q", got)
	}
	for _, want := range []string{"● 2", "source"} {
		if !strings.Contains(ansi.Strip(got), want) {
			t.Errorf("label %q missing %q", got, want)
		}
	}
}

func TestSessionLabelFallsBackToShortID(t *testing.T) {
	r := sessionRow{id: sid, sess: &queue.Session{SessionID: sid, Cwd: "/x/source"}}
	if got := sessionLabel(r, "", 0, time.Now()); !strings.Contains(got, sid[:8]) {
		t.Errorf("label %q missing short id", got)
	}
	r.sess = nil
	got := sessionLabel(r, "", 0, time.Now())
	if !strings.HasPrefix(ansi.Strip(got), "⚠") || !strings.Contains(got, sid[:8]) {
		t.Errorf("unverified label %q", got)
	}
}
