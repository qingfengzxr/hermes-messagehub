package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const oauthResource = "https://hub.example/mcp"
const oauthRedirect = "https://client.example/callback?existing=1"

var oauthVerifier = strings.Repeat("v", 43)

func oauthSetup(t *testing.T, s *Store) OAuthClient {
	t.Helper()
	if err := s.InitOAuth(context.Background(), OAuthOptions{Resource: oauthResource}); err != nil {
		t.Fatal(err)
	}
	c, err := s.RegisterOAuthClient(context.Background(), ClientRegistration{RedirectURIs: []string{oauthRedirect}, ClientName: "test", TokenEndpointAuthMethod: "none"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func oauthRequest(c OAuthClient) AuthorizationRequest {
	sum := sha256.Sum256([]byte(oauthVerifier))
	return AuthorizationRequest{ClientID: c.ClientID, RedirectURI: oauthRedirect, ResponseType: "code", State: "opaque-state", CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]), CodeChallengeMethod: "S256", Resource: oauthResource}
}
func oauthCode(t *testing.T, s *Store, c OAuthClient) string {
	t.Helper()
	ctx := context.Background()
	p, cookie, err := s.BeginAuthorization(ctx, oauthRequest(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveAuthorization(ctx, p.RequestID, "alice"); err != nil {
		t.Fatal(err)
	}
	result, err := s.ContinueAuthorization(ctx, p.RequestID, cookie)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(result.RedirectURI)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("state") != "opaque-state" || u.Query().Get("existing") != "1" {
		t.Fatal("redirect lost binding")
	}
	return u.Query().Get("code")
}
func oauthExchange(c OAuthClient, code string) TokenRequest {
	return TokenRequest{GrantType: "authorization_code", ClientID: c.ClientID, Code: code, RedirectURI: oauthRedirect, CodeVerifier: oauthVerifier, Resource: oauthResource}
}

func TestOAuthRegistrationDoesNotProvisionIdentity(t *testing.T) {
	s := testStore(t)
	c := oauthSetup(t, s)
	if c.ClientID == "" || c.TokenEndpointAuthMethod != "none" {
		t.Fatal("public client missing")
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM principals").Scan(&n); err != nil || n != 0 {
		t.Fatal("DCR created participant")
	}
	for _, redirect := range []string{"http://evil.example/cb", "https://client.example/#x", "https://user@client.example/cb", "file:///cb", "https://client.example/cb#", "https://client.example/cb?code=x"} {
		if _, err := s.RegisterOAuthClient(context.Background(), ClientRegistration{RedirectURIs: []string{redirect}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted redirect %q: %v", redirect, err)
		}
	}
	if _, err := s.RegisterOAuthClient(context.Background(), ClientRegistration{RedirectURIs: []string{"http://127.0.0.1:49152/cb"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterOAuthClient(context.Background(), ClientRegistration{RedirectURIs: []string{oauthRedirect}, TokenEndpointAuthMethod: "client_secret_post"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("accepted client secret method")
	}
}

func TestOAuthApprovalCookieBindingAndSingleUseCode(t *testing.T) {
	s := testStore(t)
	c := oauthSetup(t, s)
	provision(t, s, "alice")
	ctx := context.Background()
	for _, mutate := range []func(*AuthorizationRequest){func(r *AuthorizationRequest) { r.RedirectURI = "https://other.example/cb" }, func(r *AuthorizationRequest) { r.Resource += "/other" }, func(r *AuthorizationRequest) { r.CodeChallengeMethod = "plain" }, func(r *AuthorizationRequest) { r.CodeChallenge = "bad" }, func(r *AuthorizationRequest) { r.State = "" }, func(r *AuthorizationRequest) { r.ResponseType = "token" }} {
		r := oauthRequest(c)
		mutate(&r)
		if _, _, err := s.BeginAuthorization(ctx, r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad authorization accepted: %v", err)
		}
	}
	p, cookie, err := s.BeginAuthorization(ctx, oauthRequest(c))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.ListPendingAuthorizations(ctx)
	if err != nil || len(pending) != 1 || pending[0].RequestID != p.RequestID {
		t.Fatal("missing pending approval", err)
	}
	if _, err := s.ContinueAuthorization(ctx, p.RequestID, "wrong"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("wrong browser accepted")
	}
	result, err := s.ContinueAuthorization(ctx, p.RequestID, cookie)
	if err != nil || !result.Pending {
		t.Fatal("unapproved request issued code")
	}
	if err := s.ApproveAuthorization(ctx, p.RequestID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("nonexistent principal approved")
	}
	if err := s.ApproveAuthorization(ctx, p.RequestID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.ApproveAuthorization(ctx, p.RequestID, "alice"); !errors.Is(err, ErrConflict) {
		t.Fatal("approval can be rebound")
	}
	result, err = s.ContinueAuthorization(ctx, p.RequestID, cookie)
	if err != nil || result.Pending {
		t.Fatal(err)
	}
	if _, err := s.ContinueAuthorization(ctx, p.RequestID, cookie); !errors.Is(err, ErrConflict) {
		t.Fatal("duplicate browser release")
	}
	u, _ := url.Parse(result.RedirectURI)
	code := u.Query().Get("code")
	if code == "" || strings.Contains(result.RedirectURI, cookie) {
		t.Fatal("bad redirect")
	}
	for _, mutate := range []func(*TokenRequest){func(r *TokenRequest) { r.RedirectURI += "x" }, func(r *TokenRequest) { r.CodeVerifier = strings.Repeat("w", 43) }, func(r *TokenRequest) { r.Resource += "x" }, func(r *TokenRequest) { r.ClientID = "wrong" }} {
		r := oauthExchange(c, code)
		mutate(&r)
		if _, err := s.ExchangeOAuthToken(ctx, r); !errors.Is(err, ErrUnauthenticated) {
			t.Fatal("invalid exchange accepted", err)
		}
	}
	tokens, err := s.ExchangeOAuthToken(ctx, oauthExchange(c, code))
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.Authenticate(ctx, tokens.AccessToken)
	if err != nil || actor.PrincipalID() != "alice" {
		t.Fatal("access not Authenticate-compatible", err)
	}
	if _, err := s.Authenticate(ctx, tokens.RefreshToken); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("refresh used as access")
	}
	if _, err := s.ExchangeOAuthToken(ctx, oauthExchange(c, code)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("code replay accepted")
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM tokens WHERE token_hash IN (?,?)", tokens.AccessToken, tokens.RefreshToken).Scan(&n); err != nil || n != 0 {
		t.Fatal("clear tokens persisted")
	}
}

func TestOAuthRestartRotationAndPersistentRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := oauthSetup(t, s)
	provision(t, s, "alice")
	code := oauthCode(t, s, c)
	ctx := context.Background()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.ExchangeOAuthToken(ctx, oauthExchange(c, code))
	if err != nil {
		t.Fatal("code not persistent", err)
	}
	refresh := TokenRequest{GrantType: "refresh_token", ClientID: c.ClientID, Resource: oauthResource, RefreshToken: tok.RefreshToken}
	wrong := refresh
	wrong.Resource += "x"
	if _, err := s.ExchangeOAuthToken(ctx, wrong); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("refresh resource unchecked")
	}
	next, err := s.ExchangeOAuthToken(ctx, refresh)
	if err != nil || next.RefreshToken == tok.RefreshToken || next.AccessToken == tok.AccessToken {
		t.Fatal("did not rotate", err)
	}
	if _, err := s.ExchangeOAuthToken(ctx, refresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("refresh replay accepted")
	}
	if err := s.RevokeOAuthToken(ctx, c.ClientID, next.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, access := range []string{tok.AccessToken, next.AccessToken} {
		if _, err := s.Authenticate(ctx, access); !errors.Is(err, ErrUnauthenticated) {
			t.Fatal("revocation not persistent")
		}
	}
	refresh.RefreshToken = next.RefreshToken
	if _, err := s.ExchangeOAuthToken(ctx, refresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("revoked refresh accepted")
	}
}

func TestOAuthRejectsGrantMetadataItCannotEnforce(t *testing.T) {
	s := testStore(t)
	oauthSetup(t, s)
	for _, grants := range [][]string{{"refresh_token"}, {"authorization_code"}, {"authorization_code", "authorization_code"}} {
		if _, err := s.RegisterOAuthClient(context.Background(), ClientRegistration{RedirectURIs: []string{oauthRedirect}, GrantTypes: grants}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsupported grant metadata accepted: %v", grants)
		}
	}
}
func TestOAuthExpiredCodeAndDisabledPrincipal(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "oauth.db"), Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := oauthSetup(t, s)
	provision(t, s, "alice")
	ctx := context.Background()
	code := oauthCode(t, s, c)
	now = now.Add(time.Minute)
	if _, err := s.ExchangeOAuthToken(ctx, oauthExchange(c, code)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("expired code accepted")
	}
	code = oauthCode(t, s, c)
	if err := s.SetPrincipalActive(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExchangeOAuthToken(ctx, oauthExchange(c, code)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("disabled principal code accepted")
	}
	if err := s.SetPrincipalActive(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	tokens, err := s.ExchangeOAuthToken(ctx, oauthExchange(c, code))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrincipalActive(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExchangeOAuthToken(ctx, TokenRequest{GrantType: "refresh_token", ClientID: c.ClientID, Resource: oauthResource, RefreshToken: tokens.RefreshToken}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("disabled principal refresh accepted")
	}
}
func TestOAuthBoundsExpiryAndAtomicExchange(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "oauth.db"), Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.InitOAuth(ctx, OAuthOptions{Resource: oauthResource, MaxClients: 1, MaxPending: 1}); err != nil {
		t.Fatal(err)
	}
	c, err := s.RegisterOAuthClient(ctx, ClientRegistration{RedirectURIs: []string{oauthRedirect}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterOAuthClient(ctx, ClientRegistration{RedirectURIs: []string{oauthRedirect}}); !errors.Is(err, ErrQuota) {
		t.Fatal("client cap ignored")
	}
	provision(t, s, "alice")
	p, cookie, err := s.BeginAuthorization(ctx, oauthRequest(c))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginAuthorization(ctx, oauthRequest(c)); !errors.Is(err, ErrQuota) {
		t.Fatal("pending cap ignored")
	}
	now = now.Add(11 * time.Minute)
	if _, err := s.ContinueAuthorization(ctx, p.RequestID, cookie); !errors.Is(err, ErrExpired) {
		t.Fatal("expired request accepted")
	}
	code := oauthCode(t, s, c)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.ExchangeOAuthToken(ctx, oauthExchange(c, code)); results <- err }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatal("code not single-use atomically")
	}
}
