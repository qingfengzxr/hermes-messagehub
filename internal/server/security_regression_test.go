package server_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
	"github.com/qingfengzxr/hermes-messagehub/internal/server"
)

func reviewServer(t *testing.T) (*hub.Store, *httptest.Server, string) {
	t.Helper()
	s, err := hub.Open(filepath.Join(t.TempDir(), "hub.db"), hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	for _, id := range []string{"alice", "bob"} {
		if err := s.AddPrincipal(ctx, id, id); err != nil {
			t.Fatal(err)
		}
	}
	_, token, err := s.IssueGrant(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	app, err := server.New(s, server.Options{PublicBaseURL: "https://message.example.test", AllowedHosts: []string{"message.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	return s, ts, token
}

func TestReviewUncleanedPathsObeySecurityBoundary(t *testing.T) {
	_, ts, token := reviewServer(t)
	cases := []struct {
		path, host, origin, auth string
		body                     []byte
		want                     int
	}{
		{"/v1//identity", "evil.example.test", "", "", []byte(`{}`), 403},
		{"/v1//identity", "message.example.test", "https://evil.example.test", "", []byte(`{}`), 403},
		{"/v1//identity", "message.example.test", "", "", []byte(`{}`), 401},
		{"/v1//identity", "message.example.test", "", token, bytes.Repeat([]byte(`x`), 3<<20), 413},
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, c := range cases {
		req, err := http.NewRequest(http.MethodPost, ts.URL+c.path, bytes.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.auth != "" {
			req.Header.Set("Authorization", "Bearer "+c.auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("%s host=%s origin=%q auth=%v: status=%d want=%d", c.path, c.host, c.origin, c.auth != "", resp.StatusCode, c.want)
		}
	}
}

func TestReviewPrivateResponsesAlwaysNoStore(t *testing.T) {
	s, ts, token := reviewServer(t)
	ctx := context.Background()
	actor, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.CreateConversation(ctx, actor, hub.CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: "cache"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, actor, hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "private", Text: "PRIVATE-FIXTURE"}); err != nil {
		t.Fatal(err)
	}
	for _, probe := range []struct{ url, body string }{
		{ts.URL + "/v1/messages/read", `{"conversation_id":"` + conv.ID + `"}`},
		{ts.URL + "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"messages_read","arguments":{"conversation_id":"` + conv.ID + `"}}}`},
	} {
		req, err := http.NewRequest(http.MethodPost, probe.url, bytes.NewReader([]byte(probe.body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "message.example.test"
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || !bytes.Contains(body, []byte("PRIVATE-FIXTURE")) {
			t.Fatal("probe did not reach a successful private response")
		}
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("private response cache-control=%q want no-store", got)
		}
	}
}
