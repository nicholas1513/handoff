package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	cmd.Env = append(os.Environ(), "HANDOFF_ROOT="+root, "HANDOFF_SESSION_ID="+sid, "CLAUDE_CODE_SESSION_ID=")
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

func TestHookLog(t *testing.T) {
	root := t.TempDir()
	handoff(t, root, "", `{"session_id":"`+sid+`","prompt":"secret words"}`, "hook", "prompt").Run()
	handoff(t, root, "", `{"sessionId":"x","prompt":"secret words"}`, "hook", "prompt").Run()
	b, err := os.ReadFile(filepath.Join(root, "hook.log"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "prompt ok") || !strings.Contains(lines[1], "prompt error") {
		t.Fatalf("hook.log = %q", log)
	}
	if !strings.Contains(lines[1], "keys=[prompt sessionId]") {
		t.Fatalf("error line should list the received keys: %q", lines[1])
	}
	if strings.Contains(log, "secret") {
		t.Fatal("hook.log must not contain prompt text")
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
		"edit + argv":  {sid, []string{"add", "--desc", "d", "--edit", "/x=/y", "--", "true"}, 2},
		"edit + file":  {sid, []string{"add", "--desc", "d", "--edit", "/x=/y", "--file", "a=/y"}, 2},
		"bad --edit":   {sid, []string{"add", "--desc", "d", "--edit", "/x"}, 2},
	} {
		if code := exitCode(handoff(t, root, c.sid, "", c.args...).Run()); code != c.want {
			t.Errorf("%s: exit %d, want %d", name, code, c.want)
		}
	}
}

func TestAddEditResolvesTarget(t *testing.T) {
	root := t.TempDir()
	dir, _ := filepath.EvalSymlinks(t.TempDir()) // macOS /tmp is a symlink; Getwd returns the real path
	os.WriteFile(filepath.Join(dir, "new"), []byte("x"), 0o600)
	cmd := handoff(t, root, sid, "", "add", "--desc", "d", "--edit", "settings.json=new")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	it, err := (&queue.Queue{Root: root}).Load(strings.TrimSpace(string(out)))
	target := filepath.Join(dir, "settings.json")
	if err != nil || it.Edit != target || !slices.Equal(it.Argv, queue.EditArgv(target)) {
		t.Fatalf("item = %+v, %v", it, err)
	}
}

func TestAddFallsBackToClaudeSessionID(t *testing.T) {
	root := t.TempDir()
	cmd := handoff(t, root, "", "", "add", "--desc", "d", "--", "true")
	cmd.Env = append(cmd.Env, "CLAUDE_CODE_SESSION_ID="+sid)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add with only CLAUDE_CODE_SESSION_ID: %v %s", err, out)
	}
	items, _, _ := (&queue.Queue{Root: root}).List()
	if len(items) != 1 || items[0].SessionID != sid {
		t.Fatalf("items = %+v", items)
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

func TestRevokeOwnSessionOnly(t *testing.T) {
	root := t.TempDir()
	out, err := handoff(t, root, sid, "", "add", "--desc", "d", "--", "true").Output()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(out))
	q := &queue.Queue{Root: root}
	it, _ := q.Load(id)

	if code := exitCode(handoff(t, root, "11111111-2222-3333-4444-555555555555", "", "revoke", id).Run()); code != 1 {
		t.Fatalf("revoke from another session exit = %d, want 1", code)
	}
	if s := q.State(it, time.Now()); s != queue.Pending {
		t.Fatalf("state after foreign revoke = %s", s)
	}
	if code := exitCode(handoff(t, root, "", "", "revoke", id).Run()); code != 1 {
		t.Fatalf("revoke without a session exit = %d, want 1", code)
	}
	if out, err := handoff(t, root, sid, "", "revoke", id).CombinedOutput(); err != nil {
		t.Fatalf("revoke own item: %v %s", err, out)
	}
	if s := q.State(it, time.Now()); s != queue.Revoked {
		t.Fatalf("state = %s, want revoked", s)
	}
	if code := exitCode(handoff(t, root, sid, "", "revoke").Run()); code != 2 {
		t.Fatalf("revoke with no id exit = %d, want 2", code)
	}
}

func TestWaitExitsOnRevoke(t *testing.T) {
	root := t.TempDir()
	_, it, cmd, stdout := stageAndWait(t, root, "/usr/bin/true")
	if out, err := handoff(t, root, sid, "", "revoke", it.ID).CombinedOutput(); err != nil {
		t.Fatalf("revoke: %v %s", err, out)
	}
	if code := exitCode(cmd.Wait()); code != 1 {
		t.Fatalf("add --wait after revoke exit = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), `"revoked"`) {
		t.Fatalf("printed result = %q", stdout)
	}
}
