package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

func adminStore(t *testing.T) *hub.Store {
	t.Helper()
	s, err := hub.Open(filepath.Join(t.TempDir(), "state.db"), hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAdministrativeGrantWrittenOnlyToPrivateFile(t *testing.T) {
	ctx := context.Background()
	s := adminStore(t)
	var out bytes.Buffer
	if err := Run(ctx, s, []string{"principal-add", "--id", "alice", "--name", "Alice"}, &out); err != nil {
		t.Fatal(err)
	}
	private := t.TempDir()
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "grant.json")
	if err := Run(ctx, s, []string{"grant-create", "--principal", "alice", "--output", path}, &out); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var credential struct {
		Token   string `json:"token"`
		GrantID string `json:"grant_id"`
	}
	if err := json.Unmarshal(body, &credential); err != nil {
		t.Fatal(err)
	}
	if credential.Token == "" || strings.Contains(out.String(), credential.Token) {
		t.Fatal("credential missing or leaked")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential file not private")
	}
	if _, err := s.Authenticate(ctx, credential.Token); err != nil {
		t.Fatal("issued token unusable")
	}
	if err := Run(ctx, s, []string{"grant-revoke", "--id", credential.GrantID}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, credential.Token); !errors.Is(err, hub.ErrUnauthenticated) {
		t.Fatal("revocation ineffective")
	}
}

func TestPrincipalDisableAndEnable(t *testing.T) {
	s := adminStore(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := Run(ctx, s, []string{"principal-add", "--id", "bob", "--name", "Bob"}, &out); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.IssueGrant(ctx, "bob", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, s, []string{"principal-disable", "--id", "bob"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, hub.ErrUnauthenticated) {
		t.Fatal("disabled principal accepted")
	}
	if err := Run(ctx, s, []string{"principal-enable", "--id", "bob"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, token); err != nil {
		t.Fatal(err)
	}
}

func TestExistingCredentialAndUnsafeOutputAreNotOverwritten(t *testing.T) {
	ctx := context.Background()
	s := adminStore(t)
	var out bytes.Buffer
	if err := s.AddPrincipal(ctx, "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	private := t.TempDir()
	if err := os.Chmod(private, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, "existing.json")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dest := range []string{path, "relative.json", ""} {
		if err := Run(ctx, s, []string{"grant-create", "--principal", "alice", "--output", dest}, &out); err == nil {
			t.Fatal("unsafe output accepted")
		}
	}
	body, _ := os.ReadFile(path)
	if string(body) != "keep" {
		t.Fatal("existing file overwritten")
	}
	if err := Run(ctx, s, []string{"unknown"}, &out); err == nil {
		t.Fatal("unknown operation accepted")
	}
}
