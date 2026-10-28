package queue

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestSnapshotRefusesAnotherID: item.json is agent-written after the list read checked it; an id naming another
// directory must not reach the paths built from it.
func TestSnapshotRefusesAnotherID(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)
	r, err := q.Snapshot(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	bad := it
	bad.ID = "../../elsewhere"
	b, _ := json.Marshal(bad)
	os.WriteFile(filepath.Join(q.Root, "queue", it.ID, "item.json"), b, 0o600)
	if r, err := q.Snapshot(it.ID); err == nil || r.Item.ID != "" {
		t.Fatalf("Snapshot of an item.json naming %q: %q, %v", bad.ID, r.Item.ID, err)
	}
	if err := q.Verify(r); !errors.Is(err, ErrModified) {
		t.Fatalf("Verify = %v, want ErrModified", err)
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

func TestSnapshotFileHashesMatchShasum(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)
	r, err := q.Snapshot(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.FileHashes) != 2 {
		t.Fatalf("FileHashes = %+v, want item.json + 1 attachment", r.FileHashes)
	}
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	itemPath := filepath.Join(q.Root, "queue", it.ID, "item.json")
	raw, _ := os.ReadFile(itemPath)
	if f := r.FileHashes[0]; f.Name != "item.json" || f.Path != itemPath || f.SHA256 != sum(raw) {
		t.Errorf("item.json entry = %+v", f)
	}
	f := r.FileHashes[1]
	snapped, err := os.ReadFile(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "body.json" || f.Path != filepath.Join(r.SnapDir, "body.json") || f.SHA256 != sum(snapped) {
		t.Errorf("attachment entry = %+v", f)
	}
	// the combined hash scheme is persisted in result.json: length-prefixed item, then name and bytes
	h := sha256.New()
	put := func(b []byte) { binary.Write(h, binary.BigEndian, uint64(len(b))); h.Write(b) }
	put(raw)
	put([]byte("body.json"))
	put(snapped)
	if want := hex.EncodeToString(h.Sum(nil)); r.Hash != want {
		t.Errorf("combined Hash = %s, want %s", r.Hash, want)
	}
}

func TestRevoke(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)
	if err := q.Revoke(it, "another-session"); err == nil {
		t.Fatal("revoked an item staged by another session")
	}
	if s := q.State(it, time.Now()); s != Pending {
		t.Fatalf("state after refused revoke = %s", s)
	}
	if err := q.Revoke(it, sid); err != nil {
		t.Fatal(err)
	}
	if s := q.State(it, time.Now()); s != Revoked {
		t.Fatalf("state = %s, want revoked", s)
	}
	if err := q.Revoke(it, sid); err == nil {
		t.Fatal("revoking twice should report it is no longer pending")
	}
}

func TestRevokeTooLateOnceClaimed(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)
	if err := q.Claim(it); err != nil {
		t.Fatal(err)
	}
	if err := q.Revoke(it, sid); err == nil {
		t.Fatal("revoke must not succeed once the item is running")
	}
	if s := q.State(it, time.Now()); s != Running {
		t.Fatalf("state = %s, want running (a run is never revoked)", s)
	}
}

func TestRecordPID(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)
	if err := q.RecordPID(it, 1234); err == nil {
		t.Fatal("recorded a pid for an unclaimed item")
	}
	if err := q.Claim(it); err != nil {
		t.Fatal(err)
	}
	if _, ok := q.ClaimPID(it); ok {
		t.Fatal("a fresh claim has no pid")
	}
	if err := q.RecordPID(it, 1234); err != nil {
		t.Fatal(err)
	}
	if pid, ok := q.ClaimPID(it); !ok || pid != 1234 {
		t.Fatalf("ClaimPID = %d %v", pid, ok)
	}
}

func TestEditItem(t *testing.T) {
	q := &Queue{Root: t.TempDir()}
	dir := t.TempDir()
	target, proposed := filepath.Join(dir, "settings.json"), filepath.Join(dir, "new.json")
	os.WriteFile(target, []byte("a\nb\n"), 0o600)
	os.WriteFile(proposed, []byte("a\nc\n"), 0o600)
	edit := Item{Edit: target, Argv: EditArgv(target), Cwd: dir, SessionID: sid}

	// The command must be the one EditArgv fixes for the target, so the diff shown is the file written.
	for _, bad := range [][]string{EditArgv(filepath.Join(dir, "other")), {"/bin/rm", target}} {
		it := edit
		it.Argv = bad
		if _, err := q.Add(it, map[string]string{EditFile: proposed}); err == nil {
			t.Errorf("accepted argv %q for target %q", bad, target)
		}
	}
	rel := edit
	rel.Edit, rel.Argv = "settings.json", EditArgv("settings.json")
	if _, err := q.Add(rel, map[string]string{EditFile: proposed}); err == nil {
		t.Error("accepted a relative target")
	}

	it, err := q.Add(edit, map[string]string{EditFile: proposed})
	if err != nil {
		t.Fatal(err)
	}
	r, err := q.Snapshot(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--- current " + target, "+++ proposed", "-b\n", "+c\n"} {
		if !strings.Contains(r.Diff, want) {
			t.Errorf("diff missing %q:\n%s", want, r.Diff)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(r.SnapDir, EditBase)); string(b) != "a\nb\n" {
		t.Fatalf("snapshot base = %q, want the target as diffed", b)
	}
	if err := q.Verify(r); err != nil {
		t.Fatalf("unchanged target failed verify: %v", err)
	}
	os.WriteFile(target, []byte("a\nb\nx\n"), 0o600)
	if err := q.Verify(r); !errors.Is(err, ErrModified) {
		t.Fatalf("target changed after review: Verify = %v, want ErrModified", err)
	}

	os.Remove(target)
	if r, err = q.Snapshot(it.ID); err != nil || r.BaseSHA256 != "" || !strings.Contains(r.Diff, "+c\n") {
		t.Fatalf("new-file snapshot = %+v, %v", r, err)
	}
}

func TestAm(t *testing.T) {
	files := []string{"c.patch"}
	for _, c := range []struct {
		argv       []string
		repo, file string
	}{
		{[]string{"git", "-C", "/r", "am", "{file:c.patch}"}, "/r", "c.patch"},
		{[]string{"git", "am", "{file:c.patch}"}, "/cwd", "c.patch"},
		{[]string{"git", "-C", "sub/../x", "am", "{file:c.patch}"}, "/cwd/sub/../x", "c.patch"},
		{[]string{"git", "-C", "", "am", "{file:c.patch}"}, "/cwd/", "c.patch"},
	} {
		repo, file, ok := Am(Item{Argv: c.argv, Cwd: "/cwd", Files: files})
		if !ok || repo != c.repo || file != c.file {
			t.Errorf("%q: got %q %q %v, want %q %q", c.argv, repo, file, ok, c.repo, c.file)
		}
	}
	for _, argv := range [][]string{
		{"/usr/bin/git", "am", "{file:c.patch}"}, {"git", "am", "--3way", "{file:c.patch}"}, {"git", "am", "{file:other}"},
		{"git", "am", "x{file:c.patch}"}, {"git", "-C", "{file:c.patch}", "am", "{file:c.patch}"}, {"git", "apply", "{file:c.patch}"},
		{"git", "-c", "a=b", "am", "{file:c.patch}"}, {"git", "am"}, {"git", "-C", "/r", "am", "{file:c.patch}", "x"},
	} {
		if _, _, ok := Am(Item{Argv: argv, Cwd: "/cwd", Files: files}); ok {
			t.Errorf("%q is an am item", argv)
		}
	}
	// no cwd: the run starts in the viewer's directory, where a relative -C resolves, not at /
	if repo, _, ok := Am(Item{Argv: []string{"git", "-C", "x", "am", "{file:c.patch}"}, Files: files}); !ok || repo != "x" {
		t.Errorf("no cwd: repo %q, %v; want x", repo, ok)
	}
}
