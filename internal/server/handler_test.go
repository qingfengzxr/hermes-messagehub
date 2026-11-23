package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeBackend struct {
	*hub.Store
	mu      sync.Mutex
	called  []string
	block   chan struct{}
	entered chan struct{}
}

var _ interface {
	CloseConversation(context.Context, string) error
} = (Backend)(nil)

func (f *fakeBackend) CloseConversation(context.Context, string) error {
	panic("admin method must never be dispatched")
}

func (f *fakeBackend) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = append(f.called, name)
}
func (f *fakeBackend) Identity(ctx context.Context, a hub.Actor) (hub.Identity, error) {
	f.record("identity")
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return hub.Identity{}, ctx.Err()
		}
	}
	return hub.Identity{PrincipalID: a.PrincipalID(), Peers: []hub.Principal{}}, nil
}
func (f *fakeBackend) CreateConversation(ctx context.Context, a hub.Actor, r hub.CreateConversationRequest) (hub.Conversation, error) {
	f.record("conversations_create")
	return hub.Conversation{ID: "c", Members: r.Members}, nil
}
func (f *fakeBackend) ListConversations(ctx context.Context, a hub.Actor, r hub.PageRequest) (hub.ConversationPage, error) {
	f.record("conversations_list")
	return hub.ConversationPage{Items: []hub.Conversation{}}, nil
}
func (f *fakeBackend) Send(ctx context.Context, a hub.Actor, r hub.SendRequest) (hub.SendReceipt, error) {
	f.record("messages_send")
	if r.ConversationID != "c" {
		return hub.SendReceipt{}, hub.ErrNotFound
	}
	return hub.SendReceipt{Stored: true, Message: hub.Message{ID: "m", Sender: a.PrincipalID(), Text: r.Text}}, nil
}
func (f *fakeBackend) ReadMessages(ctx context.Context, a hub.Actor, c string, r hub.PageRequest) (hub.MessagePage, error) {
	f.record("messages_read")
	if c != "c" {
		return hub.MessagePage{}, hub.ErrNotFound
	}
	return hub.MessagePage{Items: []hub.Message{}}, nil
}
func (f *fakeBackend) ReadReplies(ctx context.Context, a hub.Actor, c, _ string, r hub.PageRequest) (hub.MessagePage, error) {
	return f.ReadMessages(ctx, a, c, r)
}
func (f *fakeBackend) MessageStatus(ctx context.Context, a hub.Actor, id string) (hub.SendReceipt, error) {
	f.record("messages_status")
	return hub.SendReceipt{Message: hub.Message{ID: id}}, nil
}
func (f *fakeBackend) PullInbox(ctx context.Context, a hub.Actor, r hub.PageRequest) (hub.InboxPage, error) {
	f.record("inbox_pull")
	return hub.InboxPage{Items: []hub.InboxItem{}}, nil
}
func (f *fakeBackend) Ack(ctx context.Context, a hub.Actor, ids []string) (hub.AckReceipt, error) {
	f.record("inbox_ack")
	return hub.AckReceipt{MessageIDs: ids, Acknowledged: true}, nil
}
func (f *fakeBackend) BeginAttachment(ctx context.Context, a hub.Actor, r hub.BeginAttachmentRequest) (hub.Attachment, error) {
	f.record("attachments_begin")
	return hub.Attachment{ID: "f", Filename: r.Filename, SizeBytes: r.SizeBytes}, nil
}
func (f *fakeBackend) WriteAttachment(ctx context.Context, a hub.Actor, id string, offset int64, data []byte) (hub.Attachment, error) {
	f.record("attachments_write")
	return hub.Attachment{ID: id, ReceivedBytes: offset + int64(len(data))}, nil
}
func (f *fakeBackend) CommitAttachment(ctx context.Context, a hub.Actor, id string) (hub.Attachment, error) {
	f.record("attachments_commit")
	return hub.Attachment{ID: id, State: "ready"}, nil
}
func (f *fakeBackend) ReadAttachment(ctx context.Context, a hub.Actor, message, id string, offset int64) (hub.AttachmentChunk, error) {
	f.record("attachments_read")
	return hub.AttachmentChunk{Attachment: hub.Attachment{ID: id}, Offset: offset, ContentBase64: "eA=="}, nil
}
func fixture(t *testing.T) (*fakeBackend, string) {
	t.Helper()
	s, err := hub.Open(filepath.Join(t.TempDir(), "hub.db"), hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	if err := s.AddPrincipal(ctx, "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.IssueGrant(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeBackend{Store: s}, token
}

var protocolTestFactory = newHandler

func testHTTP(t *testing.T, f *fakeBackend, o Options) *httptest.Server {
	t.Helper()
	o.PublicBaseURL = "https://hub.example"
	o.AllowedHosts = []string{"hub.example"}
	if o.RequestsPerSecond == 0 {
		o.RequestsPerSecond = 10000
	}
	if o.Burst == 0 {
		o.Burst = 10000
	}
	srv, err := protocolTestFactory(f, o)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}
func request(t *testing.T, ts *httptest.Server, path, token, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "hub.example"
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

var protocolCases = []struct{ name, path, body string }{
	{"identity", "/v1/identity", `{}`},
	{"conversations_create", "/v1/conversations/create", `{"members":["alice","bob"],"idempotency_key":"k"}`},
	{"conversations_list", "/v1/conversations/list", `{"limit":20}`},
	{"messages_send", "/v1/messages/send", `{"conversation_id":"c","idempotency_key":"k","text":"hello"}`},
	{"messages_read", "/v1/messages/read", `{"conversation_id":"c","limit":20}`},
	{"messages_status", "/v1/messages/status", `{"message_id":"m"}`},
	{"inbox_pull", "/v1/inbox/pull", `{"limit":20}`},
	{"inbox_ack", "/v1/inbox/ack", `{"message_ids":["m"]}`},
	{"attachments_begin", "/v1/attachments/begin", `{"conversation_id":"c","idempotency_key":"f","filename":"x.txt","media_type":"text/plain","size_bytes":1,"sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}`},
	{"attachments_write", "/v1/attachments/write", `{"attachment_id":"f","offset":0,"content_base64":"eA=="}`},
	{"attachments_commit", "/v1/attachments/commit", `{"attachment_id":"f"}`},
	{"attachments_read", "/v1/attachments/read", `{"message_id":"m","attachment_id":"f","offset":0}`},
}

func TestRESTAllCommunicationOperations(t *testing.T) {
	f, token := fixture(t)
	ts := testHTTP(t, f, Options{})
	for _, tc := range protocolCases {
		t.Run(tc.name, func(t *testing.T) {
			resp := request(t, ts, tc.path, token, tc.body, nil)
			data, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 200 {
				t.Fatalf("status=%d %s", resp.StatusCode, data)
			}
			if tc.name == "messages_send" && !bytes.Contains(data, []byte(`"sender":"alice"`)) {
				t.Fatal("identity not preserved")
			}
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.called) != 12 {
		t.Fatal("not all backend calls exercised")
	}
}
func TestRESTBoundaryRejectsBeforeDispatch(t *testing.T) {
	f, token := fixture(t)
	ts := testHTTP(t, f, Options{MaxBodyBytes: 1024})
	cases := []struct {
		path, token, body string
		headers           map[string]string
		status            int
	}{
		{"/v1/identity", "", `{}`, nil, 401},
		{"/v1/messages/send", token, `{"conversation_id":"c","idempotency_key":"k","text":"hello","sender":"bob"}`, nil, 400},
		{"/v1/identity", token, `{}`, map[string]string{"Host": "evil.example"}, 403},
		{"/v1/identity", token, `{}`, map[string]string{"Origin": "https://evil.example"}, 403},
		{"/v1/identity", token, strings.Repeat(" ", 1025), nil, 413},
		{"/v1/identity", token, `{} {}`, nil, 400},
		{"/v1/identity", token, `null`, nil, 400},
		{"/v1/attachments/read", token, `{"message_id":"m","attachment_id":"f","offset":true}`, nil, 400},
		{"/v1/attachments/read", token, `{"message_id":"m","attachment_id":"f","offset":0.5}`, nil, 400},
		{"/v1/attachments/read", token, `{"message_id":"m","attachment_id":"f","offset":-1}`, nil, 400},
		{"/v1/attachments/write", token, `{"attachment_id":"f","offset":0,"content_base64":"!"}`, nil, 400},
		{"/v1/conversations/list", token, `{"limit":101}`, nil, 400},
		{"/v1/conversations/list", token, `{"limit":-1}`, nil, 400},
		{"/v1/identity", token, `{}`, map[string]string{"Content-Type": "text/plain"}, 415},
		{"/v1/admin/approve", token, `{}`, nil, 404},
	}
	for i, tc := range cases {
		resp := request(t, ts, tc.path, tc.token, tc.body, tc.headers)
		if resp.StatusCode != tc.status {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("case %d got %d want %d %s", i, resp.StatusCode, tc.status, data)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.called) != 0 {
		t.Fatal("invalid input reached backend", f.called)
	}
}
func TestRESTPrivateDenialAndRevocation(t *testing.T) {
	f, token := fixture(t)
	ts := testHTTP(t, f, Options{})
	resp := request(t, ts, "/v1/messages/read", token, `{"conversation_id":"foreign"}`, nil)
	if resp.StatusCode != 404 {
		t.Fatal("private denial not preserved")
	}
	actor, err := f.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.RevokeGrant(context.Background(), actor.GrantID()); err != nil {
		t.Fatal(err)
	}
	resp = request(t, ts, "/v1/identity", token, `{}`, nil)
	if resp.StatusCode != 401 {
		t.Fatal("revoked REST token accepted")
	}
}

type authTransport struct {
	base  http.RoundTripper
	token string
}

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = "hub.example"
	r.Header.Set("Authorization", "Bearer "+a.token)
	return a.base.RoundTrip(r)
}
func sdkSession(t *testing.T, ts *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "protocol-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: authTransport{http.DefaultTransport, token}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal("real SDK initialize", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}
func TestMCPRealSDKInitializeListCallAndRevocation(t *testing.T) {
	f, token := fixture(t)
	ts := testHTTP(t, f, Options{})
	session := sdkSession(t, ts, token)
	ctx := context.Background()
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	expected := []string{}
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		if tool.InputSchema == nil {
			t.Fatal("no input schema")
		}
	}
	for _, tc := range protocolCases {
		expected = append(expected, tc.name)
	}
	sort.Strings(names)
	sort.Strings(expected)
	if !reflect.DeepEqual(names, expected) {
		t.Fatal("tool surface differs", names)
	}
	for _, tc := range protocolCases {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: json.RawMessage(tc.body)})
		if err != nil || result.IsError || result.StructuredContent == nil {
			t.Fatalf("%s failed: %v %+v", tc.name, err, result)
		}
	}
	for _, tc := range []struct{ name, body string }{
		{"messages_send", `{"conversation_id":"c","idempotency_key":"x","text":"x","sender":"bob"}`},
		{"attachments_read", `{"message_id":"m","attachment_id":"f","offset":true}`},
		{"attachments_read", `{"message_id":"m","attachment_id":"f","offset":0.5}`},
		{"messages_read", `{"conversation_id":"foreign"}`},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: json.RawMessage(tc.body)})
		if err == nil && !result.IsError {
			t.Fatal("invalid/forbidden MCP call succeeded", tc)
		}
	}
	actor, err := f.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.RevokeGrant(ctx, actor.GrantID()); err != nil {
		t.Fatal(err)
	}
	if _, err = session.ListTools(ctx, nil); err == nil {
		t.Fatal("revoked token can list")
	}
	if result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "identity", Arguments: map[string]any{}}); err == nil && !result.IsError {
		t.Fatal("revoked token can call")
	}
}
func TestMCPInitializeAndListNeedAuthentication(t *testing.T) {
	f, _ := fixture(t)
	ts := testHTTP(t, f, Options{})
	for _, method := range []string{"initialize", "tools/list", "tools/call"} {
		resp := request(t, ts, "/mcp", "", `{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{}}`, map[string]string{"Accept": "application/json, text/event-stream"})
		if resp.StatusCode != 401 {
			t.Fatalf("%s anonymous status %d", method, resp.StatusCode)
		}
	}
}
func TestOAuthMountedAndMCPUsesOAuthGrant(t *testing.T) {
	f, _ := fixture(t)
	ctx := context.Background()
	if err := f.InitOAuth(ctx, hub.OAuthOptions{Resource: "https://hub.example/mcp"}); err != nil {
		t.Fatal(err)
	}
	ts := testHTTP(t, f, Options{OAuth: f.Store})
	resp := request(t, ts, "/register", "", `{"redirect_uris":["https://client.example/cb"]}`, nil)
	if resp.StatusCode != 201 {
		t.Fatal("DCR not public", resp.StatusCode)
	}
	var c hub.OAuthClient
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	resp = request(t, ts, "/mcp", "", `{}`, nil)
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "oauth-protected-resource") {
		t.Fatal("missing OAuth challenge")
	}
	verifier := strings.Repeat("v", 43)
	sum := sha256.Sum256([]byte(verifier))
	p, cookie, err := f.BeginAuthorization(ctx, hub.AuthorizationRequest{ClientID: c.ClientID, RedirectURI: "https://client.example/cb", ResponseType: "code", State: "state", CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]), CodeChallengeMethod: "S256", Resource: "https://hub.example/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.ApproveAuthorization(ctx, p.RequestID, "alice"); err != nil {
		t.Fatal(err)
	}
	released, err := f.ContinueAuthorization(ctx, p.RequestID, cookie)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(released.RedirectURI)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := f.ExchangeOAuthToken(ctx, hub.TokenRequest{GrantType: "authorization_code", ClientID: c.ClientID, Code: u.Query().Get("code"), CodeVerifier: verifier, RedirectURI: "https://client.example/cb", Resource: "https://hub.example/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	session := sdkSession(t, ts, tokens.AccessToken)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "identity", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatal("OAuth access failed", err)
	}
	if err = f.RevokeOAuthToken(ctx, c.ClientID, tokens.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err = session.ListTools(ctx, nil); err == nil {
		t.Fatal("revoked OAuth token can list MCP tools")
	}
}
func TestHandlerValidatesOAuthConfiguration(t *testing.T) {
	f, _ := fixture(t)
	if _, err := newHandler(f, Options{OAuth: f.Store, PublicBaseURL: "https://hub.example"}); err == nil {
		t.Fatal("uninitialized OAuth store accepted")
	}
	if err := f.InitOAuth(context.Background(), hub.OAuthOptions{Resource: "https://other.example/mcp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := newHandler(f, Options{OAuth: f.Store, PublicBaseURL: "https://hub.example"}); err == nil {
		t.Fatal("wrong resource accepted")
	}
	for _, o := range []Options{{PublicBaseURL: "http://hub.example"}, {AllowedHosts: []string{"*"}}, {MaxConcurrent: -1}, {RequestTimeout: -time.Second}, {AllowedOrigins: []string{"https://client.example/"}}} {
		if _, err := newHandler(f, o); err == nil {
			t.Fatal("invalid options accepted", o)
		}
	}
}
func TestMCPDeadlineDoesNotCancelBeforeResponse(t *testing.T) {
	f, token := fixture(t)
	f.block = make(chan struct{})
	ts := testHTTP(t, f, Options{RequestTimeout: 50 * time.Millisecond})
	session := sdkSession(t, ts, token)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "identity", Arguments: map[string]any{}})
	if err == nil && !result.IsError {
		t.Fatal("blocked MCP operation succeeded")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("server operation deadline ignored")
	}
}

func TestRequestRateConcurrencyAndTimeout(t *testing.T) {
	t.Run("rate", func(t *testing.T) {
		f, token := fixture(t)
		ts := testHTTP(t, f, Options{RequestsPerSecond: 0.001, Burst: 1})
		if request(t, ts, "/v1/identity", token, `{}`, nil).StatusCode != 200 {
			t.Fatal("first request failed")
		}
		if request(t, ts, "/v1/identity", token, `{}`, nil).StatusCode != 429 {
			t.Fatal("rate cap ignored")
		}
	})
	t.Run("concurrency", func(t *testing.T) {
		f, token := fixture(t)
		f.block = make(chan struct{})
		f.entered = make(chan struct{}, 1)
		ts := testHTTP(t, f, Options{MaxConcurrent: 1})
		done := make(chan struct{})
		go func() {
			defer close(done)
			req, _ := http.NewRequest("POST", ts.URL+"/v1/identity", strings.NewReader(`{}`))
			req.Host = "hub.example"
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := ts.Client().Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
		select {
		case <-f.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("handler never entered")
		}
		status := request(t, ts, "/v1/identity", token, `{}`, nil).StatusCode
		close(f.block)
		<-done
		if status != 429 {
			t.Fatal("concurrency cap ignored", status)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f, token := fixture(t)
		f.block = make(chan struct{})
		ts := testHTTP(t, f, Options{RequestTimeout: 30 * time.Millisecond})
		resp := request(t, ts, "/v1/identity", token, `{}`, nil)
		if resp.StatusCode != 504 {
			t.Fatal("deadline not propagated", resp.StatusCode)
		}
	})
}
