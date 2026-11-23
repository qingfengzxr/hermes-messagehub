package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type reviewHTTP struct {
	store  *hub.Store
	server *httptest.Server
	client *http.Client
}

func reviewServer(t *testing.T, kratos bool, oauth bool) reviewHTTP {
	t.Helper()
	f, _ := fixture(t)
	o := Options{PublicBaseURL: "https://hub.example", MaxBodyBytes: 1024, RequestsPerSecond: 10000, Burst: 10000}
	if oauth {
		if err := f.InitOAuth(context.Background(), hub.OAuthOptions{Resource: "https://hub.example/mcp"}); err != nil {
			t.Fatal(err)
		}
		o.OAuth = f.Store
	}
	var h http.Handler
	var err error
	if kratos {
		h, err = New(f.Store, o)
	} else {
		h, err = newHandler(f.Store, o)
	}
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	client := ts.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return reviewHTTP{f.Store, ts, client}
}

func (f reviewHTTP) request(t *testing.T, method, path, token, contentType, body string) (int, http.Header, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "hub.example"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, data
}

func reviewOAuthTokens(t *testing.T, f reviewHTTP) (string, hub.OAuthTokens) {
	t.Helper()
	status, _, data := f.request(t, "POST", "/register", "", "application/json", `{"redirect_uris":["https://client.example/cb"]}`)
	if status != 201 {
		t.Fatalf("register %d %s", status, data)
	}
	var client hub.OAuthClient
	if err := json.Unmarshal(data, &client); err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {client.ClientID}, "redirect_uri": {"https://client.example/cb"}, "response_type": {"code"}, "state": {"opaque-state"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "resource": {"https://hub.example/mcp"}}
	status, headers, data := f.request(t, "GET", "/authorize?"+q.Encode(), "", "", "")
	if status != 202 {
		t.Fatalf("authorize %d %s", status, data)
	}
	var pending struct {
		RequestID   string `json:"request_id"`
		ContinueURI string `json:"continue_uri"`
	}
	if err := json.Unmarshal(data, &pending); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApproveAuthorization(context.Background(), pending.RequestID, "alice"); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", f.server.URL+pending.ContinueURI, nil)
	r.Host = "hub.example"
	cookies := (&http.Response{Header: headers}).Cookies()
	if len(cookies) != 1 {
		t.Fatal("cookie missing")
	}
	r.AddCookie(cookies[0])
	resp, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("continue %d", resp.StatusCode)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ClientID}, "code": {u.Query().Get("code")}, "code_verifier": {verifier}, "redirect_uri": {"https://client.example/cb"}, "resource": {"https://hub.example/mcp"}}
	status, _, data = f.request(t, "POST", "/token", "", "application/x-www-form-urlencoded", form.Encode())
	if status != 200 {
		t.Fatalf("exchange %d %s", status, data)
	}
	var tokens hub.OAuthTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		t.Fatal(err)
	}
	return client.ClientID, tokens
}

func reviewRefresh(t *testing.T, f reviewHTTP, client, refresh string) (int, hub.OAuthTokens) {
	t.Helper()
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {refresh}, "resource": {"https://hub.example/mcp"}}
	status, _, data := f.request(t, "POST", "/token", "", "application/x-www-form-urlencoded", form.Encode())
	var tokens hub.OAuthTokens
	if err := json.Unmarshal(data, &tokens); err != nil {
		t.Fatal(err)
	}
	return status, tokens
}

func TestReviewRefreshReplayMustStopDescendants(t *testing.T) {
	f := reviewServer(t, true, true)
	client, legitimate := reviewOAuthTokens(t, f)
	first, attacker := reviewRefresh(t, f, client, legitimate.RefreshToken)
	if first != 200 {
		t.Fatalf("initial refresh %d", first)
	}
	replay, _ := reviewRefresh(t, f, client, legitimate.RefreshToken)
	if replay != 400 {
		t.Fatalf("reuse unexpectedly status %d", replay)
	}
	access, _, _ := f.request(t, "POST", "/v1/identity", attacker.AccessToken, "application/json", `{}`)
	next, _ := reviewRefresh(t, f, client, attacker.RefreshToken)
	t.Logf("attacker_first_refresh=%d legitimate_reuse=%d attacker_access_after_reuse=%d attacker_descendant_refresh=%d", first, replay, access, next)
	if next == 200 {
		t.Error("replayed refresh was rejected, but the stolen-token descendant still mints credentials")
	}
}

func TestReviewRotatedRefreshCanStillRevokeGrant(t *testing.T) {
	f := reviewServer(t, true, true)
	client, original := reviewOAuthTokens(t, f)
	status, next := reviewRefresh(t, f, client, original.RefreshToken)
	if status != 200 {
		t.Fatalf("initial refresh %d", status)
	}
	form := url.Values{"client_id": {client}, "token": {original.RefreshToken}}
	revoke, _, _ := f.request(t, "POST", "/revoke", "", "application/x-www-form-urlencoded", form.Encode())
	access, _, _ := f.request(t, "POST", "/v1/identity", next.AccessToken, "application/json", `{}`)
	renewed, _ := reviewRefresh(t, f, client, next.RefreshToken)
	t.Logf("revoke_rotated_refresh=%d descendant_access=%d descendant_refresh=%d", revoke, access, renewed)
	if revoke != 200 || access != 401 || renewed != 400 {
		t.Error("revoking the original refresh returned success without revoking its grant")
	}
}

func TestReviewCanonicalRedirectBoundary(t *testing.T) {
	for _, kratos := range []bool{false, true} {
		t.Run(fmt.Sprintf("kratos=%v", kratos), func(t *testing.T) {
			f := reviewServer(t, kratos, false)
			for _, tc := range []struct {
				name, host, origin, body string
				want                     int
			}{
				{"host", "evil.example", "", "", 403},
				{"origin", "hub.example", "https://evil.example", "", 403},
				{"anonymous", "hub.example", "", "", 401},
				{"body_limit", "hub.example", "", strings.Repeat("x", 1025), 413},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r, _ := http.NewRequest("POST", f.server.URL+"/v1//identity", strings.NewReader(tc.body))
					r.Host = tc.host
					if tc.origin != "" {
						r.Header.Set("Origin", tc.origin)
					}
					resp, err := f.client.Do(r)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					t.Logf("status=%d location=%q cache_control=%q referrer_policy=%q", resp.StatusCode, resp.Header.Get("Location"), resp.Header.Get("Cache-Control"), resp.Header.Get("Referrer-Policy"))
					if resp.StatusCode != tc.want || resp.Header.Get("Cache-Control") != "no-store" {
						t.Errorf("boundary bypass: want status=%d and no-store", tc.want)
					}
				})
			}
		})
	}
}

func TestReviewMCPRetainsNoStoreForPrivateMessage(t *testing.T) {
	f := reviewServer(t, true, false)
	ctx := context.Background()
	if err := f.store.AddPrincipal(ctx, "bob", "Bob"); err != nil {
		t.Fatal(err)
	}
	_, token, err := f.store.IssueGrant(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := f.store.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := f.store.CreateConversation(ctx, actor, hub.CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: "cache-private"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Send(ctx, actor, hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "cache-secret", Text: "PRIVATE-CONTENT"}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"messages_read","arguments":{"conversation_id":%q}}}`, conv.ID)
	status, headers, data := f.request(t, "POST", "/mcp", token, "application/json", body)
	if status != 200 || !strings.Contains(string(data), "PRIVATE-CONTENT") {
		t.Fatalf("setup failed: %d %s", status, data)
	}
	restBody := fmt.Sprintf(`{"conversation_id":%q}`, conv.ID)
	restStatus, restHeaders, _ := f.request(t, "POST", "/v1/messages/read", token, "application/json", restBody)
	t.Logf("REST=%d cache_control=%q MCP=%d cache_control=%q private_content=true", restStatus, restHeaders.Get("Cache-Control"), status, headers.Get("Cache-Control"))
	if !strings.Contains(headers.Get("Cache-Control"), "no-store") {
		t.Error("SDK overwrote the outer no-store header on the private message response")
	}
}

func TestReviewRealStoreRESTAndMCPIsolation(t *testing.T) {
	f := reviewServer(t, true, false)
	ctx := context.Background()
	tokens := map[string]string{}
	actors := map[string]hub.Actor{}
	for _, name := range []string{"alice", "bob", "eve"} {
		if name != "alice" {
			if err := f.store.AddPrincipal(ctx, name, name); err != nil {
				t.Fatal(err)
			}
		}
		_, token, err := f.store.IssueGrant(ctx, name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tokens[name] = token
		actors[name], err = f.store.Authenticate(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
	}
	conv, err := f.store.CreateConversation(ctx, actors["alice"], hub.CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: "private"})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := f.store.Send(ctx, actors["alice"], hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "secret", Text: "PRIVATE-CONTENT"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		body any
	}{
		{"/v1/messages/read", map[string]any{"conversation_id": conv.ID}},
		{"/v1/messages/status", map[string]any{"message_id": receipt.Message.ID}},
		{"/v1/messages/send", map[string]any{"conversation_id": conv.ID, "idempotency_key": "eve-write", "text": "x"}},
		{"/v1/inbox/ack", map[string]any{"message_ids": []string{receipt.Message.ID}}},
	} {
		body, _ := json.Marshal(tc.body)
		for _, token := range []string{"", tokens["eve"]} {
			status, _, data := f.request(t, "POST", tc.path, token, "application/json", string(body))
			want := 404
			if token == "" {
				want = 401
			}
			if status != want || strings.Contains(string(data), "PRIVATE-CONTENT") {
				t.Fatalf("REST %s: %d %s", tc.path, status, data)
			}
		}
	}
	session := sdkSession(t, f.server, tokens["eve"])
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "messages_read", Arguments: map[string]any{"conversation_id": conv.ID}})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("MCP nonmember err=%v result=%+v", err, result)
	}
	if err := f.store.RevokeGrant(ctx, actors["eve"].GrantID()); err != nil {
		t.Fatal(err)
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "identity", Arguments: map[string]any{}})
	if err == nil && result != nil && !result.IsError {
		t.Fatal("revoked MCP session succeeded")
	}
	t.Log("anonymous and nonmember REST denied; real SDK MCP nonmember and revoked session denied")
}
