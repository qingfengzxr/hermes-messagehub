package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func reviewOAuthFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	if err := s.InitOAuth(ctx, OAuthOptions{Resource: "https://message.example.test/mcp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPrincipal(ctx, "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	client, err := s.RegisterOAuthClient(ctx, ClientRegistration{RedirectURIs: []string{"https://client.example.test/callback"}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(strings.Repeat("v", 43)))
	pending, cookie, err := s.BeginAuthorization(ctx, AuthorizationRequest{ClientID: client.ClientID, RedirectURI: client.RedirectURIs[0], ResponseType: "code", State: "fixture", CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]), CodeChallengeMethod: "S256", Resource: "https://message.example.test/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveAuthorization(ctx, pending.RequestID, "alice"); err != nil {
		t.Fatal(err)
	}
	continued, err := s.ContinueAuthorization(ctx, pending.RequestID, cookie)
	if err != nil || continued.Pending {
		t.Fatal("authorization release failed")
	}
	redirect, err := url.Parse(continued.RedirectURI)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := s.ExchangeOAuthToken(ctx, TokenRequest{GrantType: "authorization_code", ClientID: client.ClientID, Code: redirect.Query().Get("code"), RedirectURI: client.RedirectURIs[0], CodeVerifier: strings.Repeat("v", 43), Resource: "https://message.example.test/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	return s, client.ClientID, tokens.RefreshToken
}

func reviewRotate(t *testing.T, s *Store, client, refresh string) OAuthTokens {
	t.Helper()
	tokens, err := s.ExchangeOAuthToken(context.Background(), TokenRequest{GrantType: "refresh_token", ClientID: client, RefreshToken: refresh, Resource: "https://message.example.test/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

func TestReviewRefreshReplayRevokesWholeFamilyAfterManyRotations(t *testing.T) {
	s, client, original := reviewOAuthFixture(t)
	current := original
	var descendant OAuthTokens
	for range 12 {
		descendant = reviewRotate(t, s, client, current)
		current = descendant.RefreshToken
	}
	ctx := context.Background()
	if _, err := s.ExchangeOAuthToken(ctx, TokenRequest{GrantType: "refresh_token", ClientID: client, RefreshToken: original, Resource: "https://message.example.test/mcp"}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("refresh replay accepted")
	}
	if _, err := s.Authenticate(ctx, descendant.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("replayed refresh did not revoke descendant access")
	}
	if _, err := s.ExchangeOAuthToken(ctx, TokenRequest{GrantType: "refresh_token", ClientID: client, RefreshToken: descendant.RefreshToken, Resource: "https://message.example.test/mcp"}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("replayed refresh did not revoke descendant refresh")
	}
}

func TestReviewOldRefreshRevokesWholeFamilyAfterManyRotations(t *testing.T) {
	s, client, original := reviewOAuthFixture(t)
	current := original
	var descendant OAuthTokens
	for range 12 {
		descendant = reviewRotate(t, s, client, current)
		current = descendant.RefreshToken
	}
	ctx := context.Background()
	if err := s.RevokeOAuthToken(ctx, client, original); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, descendant.AccessToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("old refresh lost family revocation authority")
	}
}

func TestReviewExpiredAccessIsReclaimedWithoutDroppingRefreshHistory(t *testing.T) {
	s, client, refresh := reviewOAuthFixture(t)
	now := time.Now().UTC()
	s.opts.Now = func() time.Time { return now }
	for range 512 {
		now = now.Add(16 * time.Minute)
		refresh = reviewRotate(t, s, client, refresh).RefreshToken
	}
	var expired, used, live int
	if err := s.db.QueryRow("SELECT count(*) FROM tokens WHERE kind='access' AND expires_at<=?", s.now()).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM tokens WHERE kind='refresh_used'").Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM tokens WHERE kind='access'").Scan(&live); err != nil {
		t.Fatal(err)
	}
	if expired != 0 || used != 512 || live != 1 {
		t.Fatalf("expired=%d refresh_history=%d active_access=%d", expired, used, live)
	}
}

func TestReviewRotationQuotaPreservesCurrentToken(t *testing.T) {
	s, client, refresh := reviewOAuthFixture(t)
	ctx := context.Background()
	var grant string
	if err := s.db.QueryRow("SELECT grant_id FROM tokens WHERE token_hash=?", tokenDigest(refresh)).Scan(&grant); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4096; i++ {
		if _, err := tx.Exec("INSERT INTO tokens(token_hash,grant_id,kind,expires_at) VALUES(?,?,'refresh_used',?)", tokenDigest(fmt.Sprintf("fixture-history-%d", i)), grant, s.now()+3600); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExchangeOAuthToken(ctx, TokenRequest{GrantType: "refresh_token", ClientID: client, RefreshToken: refresh, Resource: "https://message.example.test/mcp"}); !errors.Is(err, ErrQuota) {
		t.Fatal("rotation history quota not enforced")
	}
	var active int
	if err := s.db.QueryRow("SELECT count(*) FROM tokens WHERE token_hash=? AND kind='refresh' AND expires_at>?", tokenDigest(refresh), s.now()).Scan(&active); err != nil || active != 1 {
		t.Fatal("quota consumed valid refresh token")
	}
}
