package hub

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewHardlinkAliasCannotOpenSecondWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	alias := filepath.Join(dir, "alias.db")
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(alias, Options{}); err == nil {
		t.Fatal("hardlink alias opened as second writer")
	}
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("aliased primary opened while unsafe alias exists")
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err != nil {
		t.Fatal("safe reopen after alias removal rejected")
	}
}

func TestReviewInterruptedFirstBootstrapIsAdopted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("_pragma", "journal_mode(WAL)")
	u.RawQuery = q.Encode()
	raw, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal("interrupted bootstrap not adopted")
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.AddPrincipal(ctx, "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.IssueGrant(ctx, "alice", 1e9)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); err != nil {
		t.Fatal("adopted store unusable")
	}
}

func TestReviewForeignDatabaseIsRejectedUnmodified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foreign.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("CREATE TABLE something(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("foreign database adopted")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("rejected foreign database modified to %o", info.Mode().Perm())
	}
}
