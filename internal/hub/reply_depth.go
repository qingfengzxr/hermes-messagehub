package hub

import (
	"context"
	"database/sql"
)

const (
	defaultReplyDepthLimit = 4
	maxReplyDepthLimit     = 32
)

func (w GrokWakeConfig) replyDepthLimit() int {
	if w.ReplyDepthLimit == 0 {
		return defaultReplyDepthLimit
	}
	return w.ReplyDepthLimit
}

func (s *Store) setReplyDepth(ctx context.Context, tx *sql.Tx, msg *Message) error {
	w := s.opts.GrokWake
	if w == nil || msg.ConversationID != w.ConversationID || (msg.Sender != w.Sender && msg.Sender != w.Recipient) {
		return nil
	}
	var dm bool
	if err := tx.QueryRowContext(ctx, `SELECT count(*)=2 AND count(CASE WHEN principal_id IN (?,?) THEN 1 END)=2 FROM members WHERE conversation_id=?`, w.Sender, w.Recipient, w.ConversationID).Scan(&dm); err != nil || !dm {
		return err
	}
	depth, limit := 0, w.replyDepthLimit()
	if msg.ReplyTo != "" {
		// Compare to the child's creation time, not the current read time. The
		// same live message must keep its depth as an ancestor later expires.
		// Missing, forward, foreign, and already-expired links stop at the limit.
		if err := tx.QueryRowContext(ctx, `WITH RECURSIVE chain(parent,seq,depth) AS (
   VALUES(?,?,0)
   UNION ALL
   SELECT coalesce(m.reply_to,''),m.seq,c.depth+1 FROM chain c JOIN messages m ON m.id=c.parent
    WHERE c.depth<? AND m.conversation_id=? AND m.seq<c.seq AND m.expires_at>? AND m.sender IN (?,?)
   ) SELECT coalesce((SELECT depth FROM chain WHERE parent='' OR depth=? LIMIT 1),?)`, msg.ReplyTo, msg.Sequence, limit, msg.ConversationID, msg.CreatedAt, w.Sender, w.Recipient, limit, limit).Scan(&depth); err != nil {
			return err
		}
	}
	msg.ReplyDepth = &depth
	if limit != defaultReplyDepthLimit {
		msg.ReplyDepthLimit = limit
	}
	return nil
}
