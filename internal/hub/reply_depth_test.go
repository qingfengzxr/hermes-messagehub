package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestConfiguredReplyDepthIsCappedAndBoundToRoute(t *testing.T) {
	s, opts, a, b, _, dm := wakeFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	wake, _ := json.Marshal(opts.GrokWake)
	var route map[string]any
	if err := json.Unmarshal(wake, &route); err != nil {
		t.Fatal(err)
	}
	route["reply_depth_limit"] = 32
	wake, _ = json.Marshal(route)
	if err := json.Unmarshal(wake, opts.GrokWake); err != nil {
		t.Fatal(err)
	}
	s = reopenWake(t, s.path, opts)
	ctx := context.Background()
	send := func(actor Actor, conv, key, parent string) Message {
		t.Helper()
		r, err := s.Send(ctx, actor, SendRequest{ConversationID: conv, IdempotencyKey: key, ReplyTo: parent, Data: json.RawMessage(`{"reply_depth_limit":99,"reply_requested":true}`)})
		if err != nil {
			t.Fatal(err)
		}
		return r.Message
	}
	assertDepth := func(msg Message, depth, limit string) {
		t.Helper()
		body, _ := json.Marshal(msg)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["reply_depth"]) != depth || string(fields["reply_depth_limit"]) != limit {
			t.Fatalf("depth/limit = %s/%s; want %s/%s", fields["reply_depth"], fields["reply_depth_limit"], depth, limit)
		}
	}
	root := send(a, dm.ID, "root", "")
	assertDepth(root, "0", "32")
	prev := root
	for depth := 1; depth <= 33; depth++ {
		actor := b
		if depth%2 == 0 {
			actor = a
		}
		prev = send(actor, dm.ID, fmt.Sprintf("reply-%d", depth), prev.ID)
		assertDepth(prev, fmt.Sprint(min(depth, 32)), "32")
	}
	wakeSQL(t, s, `UPDATE messages SET expires_at=? WHERE id=?`, root.CreatedAt, root.ID)
	assertDepth(send(b, dm.ID, "expired-parent", root.ID), "32", "32")
	other := messageConversation(t, s, a, "other", "alice", "bob")
	assertDepth(send(a, other.ID, "other-root", ""), "", "")
}
