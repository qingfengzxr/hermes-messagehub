package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"
)

const filesAttachmentColumns = `id,conversation_id,filename,media_type,size_bytes,sha256,state,received_bytes,expires_at`

func (s *Store) BeginAttachment(ctx context.Context, actor Actor, req BeginAttachmentRequest) (Attachment, error) {
	var result Attachment
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		if !filesSafeMetadata(req.IdempotencyKey, 128) || !filesSafeMetadata(req.Filename, 255) || !filesSafeMetadata(req.MediaType, 255) || req.SizeBytes < 0 {
			return ErrInvalid
		}
		if strings.ContainsAny(req.Filename, "/\\:") || req.Filename == "." || req.Filename == ".." {
			return ErrInvalid
		}
		media, _, err := mime.ParseMediaType(req.MediaType)
		if err != nil || !strings.Contains(media, "/") {
			return ErrInvalid
		}
		if len(req.SHA256) != 64 || req.SHA256 != strings.ToLower(req.SHA256) {
			return ErrInvalid
		}
		if _, err := hex.DecodeString(req.SHA256); err != nil {
			return ErrInvalid
		}
		if req.SizeBytes > s.opts.MaxFileBytes {
			return ErrQuota
		}
		if err := s.filesUploadConversation(ctx, tx, actor, req.ConversationID); err != nil {
			return err
		}
		metadata, err := json.Marshal(req)
		if err != nil {
			return err
		}
		digest := tokenDigest(string(metadata))
		var id, storedDigest string
		err = tx.QueryRowContext(ctx, `SELECT id,digest FROM attachments WHERE uploader=? AND conversation_id=? AND idem_key=?`, actor.PrincipalID(), req.ConversationID, req.IdempotencyKey).Scan(&id, &storedDigest)
		if err == nil {
			if storedDigest != digest {
				return ErrConflict
			}
			result, err = filesScanAttachment(tx.QueryRowContext(ctx, `SELECT `+filesAttachmentColumns+` FROM attachments WHERE id=?`, id))
			if err != nil {
				return err
			}
			if result.ExpiresAt <= s.now() {
				return ErrExpired
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := s.filesReserveCapacity(ctx, tx, req.SizeBytes); err != nil {
			return err
		}
		result = Attachment{ID: newID("f"), ConversationID: req.ConversationID, Filename: req.Filename, MediaType: req.MediaType, SizeBytes: req.SizeBytes, SHA256: req.SHA256, State: "uploading", ExpiresAt: s.opts.Now().Add(s.opts.AttachmentTTL).Unix()}
		_, err = tx.ExecContext(ctx, `INSERT INTO attachments(id,conversation_id,uploader,idem_key,digest,filename,media_type,size_bytes,sha256,state,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, result.ID, req.ConversationID, actor.PrincipalID(), req.IdempotencyKey, digest, req.Filename, req.MediaType, req.SizeBytes, req.SHA256, result.State, s.now(), result.ExpiresAt)
		return err
	})
	if err != nil {
		return Attachment{}, err
	}
	return result, nil
}

func (s *Store) WriteAttachment(ctx context.Context, actor Actor, id string, offset int64, data []byte) (Attachment, error) {
	var result Attachment
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		var err error
		result, err = s.filesOwnedAttachment(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		if len(data) == 0 || offset < 0 || offset > result.SizeBytes {
			return ErrInvalid
		}
		if len(data) > s.opts.MaxChunkBytes {
			return ErrQuota
		}
		if int64(len(data)) > result.SizeBytes-offset {
			return ErrInvalid
		}
		if result.State != "uploading" && result.State != "ready" {
			return ErrConflict
		}
		if offset < result.ReceivedBytes {
			var stored []byte
			err := tx.QueryRowContext(ctx, `SELECT data FROM attachment_chunks WHERE attachment_id=? AND offset=? AND length(data)=?`, id, offset, len(data)).Scan(&stored)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrConflict
			}
			if err != nil {
				return err
			}
			if !bytes.Equal(stored, data) {
				return ErrConflict
			}
			return nil
		}
		if offset != result.ReceivedBytes || result.State != "uploading" {
			return ErrConflict
		}
		if err := s.filesReserveCapacity(ctx, tx, 0); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachment_chunks(attachment_id,offset,data) VALUES(?,?,?)`, id, offset, data); err != nil {
			return err
		}
		result.ReceivedBytes += int64(len(data))
		_, err = tx.ExecContext(ctx, `UPDATE attachments SET received_bytes=? WHERE id=?`, result.ReceivedBytes, id)
		return err
	})
	if err != nil {
		return Attachment{}, err
	}
	return result, nil
}

func (s *Store) filesOwnedAttachment(ctx context.Context, tx *sql.Tx, actor Actor, id string) (Attachment, error) {
	a, err := filesScanAttachment(tx.QueryRowContext(ctx, `SELECT `+filesAttachmentColumns+` FROM attachments WHERE id=? AND uploader=?`, id, actor.PrincipalID()))
	if err != nil {
		return Attachment{}, err
	}
	if err := s.filesUploadConversation(ctx, tx, actor, a.ConversationID); err != nil {
		return Attachment{}, err
	}
	if a.ExpiresAt <= s.now() {
		return Attachment{}, ErrExpired
	}
	if a.SizeBytes > s.opts.MaxFileBytes {
		return Attachment{}, ErrQuota
	}
	if a.SizeBytes < 0 || a.ReceivedBytes < 0 || a.ReceivedBytes > a.SizeBytes {
		return Attachment{}, ErrConflict
	}
	return a, nil
}

func (s *Store) CommitAttachment(ctx context.Context, actor Actor, id string) (Attachment, error) {
	var result Attachment
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		var err error
		result, err = s.filesOwnedAttachment(ctx, tx, actor, id)
		if err != nil {
			return err
		}
		if result.State == "ready" {
			return nil
		}
		if result.State != "uploading" || result.ReceivedBytes != result.SizeBytes {
			return ErrConflict
		}
		if err := s.filesReserveCapacity(ctx, tx, 0); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT offset,data FROM attachment_chunks WHERE attachment_id=? ORDER BY offset`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		content := make([]byte, 0, int(result.SizeBytes))
		for rows.Next() {
			var offset int64
			var data []byte
			if err := rows.Scan(&offset, &data); err != nil {
				return err
			}
			if offset != int64(len(content)) || len(data) == 0 || len(data) > s.opts.MaxChunkBytes || int64(len(data)) > result.SizeBytes-int64(len(content)) {
				return ErrConflict
			}
			content = append(content, data...)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		if int64(len(content)) != result.SizeBytes || hex.EncodeToString(sum[:]) != result.SHA256 {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attachments SET state='ready',content=? WHERE id=?`, content, id); err != nil {
			return err
		}
		result.State = "ready"
		return nil
	})
	if err != nil {
		return Attachment{}, err
	}
	return result, nil
}

func (s *Store) ReadAttachment(ctx context.Context, actor Actor, messageID, id string, offset int64) (AttachmentChunk, error) {
	var result AttachmentChunk
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		var conversation, sender string
		var expires int64
		err := tx.QueryRowContext(ctx, `SELECT conversation_id,sender,expires_at FROM messages WHERE id=?`, messageID).Scan(&conversation, &sender, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := s.requireMember(ctx, tx, actor, conversation); err != nil {
			return err
		}
		a, err := filesScanAttachment(tx.QueryRowContext(ctx, `SELECT `+filesAttachmentColumns+` FROM attachments WHERE id=? AND conversation_id=? AND uploader=? AND message_id=? AND state='ready' AND EXISTS(SELECT 1 FROM message_attachments WHERE message_id=? AND attachment_id=attachments.id)`, id, conversation, sender, messageID, messageID))
		if err != nil {
			return err
		}
		if expires <= s.now() || a.ExpiresAt <= s.now() {
			return ErrExpired
		}
		if offset < 0 || offset > a.SizeBytes {
			return ErrInvalid
		}
		length := min(int64(s.opts.MaxChunkBytes), a.SizeBytes-offset)
		var data []byte
		var storedSize int64
		if err := tx.QueryRowContext(ctx, `SELECT substr(content,?,?),length(content) FROM attachments WHERE id=?`, offset+1, length, id).Scan(&data, &storedSize); err != nil {
			return err
		}
		if storedSize != a.SizeBytes || int64(len(data)) != length {
			return ErrConflict
		}
		result = AttachmentChunk{Attachment: a, Offset: offset, NextOffset: offset + length, EOF: offset+length == a.SizeBytes, ContentBase64: base64.StdEncoding.EncodeToString(data)}
		return nil
	})
	if err != nil {
		return AttachmentChunk{}, err
	}
	return result, nil
}

func filesSafeMetadata(value string, maxBytes int) bool {
	if len(value) > maxBytes || strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

func (s *Store) filesUploadConversation(ctx context.Context, tx *sql.Tx, actor Actor, id string) error {
	if err := s.requireMember(ctx, tx, actor, id); err != nil {
		return err
	}
	var closed bool
	if err := tx.QueryRowContext(ctx, `SELECT closed FROM conversations WHERE id=?`, id).Scan(&closed); err != nil {
		return err
	}
	if closed {
		return ErrNotFound
	}
	return nil
}

func (s *Store) filesReserveCapacity(ctx context.Context, tx *sql.Tx, additional int64) error {
	var used int64
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT coalesce(sum(size_bytes),0) FROM attachments)+(SELECT coalesce(sum(length(CAST(text AS BLOB))+length(CAST(data AS BLOB))),0) FROM messages)`).Scan(&used); err != nil {
		return err
	}
	if additional > s.opts.MaxStoreBytes || used > s.opts.MaxStoreBytes-additional {
		return ErrQuota
	}
	return nil
}

func filesScanAttachment(row *sql.Row) (Attachment, error) {
	var a Attachment
	err := row.Scan(&a.ID, &a.ConversationID, &a.Filename, &a.MediaType, &a.SizeBytes, &a.SHA256, &a.State, &a.ReceivedBytes, &a.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return a, err
}
