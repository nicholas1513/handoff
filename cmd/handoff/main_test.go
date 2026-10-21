package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicholas1513/handoff/internal/queue"
	"github.com/nicholas1513/handoff/internal/runner"
)

const sid = "0b6c2a8e-1f3d-4c55-9a7e-2d4f6b8c0e12"

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "handoff-bin")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "handoff")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// handoff runs the binary against root, with HANDOFF_SESSION_ID set unless sid is empty.
func handoff(t *testing.T, root, sid, stdin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "HANDOFF_ROOT="+root, "HANDOFF_SESSION_ID="+sid)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd
}

func exitCode(err error) int {
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestHooks(t *testing.T) {
	root, env := t.TempDir(), filepath.Join(t.TempDir(), "env")
	cmd := handoff(t, root, "", `{"session_id":"`+sid+`","cwd":"/src","source":"startup"}`, "hook", "session-start")
	cmd.Env = append(cmd.Env, "CLAUDE_ENV_FILE="+env)
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("session-start: err %v, output %q (must be silent)", err, out)
	}
	if b, _ := os.ReadFile(env); string(b) != "export HANDOFF_SESSION_ID="+sid+"\n" {
		t.Fatalf("env file = %q", b)
	}
	if out, err := handoff(t, root, "", `{"session_id":"`+sid+`","prompt":"hi"}`, "hook", "prompt").CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("prompt: err %v, output %q (must be silent)", err, out)
	}
	// A hook error must exit 1, never 2: exit 2 from UserPromptSubmit blocks the user's prompt.
	if code := exitCode(handoff(t, root, "", `not json`, "hook", "prompt").Run()); code != 1 {
		t.Fatalf("bad hook input exit = %d, want 1", code)
	}
}

func TestAddErrors(t *testing.T) {
	root := t.TempDir()
	for name, c := range map[string]struct {
		sid  string
		args []string
		want int
	}{
		"no session":   {"", []string{"add", "--desc", "d", "--", "true"}, 1},
		"no desc":      {sid, []string{"add", "--", "true"}, 2},
		"no argv":      {sid, []string{"add", "--desc", "d"}, 2},
		"bad --file":   {sid, []string{"add", "--desc", "d", "--file", "nopath", "--", "true"}, 2},
		"unsafe file":  {sid, []string{"add", "--desc", "d", "--file", "../x=/etc/hosts", "--", "true"}, 1},
		"unknown verb": {sid, []string{"bogus"}, 2},
	} {
		if code := exitCode(handoff(t, root, c.sid, "", c.args...).Run()); code != c.want {
			t.Errorf("%s: exit %d, want %d", name, code, c.want)
		}
	}
}

// stageAndWait starts `add --wait`, waits for the item to appear and returns it with the process.
func stageAndWait(t *testing.T, root string, argv ...string) (*queue.Queue, queue.Item, *exec.Cmd, *bytes.Buffer) {
	t.Helper()
	body := filepath.Join(t.TempDir(), "body.txt")
	os.WriteFile(body, []byte("reviewed body"), 0o600)
	cmd := handoff(t, root, sid, "", append([]string{"add", "--wait", "--desc", "e2e", "--file", "body.txt=" + body, "--"}, argv...)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	q := &queue.Queue{Root: root}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if items, _, _ := q.List(); len(items) == 1 {
			return q, items[0], cmd, &stdout
		}
		time.Sleep(20 * time.Millisecond)
	}
	cmd.Process.Kill()
	t.Fatal("item never appeared in the queue")
	return nil, queue.Item{}, nil, nil
}

func TestWaitWakesOnRun(t *testing.T) {
	q, it, cmd, stdout := stageAndWait(t, t.TempDir(), "/bin/cat", "{file:body.txt}")
	r, err := q.Snapshot(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), q, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("add --wait after success: %v", err)
	}
	var res queue.Result
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil || res.State != queue.Succeeded || res.Stdout != "reviewed body" {
		t.Fatalf("printed result = %q (%v)", stdout, err)
	}
}

func TestWaitExitsNonzeroOnReject(t *testing.T) {
	q, it, cmd, stdout := stageAndWait(t, t.TempDir(), "/usr/bin/true")
	if err := q.Claim(it); err != nil {
		t.Fatal(err)
	}
	if err := q.WriteResult(it, queue.Result{State: queue.Rejected, ExitCode: -1}); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(cmd.Wait()); code != 1 {
		t.Fatalf("add --wait after reject exit = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), `"rejected"`) {
		t.Fatalf("printed result = %q", stdout)
	}
}
