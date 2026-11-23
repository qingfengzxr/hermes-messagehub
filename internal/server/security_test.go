package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMCPHostOriginChunkedCapAndRevocation(t *testing.T) {
	f, token := fixture(t)
	ts := testHTTP(t, f, Options{MaxBodyBytes: 1024})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	for _, headers := range []map[string]string{{"Host": "evil.example", "X-Forwarded-Host": "hub.example"}, {"Origin": "null"}, {"Origin": "https://evil.example"}} {
		headers["Accept"] = "application/json, text/event-stream"
		resp := request(t, ts, "/mcp", token, body, headers)
		if resp.StatusCode != 403 {
			t.Fatal("MCP boundary bypass", resp.StatusCode)
		}
	}
	for _, path := range []string{"/mcp", "/v1/identity"} {
		req, err := http.NewRequest("POST", ts.URL+path, io.NopCloser(strings.NewReader(strings.Repeat("x", 1025))))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "hub.example"
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 413 {
			t.Fatal("chunked body bypassed cap", resp.StatusCode)
		}
	}
	actor, err := f.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.RevokeGrant(context.Background(), actor.GrantID()); err != nil {
		t.Fatal(err)
	}
	resp := request(t, ts, "/mcp", token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, map[string]string{"Accept": "application/json, text/event-stream"})
	if resp.StatusCode != 401 {
		t.Fatal("revoked credential initialized MCP")
	}
}
func TestTrustedOriginAndHealthSurface(t *testing.T) {
	f, token := fixture(t)
	ts := testHTTP(t, f, Options{AllowedOrigins: []string{"https://trusted.example"}})
	resp := request(t, ts, "/v1/identity", token, `{}`, map[string]string{"Origin": "https://trusted.example"})
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "https://trusted.example" {
		t.Fatal("trusted origin rejected")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/healthz", nil)
	req.Host = "hub.example"
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "{\"status\":\"ok\"}\n" {
		t.Fatal("health leaked data", string(body))
	}
	for _, path := range []string{"/approve", "/v1/conversations/close", "/v1/principals", "/v1/tools/run", "/debug/pprof/"} {
		if request(t, ts, path, token, `{}`, nil).StatusCode != 404 {
			t.Fatal("unexpected public surface", path)
		}
	}
}
