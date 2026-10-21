package queue

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sid = "0b6c2a8e-1f3d-4c55-9a7e-2d4f6b8c0e12"

func newQueue(t *testing.T) (*Queue, string) {
	t.Helper()
	root := t.TempDir()
	body := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(body, []byte(`{"name":"q1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Queue{Root: root}, body
}

func addOne(t *testing.T, q *Queue, body string) Item {
	t.Helper()
	it, err := q.Add(Item{
		Description: "create query",
		Argv:        []string{"az", "rest", "--body", "@{file:body.json}"},
		Cwd:         "/tmp",
		SessionID:   sid,
	}, map[string]string{"body.json": body})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func TestAddLoadRoundTrip(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)

	if it.ID == "" || it.Created.IsZero() {
		t.Fatalf("Add did not stamp id/created: %+v", it)
	}
	if got := it.Expires.Sub(it.Created); got != 24*time.Hour {
		t.Fatalf("expiry = %v, want 24h", got)
	}
	got, err := q.Load(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Argv[3] != "@{file:body.json}" || got.SessionID != sid || len(got.Files) != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	b, err := os.ReadFile(filepath.Join(q.Root, "queue", it.ID, "files", "body.json"))
	if err != nil || string(b) != `{"name":"q1"}` {
		t.Fatalf("attachment not copied: %q %v", b, err)
	}
}

func TestAddRejectsUnsafeNames(t *testing.T) {
	q, body := newQueue(t)
	for _, name := range []string{"../x", "a/b", "..", ".", ""} {
		if _, err := q.Add(Item{Argv: []string{"true"}, SessionID: sid}, map[string]string{name: body}); err == nil {
			t.Errorf("attachment name %q accepted", name)
		}
	}
	for _, s := range []string{"../queue", "a/b", ""} {
		if _, err := q.Add(Item{Argv: []string{"true"}, SessionID: s}, nil); err == nil {
			t.Errorf("session id %q accepted", s)
		}
	}
	if _, err := q.Add(Item{SessionID: sid}, nil); err == nil {
		t.Error("empty argv accepted")
	}
}

func TestSnapshotDetectsChanges(t *testing.T) {
	q, body := newQueue(t)

	it := addOne(t, q, body)
	r, err := q.Snapshot(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Verify(r); err != nil {
		t.Fatalf("unchanged item failed verify: %v", err)
	}
	os.WriteFile(filepath.Join(q.Root, "queue", it.ID, "files", "body.json"), []byte(`{"evil":1}`), 0o600)
	if err := q.Verify(r); !errors.Is(err, ErrModified) {
		t.Fatalf("changed attachment: err = %v, want ErrModified", err)
	}

	it2 := addOne(t, q, body)
	r2, _ := q.Snapshot(it2.ID)
	p := filepath.Join(q.Root, "queue", it2.ID, "item.json")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, append(b, ' '), 0o600)
	if err := q.Verify(r2); !errors.Is(err, ErrModified) {
		t.Fatalf("changed item.json: err = %v, want ErrModified", err)
	}
}

func TestSnapshotKeepsReviewedBytes(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)
	r, err := q.Snapshot(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(q.Root, "queue", it.ID, "files", "body.json"), []byte(`{"evil":1}`), 0o600)
	b, err := os.ReadFile(filepath.Join(r.SnapDir, "body.json"))
	if err != nil || string(b) != `{"name":"q1"}` {
		t.Fatalf("snapshot copy = %q %v, want reviewed bytes", b, err)
	}
	if rel, _ := filepath.Rel(q.Root, r.SnapDir); filepath.Dir(rel) != "snap" {
		t.Fatalf("snapshot dir %s is not under snap/", r.SnapDir)
	}
}

func TestStateAndRunOnce(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)

	if s := q.State(it, it.Created); s != Pending {
		t.Fatalf("new item state = %s", s)
	}
	if s := q.State(it, it.Expires.Add(time.Second)); s != Expired {
		t.Fatalf("past expiry state = %s", s)
	}
	res := Result{State: Succeeded, ExitCode: 0, Stdout: "ok"}
	if err := q.WriteResult(it, res); err != nil {
		t.Fatal(err)
	}
	if s := q.State(it, it.Expires.Add(time.Second)); s != Succeeded {
		t.Fatalf("state after result = %s, want succeeded even past expiry", s)
	}
	if err := q.WriteResult(it, res); err == nil {
		t.Fatal("second result accepted; items must run once")
	}
	got, err := q.ReadResult(it)
	if err != nil || got.Stdout != "ok" {
		t.Fatalf("ReadResult = %+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(q.Root, "sessions", sid, "results", it.ID+".json")); err != nil {
		t.Fatalf("result not in hook-owned sessions tree: %v", err)
	}
}

func TestList(t *testing.T) {
	q, body := newQueue(t)
	a := addOne(t, q, body)
	b := addOne(t, q, body)
	items, _, err := q.List()
	if err != nil || len(items) != 2 || items[0].ID != a.ID || items[1].ID != b.ID {
		t.Fatalf("List = %v %v, want [%s %s] oldest first", items, err, a.ID, b.ID)
	}
}

func TestClaimOnce(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)
	if err := q.Claim(it); err != nil {
		t.Fatal(err)
	}
	if s := q.State(it, it.Created); s != Running {
		t.Fatalf("claimed state = %s, want running", s)
	}
	if err := q.Claim(it); err == nil {
		t.Fatal("second claim accepted")
	}
}

func TestListReportsInvalid(t *testing.T) {
	q, body := newQueue(t)
	addOne(t, q, body)
	bad := filepath.Join(q.Root, "queue", "00000000000000000001-bad")
	os.MkdirAll(bad, 0o700)
	os.WriteFile(filepath.Join(bad, "item.json"), []byte(`{"id":"x","argv":[]}`), 0o600)
	items, invalid, err := q.List()
	if err != nil || len(items) != 1 || len(invalid) != 1 || invalid[0] != "00000000000000000001-bad" {
		t.Fatalf("List = %d items, invalid %v, err %v", len(items), invalid, err)
	}
}

func TestLoadRejectsStretchedExpiry(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)
	it.Expires = it.Created.Add(365 * 24 * time.Hour)
	b, _ := json.Marshal(it)
	os.WriteFile(filepath.Join(q.Root, "queue", it.ID, "item.json"), b, 0o600)
	if _, err := q.Load(it.ID); err == nil {
		t.Fatal("item with a year-long expiry accepted")
	}
}
