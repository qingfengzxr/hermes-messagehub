package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"unicode"
	"unicode/utf8"
)

func (s *Store) Identity(ctx context.Context, actor Actor) (Identity, error) {
	out := Identity{Peers: []Principal{}}
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT id,name,active FROM principals WHERE active=1 AND id<>? ORDER BY id", actor.PrincipalID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var peer Principal
			if err := rows.Scan(&peer.ID, &peer.Name, &peer.Active); err != nil {
				return err
			}
			out.Peers = append(out.Peers, peer)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out.PrincipalID = actor.PrincipalID()
		out.MaxMessageBytes = s.opts.MaxMessageBytes
		out.MaxFileBytes = s.opts.MaxFileBytes
		out.MaxChunkBytes = s.opts.MaxChunkBytes
		return nil
	})
	return out, err
}

func messageValidKey(key string) bool {
	if len(key) < 1 || len(key) > 128 || !utf8.ValidString(key) {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func messagePageLimit(req PageRequest) (int, error) {
	if req.Limit < 0 || req.Limit > 100 {
		return 0, ErrInvalid
	}
	if req.Limit == 0 {
		return 20, nil
	}
	return req.Limit, nil
}

func messageLoadConversation(ctx context.Context, tx *sql.Tx, id string) (Conversation, error) {
	out := Conversation{Members: []string{}}
	err := tx.QueryRowContext(ctx, "SELECT id,created_at,closed FROM conversations WHERE id=?", id).Scan(&out.ID, &out.CreatedAt, &out.Closed)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT principal_id FROM members WHERE conversation_id=? ORDER BY principal_id", id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var member string
		if err := rows.Scan(&member); err != nil {
			return out, err
		}
		out.Members = append(out.Members, member)
	}
	return out, rows.Err()
}

func (s *Store) CreateConversation(ctx context.Context, actor Actor, req CreateConversationRequest) (Conversation, error) {
	out := Conversation{Members: []string{}}
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		if !messageValidKey(req.IdempotencyKey) || len(req.Members) < 2 || len(req.Members) > s.opts.MaxMembers {
			return ErrInvalid
		}
		members := append([]string{}, req.Members...)
		sort.Strings(members)
		includesActor := false
		for i, id := range members {
			if !principalPattern.MatchString(id) || (i > 0 && id == members[i-1]) {
				return ErrInvalid
			}
			includesActor = includesActor || id == actor.PrincipalID()
		}
		if !includesActor {
			return ErrInvalid
		}
		body, err := json.Marshal(members)
		if err != nil {
			return err
		}
		digest := tokenDigest(string(body))
		var existingID, existingDigest string
		err = tx.QueryRowContext(ctx, "SELECT id,digest FROM conversations WHERE creator=? AND idem_key=?", actor.PrincipalID(), req.IdempotencyKey).Scan(&existingID, &existingDigest)
		if err == nil {
			if digest != existingDigest {
				return ErrConflict
			}
			out, err = messageLoadConversation(ctx, tx, existingID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, id := range members {
			var active bool
			err := tx.QueryRowContext(ctx, "SELECT active FROM principals WHERE id=?", id).Scan(&active)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if !active {
				return ErrNotFound
			}
		}
		out = Conversation{ID: newID("c"), Members: members, CreatedAt: s.now()}
		if _, err := tx.ExecContext(ctx, "INSERT INTO conversations(id,creator,idem_key,digest,created_at) VALUES(?,?,?,?,?)", out.ID, actor.PrincipalID(), req.IdempotencyKey, digest, out.CreatedAt); err != nil {
			return err
		}
		for _, id := range members {
			if _, err := tx.ExecContext(ctx, "INSERT INTO members(conversation_id,principal_id) VALUES(?,?)", out.ID, id); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) ListConversations(ctx context.Context, actor Actor, req PageRequest) (ConversationPage, error) {
	out := ConversationPage{Items: []Conversation{}}
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		limit, err := messagePageLimit(req)
		if err != nil {
			return err
		}
		scope := "conversations:" + actor.PrincipalID()
		after, err := s.openCursor(scope, req.Cursor)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT c.rowid,c.id FROM conversations c JOIN members m ON m.conversation_id=c.id
			WHERE m.principal_id=? AND c.rowid>? ORDER BY c.rowid LIMIT ?`, actor.PrincipalID(), after, limit+1)
		if err != nil {
			return err
		}
		type entry struct {
			seq int64
			id  string
		}
		entries := []entry{}
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.seq, &e.id); err != nil {
				_ = rows.Close()
				return err
			}
			entries = append(entries, e)
		}
		err = rows.Err()
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		out.HasMore = len(entries) > limit
		if out.HasMore {
			entries = entries[:limit]
		}
		for _, e := range entries {
			conv, err := messageLoadConversation(ctx, tx, e.id)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, conv)
		}
		if len(entries) > 0 {
			out.NextCursor = s.sealCursor(scope, entries[len(entries)-1].seq)
		}
		return nil
	})
	return out, err
}

// CloseConversation is a local administrative operation, not a participant API.
func (s *Store) CloseConversation(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE conversations SET closed=1 WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

func messageEmptyReceipt() SendReceipt {
	return SendReceipt{Message: Message{AttachmentIDs: []string{}}, Deliveries: []Delivery{}}
}

func messageIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	ids := []string{}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return ids, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) messageLoad(ctx context.Context, tx *sql.Tx, id string, now int64) (Message, error) {
	out := Message{AttachmentIDs: []string{}}
	var data string
	err := tx.QueryRowContext(ctx, `SELECT id,conversation_id,seq,sender,text,data,coalesce(reply_to,''),created_at,expires_at
		FROM messages WHERE id=?`, id).Scan(&out.ID, &out.ConversationID, &out.Sequence, &out.Sender, &out.Text, &data, &out.ReplyTo, &out.CreatedAt, &out.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if out.ExpiresAt <= now {
		out.Expired = true
		out.Text = ""
		out.ReplyTo = ""
		return out, nil
	}
	if data != "" {
		out.Data = json.RawMessage(data)
	}
	out.AttachmentIDs, err = messageIDs(ctx, tx, "SELECT attachment_id FROM message_attachments WHERE message_id=? ORDER BY attachment_id", id)
	if err != nil {
		return out, err
	}
	err = s.setReplyDepth(ctx, tx, &out)
	return out, err
}

func messageDelivery(ctx context.Context, tx *sql.Tx, id, recipient string, now int64) (Delivery, error) {
	out := Delivery{MessageID: id, Recipient: recipient, State: "pending"}
	var ack sql.NullInt64
	var expires int64
	err := tx.QueryRowContext(ctx, "SELECT d.seq,d.acked_at,m.expires_at FROM deliveries d JOIN messages m ON m.id=d.message_id WHERE d.message_id=? AND d.recipient=?", id, recipient).Scan(&out.Sequence, &ack, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if ack.Valid {
		out.State = "acked"
		out.AckedAt = ack.Int64
	} else if expires <= now {
		out.State = "expired"
	}
	return out, nil
}

func (s *Store) messageReceipt(ctx context.Context, tx *sql.Tx, actor Actor, id string) (SendReceipt, error) {
	out := messageEmptyReceipt()
	msg, err := s.messageLoad(ctx, tx, id, s.now())
	if err != nil {
		return out, err
	}
	if err := s.requireMember(ctx, tx, actor, msg.ConversationID); err != nil {
		return out, err
	}
	recipients, err := messageIDs(ctx, tx, "SELECT recipient FROM deliveries WHERE message_id=? AND (?=? OR recipient=?) ORDER BY recipient", id, msg.Sender, actor.PrincipalID(), actor.PrincipalID())
	if err != nil {
		return out, err
	}
	for _, recipient := range recipients {
		d, err := messageDelivery(ctx, tx, id, recipient, s.now())
		if err != nil {
			return out, err
		}
		out.Deliveries = append(out.Deliveries, d)
	}
	out.Message = msg
	out.Stored = true
	return out, nil
}

func (s *Store) Send(ctx context.Context, actor Actor, req SendRequest) (SendReceipt, error) {
	out := messageEmptyReceipt()
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		if !messageValidKey(req.IdempotencyKey) || !messageValidKey(req.ConversationID) || !utf8.ValidString(req.Text) || !utf8.Valid(req.Data) || len(req.AttachmentIDs) > 32 {
			return ErrInvalid
		}
		if req.Text == "" && len(req.Data) == 0 && len(req.AttachmentIDs) == 0 {
			return ErrInvalid
		}
		if len(req.Text) > s.opts.MaxMessageBytes || len(req.Data) > s.opts.MaxMessageBytes-len(req.Text) {
			return ErrQuota
		}
		if len(req.Data) > 0 {
			data := bytes.TrimSpace(req.Data)
			if !json.Valid(data) || len(data) == 0 || data[0] != '{' {
				return ErrInvalid
			}
		} else {
			req.Data = nil
		}
		files := append([]string{}, req.AttachmentIDs...)
		sort.Strings(files)
		for i, id := range files {
			if !messageValidKey(id) || (i > 0 && files[i-1] == id) {
				return ErrInvalid
			}
		}
		if req.ReplyTo != "" && !messageValidKey(req.ReplyTo) {
			return ErrInvalid
		}
		if err := s.requireMember(ctx, tx, actor, req.ConversationID); err != nil {
			return err
		}
		body, err := json.Marshal(struct {
			Text    string
			Data    json.RawMessage
			ReplyTo string
			Files   []string
		}{req.Text, req.Data, req.ReplyTo, files})
		if err != nil {
			return err
		}
		digest := tokenDigest(string(body))
		var existingID, existingDigest string
		err = tx.QueryRowContext(ctx, "SELECT id,digest FROM messages WHERE sender=? AND conversation_id=? AND idem_key=?", actor.PrincipalID(), req.ConversationID, req.IdempotencyKey).Scan(&existingID, &existingDigest)
		if err == nil {
			if digest != existingDigest {
				return ErrConflict
			}
			out, err = s.messageReceipt(ctx, tx, actor, existingID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		conv, err := messageLoadConversation(ctx, tx, req.ConversationID)
		if err != nil {
			return err
		}
		if conv.Closed {
			return ErrConflict
		}
		if req.ReplyTo != "" {
			var n int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE id=? AND conversation_id=?", req.ReplyTo, conv.ID).Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				return ErrNotFound
			}
		}
		now := s.now()
		for _, id := range files {
			var state string
			var expiry int64
			var attached sql.NullString
			err := tx.QueryRowContext(ctx, "SELECT state,expires_at,message_id FROM attachments WHERE id=? AND uploader=? AND conversation_id=?", id, actor.PrincipalID(), conv.ID).Scan(&state, &expiry, &attached)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if expiry <= now {
				return ErrExpired
			}
			if state != "ready" || attached.Valid {
				return ErrConflict
			}
		}
		var messageCount, messageBytes, fileBytes int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(length(CAST(text AS BLOB))+length(CAST(data AS BLOB))),0) FROM messages`).Scan(&messageCount, &messageBytes); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(sum(size_bytes),0) FROM attachments").Scan(&fileBytes); err != nil {
			return err
		}
		remaining := s.opts.MaxStoreBytes - int64(len(req.Text)) - int64(len(req.Data))
		if messageCount >= int64(s.opts.MaxMessages) || remaining < 0 || messageBytes > remaining || fileBytes > remaining-messageBytes {
			return ErrQuota
		}
		var sequence int64
		if err := tx.QueryRowContext(ctx, "UPDATE conversations SET next_seq=next_seq+1 WHERE id=? RETURNING next_seq-1", conv.ID).Scan(&sequence); err != nil {
			return err
		}
		id := newID("m")
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,conversation_id,seq,sender,idem_key,digest,text,data,reply_to,created_at,expires_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, conv.ID, sequence, actor.PrincipalID(), req.IdempotencyKey, digest, req.Text, string(req.Data), req.ReplyTo, now, now+int64(s.opts.MessageTTL.Seconds())); err != nil {
			return err
		}
		for _, file := range files {
			if _, err := tx.ExecContext(ctx, "INSERT INTO message_attachments(message_id,attachment_id) VALUES(?,?)", id, file); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE attachments SET message_id=? WHERE id=?", id, file); err != nil {
				return err
			}
		}
		for _, recipient := range conv.Members {
			if recipient == actor.PrincipalID() {
				continue
			}
			var seq int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO mailbox_sequences(principal_id,next_seq) VALUES(?,2)
				ON CONFLICT(principal_id) DO UPDATE SET next_seq=next_seq+1 RETURNING next_seq-1`, recipient).Scan(&seq); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO deliveries(message_id,recipient,seq) VALUES(?,?,?)", id, recipient, seq); err != nil {
				return err
			}
		}
		if err := s.enqueueWake(ctx, tx, conv, actor.PrincipalID(), id); err != nil {
			return err
		}
		out, err = s.messageReceipt(ctx, tx, actor, id)
		return err
	})
	if err != nil {
		return messageEmptyReceipt(), err
	}
	s.notifyInbox()
	return out, nil
}

func (s *Store) MessageStatus(ctx context.Context, actor Actor, id string) (SendReceipt, error) {
	out := messageEmptyReceipt()
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		var err error
		out, err = s.messageReceipt(ctx, tx, actor, id)
		return err
	})
	if err != nil {
		return messageEmptyReceipt(), err
	}
	return out, nil
}

func (s *Store) ReadMessages(ctx context.Context, actor Actor, conversationID string, req PageRequest) (MessagePage, error) {
	return s.readMessages(ctx, actor, conversationID, req, "")
}

const directRepliesSQL = "SELECT id FROM messages WHERE conversation_id=? AND reply_to=? AND seq>? AND expires_at>? ORDER BY seq LIMIT ?"

func (s *Store) ReadReplies(ctx context.Context, actor Actor, conversationID, parentID string, req PageRequest) (MessagePage, error) {
	if !messageValidKey(parentID) {
		return MessagePage{Items: []Message{}}, ErrInvalid
	}
	return s.readMessages(ctx, actor, conversationID, req, parentID)
}

func (s *Store) readMessages(ctx context.Context, actor Actor, conversationID string, req PageRequest, parentID string) (MessagePage, error) {
	out := MessagePage{Items: []Message{}}
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		if err := s.requireMember(ctx, tx, actor, conversationID); err != nil {
			return err
		}
		limit, err := messagePageLimit(req)
		if err != nil {
			return err
		}
		scope := "messages:" + actor.PrincipalID() + ":" + conversationID
		if parentID != "" {
			var found int
			err := tx.QueryRowContext(ctx, "SELECT 1 FROM messages WHERE id=? AND conversation_id=?", parentID, conversationID).Scan(&found)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			scope = "replies:" + actor.PrincipalID() + ":" + conversationID + ":" + parentID
		}
		after, err := s.openCursor(scope, req.Cursor)
		if err != nil {
			return err
		}
		now := s.now()
		var ids []string
		if parentID == "" {
			ids, err = messageIDs(ctx, tx, "SELECT id FROM messages WHERE conversation_id=? AND seq>? ORDER BY seq LIMIT ?", conversationID, after, limit+1)
		} else {
			ids, err = messageIDs(ctx, tx, directRepliesSQL, conversationID, parentID, after, now, limit+1)
		}
		if err != nil {
			return err
		}
		out.HasMore = len(ids) > limit
		if out.HasMore {
			ids = ids[:limit]
		}
		for _, id := range ids {
			msg, err := s.messageLoad(ctx, tx, id, now)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, msg)
		}
		if len(out.Items) > 0 {
			out.NextCursor = s.sealCursor(scope, out.Items[len(out.Items)-1].Sequence)
		}
		return nil
	})
	return out, err
}

func (s *Store) PullInbox(ctx context.Context, actor Actor, req PageRequest) (InboxPage, error) {
	out := InboxPage{Items: []InboxItem{}}
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		limit, err := messagePageLimit(req)
		if err != nil {
			return err
		}
		scope := "inbox:" + actor.PrincipalID()
		after, err := s.openCursor(scope, req.Cursor)
		if err != nil {
			return err
		}
		ids, err := messageIDs(ctx, tx, `SELECT d.message_id FROM deliveries d JOIN messages msg ON msg.id=d.message_id
			JOIN members m ON m.conversation_id=msg.conversation_id AND m.principal_id=d.recipient
			WHERE d.recipient=? AND d.acked_at IS NULL AND d.seq>? ORDER BY d.seq LIMIT ?`, actor.PrincipalID(), after, limit+1)
		if err != nil {
			return err
		}
		out.HasMore = len(ids) > limit
		if out.HasMore {
			ids = ids[:limit]
		}
		for _, id := range ids {
			msg, err := s.messageLoad(ctx, tx, id, s.now())
			if err != nil {
				return err
			}
			d, err := messageDelivery(ctx, tx, id, actor.PrincipalID(), s.now())
			if err != nil {
				return err
			}
			out.Items = append(out.Items, InboxItem{Message: msg, Delivery: d})
		}
		if len(out.Items) > 0 {
			out.NextCursor = s.sealCursor(scope, out.Items[len(out.Items)-1].Delivery.Sequence)
		}
		return nil
	})
	return out, err
}

func (s *Store) Ack(ctx context.Context, actor Actor, ids []string) (AckReceipt, error) {
	out := AckReceipt{MessageIDs: []string{}}
	err := s.transaction(ctx, actor, func(tx *sql.Tx) error {
		if len(ids) == 0 {
			return ErrInvalid
		}
		unique := append([]string{}, ids...)
		sort.Strings(unique)
		now := s.now()
		for i, id := range unique {
			if !messageValidKey(id) {
				return ErrInvalid
			}
			if i > 0 && unique[i-1] == id {
				continue
			}
			result, err := tx.ExecContext(ctx, `UPDATE deliveries SET acked_at=coalesce(acked_at,?) WHERE message_id=? AND recipient=?
				AND EXISTS(SELECT 1 FROM messages msg JOIN members m ON m.conversation_id=msg.conversation_id WHERE msg.id=deliveries.message_id AND m.principal_id=?)`, now, id, actor.PrincipalID(), actor.PrincipalID())
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return ErrNotFound
			}
			out.MessageIDs = append(out.MessageIDs, id)
		}
		out.Acknowledged = true
		return nil
	})
	if err != nil {
		return AckReceipt{MessageIDs: []string{}}, err
	}
	return out, nil
}
