package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestReportWriteFailureCannotLeavePASS(t *testing.T) {
	evidence := t.TempDir()
	a, err := newApp(context.Background(), featureFixture(t), evidence)
	if err != nil {
		t.Fatal(err)
	}
	defer a.cleanup()
	a.report.Outcome = "PASS"
	if err := os.Remove(evidence); err != nil {
		t.Fatal(err)
	}
	if err := a.writeReport(); err == nil {
		t.Fatal("report write should fail")
	}
	if a.report.Outcome != "ISSUES" {
		t.Fatal("failed report persistence still claims PASS")
	}
}

func TestTransportPacesRequestsWithoutRetryingWrites(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		w.WriteHeader(429)
	}))
	defer ts.Close()
	c, err := privateHTTPClient(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	gate := c.Transport.(*guardedTransport)
	var admissions []time.Time
	for i := 0; i < 3; i++ {
		res, err := c.Post(ts.URL+"/write", "application/json", bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		gate.mu.Lock()
		admissions = append(admissions, gate.last)
		gate.mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(arrivals) != 3 {
		t.Fatal("write was blindly retried")
	}
	// The gate controls client admission, not OS scheduling of server arrivals.
	for i := 1; i < len(admissions); i++ {
		if admissions[i].Sub(admissions[i-1]) < 10*time.Millisecond {
			t.Fatal("no explicit request pacing below burst")
		}
	}
}
func TestDoctorMismatchDoesNotSerializeIdentityResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Write([]byte(`{"principal_id":"PRIVATE-RESPONSE-SENTINEL"}`))
	}))
	defer ts.Close()
	a, err := newApp(context.Background(), featureFixture(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.cleanup()
	a.baseURL = ts.URL
	a.http, err = privateHTTPClient(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	a.ops = []operation{{Name: "identity", Path: "/v1/identity"}}
	// Only the alive probe is simulated. No executable or product is started.
	a.cmd = &exec.Cmd{}
	a.done = make(chan struct{})
	err = a.doctor()
	a.cmd = nil
	a.done = nil
	if err == nil {
		t.Fatal("wrong identity accepted")
	}
	body, err := json.Marshal(a.report)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("PRIVATE-RESPONSE-SENTINEL")) {
		t.Fatal("doctor wrote raw response identity")
	}
}
func TestPaginationRejectsDuplicateObjectsAcrossPages(t *testing.T) {
	count := 0
	_, err := collectPages(func(string) (page, error) {
		count++
		return page{Items: []object{{"id": "m_same"}}, HasMore: count == 1, NextCursor: "next"}, nil
	})
	if err == nil {
		t.Fatal("duplicate page objects accepted")
	}
}
func TestOAuthInvalidationChecksSuccessorAccessBeforeRefresh(t *testing.T) {
	refreshCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshCalls++
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Write([]byte(`{"principal_id":"alice"}`))
	}))
	defer ts.Close()
	a, err := newApp(context.Background(), featureFixture(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.cleanup()
	a.baseURL = ts.URL
	a.http, err = privateHTTPClient(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	a.ops = []operation{{Name: "identity", Path: "/v1/identity"}}
	if err := a.familyInvalid(oauthTokens{access: "unit-access", refresh: "unit-refresh", client: "unit-client"}); err == nil {
		t.Fatal("live successor access accepted as revoked")
	}
	if refreshCalls != 0 {
		t.Fatal("continued after invalidation assertion failed")
	}
	last := a.report.Steps[len(a.report.Steps)-1]
	if last.Expected != "HTTP 401" || last.Observed != "HTTP 200" || last.Outcome != "ISSUES" {
		t.Fatal("successor denial was not asserted")
	}
}
