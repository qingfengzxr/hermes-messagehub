package hub

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func provision(t *testing.T, s *Store, id string) (Actor, string) {
	t.Helper()
	ctx := context.Background()
	if err := s.AddPrincipal(ctx, id, id); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.IssueGrant(ctx, id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return actor, token
}

func TestReplyLookupIndexesExistingDatabase(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, b, _ := messageActors(t, s)
	conv := messageConversation(t, s, a, "legacy", "alice", "bob")
	parent := messageSend(t, s, a, conv.ID, "parent", "original message").Message
	reply, err := s.Send(ctx, b, SendRequest{ConversationID: conv.ID, IdempotencyKey: "reply", ReplyTo: parent.ID, Text: "existing reply"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP INDEX message_replies"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	page, err := reopened.ReadReplies(ctx, b, conv.ID, parent.ID, PageRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != reply.Message.ID || page.Items[0].Text != "existing reply" {
		t.Fatalf("existing reply after reopening: %+v, %v", page, err)
	}
	var id, ancestor, unused, version int
	var plan string
	if err := reopened.db.QueryRow("EXPLAIN QUERY PLAN "+directRepliesSQL, conv.ID, parent.ID, 0, reopened.now(), 21).Scan(&id, &ancestor, &unused, &plan); err != nil || !strings.Contains(plan, "USING INDEX message_replies") || !strings.Contains(plan, "reply_to=?") {
		t.Fatalf("reply lookup must seek by parent: %q, %v", plan, err)
	}
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("user_version=%d, %v", version, err)
	}
}

func TestGrantIdentityAndPrivateStorage(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	actor, token := provision(t, s, "alice")
	if actor.PrincipalID() != "alice" {
		t.Fatal("wrong principal")
	}
	if len(token) < 43 {
		t.Fatal("credential too short")
	}
	if _, err := s.Authenticate(ctx, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("empty token accepted")
	}
	if _, err := s.Authenticate(ctx, strings.Repeat("x", 48)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("unknown token accepted")
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("database must be private")
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM tokens WHERE token_hash = ?", token).Scan(&n); err != nil || n != 0 {
		t.Fatal("token stored in clear")
	}
}

func TestGrantRevocationAndExpirySurviveReopen(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "hub.db")
	opts := Options{Now: func() time.Time { return now }}
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	actor, token := provision(t, s, "alice")
	if err := s.RevokeGrant(ctx, actor.GrantID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("revoked credential accepted")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("revocation lost at restart")
	}
	_, fresh, err := s.IssueGrant(ctx, "alice", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := s.Authenticate(ctx, fresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("expiry boundary accepted")
	}
}

func TestDisabledPrincipalAndForgedActorAreRejected(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a, token := provision(t, s, "alice")
	if err := s.transaction(ctx, Actor{}, func(_ *sql.Tx) error { return nil }); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("empty actor accepted")
	}
	forged := a
	forged.principalID = "bob"
	if err := s.transaction(ctx, forged, func(_ *sql.Tx) error { return nil }); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("forged identity accepted")
	}
	if err := s.SetPrincipalActive(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("disabled principal accepted")
	}
	if err := s.transaction(ctx, a, func(_ *sql.Tx) error { return nil }); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("cached actor bypassed revocation")
	}
}

func TestCursorScopeAndOptions(t *testing.T) {
	s := testStore(t)
	cursor := s.sealCursor("alice:conversation", 7)
	n, err := s.openCursor("alice:conversation", cursor)
	if err != nil || n != 7 {
		t.Fatal("cursor did not round-trip")
	}
	if _, err := s.openCursor("bob:conversation", cursor); !errors.Is(err, ErrInvalid) {
		t.Fatal("cross-scope cursor accepted")
	}
	if _, err := s.openCursor("alice:conversation", cursor+"x"); !errors.Is(err, ErrInvalid) {
		t.Fatal("tampered cursor accepted")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "invalid.db"), Options{MaxFileBytes: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid quota accepted")
	}
}

func TestUnsafeDatabaseAndInvalidPrincipalsRejected(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link, Options{}); err == nil {
		t.Fatal("database symlink accepted")
	}
	s := testStore(t)
	for _, id := range []string{"", "../alice", "Alice", "a b", strings.Repeat("a", 65)} {
		if err := s.AddPrincipal(context.Background(), id, "name"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad principal accepted: %q", id)
		}
	}
	if _, _, err := s.IssueGrant(context.Background(), "missing", time.Hour); err == nil {
		t.Fatal("grant for nonexistent identity")
	}
}
