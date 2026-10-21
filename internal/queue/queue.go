// Package queue stores handoff items on disk.
//
// Layout under Root (see the v1 plan):
//
//	queue/<id>/item.json, files/<name>   written by the agent (sandbox-writable)
//	sessions/<sid>/results/<id>.json     written by the viewer only
//	snap/<id>/<name>                     reviewed copies of attachments, viewer only
//
// Everything under queue/ is untrusted input.
package queue

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const Expiry = 24 * time.Hour

const (
	Pending   = "pending"
	Running   = "running" // derived from a claim with no result yet
	Succeeded = "succeeded"
	Failed    = "failed"
	Rejected  = "rejected"
	Expired   = "expired"
	Modified  = "modified"
)

var ErrModified = errors.New("item changed after review")

// safeName is used for anything that becomes a path segment: ids, session ids, attachment names.
var safeName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

type Item struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Reason      string    `json:"reason,omitempty"`
	Argv        []string  `json:"argv"`
	Files       []string  `json:"files,omitempty"`
	Cwd         string    `json:"cwd"`
	SessionID   string    `json:"session_id"`
	Created     time.Time `json:"created"`
	Expires     time.Time `json:"expires"`
}

type Result struct {
	State          string    `json:"state"`
	ExitCode       int       `json:"exit_code"`
	Stdout         string    `json:"stdout"`
	Stderr         string    `json:"stderr"`
	RanAt          time.Time `json:"ran_at"`
	SHA256Reviewed string    `json:"sha256_reviewed"`
}

// Reviewed is an item as the human saw it: parsed once, hashed, attachments copied to SnapDir.
type Reviewed struct {
	Item    Item
	Hash    string
	SnapDir string
}

type Queue struct{ Root string }

func (q *Queue) itemDir(id string) string { return filepath.Join(q.Root, "queue", id) }

func (q *Queue) resultPath(it Item) string {
	return filepath.Join(q.Root, "sessions", it.SessionID, "results", it.ID+".json")
}

func validate(it Item) error {
	if len(it.Argv) == 0 || it.Argv[0] == "" {
		return errors.New("empty argv")
	}
	if !safeName.MatchString(it.SessionID) {
		return fmt.Errorf("bad session id %q", it.SessionID)
	}
	for _, f := range it.Files {
		if !safeName.MatchString(f) {
			return fmt.Errorf("bad attachment name %q", f)
		}
	}
	return nil
}

// Add stages it with attachments (name -> source path) and returns the stamped item.
func (q *Queue) Add(it Item, files map[string]string) (Item, error) {
	it.Files = nil
	for name := range files {
		it.Files = append(it.Files, name)
	}
	sort.Strings(it.Files)
	if err := validate(it); err != nil {
		return Item{}, err
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return Item{}, err
	}
	it.Created = time.Now()
	it.Expires = it.Created.Add(Expiry)
	// Zero-padded nanos first so directory names sort oldest first.
	it.ID = fmt.Sprintf("%020d-%s", it.Created.UnixNano(), hex.EncodeToString(rnd[:]))

	dir := q.itemDir(it.ID)
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o700); err != nil {
		return Item{}, err
	}
	for name, src := range files {
		b, err := os.ReadFile(src)
		if err != nil {
			return Item{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, "files", name), b, 0o600); err != nil {
			return Item{}, err
		}
	}
	b, err := json.MarshalIndent(it, "", "  ")
	if err != nil {
		return Item{}, err
	}
	return it, os.WriteFile(filepath.Join(dir, "item.json"), b, 0o600)
}

// parse also checks the timestamps, because item.json is agent-written and could stretch its own expiry.
func parse(b []byte) (Item, error) {
	var it Item
	if err := json.Unmarshal(b, &it); err != nil {
		return Item{}, err
	}
	if it.Created.After(time.Now().Add(time.Minute)) || it.Expires.After(it.Created.Add(Expiry)) {
		return Item{}, errors.New("created/expires out of range")
	}
	return it, validate(it)
}

func (q *Queue) Load(id string) (Item, error) {
	if !safeName.MatchString(id) {
		return Item{}, fmt.Errorf("bad item id %q", id)
	}
	b, err := os.ReadFile(filepath.Join(q.itemDir(id), "item.json"))
	if err != nil {
		return Item{}, err
	}
	it, err := parse(b)
	if err == nil && it.ID != id {
		err = fmt.Errorf("item.json id %q does not match directory %q", it.ID, id)
	}
	return it, err
}

// List returns every loadable item, oldest first, plus the ids of entries that failed to load.
func (q *Queue) List() (items []Item, invalid []string, err error) {
	ents, err := os.ReadDir(filepath.Join(q.Root, "queue"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ents { // ReadDir sorts by name, and ids sort by time
		if it, err := q.Load(e.Name()); err == nil {
			items = append(items, it)
		} else {
			invalid = append(invalid, e.Name())
		}
	}
	return items, invalid, nil
}

// read loads item.json and every attachment in one pass and hashes exactly those bytes.
func (q *Queue) read(id string) (Item, map[string][]byte, string, error) {
	dir := q.itemDir(id)
	raw, err := os.ReadFile(filepath.Join(dir, "item.json"))
	if err != nil {
		return Item{}, nil, "", err
	}
	it, err := parse(raw)
	if err != nil {
		return Item{}, nil, "", err
	}
	h := sha256.New()
	put := func(b []byte) { binary.Write(h, binary.BigEndian, uint64(len(b))); h.Write(b) }
	put(raw)
	files := map[string][]byte{}
	for _, name := range it.Files { // validated and sorted by Add
		b, err := os.ReadFile(filepath.Join(dir, "files", name))
		if err != nil {
			return Item{}, nil, "", err
		}
		files[name] = b
		put([]byte(name))
		put(b)
	}
	return it, files, hex.EncodeToString(h.Sum(nil)), nil
}

// Snapshot is called when the human opens an item. Attachments are written to snap/ from the
// same bytes that were hashed, so the copy matches what was shown.
func (q *Queue) Snapshot(id string) (Reviewed, error) {
	if !safeName.MatchString(id) {
		return Reviewed{}, fmt.Errorf("bad item id %q", id)
	}
	it, files, hash, err := q.read(id)
	if err != nil {
		return Reviewed{}, err
	}
	snap := filepath.Join(q.Root, "snap", id)
	if err := os.RemoveAll(snap); err != nil {
		return Reviewed{}, err
	}
	if err := os.MkdirAll(snap, 0o700); err != nil {
		return Reviewed{}, err
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(snap, name), b, 0o600); err != nil {
			return Reviewed{}, err
		}
	}
	return Reviewed{Item: it, Hash: hash, SnapDir: snap}, nil
}

// Verify re-reads the live item and returns ErrModified if anything differs from the review.
func (q *Queue) Verify(r Reviewed) error {
	_, _, hash, err := q.read(r.Item.ID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrModified, err)
	}
	if hash != r.Hash {
		return ErrModified
	}
	return nil
}

// Claim marks it as started. Only the first claim succeeds, so an item can't run twice even if two
// viewers race.
// shortcut: a crash between Claim and WriteResult leaves the item "running" forever; add recovery with gc.
func (q *Queue) Claim(it Item) error {
	if err := validate(it); err != nil {
		return err
	}
	p := strings.TrimSuffix(q.resultPath(it), ".json") + ".claim"
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// WriteResult records the outcome once. A second write fails, which is what makes items run-once.
func (q *Queue) WriteResult(it Item, r Result) error {
	if err := validate(it); err != nil {
		return err
	}
	p := q.resultPath(it)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (q *Queue) ReadResult(it Item) (Result, error) {
	var r Result
	b, err := os.ReadFile(q.resultPath(it))
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// State is the result's state if there is one, otherwise pending or expired.
func (q *Queue) State(it Item, now time.Time) string {
	if r, err := q.ReadResult(it); err == nil {
		return r.State
	}
	if _, err := os.Stat(strings.TrimSuffix(q.resultPath(it), ".json") + ".claim"); err == nil {
		return Running
	}
	if now.After(it.Expires) {
		return Expired
	}
	return Pending
}
