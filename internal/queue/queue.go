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
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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
	Canceled  = "canceled" // the reviewer stopped a run
	Revoked   = "revoked"  // the agent withdrew it before it ran; derived from a marker in queue/<id>/
)

var ErrModified = errors.New("item changed after review")

// ErrNoCommit refuses an am item whose repository had no HEAD commit when reviewed: no review copy can be built on it.
var ErrNoCommit = errors.New("git am has no commit to apply on: the repository has none yet, or isn't one")

// safeName is used for anything that becomes a path segment: ids, session ids, attachment names.
var safeName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

type Item struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Reason      string    `json:"reason,omitempty"`
	Argv        []string  `json:"argv"`
	Files       []string  `json:"files,omitempty"`
	Edit        string    `json:"edit,omitempty"` // absolute path an edit item replaces with its EditFile attachment
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
	// FileHashes are plain SHA-256s of the exact bytes Hash covers, in order: item.json, then attachments.
	FileHashes []FileHash
	// For edit items: the target's SHA-256 when the diff was taken ("" if it didn't exist), and the diff.
	BaseSHA256, Diff string
	// For am items (see Am), as snapshotted: the repository's HEAD commit, which git am was reviewed on, its top level,
	// and its git directory's device and inode ("dev:ino"), which tell it from another directory later put at that
	// path. All "" if it had no HEAD commit.
	AmBase, AmRepo, AmRepoID string
	// For am items, set by the viewer as it starts the run (AmChecked): the commits git am made in the review copy as
	// runner.AmLog reads them, "" if it made none. The run checks that git am made no other commit in the repository.
	AmCommits string
	AmChecked bool
}

type FileHash struct{ Name, Path, SHA256 string }

type Queue struct{ Root string }

func (q *Queue) itemDir(id string) string { return filepath.Join(q.Root, "queue", id) }

func (q *Queue) resultPath(it Item) string {
	return filepath.Join(q.Root, "sessions", it.SessionID, "results", it.ID+".json")
}

// EditFile is the attachment holding an edit item's proposed contents.
const EditFile = "proposed"

// EditArgv is the only command an edit item may run, so the diff shown is the file written.
func EditArgv(target string) []string { return []string{"/bin/cp", "{file:" + EditFile + "}", target} }

// Am is the repository an item hands commits over to and the attachment holding them, when its argv is exactly
// git [-C <repo>] am {file:<name>}. The repository is the directory the run's git changes to: <repo> from the item's
// cwd, joined without cleaning, so a ".." after a symlink resolves as git's chdir resolves it. With no cwd the run
// starts in the viewer's own directory, so a relative <repo> is left relative and resolves there too.
func Am(it Item) (repo, file string, ok bool) {
	a, repo := it.Argv, it.Cwd
	if len(a) == 5 && a[1] == "-C" && !strings.Contains(a[2], "{file:") {
		if repo = a[2]; !filepath.IsAbs(repo) && it.Cwd != "" {
			repo = it.Cwd + string(filepath.Separator) + repo
		}
		a = []string{a[0], a[3], a[4]}
	}
	if len(a) != 3 || a[0] != "git" || a[1] != "am" {
		return "", "", false
	}
	file, pre := strings.CutPrefix(a[2], "{file:")
	file, suf := strings.CutSuffix(file, "}")
	return repo, file, pre && suf && slices.Contains(it.Files, file)
}

func validate(it Item) error {
	if len(it.Argv) == 0 || it.Argv[0] == "" {
		return errors.New("empty argv")
	}
	if it.Edit != "" {
		if !filepath.IsAbs(it.Edit) {
			return fmt.Errorf("edit target %q is not absolute", it.Edit)
		}
		if !slices.Equal(it.Argv, EditArgv(it.Edit)) || !slices.Equal(it.Files, []string{EditFile}) {
			return errors.New("edit item's argv or attachments don't match its target")
		}
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
	b, err := q.readPlain(id, "item.json")
	if err != nil {
		return Item{}, err
	}
	return parseAs(b, id)
}

// readPlain reads queue/<id>/<path>, which must be a plain file with no other link to it, reached through no symlink:
// queue/ is agent-written, and a link there would have the viewer read a file the agent can't (a key, a login) into
// snap/. It is opened without waiting, so a FIFO is refused, not waited on.
func (q *Queue) readPlain(id string, path ...string) ([]byte, error) {
	name := filepath.Join(append([]string{q.itemDir(id)}, path...)...)
	fd, err := unix.Open(filepath.Join(q.Root, "queue"), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	for _, p := range append([]string{id}, path...) {
		if err != nil {
			break
		}
		next, e := unix.Openat(fd, p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		fd, err = next, e
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, fmt.Errorf("%s is not a plain file", name)
	}
	return io.ReadAll(f)
}

// parseAs is parse of the item.json read from directory id, which must name that directory: snapshots, claims,
// results and review copies are all placed by the item's id.
func parseAs(b []byte, id string) (Item, error) {
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
func (q *Queue) read(id string) (Item, map[string][]byte, string, []byte, error) {
	raw, err := q.readPlain(id, "item.json")
	if err != nil {
		return Item{}, nil, "", nil, err
	}
	it, err := parseAs(raw, id)
	if err != nil {
		return Item{}, nil, "", nil, err
	}
	h := sha256.New()
	put := func(b []byte) { binary.Write(h, binary.BigEndian, uint64(len(b))); h.Write(b) }
	put(raw)
	files := map[string][]byte{}
	for _, name := range it.Files { // validated and sorted by Add
		b, err := q.readPlain(id, "files", name)
		if err != nil {
			return Item{}, nil, "", nil, err
		}
		files[name] = b
		put([]byte(name))
		put(b)
	}
	return it, files, hex.EncodeToString(h.Sum(nil)), raw, nil
}

// Snapshot is called when the human opens an item. Attachments are written to snap/ from the
// same bytes that were hashed, so the copy matches what was shown.
func (q *Queue) Snapshot(id string) (Reviewed, error) {
	if !safeName.MatchString(id) {
		return Reviewed{}, fmt.Errorf("bad item id %q", id)
	}
	it, files, hash, raw, err := q.read(id)
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
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	fh := []FileHash{{"item.json", filepath.Join(q.itemDir(id), "item.json"), sum(raw)}}
	for _, name := range it.Files {
		p := filepath.Join(snap, name)
		if err := os.WriteFile(p, files[name], 0o600); err != nil {
			return Reviewed{}, err
		}
		fh = append(fh, FileHash{name, p, sum(files[name])})
	}
	r := Reviewed{Item: it, Hash: hash, SnapDir: snap, FileHashes: fh}
	if it.Edit != "" {
		if r.BaseSHA256, r.Diff, err = editDiff(it.Edit, filepath.Join(snap, EditFile), filepath.Join(snap, EditBase)); err != nil {
			return Reviewed{}, err
		}
	}
	if repo, _, ok := Am(it); ok {
		r.AmBase, r.AmRepo, r.AmRepoID = amRepo(repo)
	}
	return r, nil
}

// amTimeout bounds amRepo, which runs as the item is opened, on the viewer's event loop: a repository can make git wait
// (a FIFO for HEAD).
var amTimeout = 5 * time.Second

// amRepo is the repository git -C repo works in: its HEAD commit, top level and git directory's "dev:ino", all "" if
// it has no HEAD commit. Read with hooks, the fsmonitor and lazy fetches off (a partial clone missing HEAD would fetch
// it with its promisor remote's upload-pack, a command the repository names), and with no replace refs, as the run
// and the review copy read it: one could make HEAD^{commit} name another commit.
func amRepo(repo string) (base, top, id string) {
	ctx, stop := context.WithTimeout(context.Background(), amTimeout)
	defer stop()
	out, _ := exec.CommandContext(ctx, "git", "--no-lazy-fetch", "--no-replace-objects", "-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false", "-C", repo, "rev-parse", "--show-toplevel", "--absolute-git-dir", "--verify", "-q",
		"HEAD^{commit}").Output()
	l := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(l) != 3 {
		return "", "", ""
	}
	fi, err := os.Stat(l[1])
	if err != nil {
		return "", "", ""
	}
	s := fi.Sys().(*syscall.Stat_t) // unix only, like the runner's process groups
	return l[2], l[0], fmt.Sprintf("%d:%d", s.Dev, s.Ino)
}

// EditBase is the snapshot's copy of an edit target as it was diffed (empty if it didn't exist). It
// starts with "." so no attachment can have its name.
const EditBase = ".base"

// readTarget is the target's bytes and SHA-256, or nil and "" if it doesn't exist.
func readTarget(target string) ([]byte, string, error) {
	b, err := os.ReadFile(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	s := sha256.Sum256(b)
	return b, hex.EncodeToString(s[:]), nil
}

// editDiff copies the target to base and diffs that copy against the proposed file, so the hash, the
// diff and the context the viewer reveals all come from one read. Verify re-hashes the live target
// before a run, so one that changed since is refused rather than overwritten.
func editDiff(target, proposed, base string) (sum, diff string, err error) {
	b, sum, err := readTarget(target)
	if err != nil {
		return "", "", err
	}
	if err = os.WriteFile(base, b, 0o600); err != nil {
		return "", "", err
	}
	out, err := exec.Command("diff", "-u", "-L", "current "+target, "-L", "proposed", base, proposed).Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 { // 1 means the files differ
		err = nil
	}
	return sum, string(out), err
}

// Verify re-reads the live item and returns ErrModified if anything differs from the review,
// including an edit target that changed since its diff was taken and an am item's repository that is another one or
// whose HEAD moved.
func (q *Queue) Verify(r Reviewed) error {
	_, _, hash, _, err := q.read(r.Item.ID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrModified, err)
	}
	if hash != r.Hash {
		return ErrModified
	}
	if r.Item.Edit != "" {
		if _, base, err := readTarget(r.Item.Edit); err != nil || base != r.BaseSHA256 {
			return fmt.Errorf("%w: %s changed since the diff was taken", ErrModified, r.Item.Edit)
		}
	}
	if repo, _, ok := Am(r.Item); ok {
		switch base, top, id := amRepo(repo); {
		case top != r.AmRepo || id != r.AmRepoID:
			return fmt.Errorf("%w: %s is not the repository reviewed", ErrModified, repo)
		case base != r.AmBase:
			return fmt.Errorf("%w: %s HEAD moved since review", ErrModified, repo)
		}
	}
	return nil
}

func (q *Queue) claimPath(it Item) string { return strings.TrimSuffix(q.resultPath(it), ".json") + ".claim" }

func (q *Queue) revokedPath(it Item) string { return filepath.Join(q.itemDir(it.ID), "revoked") }

// Claim marks it as started. Only the first claim succeeds, so an item can't run twice even if two
// viewers race. A run left "running" by a viewer that went away is ended with runner.CancelStale.
func (q *Queue) Claim(it Item) error {
	if err := validate(it); err != nil {
		return err
	}
	p := q.claimPath(it)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// RecordPID stores the running process's pid in the claim, so a later viewer can stop a run whose
// viewer went away.
func (q *Queue) RecordPID(it Item, pid int) error {
	if err := validate(it); err != nil {
		return err
	}
	f, err := os.OpenFile(q.claimPath(it), os.O_WRONLY|os.O_TRUNC, 0o600) // no O_CREATE: only a claimed item
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(f, pid)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ClaimPID is the pid RecordPID stored, if any.
func (q *Queue) ClaimPID(it Item) (int, bool) {
	b, err := os.ReadFile(q.claimPath(it))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil && pid > 0
}

// Revoke withdraws a pending item for the agent session that staged it. The marker lives in the
// sandbox-writable queue dir, so an agent could also write it by hand for another session's item; a
// marker can only ever stop a run, never start one. Once claimed, an item is the reviewer's to stop.
func (q *Queue) Revoke(it Item, sessionID string) error {
	if sessionID != it.SessionID {
		return fmt.Errorf("item %s was staged by another session", it.ID)
	}
	if s := q.State(it, time.Now()); s != Pending {
		return fmt.Errorf("item is %s", s)
	}
	if err := os.WriteFile(q.revokedPath(it), []byte(time.Now().Format(time.RFC3339Nano)), 0o600); err != nil {
		return err
	}
	// The viewer may have claimed it between the check and the marker; then the run wins.
	if s := q.State(it, time.Now()); s != Revoked {
		return fmt.Errorf("too late: item is %s", s)
	}
	return nil
}

// RevokedAt is when the item was revoked, if it was.
func (q *Queue) RevokedAt(it Item) (time.Time, bool) {
	fi, err := os.Stat(q.revokedPath(it))
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
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

// State is the result's state if there is one, otherwise running, revoked, pending or expired.
// A claim outranks a revoke marker: once claimed, the run goes ahead.
func (q *Queue) State(it Item, now time.Time) string {
	if r, err := q.ReadResult(it); err == nil {
		return r.State
	}
	if _, err := os.Stat(q.claimPath(it)); err == nil {
		return Running
	}
	if _, ok := q.RevokedAt(it); ok {
		return Revoked
	}
	if now.After(it.Expires) {
		return Expired
	}
	return Pending
}
