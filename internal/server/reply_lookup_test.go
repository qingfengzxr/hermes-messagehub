package server_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

func TestRealStoreReplyLookup(t *testing.T) {
	s, tokens := actualStore(t, hub.Options{})
	ts := actualHTTP(t, s)
	const read = "/v1/messages/read"
	conversation := func(key string) string {
		t.Helper()
		var c hub.Conversation
		actualREST(t, ts, tokens["alice"], "/v1/conversations/create", hub.CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: key}, &c, 200)
		return c.ID
	}
	send := func(conv, sender, key, parent string) string {
		t.Helper()
		var r hub.SendReceipt
		actualREST(t, ts, tokens[sender], "/v1/messages/send", hub.SendRequest{ConversationID: conv, IdempotencyKey: key, Text: key, ReplyTo: parent}, &r, 200)
		return r.Message.ID
	}
	conv, other := conversation("main"), conversation("other")
	parent := send(conv, "alice", "parent", "")
	unanswered := send(conv, "alice", "unanswered", "")
	foreign := send(other, "alice", "foreign", "")
	for i := 0; i < 105; i++ {
		send(conv, "alice", fmt.Sprintf("unrelated-%d", i), "")
	}
	first := send(conv, "bob", "first direct reply", parent)
	nested := send(conv, "alice", "nested reply", first)
	second := send(conv, "alice", "second direct reply", parent)

	t.Run("filters before pagination over REST and MCP", func(t *testing.T) {
		var page hub.MessagePage
		actualREST(t, ts, tokens["bob"], read, map[string]any{"conversation_id": conv, "reply_to": parent, "limit": 1}, &page, 200)
		if len(page.Items) != 1 || page.Items[0].ID != first || page.Items[0].ReplyTo != parent || !page.HasMore || page.NextCursor == "" {
			t.Fatalf("first filtered page = %+v", page)
		}
		bob := actualMCP(t, ts, tokens["bob"])
		var next hub.MessagePage
		actualCall(t, bob, "messages_read", map[string]any{"conversation_id": conv, "reply_to": parent, "cursor": page.NextCursor, "limit": 1}, &next)
		if len(next.Items) != 1 || next.Items[0].ID != second || next.HasMore {
			t.Fatalf("second filtered page = %+v", next)
		}
		var child hub.MessagePage
		actualCall(t, bob, "messages_read", map[string]any{"conversation_id": conv, "reply_to": first}, &child)
		if len(child.Items) != 1 || child.Items[0].ID != nested || child.HasMore {
			t.Fatalf("nested lookup = %+v", child)
		}
		var status hub.SendReceipt
		actualCall(t, bob, "messages_status", map[string]any{"message_id": parent}, &status)
		if len(status.Deliveries) != 1 || status.Deliveries[0].State != "pending" {
			t.Fatalf("read changed delivery status: %+v", status.Deliveries)
		}
		for _, scope := range []struct {
			who string
			in  map[string]any
		}{
			{"bob", map[string]any{"conversation_id": conv, "cursor": page.NextCursor}},
			{"bob", map[string]any{"conversation_id": conv, "reply_to": first, "cursor": page.NextCursor}},
			{"alice", map[string]any{"conversation_id": conv, "reply_to": parent, "cursor": page.NextCursor}},
		} {
			actualREST(t, ts, tokens[scope.who], read, scope.in, nil, http.StatusBadRequest)
		}
	})

	t.Run("empty invalid and private lookups", func(t *testing.T) {
		var empty hub.MessagePage
		actualREST(t, ts, tokens["bob"], read, map[string]any{"conversation_id": conv, "reply_to": unanswered}, &empty, 200)
		if empty.Items == nil || len(empty.Items) != 0 || empty.HasMore || empty.NextCursor != "" {
			t.Fatalf("empty filtered page = %+v", empty)
		}
		for _, id := range []string{foreign, "m_not_found"} {
			actualREST(t, ts, tokens["bob"], read, map[string]any{"conversation_id": conv, "reply_to": id}, nil, http.StatusNotFound)
		}
		actualREST(t, ts, tokens["charlie"], read, map[string]any{"conversation_id": conv, "reply_to": parent}, nil, http.StatusNotFound)
		for _, invalid := range []any{"", strings.Repeat("x", 129), 42, nil} {
			actualREST(t, ts, tokens["bob"], read, map[string]any{"conversation_id": conv, "reply_to": invalid}, nil, http.StatusBadRequest)
		}
	})

	t.Run("old history cursors still work", func(t *testing.T) {
		bob := actualMCP(t, ts, tokens["bob"])
		var page, next hub.MessagePage
		actualCall(t, bob, "messages_read", map[string]any{"conversation_id": conv, "limit": 1}, &page)
		if len(page.Items) != 1 || page.Items[0].ID != parent || !page.HasMore {
			t.Fatalf("first unfiltered page = %+v", page)
		}
		actualCall(t, bob, "messages_read", map[string]any{"conversation_id": conv, "cursor": page.NextCursor, "limit": 1}, &next)
		if len(next.Items) != 1 || next.Items[0].ID != unanswered || !next.HasMore {
			t.Fatalf("second unfiltered page = %+v", next)
		}
		actualREST(t, ts, tokens["bob"], read, map[string]any{"conversation_id": conv, "reply_to": parent, "cursor": page.NextCursor}, nil, http.StatusBadRequest)
		listed, err := bob.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Tools) != 12 {
			t.Fatalf("tool count = %d", len(listed.Tools))
		}
	})
}

func TestRealStoreReplyLookupKeepsExpirySemantics(t *testing.T) {
	var elapsed atomic.Int64
	start := time.Unix(100000, 0)
	s, tokens := actualStore(t, hub.Options{
		Now:           func() time.Time { return start.Add(time.Duration(elapsed.Load()) * time.Second) },
		MessageTTL:    10 * time.Second,
		AttachmentTTL: time.Second,
	})
	ts := actualHTTP(t, s)
	var conv hub.Conversation
	actualREST(t, ts, tokens["alice"], "/v1/conversations/create", hub.CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: "expiry"}, &conv, 200)
	var parent, old, current hub.SendReceipt
	actualREST(t, ts, tokens["alice"], "/v1/messages/send", hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "old parent", Text: "question"}, &parent, 200)
	actualREST(t, ts, tokens["bob"], "/v1/messages/send", hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "old reply", ReplyTo: parent.Message.ID, Text: "expired reply"}, &old, 200)
	elapsed.Store(10)
	actualREST(t, ts, tokens["bob"], "/v1/messages/send", hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "current reply", ReplyTo: parent.Message.ID, Text: "current reply"}, &current, 200)
	var replies, history hub.MessagePage
	actualREST(t, ts, tokens["alice"], "/v1/messages/read", map[string]any{"conversation_id": conv.ID, "reply_to": parent.Message.ID, "limit": 1}, &replies, 200)
	if len(replies.Items) != 1 || replies.Items[0].ID != current.Message.ID || replies.HasMore {
		t.Fatalf("filtered page should contain only the live reply: %+v", replies)
	}
	actualREST(t, ts, tokens["alice"], "/v1/messages/read", map[string]any{"conversation_id": conv.ID}, &history, 200)
	if len(history.Items) != 3 || history.Items[1].ID != old.Message.ID || !history.Items[1].Expired || history.Items[1].ReplyTo != "" {
		t.Fatalf("history should still contain the expired tombstone: %+v", history)
	}
}
