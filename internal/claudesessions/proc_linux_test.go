package claudesessions

import (
	"os"
	"testing"
)

func TestParseStat(t *testing.T) {
	const rest = " S 40 50 50 34819 50 4194560 1 0 0 0 1 2 0 0 20 0 9 0 123456 1000 2000"
	p, ok := parseStat("99 (weird) name)" + rest)
	if !ok || p.ppid != 40 || p.tty != 34819 || p.start != 123456 || p.name != "weird) name" {
		t.Fatalf("parseStat = %+v %v", p, ok)
	}
	const noTerminal = " S 40 50 50 0 -1 4194560 1 0 0 0 1 2 0 0 20 0 9 0 123456 1000 2000"
	if p, ok := parseStat("99 (sh)" + noTerminal); !ok || p.tty != noTTY {
		t.Fatalf("parseStat = %+v %v, want no terminal", p, ok)
	}
	for _, bad := range []string{"", "99 sh S 1", "99 (sh) S 1 2"} {
		if _, ok := parseStat(bad); ok {
			t.Fatalf("parseStat(%q) succeeded", bad)
		}
	}
}

func TestProcReadsThisProcess(t *testing.T) {
	p, ok := proc(os.Getpid())
	if !ok || p.ppid != os.Getppid() || p.start == 0 {
		t.Fatalf("proc = %+v %v", p, ok)
	}
}
