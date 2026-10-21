// Package rules reads Claude Code settings to label an item with the rule that applies to it.
//
// Informational only: these rules describe what the agent may do, so nothing here gates a run.
package rules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const (
	Allow = "allow"
	Ask   = "ask"
	Deny  = "deny"
)

type Match struct {
	Kind, Rule, File string
}

type Badge struct {
	Permission *Match // the deciding Bash permission rule, if any
	Excluded   *Match // a sandbox.excludedCommands entry, if any
}

type file struct {
	path     string
	settings struct {
		Permissions struct {
			Allow, Ask, Deny []string
		}
		Sandbox struct {
			ExcludedCommands []string
		}
	}
}

type Settings struct {
	files    []file
	Problems []string // unreadable or invalid files; missing files are not problems
}

// DefaultFiles lists the settings layers for a project: user, project, local, then managed.
func DefaultFiles(home, project string) []string {
	files := []string{
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(project, ".claude", "settings.json"),
		filepath.Join(project, ".claude", "settings.local.json"),
	}
	managed := "/etc/claude-code"
	if runtime.GOOS == "darwin" {
		managed = "/Library/Application Support/ClaudeCode"
	}
	files = append(files, filepath.Join(managed, "managed-settings.json"))
	dropIns, _ := filepath.Glob(filepath.Join(managed, "managed-settings.d", "*.json"))
	return append(files, dropIns...)
}

func Load(paths []string) Settings {
	var s Settings
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		f := file{path: p}
		if err == nil {
			err = json.Unmarshal(b, &f.settings)
		}
		if err != nil {
			s.Problems = append(s.Problems, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		s.files = append(s.files, f)
	}
	return s
}

// Check finds the rules that apply to argv run as a Bash command. Deny beats ask beats allow.
func (s Settings) Check(argv []string) Badge {
	cmd := strings.Join(argv, " ")
	return Badge{Permission: s.permission(cmd), Excluded: s.excluded(cmd)}
}

func (s Settings) permission(cmd string) *Match {
	for _, kind := range []string{Deny, Ask, Allow} {
		for _, f := range s.files {
			p := f.settings.Permissions
			for _, rule := range map[string][]string{Allow: p.Allow, Ask: p.Ask, Deny: p.Deny}[kind] {
				if pattern, ok := bashPattern(rule); ok && match(pattern, cmd) {
					return &Match{Kind: kind, Rule: rule, File: f.path}
				}
			}
		}
	}
	return nil
}

func (s Settings) excluded(cmd string) *Match {
	for _, f := range s.files {
		for _, rule := range f.settings.Sandbox.ExcludedCommands {
			if match(rule, cmd) {
				return &Match{Kind: "excluded", Rule: rule, File: f.path}
			}
		}
	}
	return nil
}

// bashPattern returns the pattern of a Bash rule: "Bash" -> "", "Bash(x)" -> "x".
func bashPattern(rule string) (string, bool) {
	if rule == "Bash" {
		return "", true
	}
	if p, ok := strings.CutPrefix(rule, "Bash("); ok && strings.HasSuffix(p, ")") {
		return strings.TrimSuffix(p, ")"), true
	}
	return "", false
}

// match follows Claude Code's Bash rule forms: exact, legacy "prefix:*", and "*" wildcards.
// A trailing " *" also matches the bare command, so "docker ps *" covers "docker ps".
func match(pattern, cmd string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasSuffix(pattern, ":*"):
		p := strings.TrimSuffix(pattern, ":*")
		return cmd == p || strings.HasPrefix(cmd, p+" ")
	case strings.Contains(pattern, "*"):
		if bare, ok := strings.CutSuffix(pattern, " *"); ok && cmd == bare {
			return true
		}
		parts := strings.Split(pattern, "*")
		for i, p := range parts {
			parts[i] = regexp.QuoteMeta(p)
		}
		return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$").MatchString(cmd)
	default:
		return cmd == pattern
	}
}

// String is the one-line badge, with paths under home shortened to ~.
func (b Badge) String(home string) string {
	short := func(p string) string {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
		return p
	}
	var s string
	switch m := b.Permission; {
	case m == nil:
		s = "no Bash rule: Claude would ask"
	case m.Kind == Allow:
		s = fmt.Sprintf("allowed for Claude by %s in %s", m.Rule, short(m.File))
	case m.Kind == Ask:
		s = fmt.Sprintf("Claude would ask: %s in %s", m.Rule, short(m.File))
	default:
		s = fmt.Sprintf("denied to Claude by %s in %s", m.Rule, short(m.File))
	}
	if m := b.Excluded; m != nil {
		s += fmt.Sprintf(" · runs outside the sandbox (excludedCommands %q in %s)", m.Rule, short(m.File))
	}
	return s
}
