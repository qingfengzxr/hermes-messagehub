package hub

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewOAuthExpiredAccessStateIsBounded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "auth-growth.db")
	opts := Options{Now: func() time.Time { return now }, MaxFileBytes: 1, MaxStoreBytes: 1024}
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	c := oauthSetup(t, s)
	if err := s.AddPrincipal(ctx, "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	tok, err := s.ExchangeOAuthToken(ctx, oauthExchange(c, oauthCode(t, s, c)))
	if err != nil {
		t.Fatal(err)
	}
	var startRows, endRows, expiredRows, hashBytes int
	if err := s.db.QueryRow("SELECT count(*) FROM tokens").Scan(&startRows); err != nil {
		t.Fatal(err)
	}
	const rotations = 512
	for i := 0; i < rotations; i++ {
		now = now.Add(16 * time.Minute)
		tok, err = s.ExchangeOAuthToken(ctx, TokenRequest{GrantType: "refresh_token", ClientID: c.ClientID, Resource: oauthResource, RefreshToken: tok.RefreshToken})
		if err != nil {
			t.Fatalf("rotation %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*),sum(length(token_hash)) FROM tokens").Scan(&endRows, &hashBytes); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM tokens WHERE kind='access' AND expires_at<=?", now.Unix()).Scan(&expiredRows); err != nil {
		t.Fatal(err)
	}
	t.Logf("rotations=%d rows_before=%d rows_after_reopen=%d expired_access_rows=%d stored_hash_bytes=%d configured_MaxStoreBytes=%d", rotations, startRows, endRows, expiredRows, hashBytes, opts.MaxStoreBytes)
	if expiredRows == rotations {
		t.Error("all expired access-token rows survive repeated rotation and restart; OAuth token state has no reclamation or quota")
	}
}
