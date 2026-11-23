package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func messageStoreOptions(t *testing.T, opts Options) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func messageAssertTombstone(t *testing.T, got Message, original Message) {
	t.Helper()
	if got.ID != original.ID || got.ConversationID != original.ConversationID || got.Sequence != original.Sequence || !got.Expired || got.Text != "" || len(got.Data) != 0 || got.ReplyTo != "" || got.AttachmentIDs == nil || len(got.AttachmentIDs) != 0 {
		t.Fatalf("not a body-free tombstone: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"text", "data", "reply_to", "attachment_ids"} {
		if _, exists := fields[key]; exists {
			t.Fatalf("tombstone leaked %s: %s", key, encoded)
		}
	}
}

func TestMessageRecoveryPreservesCursorsReceiptsAndExpiredTombstones(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Int64
	clock.Store(1800000000)
	opts := Options{MessageTTL: 2 * time.Second, AttachmentTTL: time.Second, Now: func() time.Time { return time.Unix(clock.Load(), 0) }}
	s := messageStoreOptions(t, opts)
	a, tokenA := provision(t, s, "alice")
	b, tokenB := provision(t, s, "bob")
	c, _ := provision(t, s, "carol")
	conv := messageConversation(t, s, a, "group", "alice", "bob", "carol")
	other := messageConversation(t, s, a, "other", "alice", "bob")
	conversations, err := s.ListConversations(ctx, a, PageRequest{Limit: 1})
	if err != nil || !conversations.HasMore {
		t.Fatalf("conversation cursor = %+v, %v", conversations, err)
	}
	messageReplyFixture(t, s, conv.ID, "bob", "reply-fixture")
	messageAttachmentFixture(t, s, conv.ID, "alice", "file-fixture", "ready", s.now()+1, 3)
	req := SendRequest{ConversationID: conv.ID, IdempotencyKey: "recover", Text: "private text", Data: json.RawMessage(`{"private":"data"}`), ReplyTo: "reply-fixture", AttachmentIDs: []string{"file-fixture"}}
	first, err := s.Send(ctx, a, req)
	if err != nil {
		t.Fatal(err)
	}
	second := messageSend(t, s, a, conv.ID, "pending", "pending private text")
	history, err := s.ReadMessages(ctx, b, conv.ID, PageRequest{Limit: 2})
	if err != nil || !history.HasMore || history.Items[1].ID != first.Message.ID {
		t.Fatalf("history cursor = %+v, %v", history, err)
	}
	inbox, err := s.PullInbox(ctx, b, PageRequest{Limit: 1})
	if err != nil || !inbox.HasMore || inbox.Items[0].Message.ID != first.Message.ID {
		t.Fatalf("inbox cursor = %+v, %v", inbox, err)
	}
	if _, err := s.Ack(ctx, b, []string{first.Message.ID}); err != nil {
		t.Fatal(err)
	}
	status, err := s.MessageStatus(ctx, a, first.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	ackedAt := status.Deliveries[0].AckedAt
	if err := s.CloseConversation(ctx, conv.ID); err != nil {
		t.Fatal(err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err = s.Authenticate(ctx, tokenA)
	if err != nil {
		t.Fatal(err)
	}
	b, err = s.Authenticate(ctx, tokenB)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := s.ListConversations(ctx, a, PageRequest{Cursor: conversations.NextCursor})
	if err != nil || len(listed.Items) != 1 || listed.Items[0].ID != other.ID {
		t.Fatalf("restart conversation cursor = %+v, %v", listed, err)
	}
	retry, err := s.Send(ctx, a, req)
	if err != nil || retry.Message.ID != first.Message.ID || retry.Deliveries[0].AckedAt != ackedAt {
		t.Fatalf("closed retry after restart = %+v, %v", retry, err)
	}
	clock.Add(2)
	retry, err = s.Send(ctx, a, req)
	if err != nil || !retry.Stored {
		t.Fatalf("expired retry = %+v, %v", retry, err)
	}
	messageAssertTombstone(t, retry.Message, first.Message)
	read, err := s.ReadMessages(ctx, b, conv.ID, PageRequest{Cursor: history.NextCursor})
	if err != nil || len(read.Items) != 1 {
		t.Fatalf("restart history = %+v, %v", read, err)
	}
	messageAssertTombstone(t, read.Items[0], second.Message)
	pending, err := s.PullInbox(ctx, b, PageRequest{Cursor: inbox.NextCursor})
	if err != nil || len(pending.Items) != 1 || pending.Items[0].Delivery.State != "expired" {
		t.Fatalf("expired delivery lost = %+v, %v", pending, err)
	}
	messageAssertTombstone(t, pending.Items[0].Message, second.Message)
	status, err = s.MessageStatus(ctx, b, first.Message.ID)
	if err != nil || len(status.Deliveries) != 1 || status.Deliveries[0].AckedAt != ackedAt {
		t.Fatalf("expired ACK state = %+v, %v", status, err)
	}
	messageAssertTombstone(t, status.Message, first.Message)
	if _, err := s.Ack(ctx, b, []string{first.Message.ID, second.Message.ID}); err != nil {
		t.Fatal(err)
	}
	status, err = s.MessageStatus(ctx, b, first.Message.ID)
	if err != nil || status.Deliveries[0].AckedAt != ackedAt {
		t.Fatalf("repeat ACK changed timestamp: %+v, %v", status, err)
	}
	pending, err = s.PullInbox(ctx, b, PageRequest{})
	if err != nil || pending.Items == nil || len(pending.Items) != 0 {
		t.Fatalf("expired ACK pending = %+v, %v", pending, err)
	}
	peer, err := s.PullInbox(ctx, c, PageRequest{})
	if err != nil || len(peer.Items) != 2 {
		t.Fatalf("other recipient lost delivery = %+v, %v", peer, err)
	}
	messageAssertTombstone(t, peer.Items[0].Message, first.Message)
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM messages WHERE conversation_id=?", conv.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("tombstones deleted or retry duplicated: %d, %v", count, err)
	}
}

func TestMessageStorageQuotaIncludesRawUTF8DataAndAllAttachments(t *testing.T) {
	for _, state := range []string{"ready", "uploading", "expired"} {
		t.Run(state, func(t *testing.T) {
			s := messageStoreOptions(t, Options{MaxStoreBytes: 12, MaxFileBytes: 12})
			a, _, _ := messageActors(t, s)
			conv := messageConversation(t, s, a, "quota", "alice", "bob")
			expiry := s.now() + 3600
			if state == "expired" {
				expiry = s.now() - 1
			}
			messageAttachmentFixture(t, s, conv.ID, "alice", "reserved", state, expiry, 6)
			req := SendRequest{ConversationID: conv.ID, IdempotencyKey: "fits", Text: "界", Data: json.RawMessage(`{ }`)}
			first, err := s.Send(context.Background(), a, req)
			if err != nil {
				t.Fatalf("exact byte capacity rejected: %v", err)
			}
			if _, err := s.Send(context.Background(), a, SendRequest{ConversationID: conv.ID, IdempotencyKey: "overflow", Text: "x"}); !errors.Is(err, ErrQuota) {
				t.Fatalf("state %s reservation not counted: %v", state, err)
			}
			retry, err := s.Send(context.Background(), a, req)
			if err != nil || retry.Message.ID != first.Message.ID {
				t.Fatalf("full store retry = %+v, %v", retry, err)
			}
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM messages").Scan(&count); err != nil || count != 1 {
				t.Fatalf("quota failure wrote rows: %d, %v", count, err)
			}
		})
	}
}

func TestMessageQuotaCountsExpiredMessagesAndDoesNotDoubleCountBoundFiles(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Int64
	clock.Store(1800000000)
	s := messageStoreOptions(t, Options{MaxStoreBytes: 10, MaxFileBytes: 10, MaxMessages: 2, MessageTTL: time.Second, AttachmentTTL: time.Second, Now: func() time.Time { return time.Unix(clock.Load(), 0) }})
	a, _, _ := messageActors(t, s)
	conv := messageConversation(t, s, a, "quota", "alice", "bob")
	messageAttachmentFixture(t, s, conv.ID, "alice", "reserved", "ready", s.now()+1, 8)
	req := SendRequest{ConversationID: conv.ID, IdempotencyKey: "file", Text: "x", AttachmentIDs: []string{"reserved"}}
	first, err := s.Send(ctx, a, req)
	if err != nil {
		t.Fatalf("declared file counted twice: %v", err)
	}
	messageSend(t, s, a, conv.ID, "second", "y")
	clock.Add(1)
	if _, err := s.Send(ctx, a, SendRequest{ConversationID: conv.ID, IdempotencyKey: "overflow", Text: "z"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("expired bytes not counted: %v", err)
	}
	s.opts.MaxStoreBytes = 100
	if _, err := s.Send(ctx, a, SendRequest{ConversationID: conv.ID, IdempotencyKey: "count", Text: "z"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("expired message count not enforced: %v", err)
	}
	retry, err := s.Send(ctx, a, req)
	if err != nil || retry.Message.ID != first.Message.ID {
		t.Fatalf("quota tombstone retry = %+v, %v", retry, err)
	}
	messageAssertTombstone(t, retry.Message, first.Message)
}

func TestMessageCountQuotaRetainsExpiredTombstones(t *testing.T) {
	s := messageStoreOptions(t, Options{MaxMessages: 1})
	a, _, _ := messageActors(t, s)
	conv := messageConversation(t, s, a, "count", "alice", "bob")
	first := messageSend(t, s, a, conv.ID, "one", "one")
	if _, err := s.db.Exec("UPDATE messages SET expires_at=? WHERE id=?", s.now(), first.Message.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), a, SendRequest{ConversationID: conv.ID, IdempotencyKey: "two", Text: "two"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("expired count not reserved: %v", err)
	}
}

func TestMessageConcurrentRetryAcrossStoresHasOneLogicalSend(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a, b, _ := messageActors(t, s)
	other, err := Open(s.path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	stores := []*Store{s, other}
	const workers = 24
	var wg sync.WaitGroup
	convs := make([]Conversation, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			convs[i], errs[i] = stores[i%2].CreateConversation(ctx, a, CreateConversationRequest{Members: []string{"bob", "alice"}, IdempotencyKey: "concurrent"})
		}(i)
	}
	wg.Wait()
	for i := range convs {
		if errs[i] != nil || convs[i].ID != convs[0].ID {
			t.Fatalf("concurrent conversation %d = %+v, %v", i, convs[i], errs[i])
		}
	}
	messageAttachmentFixture(t, s, convs[0].ID, "alice", "concurrent-file", "ready", s.now()+3600, 4)
	req := SendRequest{ConversationID: convs[0].ID, IdempotencyKey: "retry", Text: "same", AttachmentIDs: []string{"concurrent-file"}}
	receipts := make([]SendReceipt, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); receipts[i], errs[i] = stores[i%2].Send(ctx, a, req) }(i)
	}
	wg.Wait()
	for i := range receipts {
		if errs[i] != nil || receipts[i].Message.ID != receipts[0].Message.ID || receipts[i].Message.Sequence != 1 || len(receipts[i].Deliveries) != 1 || receipts[i].Deliveries[0].Sequence != 1 {
			t.Fatalf("concurrent send %d = %+v, %v", i, receipts[i], errs[i])
		}
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := SendRequest{ConversationID: convs[0].ID, IdempotencyKey: fmt.Sprintf("distinct-%d", i), Text: "same text"}
			receipts[i], errs[i] = stores[i%2].Send(ctx, a, r)
		}(i)
	}
	wg.Wait()
	seqs := []int{}
	ids := map[string]bool{}
	for i, receipt := range receipts {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		seqs = append(seqs, int(receipt.Message.Sequence))
		ids[receipt.Message.ID] = true
		if receipt.Deliveries[0].Sequence != receipt.Message.Sequence {
			t.Fatal("mailbox sequence drift")
		}
	}
	sort.Ints(seqs)
	if len(ids) != workers {
		t.Fatalf("distinct keys collapsed: %d", len(ids))
	}
	for i, seq := range seqs {
		if seq != i+2 {
			t.Fatalf("noncontiguous sequence: %v", seqs)
		}
	}
	inbox, err := s.PullInbox(ctx, b, PageRequest{Limit: 100})
	if err != nil || len(inbox.Items) != workers+1 {
		t.Fatalf("fanout count = %+v, %v", inbox, err)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = stores[i%2].Ack(ctx, b, []string{inbox.Items[0].Message.ID})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	inbox, err = s.PullInbox(ctx, b, PageRequest{Limit: 100})
	if err != nil || len(inbox.Items) != workers {
		t.Fatalf("concurrent ACK count = %+v, %v", inbox, err)
	}
}

func TestMessageTransactionFailureRollsBackFanoutSequencesAndFiles(t *testing.T) {
	s := testStore(t)
	a, _, _ := messageActors(t, s)
	ctx := context.Background()
	conv := messageConversation(t, s, a, "group", "alice", "bob", "carol")
	messageAttachmentFixture(t, s, conv.ID, "alice", "rollback-file", "ready", s.now()+3600, 2)
	if _, err := s.db.Exec(`CREATE TRIGGER message_fail_delivery BEFORE INSERT ON deliveries WHEN NEW.recipient='carol' BEGIN SELECT RAISE(ABORT,'injected storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	req := SendRequest{ConversationID: conv.ID, IdempotencyKey: "rollback", Text: "body", AttachmentIDs: []string{"rollback-file"}}
	failed, err := s.Send(ctx, a, req)
	if err == nil || errors.Is(err, ErrNotFound) || failed.Stored || failed.Message.ID != "" {
		t.Fatalf("storage error swallowed: %+v, %v", failed, err)
	}
	for _, table := range []string{"messages", "deliveries", "mailbox_sequences", "message_attachments"} {
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s rows: %d, %v", table, count, err)
		}
	}
	var next, attached int
	if err := s.db.QueryRow("SELECT next_seq FROM conversations WHERE id=?", conv.ID).Scan(&next); err != nil || next != 1 {
		t.Fatalf("sequence not rolled back: %d, %v", next, err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM attachments WHERE message_id IS NOT NULL").Scan(&attached); err != nil || attached != 0 {
		t.Fatalf("file not rolled back: %d, %v", attached, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER message_fail_delivery"); err != nil {
		t.Fatal(err)
	}
	sent, err := s.Send(ctx, a, req)
	if err != nil || sent.Message.Sequence != 1 || len(sent.Deliveries) != 2 || sent.Deliveries[0].Sequence != 1 || sent.Deliveries[1].Sequence != 1 {
		t.Fatalf("retry after rollback = %+v, %v", sent, err)
	}
}

func TestMessageErrorsPreserveCancellationAndReturnNonNilArrays(t *testing.T) {
	s := testStore(t)
	a, _, _ := messageActors(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	identity, err := s.Identity(ctx, a)
	if !errors.Is(err, context.Canceled) || identity.Peers == nil {
		t.Fatalf("identity cancellation = %+v, %v", identity, err)
	}
	conv, err := s.CreateConversation(ctx, a, CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: "cancel"})
	if !errors.Is(err, context.Canceled) || conv.Members == nil {
		t.Fatalf("create cancellation = %+v, %v", conv, err)
	}
	list, err := s.ListConversations(ctx, a, PageRequest{})
	if !errors.Is(err, context.Canceled) || list.Items == nil {
		t.Fatalf("list cancellation = %+v, %v", list, err)
	}
	sent, err := s.Send(ctx, a, SendRequest{ConversationID: "missing", IdempotencyKey: "cancel", Text: "body"})
	if !errors.Is(err, context.Canceled) || sent.Deliveries == nil || sent.Message.AttachmentIDs == nil {
		t.Fatalf("send cancellation = %+v, %v", sent, err)
	}
	read, err := s.ReadMessages(ctx, a, "missing", PageRequest{})
	if !errors.Is(err, context.Canceled) || read.Items == nil {
		t.Fatalf("read cancellation = %+v, %v", read, err)
	}
	status, err := s.MessageStatus(ctx, a, "missing")
	if !errors.Is(err, context.Canceled) || status.Deliveries == nil || status.Message.AttachmentIDs == nil {
		t.Fatalf("status cancellation = %+v, %v", status, err)
	}
	inbox, err := s.PullInbox(ctx, a, PageRequest{})
	if !errors.Is(err, context.Canceled) || inbox.Items == nil {
		t.Fatalf("inbox cancellation = %+v, %v", inbox, err)
	}
	ack, err := s.Ack(ctx, a, []string{"missing"})
	if !errors.Is(err, context.Canceled) || ack.MessageIDs == nil {
		t.Fatalf("ack cancellation = %+v, %v", ack, err)
	}
	if err := s.CloseConversation(ctx, "missing"); !errors.Is(err, context.Canceled) {
		t.Fatalf("close cancellation = %v", err)
	}
	status, err = s.MessageStatus(context.Background(), a, "missing")
	if !errors.Is(err, ErrNotFound) || !reflect.DeepEqual(status, messageEmptyReceipt()) {
		t.Fatalf("not found status = %+v, %v", status, err)
	}
}
