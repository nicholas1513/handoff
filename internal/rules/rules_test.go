package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, cmd string
		want         bool
	}{
		{"", "anything at all", true},
		{"*", "anything at all", true},
		{"git status", "git status", true},
		{"git status", "git status -s", false},
		{"npm run test:*", "npm run test", true},
		{"npm run test:*", "npm run test --watch", true},
		{"npm run test:*", "npm run testing", false},
		{"docker ps", "docker ps", true},
		{"docker ps *", "docker ps -a", true},
		{"docker ps *", "docker ps", true},
		{"docker ps *", "docker psx", false},
		{"docker compose *", "docker compose up -d", true},
		{"docker exec db *", "docker exec web ls", false},
		{"gh * view", "gh pr view", true},
		{"gh * view", "gh pr view --x", false},
		{"a.b *", "aXb c", false}, // regex metacharacters are literal
	} {
		if got := match(c.pattern, c.cmd); got != c.want {
			t.Errorf("match(%q, %q) = %v, want %v", c.pattern, c.cmd, got, c.want)
		}
	}
}

func TestCheckPrecedenceAndSources(t *testing.T) {
	dir := t.TempDir()
	user := write(t, dir, "user.json", `{
		"permissions": {
			"allow": ["Bash(gh api:*)", "mcp__github__get_issue", "Write(/tmp/**)"],
			"deny": ["Read(~/.ssh/**)", "Edit(**/.claude/settings.json)"]
		},
		"sandbox": {"excludedCommands": ["docker ps", "docker ps *", "docker compose *"]}
	}`)
	local := write(t, dir, "local.json", `{"permissions": {"deny": ["Bash(gh api --method DELETE:*)"], "ask": ["Bash(curl *)"]}}`)
	s := Load([]string{user, local, filepath.Join(dir, "missing.json")})
	if len(s.Problems) != 0 {
		t.Fatalf("problems: %v", s.Problems)
	}

	b := s.Check([]string{"gh", "api", "--method", "GET"})
	if b.Permission == nil || b.Permission.Kind != Allow || b.Permission.File != user || b.Permission.Rule != "Bash(gh api:*)" {
		t.Fatalf("allow: %+v", b.Permission)
	}
	b = s.Check([]string{"gh", "api", "--method", "DELETE", "repos/o/r/hooks/1"})
	if b.Permission == nil || b.Permission.Kind != Deny || b.Permission.File != local {
		t.Fatalf("deny should beat allow: %+v", b.Permission)
	}
	if b = s.Check([]string{"curl", "https://x.test"}); b.Permission == nil || b.Permission.Kind != Ask {
		t.Fatalf("ask: %+v", b.Permission)
	}
	b = s.Check([]string{"cat", "/Users/x/.ssh/config"})
	if b.Permission != nil || b.Excluded != nil {
		t.Fatalf("Read()/Edit()/mcp rules must not match commands: %+v", b)
	}
	if b = s.Check([]string{"docker", "ps", "-a"}); b.Excluded == nil || b.Excluded.Rule != "docker ps *" {
		t.Fatalf("excluded: %+v", b.Excluded)
	}
}

func TestBadgeText(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, ".claude/settings.local.json", `{"permissions": {"allow": ["Bash(gh api:*)"]}}`)
	s := Load([]string{p})
	got := s.Check([]string{"gh", "api"}).String(dir)
	if got != "allowed for Claude by Bash(gh api:*) in ~/.claude/settings.local.json" {
		t.Fatalf("badge = %q", got)
	}
	if got := s.Check([]string{"curl"}).String(dir); !strings.HasPrefix(got, "no Bash rule") {
		t.Fatalf("badge = %q", got)
	}
}

func TestLoadReportsBadJSON(t *testing.T) {
	p := write(t, t.TempDir(), "s.json", `{not json`)
	if s := Load([]string{p}); len(s.Problems) != 1 {
		t.Fatalf("problems = %v", s.Problems)
	}
}

func TestDefaultFiles(t *testing.T) {
	got := DefaultFiles("/home/u", "/src/proj")
	want := []string{
		"/home/u/.claude/settings.json",
		"/src/proj/.claude/settings.json",
		"/src/proj/.claude/settings.local.json",
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("DefaultFiles[%d] = %q, want %q", i, got[i], w)
		}
	}
}
