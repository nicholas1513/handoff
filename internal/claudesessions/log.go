package claudesessions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const logFile = "claude-sessions.log"

// record adds a line to the log of what saves, restores and closes did, with the sessions by window. Folders are
// quoted, so a line stays one line. The work has already happened or failed by now, so a log that can't be written
// is not worth reporting over it.
func record(root, event, msg string, err error, windows [][]Tab) {
	if root == "" {
		return
	}
	if err != nil {
		msg = "failed: " + err.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %q", time.Now().Format(time.RFC3339), event, msg)
	for _, w := range windows {
		var tabs []string
		for _, t := range w {
			tabs = append(tabs, fmt.Sprintf("%.8q %q", t.SessionID, t.Cwd))
		}
		b.WriteString(" [" + strings.Join(tabs, ", ") + "]")
	}
	b.WriteByte('\n')
	if os.MkdirAll(root, 0o700) != nil {
		return
	}
	f, oerr := os.OpenFile(filepath.Join(root, logFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if oerr != nil {
		return
	}
	defer f.Close()
	f.WriteString(b.String())
}
