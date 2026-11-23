package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

const oauthBrowserCookie = "__Host-messagehub_oauth"

func newOAuthHandler(store *hub.Store, base string) (http.Handler, error) {
	resource, err := store.OAuthResource(context.Background())
	if err != nil {
		return nil, err
	}
	if resource != base+"/mcp" {
		return nil, hub.ErrInvalid
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		method := http.MethodPost
		if r.URL.Path == "/authorize" || r.URL.Path == "/authorize/continue" || strings.HasPrefix(r.URL.Path, "/.well-known/") {
			method = http.MethodGet
		}
		if !oauthPublicPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		if r.Method != method {
			w.Header().Set("Allow", method)
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			oauthJSON(w, 200, map[string]any{"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token", "registration_endpoint": base + "/register", "revocation_endpoint": base + "/revoke", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "revocation_endpoint_auth_methods_supported": []string{"none"}})
		case "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp":
			oauthJSON(w, 200, map[string]any{"resource": resource, "authorization_servers": []string{base}, "bearer_methods_supported": []string{"header"}})
		case "/register":
			media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || media != "application/json" {
				oauthJSON(w, 415, map[string]string{"error": "invalid_request"})
				return
			}
			raw, err := io.ReadAll(r.Body)
			var input hub.ClientRegistration
			if err != nil || strictJSON(raw, &input) != nil {
				oauthJSON(w, 400, map[string]string{"error": "invalid_client_metadata"})
				return
			}
			client, err := store.RegisterOAuthClient(r.Context(), input)
			if err != nil {
				oauthStoreError(w, err)
				return
			}
			oauthJSON(w, 201, client)
		case "/authorize":
			q, err := oauthRequestParameters(r.URL.RawQuery)
			if err != nil {
				oauthStoreError(w, err)
				return
			}
			if q.Get("scope") != "" {
				oauthJSON(w, 400, map[string]string{"error": "invalid_scope"})
				return
			}
			p, cookie, err := store.BeginAuthorization(r.Context(), hub.AuthorizationRequest{ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), ResponseType: q.Get("response_type"), State: q.Get("state"), CodeChallenge: q.Get("code_challenge"), CodeChallengeMethod: q.Get("code_challenge_method"), Resource: q.Get("resource")})
			if err != nil {
				oauthStoreError(w, err)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: oauthBrowserCookie, Value: cookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
			if !oauthPendingHTML(w, r, p.RequestID) {
				oauthJSON(w, 202, map[string]any{"request_id": p.RequestID, "continue_uri": "/authorize/continue?request_id=" + url.QueryEscape(p.RequestID), "expires_at": p.ExpiresAt, "status": "pending_local_approval"})
			}
		case "/authorize/continue":
			q, err := oauthParameters(r.URL.RawQuery, []string{"request_id"})
			if err != nil {
				oauthStoreError(w, err)
				return
			}
			cookies := r.CookiesNamed(oauthBrowserCookie)
			if len(cookies) != 1 {
				oauthStoreError(w, hub.ErrUnauthenticated)
				return
			}
			result, err := store.ContinueAuthorization(r.Context(), q.Get("request_id"), cookies[0].Value)
			if err != nil {
				oauthStoreError(w, err)
				return
			}
			if result.Pending {
				if !oauthPendingHTML(w, r, q.Get("request_id")) {
					oauthJSON(w, 202, map[string]string{"status": "pending_local_approval"})
				}
				return
			}
			http.SetCookie(w, &http.Cookie{Name: oauthBrowserCookie, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
			w.Header().Set("Location", result.RedirectURI)
			w.WriteHeader(http.StatusSeeOther)
		case "/token":
			q, ok := oauthForm(w, r)
			if !ok {
				return
			}
			tokens, err := store.ExchangeOAuthToken(r.Context(), hub.TokenRequest{GrantType: q.Get("grant_type"), ClientID: q.Get("client_id"), Code: q.Get("code"), RedirectURI: q.Get("redirect_uri"), CodeVerifier: q.Get("code_verifier"), Resource: q.Get("resource"), RefreshToken: q.Get("refresh_token")})
			if err != nil {
				if errors.Is(err, hub.ErrUnauthenticated) {
					oauthJSON(w, 400, map[string]string{"error": "invalid_grant"})
				} else if errors.Is(err, hub.ErrInvalid) {
					oauthJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
				} else {
					oauthStoreError(w, err)
				}
				return
			}
			oauthJSON(w, 200, tokens)
		case "/revoke":
			q, ok := oauthForm(w, r)
			if !ok {
				return
			}
			if q.Get("client_id") == "" || q.Get("token") == "" {
				oauthStoreError(w, hub.ErrInvalid)
				return
			}
			if err := store.RevokeOAuthToken(r.Context(), q.Get("client_id"), q.Get("token")); err != nil {
				oauthStoreError(w, err)
				return
			}
			w.WriteHeader(200)
		}
	}), nil
}
func oauthPublicPath(path string) bool {
	switch path {
	case "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp", "/register", "/authorize", "/authorize/continue", "/token", "/revoke":
		return true
	}
	return false
}
func oauthRequestParameters(raw string) (url.Values, error) {
	if len(raw) > 8192 {
		return nil, hub.ErrInvalid
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, hub.ErrInvalid
	}
	for key, values := range q {
		if len(values) != 1 {
			return nil, hub.ErrInvalid
		}
		if values[0] == "" {
			q.Del(key)
		}
	}
	return q, nil
}

func oauthParameters(raw string, allowed []string) (url.Values, error) {
	q, err := oauthRequestParameters(raw)
	if err != nil {
		return nil, err
	}
	for key := range q {
		found := false
		for _, a := range allowed {
			if a == key {
				found = true
			}
		}
		if !found {
			return nil, hub.ErrInvalid
		}
	}
	return q, nil
}
func oauthForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-www-form-urlencoded" {
		oauthJSON(w, 415, map[string]string{"error": "invalid_request"})
		return nil, false
	}
	if r.URL.RawQuery != "" || len(r.Header.Values("Authorization")) != 0 {
		oauthStoreError(w, hub.ErrInvalid)
		return nil, false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		oauthStoreError(w, hub.ErrInvalid)
		return nil, false
	}
	q, err := oauthRequestParameters(string(body))
	if err != nil {
		oauthStoreError(w, err)
		return nil, false
	}
	if q.Get("client_secret") != "" {
		oauthStoreError(w, hub.ErrInvalid)
		return nil, false
	}
	if q.Get("scope") != "" {
		oauthJSON(w, 400, map[string]string{"error": "invalid_scope"})
		return nil, false
	}
	return q, true
}
func oauthStoreError(w http.ResponseWriter, err error) {
	status, code := 500, "server_error"
	switch {
	case errors.Is(err, hub.ErrInvalid):
		status, code = 400, "invalid_request"
	case errors.Is(err, hub.ErrUnauthenticated):
		status, code = 401, "unauthenticated"
	case errors.Is(err, hub.ErrQuota):
		status, code = 429, "capacity_exceeded"
	case errors.Is(err, hub.ErrConflict):
		status, code = 409, "conflict"
	case errors.Is(err, hub.ErrExpired):
		status, code = 410, "expired"
	}
	oauthJSON(w, status, map[string]string{"error": code})
}
func oauthJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
