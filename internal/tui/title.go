package tui

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// transcriptTitle returns the /resume title: the last custom-title, else the last ai-title.
func transcriptTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var ai, custom string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `-title"`) {
			continue
		}
		var l struct{ Type, AITitle, CustomTitle string }
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		switch {
		case l.Type == "custom-title" && l.CustomTitle != "":
			custom = l.CustomTitle
		case l.Type == "ai-title" && l.AITitle != "":
			ai = l.AITitle
		}
	}
	if custom != "" {
		return custom
	}
	return ai
}

type titleEntry struct {
	mod   time.Time
	size  int64
	title string
}

// titleCache re-reads a transcript only when its mtime or size changes.
type titleCache map[string]titleEntry

func (c titleCache) get(path string) string {
	if path == "" {
		return ""
	}
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if e, ok := c[path]; ok && e.mod.Equal(st.ModTime()) && e.size == st.Size() {
		return e.title
	}
	t := transcriptTitle(path)
	c[path] = titleEntry{st.ModTime(), st.Size(), t}
	return t
}
