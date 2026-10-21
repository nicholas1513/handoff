package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicholas1513/handoff/internal/queue"
)

const sid = "0b6c2a8e-1f3d-4c55-9a7e-2d4f6b8c0e12"

func stage(t *testing.T, argv ...string) (*queue.Queue, queue.Reviewed) {
	t.Helper()
	q := &queue.Queue{Root: t.TempDir()}
	body := filepath.Join(t.TempDir(), "body.json")
	os.WriteFile(body, []byte(`{"reviewed":true}`), 0o600)
	it, err := q.Add(queue.Item{Argv: argv, Cwd: t.TempDir(), SessionID: sid}, map[string]string{"body.json": body})
	if err != nil {
		t.Fatal(err)
	}
	r, err := q.Snapshot(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	return q, r
}

func TestNoShellInterpretation(t *testing.T) {
	arg := `a; touch pwned $(id) | cat > x && echo`
	q, r := stage(t, "/bin/echo", arg)
	res, err := Run(context.Background(), q, r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.State != queue.Succeeded || res.Stdout != arg+"\n" {
		t.Fatalf("got %+v, want literal echo of %q", res, arg)
	}
	if _, err := os.Stat(filepath.Join(r.Item.Cwd, "pwned")); err == nil {
		t.Fatal("shell metacharacters were interpreted")
	}
}

func TestUsesSnapshotPath(t *testing.T) {
	q, r := stage(t, "/bin/cat", "{file:body.json}")
	argv, err := Expand(r)
	if err != nil {
		t.Fatal(err)
	}
	if argv[1] != filepath.Join(r.SnapDir, "body.json") {
		t.Fatalf("expanded to %q, want the snapshot copy", argv[1])
	}
	res, err := Run(context.Background(), q, r, time.Now())
	if err != nil || res.Stdout != `{"reviewed":true}` {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestExpandRejectsUnknownFile(t *testing.T) {
	_, r := stage(t, "/bin/cat", "@{file:other.json}")
	if _, err := Expand(r); err == nil {
		t.Fatal("unknown attachment accepted")
	}
}

func TestRefusesModified(t *testing.T) {
	q, r := stage(t, "/bin/cat", "{file:body.json}")
	os.WriteFile(filepath.Join(q.Root, "queue", r.Item.ID, "files", "body.json"), []byte("evil"), 0o600)
	_, err := Run(context.Background(), q, r, time.Now())
	if !errors.Is(err, queue.ErrModified) {
		t.Fatalf("err = %v, want ErrModified", err)
	}
	if s := q.State(r.Item, time.Now()); s != queue.Modified {
		t.Fatalf("state = %s, want modified", s)
	}
}

func TestRunsOnce(t *testing.T) {
	q, r := stage(t, "/usr/bin/true")
	if _, err := Run(context.Background(), q, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), q, r, time.Now()); err == nil {
		t.Fatal("second run allowed")
	}
}

func TestRefusesExpired(t *testing.T) {
	q, r := stage(t, "/usr/bin/true")
	if _, err := Run(context.Background(), q, r, r.Item.Expires.Add(time.Second)); err == nil {
		t.Fatal("expired item ran")
	}
}

func TestFailureCapturesExitCode(t *testing.T) {
	q, r := stage(t, "/bin/ls", "/definitely/not/here")
	res, err := Run(context.Background(), q, r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.State != queue.Failed || res.ExitCode == 0 || !strings.Contains(res.Stderr, "not/here") {
		t.Fatalf("got %+v", res)
	}
	if got, _ := q.ReadResult(r.Item); got.SHA256Reviewed != r.Hash {
		t.Fatalf("result hash = %q, want %q", got.SHA256Reviewed, r.Hash)
	}
}

func TestRunsInterpreter(t *testing.T) {
	for argv0, want := range map[string]bool{
		"bash": true, "/bin/sh": true, "zsh": true, "python3": true, "python3.12": true,
		"node": true, "osascript": true, "env": true, "xargs": true, "sudo": true,
		"az": false, "gh": false, "/usr/bin/curl": false, "pythonista": false,
	} {
		if got := RunsInterpreter([]string{argv0, "x"}); got != want {
			t.Errorf("RunsInterpreter(%q) = %v, want %v", argv0, got, want)
		}
	}
}
