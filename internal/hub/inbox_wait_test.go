package hub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type inboxWaitResult struct {
	page InboxPage
	err  error
}

func startInboxWait(t *testing.T, s *Store, a Actor, req WaitInboxRequest) <-chan inboxWaitResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan inboxWaitResult, 1)
	go func() { p, err := s.WaitInbox(ctx, a, req); done <- inboxWaitResult{p, err} }()
	return done
}

func receiveInboxWait(t *testing.T, done <-chan inboxWaitResult) inboxWaitResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not finish promptly")
		return inboxWaitResult{}
	}
}

func TestInboxWaitValidationAndScopedCursor(t *testing.T) {
	s := testStore(t)
	a, b, c := messageActors(t, s)
	conv := messageConversation(t, s, a, "wait", "alice", "bob", "carol")
	messageSend(t, s, a, conv.ID, "one", "hello")
	p, err := s.WaitInbox(context.Background(), b, WaitInboxRequest{WaitMS: 20000})
	if err != nil || len(p.Items) != 1 || p.NextCursor == "" {
		t.Fatalf("backlog: %v", err)
	}
	for _, req := range []WaitInboxRequest{
		{WaitMS: 0}, {WaitMS: -1}, {WaitMS: 20001}, {WaitMS: 1, PageRequest: PageRequest{Limit: -1}},
		{WaitMS: 1, PageRequest: PageRequest{Limit: 101}}, {WaitMS: 1, PageRequest: PageRequest{Cursor: strings.Repeat("x", 1025)}},
		{WaitMS: 1, PageRequest: PageRequest{Cursor: p.NextCursor}},
	} {
		if _, err := s.WaitInbox(context.Background(), c, req); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid request accepted: %v", err)
		}
	}
	if _, err := s.WaitInbox(context.Background(), Actor{}, WaitInboxRequest{WaitMS: 1}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("forged actor: %v", err)
	}
}

func TestInboxWaitTimeoutCancelAndDeadline(t *testing.T) {
	s := testStore(t)
	a, _ := provision(t, s, "alice")
	start := time.Now()
	p, err := s.WaitInbox(context.Background(), a, WaitInboxRequest{WaitMS: 40})
	if err != nil || p.Items == nil || len(p.Items) != 0 || p.HasMore {
		t.Fatalf("empty wait: %v", err)
	}
	if time.Since(start) < 35*time.Millisecond || time.Since(start) > time.Second {
		t.Fatal("wait duration outside bounds")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan inboxWaitResult, 1)
	go func() {
		p, err := s.WaitInbox(ctx, a, WaitInboxRequest{WaitMS: 20000})
		done <- inboxWaitResult{p, err}
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if r := receiveInboxWait(t, done); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancel: %v", r.err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.WaitInbox(ctx, a, WaitInboxRequest{WaitMS: 20000}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
}

func TestInboxWaitCommitWhileWaitingAndAnotherStore(t *testing.T) {
	for _, other := range []bool{false, true} {
		t.Run(fmt.Sprintf("another_store_%t", other), func(t *testing.T) {
			s := testStore(t)
			a, b, _ := messageActors(t, s)
			conv := messageConversation(t, s, a, "wait", "alice", "bob")
			writer := s
			if other {
				var err error
				writer, err = Open(s.path, Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
			}
			done := startInboxWait(t, s, b, WaitInboxRequest{WaitMS: 20000})
			select {
			case <-done:
				t.Fatal("empty wait returned before commit")
			case <-time.After(30 * time.Millisecond):
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			r, err := writer.Send(ctx, a, SendRequest{ConversationID: conv.ID, IdempotencyKey: "one", Text: "hello"})
			if err != nil {
				t.Fatalf("wait held database connection: %v", err)
			}
			got := receiveInboxWait(t, done)
			if got.err != nil || len(got.page.Items) != 1 || got.page.Items[0].Message.ID != r.Message.ID {
				t.Fatalf("commit not observed: %v", got.err)
			}
			pending, err := s.PullInbox(context.Background(), b, PageRequest{})
			if err != nil || len(pending.Items) != 1 {
				t.Fatal("wait changed pending state")
			}
		})
	}
}

func TestInboxWaitLocalCommitWakesWithoutPollingDelay(t *testing.T) {
	s := testStore(t)
	a, b, _ := messageActors(t, s)
	conv := messageConversation(t, s, a, "wait", "alice", "bob")
	done := startInboxWait(t, s, b, WaitInboxRequest{WaitMS: 20000})
	select {
	case <-done:
		t.Fatal("wait returned before send")
	case <-time.After(30 * time.Millisecond):
	}
	r := messageSend(t, s, a, conv.ID, "one", "hello")
	select {
	case got := <-done:
		if got.err != nil || len(got.page.Items) != 1 || got.page.Items[0].Message.ID != r.Message.ID {
			t.Fatalf("commit: %v", got.err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("local commit missed immediate wakeup")
	}
}

func TestInboxWaitRechecksAuthority(t *testing.T) {
	for _, action := range []string{"revoke", "disable", "expire-token", "expire-grant"} {
		t.Run(action, func(t *testing.T) {
			s := testStore(t)
			a, _ := provision(t, s, "alice")
			other, err := Open(s.path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			done := startInboxWait(t, s, a, WaitInboxRequest{WaitMS: 20000})
			select {
			case <-done:
				t.Fatal("empty wait returned early")
			case <-time.After(30 * time.Millisecond):
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			switch action {
			case "revoke":
				err = other.RevokeGrant(ctx, a.GrantID())
			case "disable":
				err = other.SetPrincipalActive(ctx, a.PrincipalID(), false)
			case "expire-token":
				_, err = other.db.ExecContext(ctx, "UPDATE tokens SET expires_at=0 WHERE grant_id=?", a.GrantID())
			case "expire-grant":
				_, err = other.db.ExecContext(ctx, "UPDATE grants SET expires_at=0 WHERE id=?", a.GrantID())
			}
			if err != nil {
				t.Fatal(err)
			}
			if r := receiveInboxWait(t, done); !errors.Is(r.err, ErrUnauthenticated) {
				t.Fatalf("cached authority accepted: %v", r.err)
			}
		})
	}
}

func TestInboxWaitTimeoutRevalidatesAuthority(t *testing.T) {
	s := testStore(t)
	a, _ := provision(t, s, "alice")
	done := startInboxWait(t, s, a, WaitInboxRequest{WaitMS: 80})
	time.Sleep(20 * time.Millisecond)
	if err := s.RevokeGrant(context.Background(), a.GrantID()); err != nil {
		t.Fatal(err)
	}
	if r := receiveInboxWait(t, done); !errors.Is(r.err, ErrUnauthenticated) {
		t.Fatalf("timeout returned stale authority: %v", r.err)
	}
}

func TestInboxWaitSubscribesBeforeFirstDatabaseScan(t *testing.T) {
	s := testStore(t)
	a, _ := provision(t, s, "alice")
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := startInboxWait(t, s, a, WaitInboxRequest{WaitMS: 80})
	deadline := time.Now().Add(time.Second)
	for {
		s.inboxMu.Lock()
		subscribed := s.inboxChanged != nil
		s.inboxMu.Unlock()
		if subscribed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scan started before subscription; commit wakeup can be lost")
		}
		time.Sleep(time.Millisecond)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if r := receiveInboxWait(t, done); r.err != nil {
		t.Fatal(r.err)
	}
}

func TestInboxWaitNotifyOnlyAfterSuccessfulCommit(t *testing.T) {
	s := testStore(t)
	a, b, _ := messageActors(t, s)
	conv := messageConversation(t, s, a, "wait", "alice", "bob")
	changed := s.inboxChange()
	if _, err := s.Send(context.Background(), a, SendRequest{ConversationID: conv.ID, IdempotencyKey: "invalid"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid send accepted")
	}
	select {
	case <-changed:
		t.Fatal("failed transaction notified waiters")
	default:
	}
	r := messageSend(t, s, a, conv.ID, "one", "hello")
	select {
	case <-changed:
	default:
		t.Fatal("committed send did not broadcast")
	}
	other, err := Open(s.path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	p, err := other.PullInbox(context.Background(), b, PageRequest{})
	if err != nil || len(p.Items) != 1 || p.Items[0].Message.ID != r.Message.ID {
		t.Fatalf("notification before durable delivery: %v", err)
	}
}

func TestInboxWaitBroadcastAndRepeatedReconnect(t *testing.T) {
	s := testStore(t)
	a, b, _ := messageActors(t, s)
	conv := messageConversation(t, s, a, "wait", "alice", "bob")
	for round := 0; round < 8; round++ {
		var waits []<-chan inboxWaitResult
		for i := 0; i < 8; i++ {
			waits = append(waits, startInboxWait(t, s, b, WaitInboxRequest{WaitMS: 20000}))
		}
		r := messageSend(t, s, a, conv.ID, fmt.Sprintf("round-%d", round), "hello")
		for _, wait := range waits {
			got := receiveInboxWait(t, wait)
			if got.err != nil || len(got.page.Items) != 1 || got.page.Items[0].Message.ID != r.Message.ID {
				t.Fatalf("broadcast/reconnect: %v", got.err)
			}
		}
		if _, err := s.Ack(context.Background(), b, []string{r.Message.ID}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInboxWaitIgnoresUnrelatedCommitsAndRepeatsAfterCancel(t *testing.T) {
	s := testStore(t)
	a, b, c := messageActors(t, s)
	conv := messageConversation(t, s, a, "other", "alice", "carol")
	done := startInboxWait(t, s, b, WaitInboxRequest{WaitMS: 100})
	messageSend(t, s, a, conv.ID, "other", "not for bob")
	select {
	case <-done:
		t.Fatal("notification treated as delivery")
	case <-time.After(40 * time.Millisecond):
	}
	r := receiveInboxWait(t, done)
	if r.err != nil || len(r.page.Items) != 0 {
		t.Fatalf("unrelated wait: %v", r.err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.WaitInbox(ctx, c, WaitInboxRequest{WaitMS: 20000}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled backlog: %v", err)
	}
	p, err := s.WaitInbox(context.Background(), c, WaitInboxRequest{WaitMS: 20000})
	if err != nil || len(p.Items) != 1 {
		t.Fatalf("reconnect lost backlog: %v", err)
	}
}
