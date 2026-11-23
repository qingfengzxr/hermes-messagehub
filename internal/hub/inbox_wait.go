package hub

import (
	"context"
	"time"
)

// WaitInbox reads pending deliveries without acknowledging them. Every scan
// revalidates the actor and scoped cursor in a short PullInbox transaction.
func (s *Store) WaitInbox(ctx context.Context, actor Actor, req WaitInboxRequest) (InboxPage, error) {
	if req.WaitMS < 1 || req.WaitMS > 20000 {
		return InboxPage{}, ErrInvalid
	}
	timer := time.NewTimer(time.Duration(req.WaitMS) * time.Millisecond)
	defer timer.Stop()
	recheck := time.NewTicker(250 * time.Millisecond)
	defer recheck.Stop()
	for {
		// Subscribe before scanning so a commit between scan and sleep cannot
		// be lost. Notifications only accelerate authoritative reads.
		changed := s.inboxChange()
		if err := ctx.Err(); err != nil {
			return InboxPage{}, err
		}
		page, err := s.PullInbox(ctx, actor, req.PageRequest)
		if err != nil || len(page.Items) != 0 {
			return page, err
		}
		select {
		case <-ctx.Done():
			return InboxPage{}, ctx.Err()
		case <-timer.C:
			if err := ctx.Err(); err != nil {
				return InboxPage{}, err
			}
			// A short wait may expire before the periodic check. Revalidate
			// authority and pending state once more instead of returning a snapshot.
			return s.PullInbox(ctx, actor, req.PageRequest)
		case <-changed:
		case <-recheck.C:
		}
	}
}

func (s *Store) inboxChange() <-chan struct{} {
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	if s.inboxChanged == nil {
		s.inboxChanged = make(chan struct{})
	}
	return s.inboxChanged
}

// notifyInbox is called only after Send has committed. One broadcast generation
// per store avoids retaining per-principal subscriptions after cancellation.
func (s *Store) notifyInbox() {
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	if s.inboxChanged != nil {
		close(s.inboxChanged)
		s.inboxChanged = nil
	}
}
