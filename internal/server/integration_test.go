package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
	"github.com/qingfengzxr/hermes-messagehub/internal/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var _ server.Backend = (*hub.Store)(nil)

type actualAuthTransport struct{ token string }

func (a actualAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+a.token)
	r.Host = "message.example.test"
	return http.DefaultTransport.RoundTrip(r)
}

func actualStore(t *testing.T, opts hub.Options) (*hub.Store, map[string]string) {
	t.Helper()
	s, err := hub.Open(filepath.Join(t.TempDir(), "hub.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tokens := map[string]string{}
	for _, id := range []string{"alice", "bob", "charlie"} {
		if err := s.AddPrincipal(context.Background(), id, id); err != nil {
			t.Fatal(err)
		}
		_, token, err := s.IssueGrant(context.Background(), id, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tokens[id] = token
	}
	return s, tokens
}
func actualHTTP(t *testing.T, s *hub.Store) *httptest.Server {
	t.Helper()
	app, err := server.New(s, server.Options{PublicBaseURL: "https://message.example.test", AllowedHosts: []string{"message.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	return ts
}
func actualREST(t *testing.T, ts *httptest.Server, token, path string, in, out any, status int) {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "message.example.test"
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		t.Fatalf("%s: status %d, want %d", path, resp.StatusCode, status)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
}
func actualMCP(t *testing.T, ts *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "actual-integration", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: actualAuthTransport{token: token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
func actualCall(t *testing.T, s *mcp.ClientSession, name string, in, out any) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: in})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("MCP %s rejected fixture", name)
	}
	if len(res.Content) != 1 {
		t.Fatal("unexpected result shape")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatal("missing JSON result")
	}
	if out != nil {
		if err := json.Unmarshal([]byte(text.Text), out); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRealStoreRESTAndMCPConversationFileAndRevocation(t *testing.T) {
	s, tokens := actualStore(t, hub.Options{})
	ts := actualHTTP(t, s)
	a := actualMCP(t, ts, tokens["alice"])
	b := actualMCP(t, ts, tokens["bob"])
	listed, err := a.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 12 {
		t.Fatalf("tool count %d", len(listed.Tools))
	}
	var identity hub.Identity
	actualCall(t, a, "identity", map[string]any{}, &identity)
	if identity.PrincipalID != "alice" {
		t.Fatal("wrong MCP identity")
	}
	var conv hub.Conversation
	actualREST(t, ts, tokens["alice"], "/v1/conversations/create", hub.CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: "conversation"}, &conv, 200)
	var page hub.ConversationPage
	actualCall(t, b, "conversations_list", map[string]any{}, &page)
	if len(page.Items) != 1 || page.Items[0].ID != conv.ID {
		t.Fatal("shared conversation missing")
	}
	var sent hub.SendReceipt
	request := hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "hello", Text: "你好", Data: json.RawMessage(`{"exact":9007199254740993}`)}
	actualCall(t, a, "messages_send", request, &sent)
	var retry hub.SendReceipt
	actualREST(t, ts, tokens["alice"], "/v1/messages/send", request, &retry, 200)
	if retry.Message.ID != sent.Message.ID {
		t.Fatal("duplicate logical message")
	}
	var inbox hub.InboxPage
	actualCall(t, b, "inbox_pull", map[string]any{}, &inbox)
	if len(inbox.Items) != 1 || !bytes.Contains(inbox.Items[0].Message.Data, []byte("9007199254740993")) {
		t.Fatal("inbox or JSON precision lost")
	}
	var ack hub.AckReceipt
	actualREST(t, ts, tokens["bob"], "/v1/inbox/ack", map[string]any{"message_ids": []string{sent.Message.ID}}, &ack, 200)
	var status hub.SendReceipt
	actualCall(t, a, "messages_status", map[string]any{"message_id": sent.Message.ID}, &status)
	if len(status.Deliveries) != 1 || status.Deliveries[0].State != "acked" {
		t.Fatal("ACK missing")
	}
	var reply hub.SendReceipt
	actualCall(t, b, "messages_send", hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "reply", ReplyTo: sent.Message.ID, Text: "收到"}, &reply)
	var history hub.MessagePage
	actualCall(t, a, "messages_read", map[string]any{"conversation_id": conv.ID}, &history)
	if len(history.Items) != 2 || history.Items[1].ReplyTo != sent.Message.ID {
		t.Fatal("thread continuity lost")
	}
	raw := append([]byte{0, 1, 255, 2}, []byte("原始文件内容")...)
	sum := sha256.Sum256(raw)
	var file hub.Attachment
	actualCall(t, a, "attachments_begin", hub.BeginAttachmentRequest{ConversationID: conv.ID, IdempotencyKey: "file", Filename: "原文件.bin", MediaType: "application/octet-stream", SizeBytes: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])}, &file)
	for _, chunk := range []struct {
		offset int
		data   []byte
	}{{0, raw[:4]}, {4, raw[4:]}} {
		actualREST(t, ts, tokens["alice"], "/v1/attachments/write", map[string]any{"attachment_id": file.ID, "offset": chunk.offset, "content_base64": base64.StdEncoding.EncodeToString(chunk.data)}, nil, 200)
	}
	actualCall(t, a, "attachments_commit", map[string]any{"attachment_id": file.ID}, &file)
	var fileMessage hub.SendReceipt
	actualCall(t, a, "messages_send", hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "file-message", AttachmentIDs: []string{file.ID}}, &fileMessage)
	var chunk hub.AttachmentChunk
	actualCall(t, b, "attachments_read", map[string]any{"message_id": fileMessage.Message.ID, "attachment_id": file.ID, "offset": 0}, &chunk)
	got, err := base64.StdEncoding.DecodeString(chunk.ContentBase64)
	if err != nil || !bytes.Equal(got, raw) || !chunk.EOF {
		t.Fatal("original bytes not delivered")
	}
	for _, probe := range []struct {
		path string
		body any
	}{
		{"/v1/messages/read", map[string]any{"conversation_id": conv.ID}},
		{"/v1/messages/status", map[string]any{"message_id": sent.Message.ID}},
		{"/v1/attachments/read", map[string]any{"message_id": fileMessage.Message.ID, "attachment_id": file.ID, "offset": 0}},
		{"/v1/inbox/ack", map[string]any{"message_ids": []string{sent.Message.ID}}},
	} {
		actualREST(t, ts, tokens["charlie"], probe.path, probe.body, nil, 404)
		actualREST(t, ts, "", probe.path, probe.body, nil, 401)
	}
	actualREST(t, ts, tokens["alice"], "/v1/messages/send", map[string]any{"conversation_id": conv.ID, "idempotency_key": "forged", "text": "x", "sender": "bob"}, nil, 400)
	actor, err := s.Authenticate(context.Background(), tokens["bob"])
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeGrant(context.Background(), actor.GrantID()); err != nil {
		t.Fatal(err)
	}
	actualREST(t, ts, tokens["bob"], "/v1/inbox/pull", map[string]any{}, nil, 401)
	if result, err := b.CallTool(context.Background(), &mcp.CallToolParams{Name: "identity", Arguments: map[string]any{}}); err == nil && (result == nil || !result.IsError) {
		t.Fatal("revoked MCP connection retained access")
	}
}

func TestExpiredDeliveryIsNotReportedAsStillPending(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s, tokens := actualStore(t, hub.Options{Now: func() time.Time { return now }, MessageTTL: time.Second, AttachmentTTL: time.Second})
	ts := actualHTTP(t, s)
	var conv hub.Conversation
	var sent hub.SendReceipt
	actualREST(t, ts, tokens["alice"], "/v1/conversations/create", hub.CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: "expire-conversation"}, &conv, 200)
	actualREST(t, ts, tokens["alice"], "/v1/messages/send", hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "expire-message", Text: "secret"}, &sent, 200)
	now = now.Add(2 * time.Second)
	var inbox hub.InboxPage
	actualREST(t, ts, tokens["bob"], "/v1/inbox/pull", map[string]any{}, &inbox, 200)
	if len(inbox.Items) != 1 || !inbox.Items[0].Message.Expired || inbox.Items[0].Message.Text != "" {
		t.Fatal("expired message not tombstoned")
	}
	if inbox.Items[0].Delivery.State != "expired" {
		t.Fatalf("expired delivery state=%s; want expired", inbox.Items[0].Delivery.State)
	}
	var status hub.SendReceipt
	actualREST(t, ts, tokens["alice"], "/v1/messages/status", map[string]any{"message_id": sent.Message.ID}, &status, 200)
	if status.Deliveries[0].State != "expired" {
		t.Fatal("sender sees expired delivery as live")
	}
}
