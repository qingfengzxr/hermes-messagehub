package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type filesFixture struct {
	s               *Store
	alice, bob, eve Actor
	conversation    string
	now             *time.Time
}

func filesTestFixture(t *testing.T, opts Options) filesFixture {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	opts.Now = func() time.Time { return now }
	s, err := Open(filepath.Join(t.TempDir(), "files.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	alice, _ := provision(t, s, "alice")
	bob, _ := provision(t, s, "bob")
	eve, _ := provision(t, s, "eve")
	f := filesFixture{s: s, alice: alice, bob: bob, eve: eve, now: &now}
	f.conversation = filesTestConversation(t, s, alice, bob)
	return f
}

func filesTestConversation(t *testing.T, s *Store, actors ...Actor) string {
	t.Helper()
	id := newID("test_conversation")
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO conversations(id,creator,idem_key,digest,created_at) VALUES(?,?,?,?,?)`, id, actors[0].PrincipalID(), id, "test-only", s.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range actors {
		if _, err := tx.Exec(`INSERT INTO members(conversation_id,principal_id) VALUES(?,?)`, id, actor.PrincipalID()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func filesTestRequest(f filesFixture, data []byte) BeginAttachmentRequest {
	sum := sha256.Sum256(data)
	return BeginAttachmentRequest{ConversationID: f.conversation, IdempotencyKey: "upload-1", Filename: "原文件.bin", MediaType: "application/octet-stream", SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

func filesTestError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want %v", got, want)
	}
}

func TestFilesBeginReservesMetadataAndRetries(t *testing.T) {
	f := filesTestFixture(t, Options{MaxFileBytes: 8, MaxStoreBytes: 8})
	ctx := context.Background()
	req := filesTestRequest(f, []byte{0, 255, 128, 13, 10, 42, 1, 2})
	a, err := f.s.BeginAttachment(ctx, f.alice, req)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.ConversationID != req.ConversationID || a.Filename != req.Filename || a.MediaType != req.MediaType || a.SHA256 != req.SHA256 || a.SizeBytes != 8 || a.ReceivedBytes != 0 || a.State != "uploading" || a.ExpiresAt != f.now.Add(f.s.opts.AttachmentTTL).Unix() {
		t.Fatalf("wrong metadata: %+v", a)
	}
	retry, err := f.s.BeginAttachment(ctx, f.alice, req)
	if err != nil || retry != a {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	var count, reserved int64
	if err := f.s.db.QueryRow(`SELECT count(*),sum(size_bytes) FROM attachments`).Scan(&count, &reserved); err != nil {
		t.Fatal(err)
	}
	if count != 1 || reserved != 8 {
		t.Fatalf("rows=%d reserved=%d", count, reserved)
	}
	req.IdempotencyKey = "next"
	_, err = f.s.BeginAttachment(ctx, f.alice, req)
	filesTestError(t, err, ErrQuota)
}

func TestFilesBeginMetadataConflictAndPrivateScope(t *testing.T) {
	f := filesTestFixture(t, Options{})
	ctx := context.Background()
	req := filesTestRequest(f, []byte("hello"))
	a, err := f.s.BeginAttachment(ctx, f.alice, req)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*BeginAttachmentRequest){
		"filename": func(r *BeginAttachmentRequest) { r.Filename = "other.bin" },
		"media":    func(r *BeginAttachmentRequest) { r.MediaType = "text/plain" },
		"size":     func(r *BeginAttachmentRequest) { r.SizeBytes++ },
		"hash":     func(r *BeginAttachmentRequest) { r.SHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := req
			change(&changed)
			_, err := f.s.BeginAttachment(ctx, f.alice, changed)
			filesTestError(t, err, ErrConflict)
		})
	}
	b, err := f.s.BeginAttachment(ctx, f.bob, req)
	if err != nil || b.ID == a.ID {
		t.Fatalf("principal scope: %+v %v", b, err)
	}
	req.ConversationID = filesTestConversation(t, f.s, f.alice, f.eve)
	c, err := f.s.BeginAttachment(ctx, f.alice, req)
	if err != nil || c.ID == a.ID {
		t.Fatalf("conversation scope: %+v %v", c, err)
	}
	_, err = f.s.BeginAttachment(ctx, f.bob, req)
	filesTestError(t, err, ErrNotFound)
	_, token, err := f.s.IssueGrant(ctx, "alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := f.s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := f.s.BeginAttachment(ctx, fresh, req)
	if err != nil || retry.ID != c.ID {
		t.Fatalf("new grant changed principal binding: %+v %v", retry, err)
	}
}

func TestFilesBeginRejectsUnsafeMetadata(t *testing.T) {
	f := filesTestFixture(t, Options{MaxFileBytes: 8})
	ctx := context.Background()
	base := filesTestRequest(f, []byte("hi"))
	for _, name := range []string{"", ".", "..", "../file", "dir/file", `dir\file`, "/etc/passwd", "a\x00b", "a\rb", "a\nb", "a\tb", "a\x7fb", "a\u0085b", string([]byte{0xff}), strings.Repeat("x", 256)} {
		t.Run("filename_"+name, func(t *testing.T) {
			req := base
			req.Filename = name
			_, err := f.s.BeginAttachment(ctx, f.alice, req)
			filesTestError(t, err, ErrInvalid)
		})
	}
	for _, key := range []string{"", "a\x00b", "a\nb", "a\x7fb", "a\u0085b", string([]byte{0xff}), strings.Repeat("k", 129)} {
		t.Run("key_"+key, func(t *testing.T) {
			req := base
			req.IdempotencyKey = key
			_, err := f.s.BeginAttachment(ctx, f.alice, req)
			filesTestError(t, err, ErrInvalid)
		})
	}
	for _, media := range []string{"", "plain", "text/", "text/plain\r\nX-Header: evil", "text/plain\x00", "text/plain; x=", string([]byte{0xff}), "text/plain; x=" + strings.Repeat("x", 256)} {
		t.Run("media_"+media, func(t *testing.T) {
			req := base
			req.MediaType = media
			_, err := f.s.BeginAttachment(ctx, f.alice, req)
			filesTestError(t, err, ErrInvalid)
		})
	}
	for _, hash := range []string{"", strings.Repeat("0", 63), strings.Repeat("0", 65), strings.Repeat("z", 64), strings.Repeat("A", 64), " " + base.SHA256} {
		t.Run("hash_"+hash, func(t *testing.T) {
			req := base
			req.SHA256 = hash
			_, err := f.s.BeginAttachment(ctx, f.alice, req)
			filesTestError(t, err, ErrInvalid)
		})
	}
	req := base
	req.SizeBytes = -1
	_, err := f.s.BeginAttachment(ctx, f.alice, req)
	filesTestError(t, err, ErrInvalid)
	req.SizeBytes = 9
	_, err = f.s.BeginAttachment(ctx, f.alice, req)
	filesTestError(t, err, ErrQuota)
	var count int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM attachments`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid requests allocated %d rows, %v", count, err)
	}
}

func TestFilesBeginAuthClosureAndExpiry(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"outsider", "unknown_conversation", "closed", "revoked", "disabled", "expired_grant", "forged", "empty_actor", "expired_file"} {
		t.Run(mode, func(t *testing.T) {
			f := filesTestFixture(t, Options{AttachmentTTL: time.Minute})
			req := filesTestRequest(f, []byte("hi"))
			actor, want := f.alice, ErrNotFound
			switch mode {
			case "outsider":
				actor = f.eve
			case "unknown_conversation":
				req.ConversationID = "unknown"
			case "closed":
				filesTestExec(t, f.s, `UPDATE conversations SET closed=1 WHERE id=?`, f.conversation)
			case "revoked":
				if err := f.s.RevokeGrant(ctx, actor.GrantID()); err != nil {
					t.Fatal(err)
				}
				want = ErrUnauthenticated
			case "disabled":
				if err := f.s.SetPrincipalActive(ctx, "alice", false); err != nil {
					t.Fatal(err)
				}
				want = ErrUnauthenticated
			case "expired_grant":
				*f.now = f.now.Add(time.Hour)
				want = ErrUnauthenticated
			case "forged":
				actor.principalID = "bob"
				want = ErrUnauthenticated
			case "empty_actor":
				actor = Actor{}
				want = ErrUnauthenticated
			case "expired_file":
				if _, err := f.s.BeginAttachment(ctx, actor, req); err != nil {
					t.Fatal(err)
				}
				*f.now = f.now.Add(time.Minute)
				want = ErrExpired
			}
			_, err := f.s.BeginAttachment(ctx, actor, req)
			filesTestError(t, err, want)
		})
	}
}

func filesTestExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func filesTestMessage(t *testing.T, f filesFixture, sender Actor, conversation string, expires int64) string {
	t.Helper()
	id := newID("test_message")
	filesTestExec(t, f.s, `INSERT INTO messages(id,conversation_id,seq,sender,idem_key,digest,text,data,created_at,expires_at) VALUES(?,?,(SELECT coalesce(max(seq),0)+1 FROM messages WHERE conversation_id=?),?,?,'test-only','','',?,?)`, id, conversation, conversation, sender.PrincipalID(), id, f.s.now(), expires)
	return id
}

func filesTestBind(t *testing.T, f filesFixture, message, attachment string) {
	t.Helper()
	filesTestExec(t, f.s, `UPDATE attachments SET message_id=? WHERE id=?`, message, attachment)
	filesTestExec(t, f.s, `INSERT INTO message_attachments(message_id,attachment_id) VALUES(?,?)`, message, attachment)
}

func filesTestReady(t *testing.T, f filesFixture, data []byte, key string) Attachment {
	t.Helper()
	ctx := context.Background()
	req := filesTestRequest(f, data)
	req.IdempotencyKey = key
	a, err := f.s.BeginAttachment(ctx, f.alice, req)
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(data); {
		end := min(offset+f.s.opts.MaxChunkBytes, len(data))
		a, err = f.s.WriteAttachment(ctx, f.alice, a.ID, int64(offset), data[offset:end])
		if err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	a, err = f.s.CommitAttachment(ctx, f.alice, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestFilesRawBytesRoundTripBoundedChunks(t *testing.T) {
	f := filesTestFixture(t, Options{MaxChunkBytes: 7})
	ctx := context.Background()
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	data = append(data, []byte("\x00<script>alert(1)</script>\r\nhttps://127.0.0.1/private\n#!/bin/sh\n")...)
	a := filesTestReady(t, f, data, "binary")
	if a.State != "ready" || a.ReceivedBytes != int64(len(data)) {
		t.Fatalf("not ready: %+v", a)
	}
	var content []byte
	if err := f.s.db.QueryRow(`SELECT content FROM attachments WHERE id=?`, a.ID).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, data) {
		t.Fatal("stored bytes changed")
	}
	retry, err := f.s.CommitAttachment(ctx, f.alice, a.ID)
	if err != nil || retry != a {
		t.Fatalf("commit retry = %+v, %v", retry, err)
	}
	writeRetry, err := f.s.WriteAttachment(ctx, f.alice, a.ID, 0, data[:7])
	if err != nil || writeRetry != a {
		t.Fatalf("write retry after commit = %+v, %v", writeRetry, err)
	}
	req := filesTestRequest(f, data)
	req.IdempotencyKey = "binary"
	beginRetry, err := f.s.BeginAttachment(ctx, f.alice, req)
	if err != nil || beginRetry != a {
		t.Fatalf("begin retry after commit = %+v, %v", beginRetry, err)
	}
	message := filesTestMessage(t, f, f.alice, f.conversation, f.s.now()+60)
	filesTestBind(t, f, message, a.ID)
	for _, actor := range []Actor{f.alice, f.bob} {
		var downloaded []byte
		for offset := int64(0); ; {
			chunk, err := f.s.ReadAttachment(ctx, actor, message, a.ID, offset)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := base64.StdEncoding.DecodeString(chunk.ContentBase64)
			if err != nil {
				t.Fatal(err)
			}
			if len(decoded) > 7 || chunk.Offset != offset || chunk.NextOffset != offset+int64(len(decoded)) || chunk.Attachment != a {
				t.Fatalf("bad chunk: %+v", chunk)
			}
			downloaded = append(downloaded, decoded...)
			if chunk.EOF {
				if chunk.NextOffset != int64(len(data)) {
					t.Fatal("early EOF")
				}
				break
			}
			if chunk.NextOffset <= offset {
				t.Fatal("download stalled")
			}
			offset = chunk.NextOffset
		}
		if !bytes.Equal(downloaded, data) {
			t.Fatal("download changed raw bytes")
		}
	}
	last, err := f.s.ReadAttachment(ctx, f.bob, message, a.ID, a.SizeBytes)
	if err != nil || !last.EOF || last.ContentBase64 != "" || last.NextOffset != a.SizeBytes {
		t.Fatalf("EOF = %+v %v", last, err)
	}
	for _, offset := range []int64{-1, a.SizeBytes + 1, math.MaxInt64} {
		_, err := f.s.ReadAttachment(ctx, f.bob, message, a.ID, offset)
		filesTestError(t, err, ErrInvalid)
	}
}

func TestFilesEmptyFileAndInertMedia(t *testing.T) {
	f := filesTestFixture(t, Options{})
	ctx := context.Background()
	req := filesTestRequest(f, nil)
	req.Filename = "程序.sh"
	req.MediaType = `text/html; charset=utf-8; name="https://127.0.0.1/no-fetch"`
	a, err := f.s.BeginAttachment(ctx, f.alice, req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.WriteAttachment(ctx, f.alice, a.ID, 0, nil)
	filesTestError(t, err, ErrInvalid)
	a, err = f.s.CommitAttachment(ctx, f.alice, a.ID)
	if err != nil || a.State != "ready" || a.SizeBytes != 0 || a.MediaType != req.MediaType {
		t.Fatalf("empty commit: %+v %v", a, err)
	}
	message := filesTestMessage(t, f, f.alice, f.conversation, f.s.now()+60)
	filesTestBind(t, f, message, a.ID)
	chunk, err := f.s.ReadAttachment(ctx, f.bob, message, a.ID, 0)
	if err != nil || !chunk.EOF || chunk.NextOffset != 0 || chunk.ContentBase64 != "" || chunk.Attachment.SHA256 != req.SHA256 {
		t.Fatalf("empty download: %+v %v", chunk, err)
	}
}

func TestFilesWriteSequentialAndExactRetryOnly(t *testing.T) {
	f := filesTestFixture(t, Options{MaxChunkBytes: 4})
	ctx := context.Background()
	a, err := f.s.BeginAttachment(ctx, f.alice, filesTestRequest(f, []byte("abcdefgh")))
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.s.WriteAttachment(ctx, f.alice, a.ID, 0, []byte("abcd"))
	if err != nil || a.ReceivedBytes != 4 {
		t.Fatalf("first write: %+v %v", a, err)
	}
	for _, tc := range []struct {
		name   string
		offset int64
		data   string
		want   error
	}{
		{"different bytes", 0, "abXd", ErrConflict},
		{"prefix is not same chunk", 0, "ab", ErrConflict},
		{"overlap", 2, "cdef", ErrConflict},
		{"gap", 6, "gh", ErrConflict},
		{"negative", -1, "x", ErrInvalid},
		{"past size", 9, "x", ErrInvalid},
		{"overflow", math.MaxInt64, "x", ErrInvalid},
		{"overrun", 7, "xx", ErrInvalid},
		{"zero chunk", 4, "", ErrInvalid},
		{"oversized chunk", 0, "abcde", ErrQuota},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.s.WriteAttachment(ctx, f.alice, a.ID, tc.offset, []byte(tc.data))
			filesTestError(t, err, tc.want)
			if got.ID != "" {
				t.Fatal("error returned attachment metadata")
			}
		})
	}
	var received, count int64
	if err := f.s.db.QueryRow(`SELECT received_bytes,(SELECT count(*) FROM attachment_chunks WHERE attachment_id=attachments.id) FROM attachments WHERE id=?`, a.ID).Scan(&received, &count); err != nil {
		t.Fatal(err)
	}
	if received != 4 || count != 1 {
		t.Fatalf("failed writes changed storage: received=%d chunks=%d", received, count)
	}
	retry, err := f.s.WriteAttachment(ctx, f.alice, a.ID, 0, []byte("abcd"))
	if err != nil || retry != a {
		t.Fatalf("same chunk retry: %+v %v", retry, err)
	}
	a, err = f.s.WriteAttachment(ctx, f.alice, a.ID, 4, []byte("efgh"))
	if err != nil || a.ReceivedBytes != 8 {
		t.Fatalf("append: %+v %v", a, err)
	}
	retry, err = f.s.WriteAttachment(ctx, f.alice, a.ID, 0, []byte("abcd"))
	if err != nil || retry != a {
		t.Fatalf("older chunk retry: %+v %v", retry, err)
	}
	if _, err := f.s.CommitAttachment(ctx, f.alice, a.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.WriteAttachment(ctx, f.alice, a.ID, 0, []byte("wxyz"))
	filesTestError(t, err, ErrConflict)
}

func TestFilesCommitRequiresExactSizeAndSHA(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"incomplete", "wrong_hash", "empty_wrong_hash", "missing_chunk", "shifted_chunk", "counter_mismatch", "injected_oversize"} {
		t.Run(mode, func(t *testing.T) {
			f := filesTestFixture(t, Options{MaxChunkBytes: 4})
			data := []byte("abcd")
			if mode == "empty_wrong_hash" {
				data = nil
			}
			req := filesTestRequest(f, data)
			if mode == "wrong_hash" || mode == "empty_wrong_hash" {
				req.SHA256 = strings.Repeat("0", 64)
			}
			a, err := f.s.BeginAttachment(ctx, f.alice, req)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "incomplete" {
				data = data[:2]
			}
			if len(data) != 0 {
				if _, err := f.s.WriteAttachment(ctx, f.alice, a.ID, 0, data); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "missing_chunk":
				filesTestExec(t, f.s, `DELETE FROM attachment_chunks WHERE attachment_id=?`, a.ID)
			case "shifted_chunk":
				filesTestExec(t, f.s, `UPDATE attachment_chunks SET offset=1 WHERE attachment_id=?`, a.ID)
			case "counter_mismatch":
				filesTestExec(t, f.s, `UPDATE attachments SET received_bytes=3 WHERE id=?`, a.ID)
			case "injected_oversize":
				filesTestExec(t, f.s, `UPDATE attachment_chunks SET data=? WHERE attachment_id=?`, []byte("abcde"), a.ID)
			}
			got, err := f.s.CommitAttachment(ctx, f.alice, a.ID)
			filesTestError(t, err, ErrConflict)
			if got.ID != "" {
				t.Fatal("failed commit returned metadata")
			}
			var state string
			var noContent bool
			if err := f.s.db.QueryRow(`SELECT state,content IS NULL FROM attachments WHERE id=?`, a.ID).Scan(&state, &noContent); err != nil {
				t.Fatal(err)
			}
			if state != "uploading" || !noContent {
				t.Fatalf("failed commit mutated file: state=%s noContent=%v", state, noContent)
			}
			if mode == "incomplete" {
				if _, err := f.s.WriteAttachment(ctx, f.alice, a.ID, 2, []byte("cd")); err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.CommitAttachment(ctx, f.alice, a.ID); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestFilesOwnerOnlyStagingCurrentMemberDownload(t *testing.T) {
	f := filesTestFixture(t, Options{})
	ctx := context.Background()
	a, err := f.s.BeginAttachment(ctx, f.alice, filesTestRequest(f, []byte("abcd")))
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []Actor{f.bob, f.eve} {
		_, err := f.s.WriteAttachment(ctx, actor, a.ID, 0, []byte("abcd"))
		filesTestError(t, err, ErrNotFound)
		_, err = f.s.CommitAttachment(ctx, actor, a.ID)
		filesTestError(t, err, ErrNotFound)
	}
	_, err = f.s.WriteAttachment(ctx, f.alice, "unknown", 0, []byte("abcd"))
	filesTestError(t, err, ErrNotFound)
	_, err = f.s.CommitAttachment(ctx, f.alice, "unknown")
	filesTestError(t, err, ErrNotFound)
	a = filesTestReady(t, f, []byte("abcd"), "upload-1")
	message := filesTestMessage(t, f, f.alice, f.conversation, f.s.now()+60)
	filesTestBind(t, f, message, a.ID)
	for _, actor := range []Actor{f.alice, f.bob} {
		if _, err := f.s.ReadAttachment(ctx, actor, message, a.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	_, err = f.s.ReadAttachment(ctx, f.eve, message, a.ID, 0)
	filesTestError(t, err, ErrNotFound)
	filesTestExec(t, f.s, `DELETE FROM members WHERE conversation_id=? AND principal_id='bob'`, f.conversation)
	_, err = f.s.ReadAttachment(ctx, f.bob, message, a.ID, 0)
	filesTestError(t, err, ErrNotFound)
	filesTestExec(t, f.s, `DELETE FROM members WHERE conversation_id=? AND principal_id='alice'`, f.conversation)
	_, err = f.s.WriteAttachment(ctx, f.alice, a.ID, 0, []byte("abcd"))
	filesTestError(t, err, ErrNotFound)
	_, err = f.s.CommitAttachment(ctx, f.alice, a.ID)
	filesTestError(t, err, ErrNotFound)
	_, err = f.s.ReadAttachment(ctx, f.alice, message, a.ID, 0)
	filesTestError(t, err, ErrNotFound)
}

func TestFilesReadRequiresExactReadyMessageLink(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"unbound", "only_attachment_link", "only_message_link", "wrong_message", "unknown_message", "unknown_file", "cross_conversation", "different_sender", "not_ready"} {
		t.Run(mode, func(t *testing.T) {
			f := filesTestFixture(t, Options{})
			a := filesTestReady(t, f, []byte("abcd"), "linked")
			conversation, sender := f.conversation, f.alice
			if mode == "cross_conversation" {
				conversation = filesTestConversation(t, f.s, f.bob, f.eve)
			}
			if mode == "different_sender" {
				sender = f.bob
			}
			message := filesTestMessage(t, f, sender, conversation, f.s.now()+60)
			switch mode {
			case "unbound":
			case "only_attachment_link":
				filesTestExec(t, f.s, `UPDATE attachments SET message_id=? WHERE id=?`, message, a.ID)
			case "only_message_link":
				filesTestExec(t, f.s, `INSERT INTO message_attachments(message_id,attachment_id) VALUES(?,?)`, message, a.ID)
			default:
				filesTestBind(t, f, message, a.ID)
			}
			switch mode {
			case "wrong_message":
				message = filesTestMessage(t, f, f.alice, f.conversation, f.s.now()+60)
			case "unknown_message":
				message = "missing"
			case "unknown_file":
				a.ID = "missing"
			case "not_ready":
				filesTestExec(t, f.s, `UPDATE attachments SET state='uploading' WHERE id=?`, a.ID)
			}
			chunk, err := f.s.ReadAttachment(ctx, f.bob, message, a.ID, 0)
			filesTestError(t, err, ErrNotFound)
			if chunk.Attachment.ID != "" || chunk.ContentBase64 != "" {
				t.Fatal("denied download leaked data")
			}
		})
	}
}

func TestFilesAllOperationsRevalidateCachedCredentials(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"revoked", "disabled", "expired_grant", "expired_token", "forged", "empty"} {
		t.Run(mode, func(t *testing.T) {
			f := filesTestFixture(t, Options{})
			ready := filesTestReady(t, f, []byte("abcd"), "ready")
			message := filesTestMessage(t, f, f.alice, f.conversation, f.s.now()+60)
			filesTestBind(t, f, message, ready.ID)
			req := filesTestRequest(f, []byte("abcd"))
			staging, err := f.s.BeginAttachment(ctx, f.alice, req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.WriteAttachment(ctx, f.alice, staging.ID, 0, []byte("abcd")); err != nil {
				t.Fatal(err)
			}
			actor := f.alice
			switch mode {
			case "revoked":
				if err := f.s.RevokeGrant(ctx, actor.GrantID()); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := f.s.SetPrincipalActive(ctx, "alice", false); err != nil {
					t.Fatal(err)
				}
			case "expired_grant":
				filesTestExec(t, f.s, `UPDATE grants SET expires_at=? WHERE id=?`, f.s.now(), actor.GrantID())
			case "expired_token":
				filesTestExec(t, f.s, `UPDATE tokens SET expires_at=? WHERE token_hash=?`, f.s.now(), actor.tokenHash)
			case "forged":
				actor.principalID = "bob"
			case "empty":
				actor = Actor{}
			}
			a, err := f.s.BeginAttachment(ctx, actor, req)
			filesTestError(t, err, ErrUnauthenticated)
			if a.ID != "" {
				t.Fatal("begin leaked")
			}
			a, err = f.s.WriteAttachment(ctx, actor, staging.ID, 0, []byte("abcd"))
			filesTestError(t, err, ErrUnauthenticated)
			if a.ID != "" {
				t.Fatal("write leaked")
			}
			a, err = f.s.CommitAttachment(ctx, actor, staging.ID)
			filesTestError(t, err, ErrUnauthenticated)
			if a.ID != "" {
				t.Fatal("commit leaked")
			}
			chunk, err := f.s.ReadAttachment(ctx, actor, message, ready.ID, 0)
			filesTestError(t, err, ErrUnauthenticated)
			if chunk.Attachment.ID != "" || chunk.ContentBase64 != "" {
				t.Fatal("read leaked")
			}
			var state string
			if err := f.s.db.QueryRow(`SELECT state FROM attachments WHERE id=?`, staging.ID).Scan(&state); err != nil || state != "uploading" {
				t.Fatalf("unauthorized commit changed state: %s %v", state, err)
			}
		})
	}
}

func TestFilesClosureStopsStagingButKeepsHistory(t *testing.T) {
	f := filesTestFixture(t, Options{})
	ctx := context.Background()
	a := filesTestReady(t, f, []byte("abcd"), "ready")
	message := filesTestMessage(t, f, f.alice, f.conversation, f.s.now()+60)
	filesTestBind(t, f, message, a.ID)
	req := filesTestRequest(f, []byte("abcd"))
	staging, err := f.s.BeginAttachment(ctx, f.alice, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.WriteAttachment(ctx, f.alice, staging.ID, 0, []byte("abcd")); err != nil {
		t.Fatal(err)
	}
	filesTestExec(t, f.s, `UPDATE conversations SET closed=1 WHERE id=?`, f.conversation)
	_, err = f.s.BeginAttachment(ctx, f.alice, req)
	filesTestError(t, err, ErrNotFound)
	_, err = f.s.WriteAttachment(ctx, f.alice, staging.ID, 0, []byte("abcd"))
	filesTestError(t, err, ErrNotFound)
	_, err = f.s.CommitAttachment(ctx, f.alice, staging.ID)
	filesTestError(t, err, ErrNotFound)
	if _, err := f.s.ReadAttachment(ctx, f.bob, message, a.ID, 0); err != nil {
		t.Fatal(err)
	}
}

func TestFilesExpiryBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"file", "message"} {
		t.Run(mode, func(t *testing.T) {
			f := filesTestFixture(t, Options{AttachmentTTL: 2 * time.Second})
			a := filesTestReady(t, f, []byte("abcd"), "ready")
			expires := f.s.now() + 60
			if mode == "message" {
				expires = f.s.now() + 1
			}
			message := filesTestMessage(t, f, f.alice, f.conversation, expires)
			filesTestBind(t, f, message, a.ID)
			req := filesTestRequest(f, []byte("abcd"))
			staging, err := f.s.BeginAttachment(ctx, f.alice, req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.WriteAttachment(ctx, f.alice, staging.ID, 0, []byte("abcd")); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.ReadAttachment(ctx, f.bob, message, a.ID, 0); err != nil {
				t.Fatal(err)
			}
			if mode == "message" {
				*f.now = f.now.Add(time.Second)
			} else {
				*f.now = f.now.Add(2 * time.Second)
			}
			_, err = f.s.ReadAttachment(ctx, f.bob, message, a.ID, 0)
			filesTestError(t, err, ErrExpired)
			if mode == "file" {
				_, err = f.s.BeginAttachment(ctx, f.alice, req)
				filesTestError(t, err, ErrExpired)
				_, err = f.s.WriteAttachment(ctx, f.alice, staging.ID, 0, []byte("abcd"))
				filesTestError(t, err, ErrExpired)
				_, err = f.s.CommitAttachment(ctx, f.alice, staging.ID)
				filesTestError(t, err, ErrExpired)
				_, err = f.s.CommitAttachment(ctx, f.alice, a.ID)
				filesTestError(t, err, ErrExpired)
			}
			_, err = f.s.ReadAttachment(ctx, f.eve, message, a.ID, 0)
			filesTestError(t, err, ErrNotFound)
		})
	}
}

func TestFilesResumeAndReadAfterReopen(t *testing.T) {
	f := filesTestFixture(t, Options{MaxChunkBytes: 4})
	ctx := context.Background()
	data := []byte{0, 128, 255, 1, 2, 13, 10, 0}
	req := filesTestRequest(f, data)
	a, err := f.s.BeginAttachment(ctx, f.alice, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.WriteAttachment(ctx, f.alice, a.ID, 0, data[:4]); err != nil {
		t.Fatal(err)
	}
	path, opts := f.s.path, f.s.opts
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s, err = Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.Close()
	a, err = f.s.BeginAttachment(ctx, f.alice, req)
	if err != nil || a.ReceivedBytes != 4 {
		t.Fatalf("resume metadata: %+v %v", a, err)
	}
	if _, err := f.s.WriteAttachment(ctx, f.alice, a.ID, 0, data[:4]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.WriteAttachment(ctx, f.alice, a.ID, 4, data[4:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CommitAttachment(ctx, f.alice, a.ID); err != nil {
		t.Fatal(err)
	}
	message := filesTestMessage(t, f, f.alice, f.conversation, f.s.now()+60)
	filesTestBind(t, f, message, a.ID)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s, err = Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer f.s.Close()
	var got []byte
	for _, offset := range []int64{0, 4} {
		chunk, err := f.s.ReadAttachment(ctx, f.bob, message, a.ID, offset)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.StdEncoding.DecodeString(chunk.ContentBase64)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, raw...)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reopen lost bytes")
	}
}

func TestFilesConcurrentRetriesAndReservation(t *testing.T) {
	ctx := context.Background()
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprintf("same-key=%v", same), func(t *testing.T) {
			f := filesTestFixture(t, Options{MaxFileBytes: 4, MaxStoreBytes: 8, MaxChunkBytes: 4})
			type outcome struct {
				attachment Attachment
				err        error
			}
			results := make(chan outcome, 12)
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					req := filesTestRequest(f, []byte("abcd"))
					if !same {
						req.IdempotencyKey = fmt.Sprintf("key-%d", i)
					}
					a, err := f.s.BeginAttachment(ctx, f.alice, req)
					if err == nil && same {
						a, err = f.s.WriteAttachment(ctx, f.alice, a.ID, 0, []byte("abcd"))
						if err == nil {
							a, err = f.s.CommitAttachment(ctx, f.alice, a.ID)
						}
					}
					results <- outcome{a, err}
				}(i)
			}
			wg.Wait()
			close(results)
			ids := map[string]bool{}
			successes := 0
			for r := range results {
				if r.err != nil {
					if same {
						t.Fatal(r.err)
					}
					filesTestError(t, r.err, ErrQuota)
					continue
				}
				successes++
				ids[r.attachment.ID] = true
			}
			var count, reserved int64
			if err := f.s.db.QueryRow(`SELECT count(*),sum(size_bytes) FROM attachments`).Scan(&count, &reserved); err != nil {
				t.Fatal(err)
			}
			if same {
				if successes != 12 || len(ids) != 1 || count != 1 || reserved != 4 {
					t.Fatalf("retry: success=%d ids=%d rows=%d reserved=%d", successes, len(ids), count, reserved)
				}
			} else if successes != 2 || len(ids) != 2 || count != 2 || reserved != 8 {
				t.Fatalf("quota: success=%d ids=%d rows=%d reserved=%d", successes, len(ids), count, reserved)
			}
		})
	}
}

func TestFilesBeginRejectsDriveRelativeAndAlternateStreamNames(t *testing.T) {
	f := filesTestFixture(t, Options{})
	for _, filename := range []string{"C:relative.txt", "notes.txt:secret"} {
		t.Run(filename, func(t *testing.T) {
			req := filesTestRequest(f, []byte("x"))
			req.Filename = filename
			req.IdempotencyKey = filename
			_, err := f.s.BeginAttachment(context.Background(), f.alice, req)
			filesTestError(t, err, ErrInvalid)
		})
	}
}

func TestFilesBeginRejectsInvisibleFilenameControls(t *testing.T) {
	f := filesTestFixture(t, Options{})
	for _, filename := range []string{"photo\u202egnp.exe", "a\u2066b.txt", "a\u200bb.txt", "a\u2028b.txt", "a\u2029b.txt"} {
		t.Run(fmt.Sprintf("%q", filename), func(t *testing.T) {
			req := filesTestRequest(f, []byte("x"))
			req.Filename = filename
			_, err := f.s.BeginAttachment(context.Background(), f.alice, req)
			filesTestError(t, err, ErrInvalid)
		})
	}
}

func TestFilesBeginQuotaIncludesMessagesAndExpiredReservations(t *testing.T) {
	f := filesTestFixture(t, Options{MaxFileBytes: 8, MaxStoreBytes: 10, AttachmentTTL: time.Second})
	ctx := context.Background()
	filesTestExec(t, f.s, `INSERT INTO messages(id,conversation_id,seq,sender,idem_key,digest,text,data,created_at,expires_at) VALUES('quota-msg',?,1,'alice','quota-msg','fixture','字','{}',?,?)`, f.conversation, f.s.now(), f.s.now()-1)
	req := filesTestRequest(f, []byte("123456"))
	_, err := f.s.BeginAttachment(ctx, f.alice, req)
	filesTestError(t, err, ErrQuota)
	req = filesTestRequest(f, []byte("12345"))
	if _, err := f.s.BeginAttachment(ctx, f.alice, req); err != nil {
		t.Fatal(err)
	}
	*f.now = f.now.Add(time.Second)
	req = filesTestRequest(f, []byte("1"))
	req.IdempotencyKey = "after-expiry"
	_, err = f.s.BeginAttachment(ctx, f.alice, req)
	filesTestError(t, err, ErrQuota)
}
