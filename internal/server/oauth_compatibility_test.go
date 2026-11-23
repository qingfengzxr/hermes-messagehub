package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

func compatibilityAuthorization(t *testing.T, s *hub.Store) (hub.OAuthClient, url.Values) {
	t.Helper()
	c, err := s.RegisterOAuthClient(context.Background(), hub.ClientRegistration{RedirectURIs: []string{"https://grok.com/connectors-oauth-exchange-code/"}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(strings.Repeat("v", 43)))
	return c, url.Values{"client_id": {c.ClientID}, "redirect_uri": {c.RedirectURIs[0]}, "response_type": {"code"}, "state": {"opaque-state"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "resource": {"https://hub.example/mcp"}}
}

func TestOAuthAuthorizationOptionalParameters(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		values    []string
		status    int
		errorCode string
	}{
		{"empty scope", "scope", []string{""}, 202, ""},
		{"extension", "prompt", []string{"consent"}, 202, ""},
		{"identity extension does not grant identity", "principal_id", []string{"mallory"}, 202, ""},
		{"unsupported scope", "scope", []string{"admin"}, 400, "invalid_scope"},
		{"duplicate extension", "prompt", []string{"consent", ""}, 400, "invalid_request"},
		{"duplicate scope", "scope", []string{"", ""}, 400, "invalid_request"},
		{"empty required resource", "resource", []string{""}, 400, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, h := oauthHTTPFixture(t)
			_, q := compatibilityAuthorization(t, s)
			q[tc.key] = tc.values
			w := oauthHTTP(h, "GET", "/authorize?"+q.Encode(), "", "", nil)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			if tc.errorCode != "" {
				var body map[string]string
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != tc.errorCode {
					t.Fatal("wrong OAuth error", err)
				}
			}
			pending, err := s.ListPendingAuthorizations(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.status == 202 {
				if len(pending) != 1 {
					t.Fatal("expected exactly one local approval request")
				}
				cookies := w.Result().Cookies()
				if len(cookies) != 1 {
					t.Fatal("missing initiating browser cookie")
				}
				result, err := s.ContinueAuthorization(context.Background(), pending[0].RequestID, cookies[0].Value)
				if err != nil || !result.Pending || result.RedirectURI != "" {
					t.Fatal("request extensions bypassed local approval")
				}
			} else if len(pending) != 0 {
				t.Fatal("rejected request persisted")
			}
		})
	}
}

func TestOAuthLongStateAndEmptyFormFieldsRoundTrip(t *testing.T) {
	s, h := oauthHTTPFixture(t)
	ctx := context.Background()
	c, q := compatibilityAuthorization(t, s)
	state := strings.Repeat("s", 2048) + "+/&=opaque"
	q.Set("state", state)
	q.Set("scope", "")
	w := oauthHTTP(h, "GET", "/authorize?"+q.Encode(), "", "", nil)
	if w.Code != 202 {
		t.Fatalf("long opaque state returned %d, want 202", w.Code)
	}
	var pending struct {
		RequestID   string `json:"request_id"`
		ContinueURI string `json:"continue_uri"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	cookie := w.Result().Cookies()[0]
	if err := s.ApproveAuthorization(ctx, pending.RequestID, "alice"); err != nil {
		t.Fatal(err)
	}
	w = oauthHTTP(h, "GET", pending.ContinueURI, "", "", cookie)
	if w.Code != http.StatusSeeOther {
		t.Fatal("approved continuation failed")
	}
	callback, err := url.Parse(w.Header().Get("Location"))
	if err != nil || callback.Query().Get("state") != state {
		t.Fatal("opaque state was not preserved exactly")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {c.ClientID}, "redirect_uri": {c.RedirectURIs[0]}, "code": {callback.Query().Get("code")}, "code_verifier": {strings.Repeat("v", 43)}, "resource": {"https://hub.example/mcp"}, "client_secret": {""}, "scope": {""}, "sdk_extension": {"ignored"}}
	w = oauthHTTP(h, "POST", "/token", form.Encode(), "application/x-www-form-urlencoded", nil)
	if w.Code != 200 {
		t.Fatalf("public-client code exchange returned %d", w.Code)
	}
	var tokens hub.OAuthTokens
	if err := json.Unmarshal(w.Body.Bytes(), &tokens); err != nil {
		t.Fatal(err)
	}
	actor, err := s.Authenticate(ctx, tokens.AccessToken)
	if err != nil || actor.PrincipalID() != "alice" {
		t.Fatal("issued token has wrong identity")
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ClientID}, "refresh_token": {tokens.RefreshToken}, "resource": {"https://hub.example/mcp"}, "scope": {""}, "client_secret": {""}}
	w = oauthHTTP(h, "POST", "/token", refresh.Encode(), "application/x-www-form-urlencoded", nil)
	if w.Code != 200 {
		t.Fatalf("refresh returned %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tokens); err != nil {
		t.Fatal(err)
	}
	revoke := url.Values{"client_id": {c.ClientID}, "token": {tokens.RefreshToken}, "client_secret": {""}, "sdk_extension": {"ignored"}}
	w = oauthHTTP(h, "POST", "/revoke", revoke.Encode(), "application/x-www-form-urlencoded", nil)
	if w.Code != 200 {
		t.Fatalf("revoke returned %d", w.Code)
	}
	if _, err := s.Authenticate(ctx, tokens.AccessToken); err == nil {
		t.Fatal("revocation did not persist")
	}
}
