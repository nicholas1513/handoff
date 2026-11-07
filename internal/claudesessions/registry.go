package claudesessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/nicholas1513/handoff/internal/queue"
)

// Claude Code lists each running session in <config dir>/sessions/<pid>.json, plugin or not, and keeps the id current
// across /clear and /resume. The format is its own and undocumented, so anything that doesn't read as expected is
// skipped, and the hook's records (see liveFromHooks) still cover those sessions.
type registered struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"` // Unix ms
	ProcStart string `json:"procStart"`
	Kind      string `json:"kind"`
}

// startedNearBy is how far a process's start may be from when Claude Code recorded it.
const startedNearBy = 2 * time.Minute

// claudeDir is Claude Code's config folder. Tests replace it.
var claudeDir = func() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// startedNear compares a Unix ms time with a process start in Unix µs.
func startedNear(ms, startUs int64) bool {
	d := time.Duration(ms*1000-startUs) * time.Microsecond
	return d > -startedNearBy && d < startedNearBy
}

// liveFromClaude is the sessions Claude Code lists that are still running in a terminal. A crash leaves its file
// behind, so each is checked against the process.
func liveFromClaude() map[int]liveSession {
	out := map[int]liveSession{}
	dir := claudeDir()
	if dir == "" {
		return out
	}
	files, _ := filepath.Glob(filepath.Join(dir, "sessions", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var r registered
		if json.Unmarshal(b, &r) != nil || r.Kind != "interactive" || r.PID <= 0 || !uuid.MatchString(r.SessionID) {
			continue
		}
		p, ok := proc(r.PID)
		if !ok || p.tty == noTTY || !startMatches(r, p) {
			continue
		}
		out[r.PID] = liveSession{queue.Session{SessionID: r.SessionID, Cwd: r.Cwd, TranscriptPath: transcriptOf(dir, r.SessionID),
			Started: time.UnixMilli(r.StartedAt), ClaudePID: r.PID}, p.tty}
	}
	return out
}

// transcriptOf finds a session's transcript by id: the folder name under projects/ is derived from the working
// directory by rules that are Claude Code's own. The id has been checked, so it has no glob characters. "" when there
// is none yet, as for a session with no message: restore then counts it as gone.
func transcriptOf(dir, id string) string {
	m, _ := filepath.Glob(filepath.Join(dir, "projects", "*", id+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}
