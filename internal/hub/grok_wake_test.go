package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const wakeTestURL = "https://api2.cursor.sh/automations/webhook/00000000-0000-0000-0000-000000000000"
const wakeTestToken = "synthetic-test-token"

func wakeFixture(t *testing.T) (*Store, Options, Actor, Actor, Actor, Conversation) {
	t.Helper()
	base := testStore(t)
	a, b, c := messageActors(t, base)
	dm := messageConversation(t, base, a, "configured", "alice", "bob")
	// Make the database look like an upgrade from the existing version.
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	opts := Options{Now: func() time.Time { return now }, GrokWake: &GrokWakeConfig{Generation: "g1", Sender: "alice", Recipient: "bob", ConversationID: dm.ID, CredentialFile: filepath.Join(t.TempDir(), "credential.json")}}
	s := reopenWake(t, base.path, opts)
	return s, opts, a, b, c, dm
}

func reopenWake(t *testing.T, path string, opts Options) *Store {
	t.Helper()
	s, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func writeWakeCredential(t *testing.T, path, url, token string, mode os.FileMode) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"url": url, "token": token})
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
}

func wakeSQL(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func wakeState(t *testing.T, s *Store, id string) string {
	t.Helper()
	var state string
	if err := s.db.QueryRow("SELECT state FROM grok_wake_outbox WHERE message_id=?", id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestGrokWakeOnlyNewAuthenticDirectedDMAndAtomicRollback(t *testing.T) {
	s, opts, a, b, c, dm := wakeFixture(t)
	ctx := context.Background()
	other := messageConversation(t, s, a, "other", "alice", "bob")
	group := messageConversation(t, s, a, "group", "alice", "bob", "carol")
	for _, m := range []Message{
		messageSend(t, s, b, dm.ID, "incoming", "incoming").Message,
		messageSend(t, s, a, group.ID, "group", "group").Message,
		messageSend(t, s, a, other.ID, "other", "other").Message,
	} {
		if m.ID == "" {
			t.Fatal("fixture not stored")
		}
	}
	if _, err := s.Send(ctx, c, SendRequest{ConversationID: dm.ID, IdempotencyKey: "outsider", Text: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	wakeSQL(t, s, `CREATE TRIGGER fail_wake BEFORE INSERT ON grok_wake_outbox BEGIN SELECT RAISE(ABORT,'synthetic rollback'); END`)
	req := SendRequest{ConversationID: dm.ID, IdempotencyKey: "finish", Text: "terminal", Data: json.RawMessage(`{"reply_requested":false,"sender":"carol"}`)}
	if _, err := s.Send(ctx, a, req); err == nil {
		t.Fatal("outbox failure did not roll back send")
	}
	pending, err := s.PullInbox(ctx, b, PageRequest{})
	if err != nil || len(pending.Items) != 2 {
		t.Fatalf("rollback left a delivery: %v, %v", len(pending.Items), err)
	}
	wakeSQL(t, s, `DROP TRIGGER fail_wake`)
	r, err := s.Send(ctx, a, req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Send(ctx, a, req)
	if err != nil || r2.Message.ID != r.Message.ID {
		t.Fatalf("idempotent retry: %+v, %v", r2, err)
	}
	var count, version int
	if e := s.db.QueryRow("SELECT count(*) FROM grok_wake_outbox").Scan(&count); e != nil || count != 1 {
		t.Fatalf("outbox count=%d, %v", count, e)
	}
	if e := s.db.QueryRow("PRAGMA user_version").Scan(&version); e != nil || version != 1 {
		t.Fatalf("user_version=%d, %v", version, e)
	}
	if got := wakeState(t, s, r.Message.ID); got != "queued" {
		t.Fatal(got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	opts.GrokWake = nil
	off := reopenWake(t, s.path, opts)
	offMessage := messageSend(t, off, a, dm.ID, "off", "stored without a wake").Message
	stop, err := off.StartWake(ctx, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if got := wakeState(t, off, r.Message.ID); got != "skipped" {
		t.Fatal(got)
	}
	if err := off.Close(); err != nil {
		t.Fatal(err)
	}
	on := reopenWake(t, s.path, s.opts)
	if _, err := on.Send(ctx, a, SendRequest{ConversationID: dm.ID, IdempotencyKey: "off", Text: "stored without a wake"}); err != nil {
		t.Fatal(err)
	}
	if e := on.db.QueryRow("SELECT count(*) FROM grok_wake_outbox WHERE message_id=?", offMessage.ID).Scan(&count); e != nil || count != 0 {
		t.Fatalf("history was backfilled: %d %v", count, e)
	}
}

func TestGrokWakeReplyDepthStableAndBoundToDM(t *testing.T) {
	s, _, a, b, c, dm := wakeFixture(t)
	ctx := context.Background()
	expect := func(msg Message, want string) {
		t.Helper()
		raw, _ := json.Marshal(msg)
		var obj map[string]json.RawMessage
		_ = json.Unmarshal(raw, &obj)
		if got := string(obj["reply_depth"]); got != want {
			t.Fatalf("reply_depth = %q, want %q", got, want)
		}
	}
	send := func(actor Actor, conv, key, parent string) Message {
		t.Helper()
		r, err := s.Send(ctx, actor, SendRequest{ConversationID: conv, IdempotencyKey: key, ReplyTo: parent, Data: json.RawMessage(`{"reply_depth":99,"reply_requested":true}`)})
		if err != nil {
			t.Fatal(err)
		}
		return r.Message
	}
	root := send(a, dm.ID, "r", "")
	expect(root, "0")
	prev := root
	var first Message
	for i, actor := range []Actor{b, a, b, a, b} {
		prev = send(actor, dm.ID, fmt.Sprintf("reply%d", i), prev.ID)
		expect(prev, fmt.Sprint(min(i+1, 4)))
		if i == 0 {
			first = prev
		}
	}
	for _, actor := range []Actor{a, b} {
		inbox, err := s.PullInbox(ctx, actor, PageRequest{})
		if err != nil || len(inbox.Items) != 3 {
			t.Fatalf("inbox %d %v", len(inbox.Items), err)
		}
		for _, item := range inbox.Items {
			expect(item.Message, fmt.Sprint(min(item.Message.Sequence-1, 4)))
		}
	}
	wakeSQL(t, s, `UPDATE messages SET expires_at=? WHERE id=?`, root.CreatedAt+1, root.ID)
	s.opts.Now = func() time.Time { return time.Unix(root.CreatedAt+2, 0) }
	got, err := s.MessageStatus(ctx, a, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	expect(got.Message, "1")
	expect(send(b, dm.ID, "late", root.ID), "4")
	wakeSQL(t, s, `UPDATE messages SET reply_to='m_missing' WHERE id=?`, first.ID)
	got, err = s.MessageStatus(ctx, b, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	expect(got.Message, "4")
	wakeSQL(t, s, `UPDATE messages SET reply_to=? WHERE id=?`, prev.ID, first.ID)
	got, err = s.MessageStatus(ctx, b, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	expect(got.Message, "4")
	other := messageConversation(t, s, a, "other", "alice", "bob")
	expect(send(a, other.ID, "other-root", ""), "")
	if _, err := s.MessageStatus(ctx, c, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider read: %v", err)
	}
}

type wakeTestTransport func(*http.Request) (*http.Response, error)

func (f wakeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func wakeWaitState(t *testing.T, s *Store, id, want string) {
	t.Helper()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if wakeState(t, s, id) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("wake state=%s, want %s", wakeState(t, s, id), want)
}

func TestGrokWakeRealTLSWorkerSendsOnlyEnvelopeAndJoins(t *testing.T) {
	s, opts, a, b, _, dm := wakeFixture(t)
	ctx := context.Background()
	requests := make(chan string, 2)
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/automations/webhook/00000000-0000-0000-0000-000000000000", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- r.Method + " " + r.Header.Get("Authorization") + " " + r.Header.Get("Content-Type") + " " + string(body)
		select {
		case <-release:
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	})
	tlsServer := httptest.NewTLSServer(mux)
	defer tlsServer.Close()
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	production := wakeHTTPClient(nil)
	tr := production.Transport.(*http.Transport)
	addr := tlsServer.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "api2.cursor.sh:443" {
			return nil, errors.New("unexpected destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	tr.TLSClientConfig.ServerName = "example.com" // isolate the trust-chain check from hostname mismatch
	if got := postWake(ctx, production, wakeCredential{URL: wakeTestURL, Token: wakeTestToken}, "m_untrusted"); got != wakeUncertain {
		t.Fatalf("untrusted TLS got %s", got)
	}
	select {
	case <-requests:
		t.Fatal("request passed TLS without a trusted certificate")
	default:
	}
	// New transport so configuration is immutable after the first use.
	trusted := tr.Clone()
	trusted.TLSClientConfig.RootCAs = tlsServer.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	writeWakeCredential(t, opts.GrokWake.CredentialFile, wakeTestURL, wakeTestToken, 0600)
	stop, err := s.startWake(ctx, slog.Default(), trusted)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	result := messageSend(t, s, a, dm.ID, "wake", "synthetic private body")
	select {
	case got := <-requests:
		want := `POST Bearer ` + wakeTestToken + ` application/json {"event":"messagehub.new_message","message_id":"` + result.Message.ID + `"}`
		if got != want {
			t.Fatalf("unexpected envelope: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not wake after commit")
	}
	opCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if page, err := s.PullInbox(opCtx, b, PageRequest{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("pull blocked by network: %v %v", len(page.Items), err)
	}
	close(release)
	wakeWaitState(t, s, result.Message.ID, "accepted")
	stop()
}

func TestGrokWakeStopJoinsAndPersistsInFlightAsUncertain(t *testing.T) {
	s, opts, a, _, _, dm := wakeFixture(t)
	ctx := context.Background()
	writeWakeCredential(t, opts.GrokWake.CredentialFile, wakeTestURL, wakeTestToken, 0600)
	id := messageSend(t, s, a, dm.ID, "shutdown", "synthetic").Message.ID
	started := make(chan struct{})
	stop, err := s.startWake(ctx, slog.Default(), wakeTestTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("startup scan did not send committed job")
	}
	stop()
	if got := wakeState(t, s, id); got != "uncertain" {
		t.Fatal(got)
	}
}

func TestGrokWakeRestartSkipsIneligibleAndDoesNotBypassRate(t *testing.T) {
	s, opts, a, b, _, dm := wakeFixture(t)
	ctx := context.Background()
	id := func(key string) string { return messageSend(t, s, a, dm.ID, key, "x").Message.ID }
	crashed, acked, expired, old, next := id("crashed"), id("acked"), id("expired"), id("old"), id("next")
	wakeSQL(t, s, `UPDATE grok_wake_outbox SET state='sending' WHERE message_id=?`, crashed)
	if _, err := s.Ack(ctx, b, []string{acked}); err != nil {
		t.Fatal(err)
	}
	wakeSQL(t, s, `UPDATE messages SET expires_at=created_at WHERE id=?`, expired)
	wakeSQL(t, s, `UPDATE grok_wake_outbox SET generation='old' WHERE message_id=?`, old)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = reopenWake(t, s.path, opts)
	writeWakeCredential(t, opts.GrokWake.CredentialFile, wakeTestURL, wakeTestToken, 0600)
	called := make(chan struct{}, 1)
	stop, err := s.startWake(ctx, slog.Default(), wakeTestTransport(func(*http.Request) (*http.Response, error) { called <- struct{}{}; return nil, io.ErrUnexpectedEOF }))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for _, v := range []string{acked, expired, old} {
		wakeWaitState(t, s, v, "skipped")
	}
	if got := wakeState(t, s, crashed); got != "uncertain" {
		t.Fatal(got)
	}
	if got := wakeState(t, s, next); got != "queued" {
		t.Fatal(got)
	}
	select {
	case <-called:
		t.Fatal("restart bypassed hard rate limit")
	default:
	}
	if err := s.CloseConversation(ctx, dm.ID); err != nil {
		t.Fatal(err)
	}
	wakeWaitState(t, s, next, "skipped")
}

func TestGrokWakeOnlyRetriesProvenDialFailuresWithBoundedBackoff(t *testing.T) {
	s, _, a, _, _, dm := wakeFixture(t)
	ctx := context.Background()
	now := s.opts.Now()
	s.opts.Now = func() time.Time { return now }
	id := messageSend(t, s, a, dm.ID, "retry", "synthetic").Message.ID
	client := wakeHTTPClient(wakeTestTransport(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	}))
	for attempt, delay := range []time.Duration{30 * time.Second, 60 * time.Second, 0} {
		job, err := s.claimWake(ctx)
		if err != nil || job.id != id || job.attempts != attempt+1 {
			t.Fatalf("attempt %d: %+v, %v", attempt+1, job, err)
		}
		if err := s.finishWake(ctx, job, postWake(ctx, client, wakeCredential{URL: wakeTestURL, Token: wakeTestToken}, job.id)); err != nil {
			t.Fatal(err)
		}
		if delay != 0 {
			now = now.Add(delay - time.Nanosecond)
			if job, err := s.claimWake(ctx); err != nil || job.id != "" {
				t.Fatalf("retried before backoff: %+v, %v", job, err)
			}
			now = now.Add(time.Nanosecond)
		}
	}
	if got := wakeState(t, s, id); got != "rejected" {
		t.Fatal(got)
	}
	now = now.Add(10 * time.Minute)
	if job, err := s.claimWake(ctx); err != nil || job.id != "" {
		t.Fatalf("exceeded attempt budget: %+v, %v", job, err)
	}

	id = messageSend(t, s, a, dm.ID, "disable", "synthetic").Message.ID
	if err := s.SetPrincipalActive(ctx, "bob", false); err != nil {
		t.Fatal(err)
	}
	if job, err := s.claimWake(ctx); err != nil || job.id != "" {
		t.Fatalf("disabled recipient still woke: %+v, %v", job, err)
	}
	if got := wakeState(t, s, id); got != "skipped" {
		t.Fatal(got)
	}
}

func TestGrokWakeAttemptClassificationAndNoReplay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		err    error
		want   wakeOutcome
	}{
		{"accepted", 200, nil, wakeAccepted}, {"not accepted", 201, nil, wakeRejected}, {"redirect", 302, nil, wakeRejected},
		{"gateway ambiguous", 503, nil, wakeUncertain}, {"lost response", 0, io.ErrUnexpectedEOF, wakeUncertain},
		{"preconnection refused", 0, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, wakeRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := wakeHTTPClient(wakeTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.GetBody != nil || r.ContentLength <= 0 || r.Header.Get("Idempotency-Key") != "" {
					t.Fatal("request can be replayed")
				}
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": []string{"https://example.invalid/redirect"}}, Body: io.NopCloser(strings.NewReader("untrusted"))}, nil
			}))
			if got := postWake(context.Background(), client, wakeCredential{URL: wakeTestURL, Token: wakeTestToken}, "m_test"); got != tc.want || calls != 1 {
				t.Fatalf("outcome %s calls %d", got, calls)
			}
		})
	}
}

func TestGrokWakeCredentialOnlyPrivateFixedOrigin(t *testing.T) {
	dir := t.TempDir()
	for i, url := range []string{
		wakeTestURL, wakeTestURL + "?", wakeTestURL + "#", strings.Replace(wakeTestURL, "https:", "http:", 1),
		strings.Replace(wakeTestURL, "api2.cursor.sh", "api2.cursor.sh:443", 1), strings.Replace(wakeTestURL, "api2.cursor.sh", "api2.cursor.sh@127.0.0.1", 1),
		strings.Replace(wakeTestURL, "/automations/", "/%61utomations/", 1), "https://127.0.0.1:1234/automations/webhook/00000000-0000-0000-0000-000000000000",
	} {
		path := filepath.Join(dir, fmt.Sprint(i))
		writeWakeCredential(t, path, url, wakeTestToken, 0600)
		cred, err := loadWakeCredential(path)
		if i == 0 {
			if err != nil || cred.URL != url || cred.Token != wakeTestToken {
				t.Fatal("valid credential rejected")
			}
		} else if err == nil || strings.Contains(err.Error(), url) || strings.Contains(err.Error(), wakeTestToken) {
			t.Fatal("invalid credential accepted or disclosed")
		}
	}
	public := filepath.Join(dir, "public")
	writeWakeCredential(t, public, wakeTestURL, wakeTestToken, 0644)
	if _, err := loadWakeCredential(public); err == nil {
		t.Fatal("public credential accepted")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "0"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWakeCredential(link); err == nil {
		t.Fatal("credential symlink accepted")
	}
}
