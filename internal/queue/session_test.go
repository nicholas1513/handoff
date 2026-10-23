package queue

import (
	"context"
	"testing"
	"time"
)

func TestSessionRoundTrip(t *testing.T) {
	q := &Queue{Root: t.TempDir()}
	if err := q.WriteSession(Session{SessionID: sid, Cwd: "/src"}); err != nil {
		t.Fatal(err)
	}
	s, err := q.ReadSession(sid)
	if err != nil || s.Cwd != "/src" {
		t.Fatalf("ReadSession = %+v %v", s, err)
	}
	if _, err := q.ReadSession("../x"); err == nil {
		t.Fatal("bad session id accepted")
	}
	if err := q.WriteSession(Session{SessionID: "../x"}); err == nil {
		t.Fatal("bad session id written")
	}
}

func TestPromptsKeepNewestAndLookup(t *testing.T) {
	q := &Queue{Root: t.TempDir()}
	base := time.Now().Add(-time.Hour)
	for i := 0; i < KeepPrompts+5; i++ {
		if err := q.AddPrompt(Prompt{SessionID: sid, TS: base.Add(time.Duration(i) * time.Minute), Prompt: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := q.PromptBefore(sid, base.Add(10*time.Minute+time.Second))
	if err != nil || p.Prompt != string(rune('a'+10)) {
		t.Fatalf("PromptBefore = %+v %v, want prompt k", p, err)
	}
	if _, err := q.PromptBefore(sid, base.Add(time.Minute)); err == nil {
		t.Fatal("found a prompt that should have been pruned")
	}
}

func TestWait(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)
	go func() {
		time.Sleep(50 * time.Millisecond)
		q.WriteResult(it, Result{State: Succeeded, Stdout: "done"})
	}()
	r, err := q.Wait(context.Background(), it, 10*time.Millisecond)
	if err != nil || r.Stdout != "done" {
		t.Fatalf("Wait = %+v %v", r, err)
	}

	old := addOne(t, q, body)
	old.Expires = time.Now().Add(-time.Second)
	if r, _ := q.Wait(context.Background(), old, 10*time.Millisecond); r.State != Expired {
		t.Fatalf("Wait on expired = %+v", r)
	}
}

func TestWaitReturnsOnRevoke(t *testing.T) {
	q, body := newQueue(t)
	it := addOne(t, q, body)
	go func() {
		time.Sleep(50 * time.Millisecond)
		q.Revoke(it, sid)
	}()
	r, err := q.Wait(context.Background(), it, 10*time.Millisecond)
	if err != nil || r.State != Revoked {
		t.Fatalf("Wait = %+v %v, want revoked", r, err)
	}
}
