package claudesessions

import "testing"

// fakeProcs replaces the kernel lookups for one test.
func fakeProcs(t *testing.T, procs map[int]procInfo) {
	old := proc
	proc = func(pid int) (procInfo, bool) { p, ok := procs[pid]; return p, ok }
	t.Cleanup(func() { proc = old })
}

func TestOwnerSkipsTerminallessWrappersAndShells(t *testing.T) {
	fakeProcs(t, map[int]procInfo{
		50: {ppid: 40, tty: noTTY, name: "sh"},            // the hook's shell
		40: {ppid: 30, tty: noTTY, name: "wrapper"},       // anything else between Claude and the hook
		30: {ppid: 20, tty: 7, name: "2.1.319", start: 9}, // Claude
		20: {ppid: 10, tty: 7, name: "zsh"},
	})
	if pid, start, ok := Owner(50); !ok || pid != 30 || start != 9 {
		t.Fatalf("Owner = %d %d %v, want 30 9 true", pid, start, ok)
	}
}

// A shell that kept the terminal is still not Claude.
func TestOwnerSkipsShellWithTerminal(t *testing.T) {
	fakeProcs(t, map[int]procInfo{50: {ppid: 30, tty: 7, name: "sh"}, 30: {ppid: 20, tty: 7, name: "2.1.319", start: 9}})
	if pid, _, ok := Owner(50); !ok || pid != 30 {
		t.Fatalf("Owner = %d %v, want 30", pid, ok)
	}
}

// claude -p from a script or an IDE has no terminal anywhere up the chain: no owner, so nothing to save.
func TestOwnerWithoutTerminal(t *testing.T) {
	fakeProcs(t, map[int]procInfo{50: {ppid: 30, tty: noTTY, name: "sh"}, 30: {ppid: 1, tty: noTTY, name: "2.1.319"}})
	if _, _, ok := Owner(50); ok {
		t.Fatal("Owner found a process with no terminal")
	}
}
