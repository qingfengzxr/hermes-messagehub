package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

func oauthHTTPFixture(t *testing.T) (*hub.Store, http.Handler) {
	t.Helper()
	s, err := hub.Open(filepath.Join(t.TempDir(), "oauth.db"), hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.InitOAuth(context.Background(), hub.OAuthOptions{Resource: "https://hub.example/mcp"}); err != nil {
		t.Fatal(err)
	}
	if err = s.AddPrincipal(context.Background(), "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	h, err := newOAuthHandler(s, "https://hub.example")
	if err != nil {
		t.Fatal(err)
	}
	return s, h
}
func oauthHTTP(h http.Handler, method, path, body, contentType string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://hub.example"+path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestOAuthHTTPPublicRegistrationApprovalAndExchange(t *testing.T) {
	s, h := oauthHTTPFixture(t)
	ctx := context.Background()
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		w := oauthHTTP(h, "GET", path, "", "", nil)
		if w.Code != 200 {
			t.Fatal("discovery", path, w.Code)
		}
		if !strings.Contains(w.Body.String(), "https://hub.example") {
			t.Fatal("issuer not configured")
		}
	}
	w := oauthHTTP(h, "POST", "/register", `{"redirect_uris":["https://client.example/cb"],"client_name":"Test","application_type":"native"}`, "application/json", nil)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var client hub.OAuthClient
	if err := json.Unmarshal(w.Body.Bytes(), &client); err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {client.ClientID}, "redirect_uri": {"https://client.example/cb"}, "response_type": {"code"}, "state": {"opaque-state"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "resource": {"https://hub.example/mcp"}}
	w = oauthHTTP(h, "GET", "/authorize?"+q.Encode(), "", "", nil)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var pending struct {
		RequestID   string `json:"request_id"`
		ContinueURI string `json:"continue_uri"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing browser cookie")
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatal("unsafe browser cookie")
	}
	if strings.Contains(w.Body.String(), cookie.Value) {
		t.Fatal("cookie leaked in body")
	}
	w = oauthHTTP(h, "GET", pending.ContinueURI, "", "", nil)
	if w.Code != 401 {
		t.Fatal("continuation without browser")
	}
	w = oauthHTTP(h, "GET", pending.ContinueURI, "", "", cookie)
	if w.Code != 202 {
		t.Fatal("unapproved continuation", w.Code)
	}
	w = oauthHTTP(h, "POST", "/approve", `{"principal_id":"alice"}`, "application/json", cookie)
	if w.Code != 404 {
		t.Fatal("web approval exposed")
	}
	if err := s.ApproveAuthorization(ctx, pending.RequestID, "alice"); err != nil {
		t.Fatal(err)
	}
	w = oauthHTTP(h, "GET", pending.ContinueURI, "", "", cookie)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("state") != "opaque-state" || u.Query().Get("code") == "" || strings.Contains(location, cookie.Value) {
		t.Fatal("bad redirect")
	}
	tokenForm := url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ClientID}, "redirect_uri": {"https://client.example/cb"}, "code_verifier": {verifier}, "code": {u.Query().Get("code")}, "resource": {"https://hub.example/mcp"}}
	w = oauthHTTP(h, "POST", "/token", tokenForm.Encode(), "application/x-www-form-urlencoded", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var tok hub.OAuthTokens
	if err := json.Unmarshal(w.Body.Bytes(), &tok); err != nil {
		t.Fatal(err)
	}
	if tok.TokenType != "Bearer" || tok.RefreshToken == "" {
		t.Fatal("bad token response")
	}
	actor, err := s.Authenticate(ctx, tok.AccessToken)
	if err != nil || actor.PrincipalID() != "alice" {
		t.Fatal("unbound OAuth token")
	}
	w = oauthHTTP(h, "POST", "/token", tokenForm.Encode(), "application/x-www-form-urlencoded", nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_grant") {
		t.Fatal("code replay", w.Code, w.Body.String())
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {client.ClientID}, "refresh_token": {tok.RefreshToken}, "resource": {"https://hub.example/mcp"}}
	w = oauthHTTP(h, "POST", "/token", refresh.Encode(), "application/x-www-form-urlencoded", nil)
	if w.Code != 200 {
		t.Fatal("refresh failed", w.Code, w.Body.String())
	}
	var next hub.OAuthTokens
	if err := json.Unmarshal(w.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	w = oauthHTTP(h, "POST", "/revoke", url.Values{"client_id": {client.ClientID}, "token": {next.RefreshToken}}.Encode(), "application/x-www-form-urlencoded", nil)
	if w.Code != 200 {
		t.Fatal("revoke failed")
	}
	if _, err := s.Authenticate(ctx, next.AccessToken); err == nil {
		t.Fatal("revoked token accepted")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("OAuth cacheable")
	}
}
func TestOAuthHTTPStrictParamsAndUnsupportedForms(t *testing.T) {
	_, h := oauthHTTPFixture(t)
	cases := []struct {
		method, path, body, content string
		status                      int
	}{
		{"POST", "/register", `{"redirect_uris":["https://client.example/cb"],"principal_id":"alice"}`, "application/json", 400},
		{"POST", "/register", `{"redirect_uris":["https://client.example/cb"]}`, "text/plain", 415},
		{"GET", "/authorize?client_id=x&client_id=y", "", "", 400},
		{"GET", "/authorize?client_id=x&sender=alice", "", "", 400},
		{"GET", "/authorize/continue?request_id=x&state=other", "", "", 400},
		{"POST", "/token", "grant_type=password&client_id=x", "application/x-www-form-urlencoded", 400},
		{"POST", "/token", "grant_type=authorization_code&client_id=x&client_secret=x", "application/x-www-form-urlencoded", 400},
		{"POST", "/token?client_id=x", "grant_type=authorization_code&client_id=y", "application/x-www-form-urlencoded", 400},
		{"POST", "/token", "grant_type=authorization_code&client_id=x&client_id=y", "application/x-www-form-urlencoded", 400},
		{"POST", "/token", `{}`, "application/json", 415},
		{"GET", "/token", "", "", 405},
	}
	for i, c := range cases {
		w := oauthHTTP(h, c.method, c.path, c.body, c.content, nil)
		data, _ := io.ReadAll(w.Result().Body)
		if w.Code != c.status {
			t.Fatalf("case %d got %d want %d %s", i, w.Code, c.status, data)
		}
	}
}
