package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

func waitHTTPFixture(t *testing.T, o Options) (*hub.Store, *httptest.Server, string, string, hub.Actor, hub.Actor, string) {
	t.Helper()
	f, aliceToken := fixture(t)
	s := f.Store
	ctx := context.Background()
	if err := s.AddPrincipal(ctx, "bob", "Bob"); err != nil {
		t.Fatal(err)
	}
	_, bobToken, err := s.IssueGrant(ctx, "bob", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := s.Authenticate(ctx, aliceToken)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.Authenticate(ctx, bobToken)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.CreateConversation(ctx, alice, hub.CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: "wait-conversation"})
	if err != nil {
		t.Fatal(err)
	}
	ts := waitHTTPServer(t, s, o)
	return s, ts, aliceToken, bobToken, alice, bob, conv.ID
}

func waitHTTPServer(t *testing.T, backend Backend, o Options) *httptest.Server {
	t.Helper()
	o.PublicBaseURL = "https://hub.example"
	if o.RequestsPerSecond == 0 {
		o.RequestsPerSecond = 10000
	}
	if o.Burst == 0 {
		o.Burst = 10000
	}
	app, err := New(backend, o)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(app)
	ts.Config.ReadHeaderTimeout = app.ReadHeaderTimeout
	ts.Config.ReadTimeout = app.ReadTimeout
	ts.Config.WriteTimeout = app.WriteTimeout
	ts.Config.IdleTimeout = app.IdleTimeout
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

type observedInboxWait struct {
	*hub.Store
	entered  chan struct{}
	finished chan struct{}
}

func (s *observedInboxWait) WaitInbox(ctx context.Context, a hub.Actor, r hub.WaitInboxRequest) (hub.InboxPage, error) {
	s.entered <- struct{}{}
	defer func() { s.finished <- struct{}{} }()
	return s.Store.WaitInbox(ctx, a, r)
}

type httpInboxWaitResult struct {
	status int
	page   hub.InboxPage
	err    error
}

func startHTTPInboxWait(t *testing.T, ts *httptest.Server, token string, entered <-chan struct{}) (<-chan httpInboxWaitResult, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/inbox/wait", strings.NewReader(`{"wait_ms":20000}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "hub.example"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	done := make(chan httpInboxWaitResult, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		result := httpInboxWaitResult{err: err}
		if err == nil {
			defer resp.Body.Close()
			result.status = resp.StatusCode
			if resp.StatusCode == 200 {
				result.err = json.NewDecoder(resp.Body).Decode(&result.page)
			}
		}
		done <- result
	}()
	select {
	case <-entered:
	case result := <-done:
		t.Fatalf("wait failed before admission: status=%d, err=%v", result.status, result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("wait not admitted")
	}
	return done, cancel
}

func awaitWaitFinished(t *testing.T, finished <-chan struct{}) {
	t.Helper()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled wait held server resources")
	}
}

func TestInboxWaitHTTPDoesNotStarveSendACK(t *testing.T) {
	s, _, aliceToken, bobToken, _, _, conv := waitHTTPFixture(t, Options{})
	observed := &observedInboxWait{Store: s, entered: make(chan struct{}, 4), finished: make(chan struct{}, 4)}
	ts := waitHTTPServer(t, observed, Options{MaxConcurrent: 1})
	wait, cancel := startHTTPInboxWait(t, ts, aliceToken, observed.entered)
	body, _ := json.Marshal(hub.SendRequest{ConversationID: conv, IdempotencyKey: "while-waiting", Text: "hello"})
	resp := request(t, ts, "/v1/messages/send", aliceToken, string(body), nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("wait starved Send: status=%d", resp.StatusCode)
	}
	var receipt hub.SendReceipt
	if err := json.NewDecoder(resp.Body).Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(map[string]any{"message_ids": []string{receipt.Message.ID}})
	resp = request(t, ts, "/v1/inbox/ack", bobToken, string(body), nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("wait starved ACK: status=%d", resp.StatusCode)
	}
	select {
	case <-wait:
		t.Fatal("sender wait returned an unrelated delivery")
	default:
	}
	cancel()
	awaitWaitFinished(t, observed.finished)
}

func TestInboxWaitHTTPBoundedWaitSlotsAndRelease(t *testing.T) {
	s, _, aliceToken, bobToken, _, _, _ := waitHTTPFixture(t, Options{})
	observed := &observedInboxWait{Store: s, entered: make(chan struct{}, 8), finished: make(chan struct{}, 8)}
	ts := waitHTTPServer(t, observed, Options{MaxConcurrent: 1, MaxConcurrentWaits: 2})
	_, cancelFirst := startHTTPInboxWait(t, ts, aliceToken, observed.entered)
	_, cancelSecond := startHTTPInboxWait(t, ts, bobToken, observed.entered)
	resp := request(t, ts, "/v1/inbox/wait", aliceToken, `{"wait_ms":1}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("wait quota status=%d", resp.StatusCode)
	}
	cancelFirst()
	cancelSecond()
	awaitWaitFinished(t, observed.finished)
	awaitWaitFinished(t, observed.finished)
	deadline := time.Now().Add(time.Second)
	for {
		resp = request(t, ts, "/v1/inbox/wait", aliceToken, `{"wait_ms":1}`, nil)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			break
		}
		if resp.StatusCode != 429 || time.Now().After(deadline) {
			t.Fatalf("wait slot leaked: status=%d", resp.StatusCode)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestInboxWaitOptionsAndSocketDeadline(t *testing.T) {
	for _, n := range []int{-1, 10001} {
		if _, err := normalize(Options{MaxConcurrentWaits: n}); err == nil {
			t.Fatal("invalid wait quota accepted")
		}
	}
	o, err := normalize(Options{})
	if err != nil || o.MaxConcurrentWaits != 64 {
		t.Fatal("missing bounded default wait quota")
	}
	f, _ := fixture(t)
	app, err := New(f.Store, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if app.WriteTimeout <= o.RequestTimeout || o.RequestTimeout <= 20*time.Second {
		t.Fatal("HTTP timeout cannot contain default maximum wait")
	}
}

func decodeWaitPage(t *testing.T, resp *http.Response) hub.InboxPage {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("wait status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}
	var page hub.InboxPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if page.Items == nil {
		t.Fatal("items must be an array")
	}
	return page
}

func TestInboxWaitHTTPBacklogPaginationReconnectWithoutACK(t *testing.T) {
	s, ts, _, token, alice, bob, conv := waitHTTPFixture(t, Options{})
	var ids []string
	for _, key := range []string{"one", "two"} {
		r, err := s.Send(context.Background(), alice, hub.SendRequest{ConversationID: conv, IdempotencyKey: key, Text: key})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.Message.ID)
	}
	start := time.Now()
	p := decodeWaitPage(t, request(t, ts, "/v1/inbox/wait", token, `{"wait_ms":20000,"limit":1}`, nil))
	if time.Since(start) > time.Second {
		t.Fatal("backlog waited")
	}
	if len(p.Items) != 1 || p.Items[0].Message.ID != ids[0] || !p.HasMore || p.NextCursor == "" {
		t.Fatal("incorrect first page")
	}
	body, _ := json.Marshal(map[string]any{"wait_ms": 20000, "limit": 1, "cursor": p.NextCursor})
	p = decodeWaitPage(t, request(t, ts, "/v1/inbox/wait", token, string(body), nil))
	if len(p.Items) != 1 || p.Items[0].Message.ID != ids[1] || p.HasMore {
		t.Fatal("incorrect second page")
	}
	p = decodeWaitPage(t, request(t, ts, "/v1/inbox/wait", token, `{"wait_ms":1}`, nil))
	if len(p.Items) != 2 {
		t.Fatal("reconnect lost pending deliveries")
	}
	pending, err := s.PullInbox(context.Background(), bob, hub.PageRequest{})
	if err != nil || len(pending.Items) != 2 {
		t.Fatalf("wait acknowledged inbox: %v", err)
	}
	for _, item := range pending.Items {
		if item.Delivery.State != "pending" {
			t.Fatal("wait changed delivery state")
		}
	}
}

func TestInboxWaitHTTPStrictJSON(t *testing.T) {
	_, ts, _, token, _, _, _ := waitHTTPFixture(t, Options{})
	for _, body := range []string{
		`{}`, `{"wait_ms":0}`, `{"wait_ms":-1}`, `{"wait_ms":20001}`, `{"wait_ms":1.5}`,
		`{"wait_ms":null}`, `{"wait_ms":"1"}`, `{"wait_ms":1,"limit":101}`, `{"wait_ms":1,"limit":-1}`,
		`{"wait_ms":1,"unknown":true}`, `{"wait_ms":1,"wait_ms":2}`, `{"wait_ms":1} {}`,
		`{"WAIT_MS":1}`, `{"wait_ms":1,"limit":null}`, `{"wait_ms":1,"cursor":null}`, `[]`, `null`,
	} {
		t.Run(body, func(t *testing.T) {
			resp := request(t, ts, "/v1/inbox/wait", token, body, nil)
			defer resp.Body.Close()
			if resp.StatusCode != 400 {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
		})
	}
}

func TestInboxWaitHTTPTimeoutAndRequestDeadline(t *testing.T) {
	t.Run("empty timeout is page", func(t *testing.T) {
		_, ts, _, token, _, _, _ := waitHTTPFixture(t, Options{})
		start := time.Now()
		p := decodeWaitPage(t, request(t, ts, "/v1/inbox/wait", token, `{"wait_ms":40}`, nil))
		if time.Since(start) < 35*time.Millisecond || time.Since(start) > time.Second {
			t.Fatal("wait not bounded by requested duration")
		}
		if len(p.Items) != 0 || p.HasMore {
			t.Fatal("nonempty timeout")
		}
	})
	t.Run("request deadline wins", func(t *testing.T) {
		_, ts, _, token, _, _, _ := waitHTTPFixture(t, Options{RequestTimeout: 40 * time.Millisecond})
		start := time.Now()
		resp := request(t, ts, "/v1/inbox/wait", token, `{"wait_ms":20000}`, nil)
		defer resp.Body.Close()
		if resp.StatusCode != 504 {
			t.Fatalf("status = %d, want deadline 504", resp.StatusCode)
		}
		if time.Since(start) > time.Second {
			t.Fatal("ignored request deadline")
		}
	})
}

func TestInboxWaitHTTPWakeRevokeAndCrossUserCursor(t *testing.T) {
	s, _, aliceToken, bobToken, alice, bob, conv := waitHTTPFixture(t, Options{})
	observed := &observedInboxWait{Store: s, entered: make(chan struct{}, 8), finished: make(chan struct{}, 8)}
	ts := waitHTTPServer(t, observed, Options{MaxConcurrent: 1, MaxConcurrentWaits: 2})
	done, _ := startHTTPInboxWait(t, ts, bobToken, observed.entered)
	r, err := s.Send(context.Background(), alice, hub.SendRequest{ConversationID: conv, IdempotencyKey: "wake", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var p hub.InboxPage
	select {
	case result := <-done:
		if result.err != nil || result.status != 200 || len(result.page.Items) != 1 || result.page.Items[0].Message.ID != r.Message.ID {
			t.Fatalf("HTTP wake status=%d, err=%v", result.status, result.err)
		}
		p = result.page
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP wait missed commit")
	}
	awaitWaitFinished(t, observed.finished)
	body, _ := json.Marshal(map[string]any{"wait_ms": 1, "cursor": p.NextCursor})
	resp := request(t, ts, "/v1/inbox/wait", aliceToken, string(body), nil)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("cross-user cursor accepted: %d", resp.StatusCode)
	}
	<-observed.entered
	awaitWaitFinished(t, observed.finished)
	if _, err := s.Ack(context.Background(), bob, []string{r.Message.ID}); err != nil {
		t.Fatal(err)
	}
	done, _ = startHTTPInboxWait(t, ts, bobToken, observed.entered)
	if err := s.RevokeGrant(context.Background(), bob.GrantID()); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.status != 401 {
			t.Fatalf("HTTP revocation status=%d", result.status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP wait cached revoked actor")
	}
	awaitWaitFinished(t, observed.finished)
}

func TestInboxWaitHTTPRateAndMethod(t *testing.T) {
	s, _, _, token, _, _, _ := waitHTTPFixture(t, Options{})
	ts := waitHTTPServer(t, s, Options{RequestsPerSecond: 0.001, Burst: 1})
	decodeWaitPage(t, request(t, ts, "/v1/inbox/wait", token, `{"wait_ms":1}`, nil))
	resp := request(t, ts, "/v1/inbox/wait", token, `{"wait_ms":1}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "1" {
		t.Fatal("wait bypassed shared rate limit")
	}
	ts = waitHTTPServer(t, s, Options{})
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/inbox/wait", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "hub.example"
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("wait allowed GET or omitted no-store")
	}
}

func TestInboxWaitHTTPMaximumDuration(t *testing.T) {
	if testing.Short() {
		t.Skip("real 20 second wait")
	}
	_, ts, _, token, _, _, _ := waitHTTPFixture(t, Options{})
	start := time.Now()
	p := decodeWaitPage(t, request(t, ts, "/v1/inbox/wait", token, `{"wait_ms":20000}`, nil))
	elapsed := time.Since(start)
	if len(p.Items) != 0 || elapsed < 20*time.Second || elapsed > 22*time.Second {
		t.Fatalf("maximum wait elapsed=%v", elapsed)
	}
	t.Logf("maximum-duration empty response: %v", elapsed)
}

func TestInboxWaitHTTPBoundary(t *testing.T) {
	_, ts, _, token, _, _, _ := waitHTTPFixture(t, Options{MaxBodyBytes: 128})
	for _, tc := range []struct {
		name, credential, body string
		headers                map[string]string
		status                 int
	}{
		{"bearer", "", `{"wait_ms":1}`, nil, 401},
		{"host", token, `{"wait_ms":1}`, map[string]string{"Host": "evil.example"}, 403},
		{"origin", token, `{"wait_ms":1}`, map[string]string{"Origin": "https://evil.example"}, 403},
		{"body", token, strings.Repeat(" ", 129), nil, 413},
		{"content type", token, `{"wait_ms":1}`, map[string]string{"Content-Type": "text/plain"}, 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := request(t, ts, "/v1/inbox/wait", tc.credential, tc.body, tc.headers)
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
		})
	}
}
