package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func messageActors(t *testing.T, s *Store) (Actor, Actor, Actor) {
	t.Helper()
	a, _ := provision(t, s, "alice")
	b, _ := provision(t, s, "bob")
	c, _ := provision(t, s, "carol")
	return a, b, c
}

func messageConversation(t *testing.T, s *Store, a Actor, key string, members ...string) Conversation {
	t.Helper()
	c, err := s.CreateConversation(context.Background(), a, CreateConversationRequest{Members: members, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func messageSend(t *testing.T, s *Store, a Actor, conv, key, text string) SendReceipt {
	t.Helper()
	r, err := s.Send(context.Background(), a, SendRequest{ConversationID: conv, IdempotencyKey: key, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func messageAttachmentFixture(t *testing.T, s *Store, conv, owner, id, state string, expires, size int64) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO attachments(id,conversation_id,uploader,idem_key,digest,filename,media_type,size_bytes,sha256,state,received_bytes,created_at,expires_at)
		VALUES(?,?,?,?,?,'fixture.txt','text/plain',?,?,?,0,?,?)`, id, conv, owner, id, "fixture-digest", size, strings.Repeat("0", 64), state, s.now(), expires)
	if err != nil {
		t.Fatal(err)
	}
}

func messageReplyFixture(t *testing.T, s *Store, conv, sender, id string) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO messages(id,conversation_id,seq,sender,idem_key,digest,text,data,created_at,expires_at)
		VALUES(?,?,1,?,?,?,'fixture','',?,?)`, id, conv, sender, id, "fixture-digest", s.now(), s.now()+3600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE conversations SET next_seq=2 WHERE id=?", conv); err != nil {
		t.Fatal(err)
	}
}

func TestMessageIdentityListsOnlyActivePeersAndBounds(t *testing.T) {
	s := testStore(t)
	a, _ := provision(t, s, "alice")
	ctx := context.Background()
	id, err := s.Identity(ctx, a)
	if err != nil || id.PrincipalID != "alice" || id.Peers == nil || len(id.Peers) != 0 {
		t.Fatalf("empty identity = %+v, %v", id, err)
	}
	b, _ := provision(t, s, "bob")
	_, _ = provision(t, s, "carol")
	if err := s.SetPrincipalActive(ctx, "carol", false); err != nil {
		t.Fatal(err)
	}
	id, err = s.Identity(ctx, a)
	if err != nil || !reflect.DeepEqual(id.Peers, []Principal{{ID: "bob", Name: "bob", Active: true}}) {
		t.Fatalf("identity = %+v, %v", id, err)
	}
	if id.MaxMessageBytes != s.opts.MaxMessageBytes || id.MaxFileBytes != s.opts.MaxFileBytes || id.MaxChunkBytes != s.opts.MaxChunkBytes {
		t.Fatal("identity omitted configured bounds")
	}
	if err := s.RevokeGrant(ctx, b.GrantID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Identity(ctx, b); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("cached revoked identity = %v", err)
	}
}

func TestMessageConversationsArePrivateFixedAndIdempotent(t *testing.T) {
	s := testStore(t)
	a, b, c := messageActors(t, s)
	ctx := context.Background()
	conv := messageConversation(t, s, a, "private", "bob", "alice")
	if conv.ID == "" || conv.CreatedAt == 0 || conv.Closed || !reflect.DeepEqual(conv.Members, []string{"alice", "bob"}) {
		t.Fatalf("conversation = %+v", conv)
	}
	retry := messageConversation(t, s, a, "private", "alice", "bob")
	if !reflect.DeepEqual(conv, retry) {
		t.Fatalf("canonical retry changed conversation: %+v", retry)
	}
	if _, err := s.CreateConversation(ctx, a, CreateConversationRequest{Members: []string{"alice", "carol"}, IdempotencyKey: "private"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry = %v", err)
	}
	otherCreator := messageConversation(t, s, b, "private", "alice", "bob")
	if otherCreator.ID == conv.ID {
		t.Fatal("idempotency not creator scoped")
	}
	for _, actor := range []Actor{a, b} {
		page, err := s.ListConversations(ctx, actor, PageRequest{})
		if err != nil || page.Items == nil || len(page.Items) != 2 {
			t.Fatalf("member list = %+v, %v", page, err)
		}
	}
	page, err := s.ListConversations(ctx, c, PageRequest{})
	if err != nil || page.Items == nil || len(page.Items) != 0 || page.HasMore {
		t.Fatalf("outsider list = %+v, %v", page, err)
	}
	if err := s.CloseConversation(ctx, conv.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseConversation(ctx, conv.ID); err != nil {
		t.Fatalf("close not idempotent: %v", err)
	}
	page, err = s.ListConversations(ctx, b, PageRequest{})
	if err != nil || !page.Items[0].Closed {
		t.Fatalf("closed history hidden: %+v, %v", page, err)
	}
	if err := s.CloseConversation(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing close = %v", err)
	}
}

func TestMessageConversationValidation(t *testing.T) {
	s := testStore(t)
	a, _, _ := messageActors(t, s)
	ctx := context.Background()
	cases := []struct {
		name string
		req  CreateConversationRequest
		want error
	}{
		{"missing creator", CreateConversationRequest{[]string{"bob", "carol"}, "key"}, ErrInvalid},
		{"one member", CreateConversationRequest{[]string{"alice"}, "key"}, ErrInvalid},
		{"duplicates", CreateConversationRequest{[]string{"alice", "alice"}, "key"}, ErrInvalid},
		{"unknown", CreateConversationRequest{[]string{"alice", "missing"}, "key"}, ErrNotFound},
		{"empty key", CreateConversationRequest{[]string{"alice", "bob"}, ""}, ErrInvalid},
		{"long key", CreateConversationRequest{[]string{"alice", "bob"}, strings.Repeat("x", 129)}, ErrInvalid},
		{"control key", CreateConversationRequest{[]string{"alice", "bob"}, "bad\nkey"}, ErrInvalid},
		{"invalid utf8", CreateConversationRequest{[]string{"alice", "bob"}, string([]byte{255})}, ErrInvalid},
		{"invalid member", CreateConversationRequest{[]string{"alice", "bob\n"}, "key"}, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CreateConversation(ctx, a, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	s.opts.MaxMembers = 2
	if _, err := s.CreateConversation(ctx, a, CreateConversationRequest{[]string{"alice", "bob", "carol"}, "three"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("member limit = %v", err)
	}
	if err := s.SetPrincipalActive(ctx, "bob", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConversation(ctx, a, CreateConversationRequest{[]string{"alice", "bob"}, "disabled"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled member = %v", err)
	}
	if err := s.RevokeGrant(ctx, a.GrantID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConversation(ctx, a, CreateConversationRequest{[]string{"alice", "carol"}, "revoked"}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("cached create = %v", err)
	}
	if _, err := s.ListConversations(ctx, a, PageRequest{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("cached list = %v", err)
	}
}

func TestMessageConversationPagingUsesBoundCursor(t *testing.T) {
	s := testStore(t)
	a, b, _ := messageActors(t, s)
	ctx := context.Background()
	first := messageConversation(t, s, a, "one", "alice", "bob")
	second := messageConversation(t, s, a, "two", "alice", "bob")
	page, err := s.ListConversations(ctx, a, PageRequest{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != first.ID || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	next, err := s.ListConversations(ctx, a, PageRequest{Cursor: page.NextCursor, Limit: 1})
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != second.ID || next.HasMore {
		t.Fatalf("second page = %+v, %v", next, err)
	}
	for _, req := range []PageRequest{{Limit: -1}, {Limit: 101}, {Cursor: page.NextCursor + "x"}} {
		if _, err := s.ListConversations(ctx, a, req); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid page accepted: %+v, %v", req, err)
		}
	}
	if _, err := s.ListConversations(ctx, b, PageRequest{Cursor: page.NextCursor}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-actor cursor accepted: %v", err)
	}
}

func TestMessageSendReadAndExplicitAck(t *testing.T) {
	s := testStore(t)
	a, b, c := messageActors(t, s)
	ctx := context.Background()
	conv := messageConversation(t, s, a, "private", "alice", "bob")
	empty, err := s.ReadMessages(ctx, b, conv.ID, PageRequest{})
	if err != nil || empty.Items == nil || len(empty.Items) != 0 {
		t.Fatalf("empty history = %+v, %v", empty, err)
	}
	for _, actor := range []Actor{a, b, c} {
		inbox, err := s.PullInbox(ctx, actor, PageRequest{})
		if err != nil || inbox.Items == nil || len(inbox.Items) != 0 {
			t.Fatalf("empty inbox = %+v, %v", inbox, err)
		}
	}
	req := SendRequest{ConversationID: conv.ID, IdempotencyKey: "send", Text: "你好", Data: json.RawMessage(`{"task":"draft","n":1}`)}
	sent, err := s.Send(ctx, a, req)
	if err != nil || !sent.Stored || sent.Message.ID == "" || sent.Message.Sequence != 1 || sent.Message.Sender != "alice" || sent.Message.Text != req.Text || sent.Message.AttachmentIDs == nil || sent.Message.Expired {
		t.Fatalf("send = %+v, %v", sent, err)
	}
	if len(sent.Deliveries) != 1 || sent.Deliveries[0].Recipient != "bob" || sent.Deliveries[0].Sequence != 1 || sent.Deliveries[0].State != "pending" || sent.Deliveries[0].AckedAt != 0 {
		t.Fatalf("delivery = %+v", sent.Deliveries)
	}
	for i := 0; i < 2; i++ {
		inbox, err := s.PullInbox(ctx, b, PageRequest{})
		if err != nil || len(inbox.Items) != 1 || inbox.Items[0].Message.ID != sent.Message.ID || inbox.Items[0].Delivery.State != "pending" {
			t.Fatalf("read implicitly ACKed: %+v, %v", inbox, err)
		}
		history, err := s.ReadMessages(ctx, b, conv.ID, PageRequest{})
		if err != nil || len(history.Items) != 1 || history.Items[0].Text != req.Text {
			t.Fatalf("history = %+v, %v", history, err)
		}
		status, err := s.MessageStatus(ctx, a, sent.Message.ID)
		if err != nil || status.Deliveries[0].State != "pending" {
			t.Fatalf("read mutated delivery: %+v, %v", status, err)
		}
	}
	for _, actor := range []Actor{a, c} {
		inbox, err := s.PullInbox(ctx, actor, PageRequest{})
		if err != nil || len(inbox.Items) != 0 {
			t.Fatalf("wrong mailbox = %+v, %v", inbox, err)
		}
	}
	for i := 0; i < 2; i++ {
		ack, err := s.Ack(ctx, b, []string{sent.Message.ID, sent.Message.ID})
		if err != nil || !ack.Acknowledged || !reflect.DeepEqual(ack.MessageIDs, []string{sent.Message.ID}) {
			t.Fatalf("ACK = %+v, %v", ack, err)
		}
	}
	inbox, err := s.PullInbox(ctx, b, PageRequest{})
	if err != nil || inbox.Items == nil || len(inbox.Items) != 0 {
		t.Fatalf("ACK did not clear pending = %+v, %v", inbox, err)
	}
	status, err := s.MessageStatus(ctx, a, sent.Message.ID)
	if err != nil || status.Deliveries[0].State != "acked" || status.Deliveries[0].AckedAt == 0 {
		t.Fatalf("ACK status = %+v, %v", status, err)
	}
	retry, err := s.Send(ctx, a, req)
	if err != nil || retry.Message.ID != sent.Message.ID || retry.Deliveries[0].State != "acked" {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	req.Text = "different"
	if _, err := s.Send(ctx, a, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry = %v", err)
	}
	other := messageSend(t, s, a, conv.ID, "different-key", "你好")
	if other.Message.ID == sent.Message.ID || other.Message.Sequence != 2 || other.Deliveries[0].Sequence != 2 {
		t.Fatalf("distinct send = %+v", other)
	}
}

func TestMessageMembershipAndStatusIsolation(t *testing.T) {
	s := testStore(t)
	a, b, c := messageActors(t, s)
	ctx := context.Background()
	ab := messageConversation(t, s, a, "ab", "alice", "bob")
	private := messageSend(t, s, a, ab.ID, "private", "only AB")
	if _, err := s.ReadMessages(ctx, c, ab.ID, PageRequest{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider history = %v", err)
	}
	if _, err := s.Send(ctx, c, SendRequest{ConversationID: ab.ID, IdempotencyKey: "intruder", Text: "bad"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider send = %v", err)
	}
	if _, err := s.MessageStatus(ctx, c, private.Message.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider status = %v", err)
	}
	for _, actor := range []Actor{a, c} {
		if _, err := s.Ack(ctx, actor, []string{private.Message.ID}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign ACK = %v", err)
		}
	}
	group := messageConversation(t, s, a, "group", "alice", "bob", "carol")
	sent := messageSend(t, s, a, group.ID, "fanout", "group")
	if len(sent.Deliveries) != 2 || sent.Deliveries[0].Recipient != "bob" || sent.Deliveries[1].Recipient != "carol" {
		t.Fatalf("fanout = %+v", sent)
	}
	if _, err := s.Ack(ctx, b, []string{sent.Message.ID}); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []Actor{b, c} {
		status, err := s.MessageStatus(ctx, actor, sent.Message.ID)
		if err != nil || len(status.Deliveries) != 1 || status.Deliveries[0].Recipient != actor.PrincipalID() {
			t.Fatalf("member status leaked peers = %+v, %v", status, err)
		}
	}
	status, err := s.MessageStatus(ctx, a, sent.Message.ID)
	if err != nil || len(status.Deliveries) != 2 || status.Deliveries[0].State != "acked" || status.Deliveries[1].State != "pending" {
		t.Fatalf("sender receipts = %+v, %v", status, err)
	}
	if err := s.CloseConversation(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, a, SendRequest{ConversationID: group.ID, IdempotencyKey: "after-close", Text: "no"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("closed send = %v", err)
	}
	if page, err := s.ReadMessages(ctx, c, group.ID, PageRequest{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("closed history = %+v, %v", page, err)
	}
	if _, err := s.Ack(ctx, c, []string{sent.Message.ID}); err != nil {
		t.Fatalf("closed ACK = %v", err)
	}
}

func TestMessageAckBatchRollsBackOnOneForbiddenID(t *testing.T) {
	s := testStore(t)
	a, b, _ := messageActors(t, s)
	ctx := context.Background()
	ab := messageConversation(t, s, a, "ab", "alice", "bob")
	ac := messageConversation(t, s, a, "ac", "alice", "carol")
	valid := messageSend(t, s, a, ab.ID, "valid", "valid")
	foreign := messageSend(t, s, a, ac.ID, "foreign", "foreign")
	for _, ids := range [][]string{{valid.Message.ID, foreign.Message.ID}, {valid.Message.ID, "missing"}} {
		if _, err := s.Ack(ctx, b, ids); !errors.Is(err, ErrNotFound) {
			t.Fatalf("batch = %v", err)
		}
		status, err := s.MessageStatus(ctx, a, valid.Message.ID)
		if err != nil || status.Deliveries[0].State != "pending" {
			t.Fatalf("partial ACK persisted = %+v, %v", status, err)
		}
	}
	if _, err := s.Ack(ctx, b, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty ACK = %v", err)
	}
}

func TestMessagePayloadValidationAndIdempotencyScope(t *testing.T) {
	s := testStore(t)
	a, b, _ := messageActors(t, s)
	ctx := context.Background()
	conv := messageConversation(t, s, a, "ab", "alice", "bob")
	valid := SendRequest{ConversationID: conv.ID, IdempotencyKey: "valid", Text: "ok"}
	cases := []struct {
		name string
		edit func(*SendRequest)
	}{
		{"empty", func(r *SendRequest) { r.Text = "" }},
		{"bad text UTF8", func(r *SendRequest) { r.Text = string([]byte{255}) }},
		{"empty key", func(r *SendRequest) { r.IdempotencyKey = "" }},
		{"long key", func(r *SendRequest) { r.IdempotencyKey = strings.Repeat("界", 43) }},
		{"control key", func(r *SendRequest) { r.IdempotencyKey = "a\x00" }},
		{"array", func(r *SendRequest) { r.Data = json.RawMessage(`[1]`) }},
		{"null", func(r *SendRequest) { r.Data = json.RawMessage(`null`) }},
		{"string", func(r *SendRequest) { r.Data = json.RawMessage(`"x"`) }},
		{"number", func(r *SendRequest) { r.Data = json.RawMessage(`1`) }},
		{"boolean", func(r *SendRequest) { r.Data = json.RawMessage(`true`) }},
		{"broken json", func(r *SendRequest) { r.Data = json.RawMessage(`{`) }},
		{"trailing json", func(r *SendRequest) { r.Data = json.RawMessage(`{} {}`) }},
		{"bad data UTF8", func(r *SendRequest) { r.Data = append([]byte(`{"x":"`), 255, '"', '}') }},
		{"duplicate files", func(r *SendRequest) { r.AttachmentIDs = []string{"file", "file"} }},
		{"too many files", func(r *SendRequest) {
			for i := 0; i < 33; i++ {
				r.AttachmentIDs = append(r.AttachmentIDs, fmt.Sprintf("file-%d", i))
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid
			tc.edit(&r)
			if _, err := s.Send(ctx, a, r); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want invalid", err)
			}
		})
	}
	s.opts.MaxMessageBytes = 5
	if _, err := s.Send(ctx, a, SendRequest{ConversationID: conv.ID, IdempotencyKey: "bytes", Text: "你好"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("UTF8 byte limit = %v", err)
	}
	if _, err := s.Send(ctx, a, SendRequest{ConversationID: conv.ID, IdempotencyKey: "combined", Text: "four", Data: json.RawMessage(`{}`)}); !errors.Is(err, ErrQuota) {
		t.Fatalf("combined data size = %v", err)
	}
	s.opts.MaxMessageBytes = 1024
	one := messageSend(t, s, a, conv.ID, "scoped", "same")
	two := messageSend(t, s, b, conv.ID, "scoped", "same")
	otherConv := messageConversation(t, s, a, "another", "alice", "bob")
	three := messageSend(t, s, a, otherConv.ID, "scoped", "same")
	if one.Message.ID == two.Message.ID || one.Message.ID == three.Message.ID || two.Message.ID == three.Message.ID {
		t.Fatal("send idempotency crossed principal or conversation")
	}
	r, err := s.Send(ctx, a, SendRequest{ConversationID: conv.ID, IdempotencyKey: "only-data", Data: json.RawMessage(`{"large":9007199254740993}`)})
	if err != nil || string(r.Message.Data) != `{"large":9007199254740993}` {
		t.Fatalf("data-only or precise JSON number corrupted = %+v, %v", r, err)
	}
}

func TestMessageReplyAndAttachmentsMustBelongToSenderAndConversation(t *testing.T) {
	s := testStore(t)
	a, _, _ := messageActors(t, s)
	ctx := context.Background()
	ab := messageConversation(t, s, a, "ab", "alice", "bob")
	ac := messageConversation(t, s, a, "ac", "alice", "carol")
	messageReplyFixture(t, s, ab.ID, "bob", "reply-ab")
	messageReplyFixture(t, s, ac.ID, "carol", "reply-ac")
	for _, reply := range []string{"missing", "reply-ac"} {
		if _, err := s.Send(ctx, a, SendRequest{ConversationID: ab.ID, IdempotencyKey: reply, Text: "reply", ReplyTo: reply}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign reply %q = %v", reply, err)
		}
	}
	for _, f := range []struct {
		id, conv, owner, state string
		expiry                 int64
	}{
		{"f-a", ab.ID, "alice", "ready", s.now() + 3600},
		{"f-z", ab.ID, "alice", "ready", s.now() + 3600},
		{"f-owner", ab.ID, "bob", "ready", s.now() + 3600},
		{"f-conv", ac.ID, "alice", "ready", s.now() + 3600},
		{"f-upload", ab.ID, "alice", "uploading", s.now() + 3600},
		{"f-expired", ab.ID, "alice", "ready", s.now()},
	} {
		messageAttachmentFixture(t, s, f.conv, f.owner, f.id, f.state, f.expiry, 4)
	}
	for _, tc := range []struct {
		id   string
		want error
	}{
		{"f-owner", ErrNotFound}, {"f-conv", ErrNotFound}, {"missing", ErrNotFound}, {"f-upload", ErrConflict}, {"f-expired", ErrExpired},
	} {
		req := SendRequest{ConversationID: ab.ID, IdempotencyKey: tc.id, Text: "files", AttachmentIDs: []string{"f-a", tc.id}}
		if _, err := s.Send(ctx, a, req); !errors.Is(err, tc.want) {
			t.Fatalf("file %q = %v, want %v", tc.id, err, tc.want)
		}
	}
	req := SendRequest{ConversationID: ab.ID, IdempotencyKey: "files", ReplyTo: "reply-ab", AttachmentIDs: []string{"f-z", "f-a"}}
	sent, err := s.Send(ctx, a, req)
	if err != nil || sent.Message.Sequence != 2 || sent.Message.ReplyTo != "reply-ab" || !reflect.DeepEqual(sent.Message.AttachmentIDs, []string{"f-a", "f-z"}) {
		t.Fatalf("file-only send = %+v, %v", sent, err)
	}
	req.AttachmentIDs = []string{"f-a", "f-z"}
	retry, err := s.Send(ctx, a, req)
	if err != nil || retry.Message.ID != sent.Message.ID {
		t.Fatalf("already-attached canonical retry = %+v, %v", retry, err)
	}
	req.IdempotencyKey = "reuse"
	if _, err := s.Send(ctx, a, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused attachment = %v", err)
	}
	var links, bindings int
	if err := s.db.QueryRow("SELECT count(*) FROM message_attachments WHERE message_id=?", sent.Message.ID).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM attachments WHERE message_id=?", sent.Message.ID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if links != 2 || bindings != 2 {
		t.Fatalf("attachment binding not atomic: %d, %d", links, bindings)
	}
}

func TestMessagePagingScopesAndMailboxSequences(t *testing.T) {
	s := testStore(t)
	a, b, c := messageActors(t, s)
	ctx := context.Background()
	ab := messageConversation(t, s, a, "ab", "alice", "bob")
	bc := messageConversation(t, s, b, "bc", "bob", "carol")
	first := messageSend(t, s, a, ab.ID, "one", "first")
	second := messageSend(t, s, c, bc.ID, "two", "second")
	third := messageSend(t, s, a, ab.ID, "three", "third")
	page, err := s.ReadMessages(ctx, b, ab.ID, PageRequest{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != first.Message.ID || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("history first = %+v, %v", page, err)
	}
	next, err := s.ReadMessages(ctx, b, ab.ID, PageRequest{Cursor: page.NextCursor, Limit: 1})
	if err != nil || len(next.Items) != 1 || next.Items[0].ID != third.Message.ID || next.HasMore || next.Items[0].Sequence != 2 {
		t.Fatalf("history next = %+v, %v", next, err)
	}
	inbox, err := s.PullInbox(ctx, b, PageRequest{Limit: 2})
	if err != nil || len(inbox.Items) != 2 || inbox.Items[0].Message.ID != first.Message.ID || inbox.Items[1].Message.ID != second.Message.ID || !inbox.HasMore {
		t.Fatalf("mailbox first = %+v, %v", inbox, err)
	}
	nextInbox, err := s.PullInbox(ctx, b, PageRequest{Cursor: inbox.NextCursor, Limit: 2})
	if err != nil || len(nextInbox.Items) != 1 || nextInbox.Items[0].Message.ID != third.Message.ID || nextInbox.Items[0].Delivery.Sequence != 3 || nextInbox.HasMore {
		t.Fatalf("mailbox next = %+v, %v", nextInbox, err)
	}
	if _, err := s.ReadMessages(ctx, a, ab.ID, PageRequest{Cursor: page.NextCursor}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("actor cursor = %v", err)
	}
	if _, err := s.ReadMessages(ctx, b, bc.ID, PageRequest{Cursor: page.NextCursor}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("conversation cursor = %v", err)
	}
	if _, err := s.PullInbox(ctx, b, PageRequest{Cursor: page.NextCursor}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("operation cursor = %v", err)
	}
	if _, err := s.PullInbox(ctx, a, PageRequest{Cursor: inbox.NextCursor}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mailbox actor cursor = %v", err)
	}
	for _, req := range []PageRequest{{Limit: -1}, {Limit: 101}, {Cursor: "tampered"}} {
		if _, err := s.ReadMessages(ctx, a, ab.ID, req); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad history page = %v", err)
		}
		if _, err := s.PullInbox(ctx, a, req); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad inbox page = %v", err)
		}
	}
	for i := 0; i < 21; i++ {
		messageSend(t, s, a, ab.ID, fmt.Sprintf("page-%d", i), "page")
	}
	history, err := s.ReadMessages(ctx, a, ab.ID, PageRequest{})
	if err != nil || len(history.Items) != 20 || !history.HasMore {
		t.Fatalf("default history page = %+v, %v", history, err)
	}
	inbox, err = s.PullInbox(ctx, b, PageRequest{})
	if err != nil || len(inbox.Items) != 20 || !inbox.HasMore {
		t.Fatalf("default inbox page = %+v, %v", inbox, err)
	}
}

func TestMessageEmptyOptionalDataMatchesOmittedData(t *testing.T) {
	s := testStore(t)
	a, _, _ := messageActors(t, s)
	conv := messageConversation(t, s, a, "empty-data", "alice", "bob")
	req := SendRequest{ConversationID: conv.ID, IdempotencyKey: "empty-data", Text: "text", Data: json.RawMessage{}}
	sent, err := s.Send(context.Background(), a, req)
	if err != nil || !sent.Stored {
		t.Fatalf("empty optional data rejected: %+v, %v", sent, err)
	}
	req.Data = nil
	retry, err := s.Send(context.Background(), a, req)
	if err != nil || retry.Message.ID != sent.Message.ID {
		t.Fatalf("omitted data changed payload: %+v, %v", retry, err)
	}
}

func TestMessageEveryOperationRechecksCachedActor(t *testing.T) {
	s := testStore(t)
	a, b, _ := messageActors(t, s)
	ctx := context.Background()
	conv := messageConversation(t, s, a, "ab", "alice", "bob")
	sent := messageSend(t, s, a, conv.ID, "one", "hello")
	if err := s.RevokeGrant(ctx, b.GrantID()); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		run  func() error
	}{
		{"send", func() error {
			_, err := s.Send(ctx, b, SendRequest{ConversationID: conv.ID, IdempotencyKey: "bad", Text: "bad"})
			return err
		}},
		{"read", func() error { _, err := s.ReadMessages(ctx, b, conv.ID, PageRequest{}); return err }},
		{"status", func() error { _, err := s.MessageStatus(ctx, b, sent.Message.ID); return err }},
		{"inbox", func() error { _, err := s.PullInbox(ctx, b, PageRequest{}); return err }},
		{"ack", func() error { _, err := s.Ack(ctx, b, []string{sent.Message.ID}); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("cached actor = %v", err)
			}
		})
	}
}
