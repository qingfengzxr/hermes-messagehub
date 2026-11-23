package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

func TestLocalOAuthApprovalBindsExplicitIdentity(t *testing.T) {
	s := adminStore(t)
	ctx := context.Background()
	var out bytes.Buffer
	if err := s.InitOAuth(ctx, hub.OAuthOptions{Resource: "https://message.example.test/mcp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPrincipal(ctx, "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	client, err := s.RegisterOAuthClient(ctx, hub.ClientRegistration{RedirectURIs: []string{"https://client.example.test/callback"}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(strings.Repeat("v", 43)))
	pending, cookie, err := s.BeginAuthorization(ctx, hub.AuthorizationRequest{ClientID: client.ClientID, RedirectURI: client.RedirectURIs[0], ResponseType: "code", State: "fixture", CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]), CodeChallengeMethod: "S256", Resource: "https://message.example.test/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, s, []string{"oauth-pending"}, &out); err != nil {
		t.Fatal(err)
	}
	var list []hub.PendingAuthorization
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list) != 1 || list[0].RequestID != pending.RequestID {
		t.Fatal("pending request not listed")
	}
	out.Reset()
	if err := Run(ctx, s, []string{"oauth-approve", "--request", pending.RequestID, "--principal", "alice"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), cookie) {
		t.Fatal("browser credential leaked")
	}
	continued, err := s.ContinueAuthorization(ctx, pending.RequestID, cookie)
	if err != nil || continued.Pending || continued.RedirectURI == "" {
		t.Fatal("approval did not release bound request")
	}
}

func TestLocalConversationClosePreservesHistory(t *testing.T) {
	s := adminStore(t)
	ctx := context.Background()
	var out bytes.Buffer
	for _, id := range []string{"alice", "bob"} {
		if err := s.AddPrincipal(ctx, id, id); err != nil {
			t.Fatal(err)
		}
	}
	_, token, err := s.IssueGrant(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.CreateConversation(ctx, a, hub.CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, s, []string{"conversation-close", "--id", conv.ID}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, a, hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "m", Text: "no"}); err == nil {
		t.Fatal("closed conversation accepted new message")
	}
	if _, err := s.ReadMessages(ctx, a, conv.ID, hub.PageRequest{}); err != nil {
		t.Fatal("closed history unavailable")
	}
}
