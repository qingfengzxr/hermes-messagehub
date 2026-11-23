package hub

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// GrokWakeConfig binds one outbound route to a fixed two-principal conversation.
// It contains no URL or bearer token.
type GrokWakeConfig struct {
	Generation      string `json:"generation"`
	Sender          string `json:"sender"`
	Recipient       string `json:"recipient"`
	ConversationID  string `json:"conversation_id"`
	CredentialFile  string `json:"credential_file"`
	ReplyDepthLimit int    `json:"reply_depth_limit,omitempty"`
}

func (w GrokWakeConfig) Valid() bool {
	limit := w.replyDepthLimit()
	return limit >= defaultReplyDepthLimit && limit <= maxReplyDepthLimit &&
		messageValidKey(w.Generation) && principalPattern.MatchString(w.Sender) && principalPattern.MatchString(w.Recipient) && w.Sender != w.Recipient && messageValidKey(w.ConversationID) && filepath.IsAbs(w.CredentialFile) && filepath.Clean(w.CredentialFile) != "/" && !strings.ContainsRune(w.CredentialFile, 0)
}

const (
	wakeInterval     = 10 * time.Second
	wakeScanInterval = time.Second
	wakeAttempts     = 3
	wakeBackoff      = 30 * time.Second
)

const wakeSchema = `CREATE TABLE IF NOT EXISTS grok_wake_outbox (
 message_id TEXT PRIMARY KEY REFERENCES messages(id),
 conversation_id TEXT NOT NULL,
 sender TEXT NOT NULL,
 recipient TEXT NOT NULL,
 generation TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('queued','sending','accepted','rejected','uncertain','skipped')),
 attempts INTEGER NOT NULL DEFAULT 0,
 ready_at INTEGER NOT NULL DEFAULT 0,
 last_attempt_at INTEGER
);
CREATE INDEX IF NOT EXISTS grok_wake_queue ON grok_wake_outbox (ready_at,message_id) WHERE state='queued';
CREATE INDEX IF NOT EXISTS grok_wake_route ON grok_wake_outbox (conversation_id,sender,recipient,last_attempt_at);`

func (s *Store) enqueueWake(ctx context.Context, tx *sql.Tx, conv Conversation, sender, id string) error {
	w := s.opts.GrokWake
	if w == nil || conv.ID != w.ConversationID || sender != w.Sender || len(conv.Members) != 2 ||
		!((conv.Members[0] == w.Sender && conv.Members[1] == w.Recipient) || (conv.Members[1] == w.Sender && conv.Members[0] == w.Recipient)) {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO grok_wake_outbox(message_id,conversation_id,sender,recipient,generation,state) VALUES(?,?,?,?,?,'queued')`, id, conv.ID, sender, w.Recipient, w.Generation)
	return err
}

func (s *Store) hasWakeTable(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='grok_wake_outbox')`).Scan(&exists)
	return exists, err
}

// StartWake is called once by serve. The returned function cancels and joins
// the worker; callers must use it before closing the Store.
func (s *Store) StartWake(ctx context.Context, log *slog.Logger) (func(), error) {
	return s.startWake(ctx, log, nil)
}

func (s *Store) startWake(ctx context.Context, log *slog.Logger, transport http.RoundTripper) (func(), error) {
	w := s.opts.GrokWake
	exists, err := s.hasWakeTable(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return func() {}, nil
	}
	var cred wakeCredential
	if w != nil {
		cred, err = loadWakeCredential(w.CredentialFile)
		if err != nil {
			return nil, err
		}
	}
	if err := s.recoverWake(ctx, w == nil); err != nil {
		return nil, err
	}
	if w == nil {
		return func() {}, nil
	}
	client := wakeHTTPClient(transport)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer client.CloseIdleConnections()
		s.runWake(ctx, log, client, cred)
	}()
	return func() { cancel(); <-done }, nil
}

func (s *Store) recoverWake(ctx context.Context, disabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The process might have sent the POST just before it died. Recover its
	// rate reservation as of now, not as of when it originally claimed the job.
	if _, err = tx.ExecContext(ctx, `UPDATE grok_wake_outbox SET state='uncertain',last_attempt_at=max(coalesce(last_attempt_at,0),?) WHERE state='sending'`, s.opts.Now().UnixNano()); err != nil {
		return err
	}
	if disabled {
		if _, err = tx.ExecContext(ctx, `UPDATE grok_wake_outbox SET state='skipped' WHERE state='queued'`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type wakeJob struct {
	id       string
	attempts int
}

func (s *Store) claimWake(ctx context.Context) (wakeJob, error) {
	var job wakeJob
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM grok_wake_outbox WHERE state='queued')`).Scan(&exists); err != nil || !exists {
		return job, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return job, err
	}
	defer tx.Rollback()
	w := s.opts.GrokWake
	now := s.opts.Now()
	// Check the route, membership, active identities, and actual pending delivery
	// together with the durable claim. No request can run in this transaction.
	_, err = tx.ExecContext(ctx, `UPDATE grok_wake_outbox AS o SET state='skipped' WHERE state='queued' AND (
  generation<>? OR conversation_id<>? OR sender<>? OR recipient<>? OR NOT EXISTS (
   SELECT 1 FROM messages m JOIN conversations c ON c.id=m.conversation_id
   JOIN deliveries d ON d.message_id=m.id AND d.recipient=o.recipient
   JOIN members ms ON ms.conversation_id=c.id AND ms.principal_id=o.sender
   JOIN members mr ON mr.conversation_id=c.id AND mr.principal_id=o.recipient
   JOIN principals ps ON ps.id=ms.principal_id JOIN principals pr ON pr.id=mr.principal_id
   WHERE m.id=o.message_id AND m.conversation_id=o.conversation_id AND m.sender=o.sender
     AND c.closed=0 AND ps.active=1 AND pr.active=1 AND m.expires_at>? AND d.acked_at IS NULL
     AND (SELECT count(*) FROM members WHERE conversation_id=c.id)=2
  ))`, w.Generation, w.ConversationID, w.Sender, w.Recipient, now.Unix())
	if err != nil {
		return job, err
	}
	err = tx.QueryRowContext(ctx, `UPDATE grok_wake_outbox SET state='sending',attempts=attempts+1,last_attempt_at=?
  WHERE message_id=(SELECT message_id FROM grok_wake_outbox WHERE state='queued' AND ready_at<=?
   AND NOT EXISTS (SELECT 1 FROM grok_wake_outbox WHERE conversation_id=? AND sender=? AND recipient=? AND last_attempt_at>?)
   ORDER BY ready_at,message_id LIMIT 1)
  RETURNING message_id,attempts`, now.UnixNano(), now.UnixNano(), w.ConversationID, w.Sender, w.Recipient, now.Add(-wakeInterval).UnixNano()).Scan(&job.id, &job.attempts)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return wakeJob{}, err
	}
	return job, tx.Commit()
}

func (s *Store) finishWake(ctx context.Context, job wakeJob, outcome wakeOutcome) error {
	now := s.opts.Now()
	state, ready := outcome, int64(0)
	if outcome == wakeRetry {
		if job.attempts >= wakeAttempts {
			state = wakeRejected
		} else {
			state = "queued"
			ready = now.Add(wakeBackoff * time.Duration(job.attempts)).UnixNano()
		}
	}
	// Reserve an interval after completion too: a process pause between claim
	// and actual POST must not let the next attempt use up an old reservation.
	_, err := s.db.ExecContext(ctx, `UPDATE grok_wake_outbox SET state=?,ready_at=?,last_attempt_at=max(last_attempt_at,?) WHERE message_id=? AND state='sending'`, state, ready, now.UnixNano(), job.id)
	return err
}

func (s *Store) runWake(ctx context.Context, log *slog.Logger, client *http.Client, cred wakeCredential) {
	tick := time.NewTicker(wakeScanInterval)
	defer tick.Stop()
	for ctx.Err() == nil {
		changed := s.inboxChange() // subscribe before checking for just-committed sends
		job, err := s.claimWake(ctx)
		if err != nil && ctx.Err() == nil {
			log.Error("grok wake outbox scan failed")
		}
		if err == nil && job.id != "" {
			outcome := postWake(ctx, client, cred, job.id)
			// Persist the result even when the network context was canceled on stop.
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err := s.finishWake(finishCtx, job, outcome)
			cancel()
			if err != nil {
				log.Error("grok wake result could not be recorded; worker stopped")
				return // leave the durable claim for conservative restart recovery
			}
			continue
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-tick.C:
		}
	}
}

// WakeCounts exposes only aggregate outbox states to local administration.
func (s *Store) WakeCounts(ctx context.Context) (map[string]int, error) {
	counts := map[string]int{}
	exists, err := s.hasWakeTable(ctx)
	if err != nil || !exists {
		return counts, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT state,count(*) FROM grok_wake_outbox GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		counts[state] = n
	}
	return counts, rows.Err()
}
