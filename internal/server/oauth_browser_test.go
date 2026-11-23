package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func browserOAuthRequest(h http.Handler, path, accept string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "https://hub.example"+path, nil)
	r.Header.Set("Accept", accept)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestOAuthBrowserWaitsAndContinuesSameAuthorization(t *testing.T) {
	s, h := oauthHTTPFixture(t)
	_, q := compatibilityAuthorization(t, s)
	q.Set("state", "opaque-browser-state-that-must-not-be-rendered")
	w := browserOAuthRequest(h, "/authorize?"+q.Encode(), "text/html,application/xhtml+xml,*/*;q=0.8", nil)
	if w.Code != 202 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("browser response status=%d type=%q", w.Code, w.Header().Get("Content-Type"))
	}
	pending, err := s.ListPendingAuthorizations(context.Background())
	if err != nil || len(pending) != 1 {
		t.Fatal("missing authorization request")
	}
	requestID := pending[0].RequestID
	continuation := "/authorize/continue?request_id=" + url.QueryEscape(requestID)
	body := w.Body.String()
	if !strings.Contains(body, requestID) || !strings.Contains(body, continuation) || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("browser cannot observe approval and continue the same request")
	}
	if strings.Contains(body, q.Get("state")) || strings.Contains(body, q.Get("code_challenge")) || strings.Contains(body, "<script") {
		t.Fatal("browser page exposed state/challenge or included script")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("browser approval page is not private")
	}
	cookie := w.Result().Cookies()[0]
	if strings.Contains(body, cookie.Value) {
		t.Fatal("browser cookie rendered into HTML")
	}
	w = browserOAuthRequest(h, continuation, "text/html", nil)
	if w.Code != 401 {
		t.Fatal("browser continuation without cookie accepted")
	}
	w = browserOAuthRequest(h, continuation, "text/html", cookie)
	if w.Code != 202 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatal("pending browser continuation did not remain usable")
	}
	pending, err = s.ListPendingAuthorizations(context.Background())
	if err != nil || len(pending) != 1 || pending[0].RequestID != requestID {
		t.Fatal("browser polling recreated the authorization")
	}
	if err := s.ApproveAuthorization(context.Background(), requestID, "alice"); err != nil {
		t.Fatal(err)
	}
	w = browserOAuthRequest(h, continuation, "text/html", cookie)
	if w.Code != 303 {
		t.Fatal("approved browser did not continue")
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil || u.Query().Get("state") != q.Get("state") || u.Query().Get("code") == "" {
		t.Fatal("browser callback lost its binding")
	}
}

func TestOAuthBrowserNegotiationPreservesJSONClients(t *testing.T) {
	for _, accept := range []string{"", "*/*", "application/json", "text/html;q=0,application/json", "application/json;q=1,text/html;q=0.5", "text/html;q=invalid"} {
		t.Run(accept, func(t *testing.T) {
			s, h := oauthHTTPFixture(t)
			_, q := compatibilityAuthorization(t, s)
			w := browserOAuthRequest(h, "/authorize?"+q.Encode(), accept, nil)
			if w.Code != 202 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), `"continue_uri"`) {
				t.Fatal("JSON client contract changed")
			}
		})
	}
}
