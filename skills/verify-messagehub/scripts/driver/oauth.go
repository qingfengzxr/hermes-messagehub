package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type oauthTokens struct{ access, refresh, client string }

func parseTokens(raw []byte) (oauthTokens, error) {
	var out object
	if err := decodeJSON(raw, &out); err != nil {
		return oauthTokens{}, err
	}
	if str(out, "access_token") == "" || str(out, "refresh_token") == "" || str(out, "token_type") != "Bearer" || number(out, "expires_in") <= 0 {
		return oauthTokens{}, errors.New("OAuth token response shape mismatch")
	}
	return oauthTokens{access: str(out, "access_token"), refresh: str(out, "refresh_token")}, nil
}
func validBrowserCookie(c *http.Cookie) bool {
	return c != nil && c.Name == "__Host-messagehub_oauth" && c.Value != "" && c.Path == "/" && c.Secure && c.HttpOnly && c.SameSite == http.SameSiteLaxMode
}
func authorizationCode(location, redirect, state string) (string, error) {
	u, err := url.Parse(location)
	if err != nil {
		return "", errors.New("invalid authorization redirect")
	}
	target, err := url.Parse(redirect)
	if err != nil {
		return "", errors.New("invalid fixture redirect")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || u.Scheme != target.Scheme || u.Host != target.Host || u.Path != target.Path || u.User != nil || u.Fragment != "" || len(q["code"]) != 1 || q.Get("code") == "" || len(q["state"]) != 1 || q.Get("state") != state || q.Has("error") {
		return "", errors.New("authorization redirect mismatch")
	}
	return q.Get("code"), nil
}
func (a *App) oauthHTTP(action, method, path, media string, body []byte, headers http.Header, status int) (*http.Response, []byte, error) {
	res, raw, err := a.request(method, path, actor{}, media, body, headers)
	actual := 0
	if res != nil {
		actual = res.StatusCode
	}
	entry := method + " " + strings.SplitN(path, "?", 2)[0]
	if e := a.assert(entry, action, fmt.Sprintf("HTTP %d", status), fmt.Sprintf("HTTP %d", actual), err == nil && actual == status); e != nil {
		return nil, nil, e
	}
	return res, raw, nil
}
func (a *App) oauthForm(action, path string, form url.Values, status int) ([]byte, error) {
	_, raw, err := a.oauthHTTP(action, http.MethodPost, path, "application/x-www-form-urlencoded", []byte(form.Encode()), nil, status)
	if err != nil {
		return nil, err
	}
	if status == 400 {
		var out object
		if decodeJSON(raw, &out) != nil || str(out, "error") != "invalid_grant" {
			return nil, errors.New("OAuth rejection response mismatch")
		}
	}
	return raw, nil
}
func (a *App) oauthFamily() (oauthTokens, error) {
	redirect := "https://client.example.invalid/callback"
	registration := object{"redirect_uris": []string{redirect}, "client_name": "verify-messagehub fixture", "application_type": "native", "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}}
	body, _ := json.Marshal(registration)
	_, raw, err := a.oauthHTTP("register public OAuth client", http.MethodPost, "/register", "application/json", body, nil, 201)
	if err != nil {
		return oauthTokens{}, err
	}
	var client object
	if err := decodeJSON(raw, &client); err != nil {
		return oauthTokens{}, err
	}
	clientID := str(client, "client_id")
	valid := clientID != "" && str(client, "token_endpoint_auth_method") == "none"
	for _, key := range []string{"principal", "principal_id", "client_secret", "access_token", "refresh_token", "token"} {
		if _, found := client[key]; found {
			valid = false
		}
	}
	if err := a.check("POST /register", "DCR grants no principal or secret", "client metadata only", valid); err != nil {
		return oauthTokens{}, err
	}
	verifier, state := rand.Text()+rand.Text(), strings.Repeat(rand.Text(), 80)
	sum := sha256.Sum256([]byte(verifier))
	params := url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "resource": {publicURL + "/mcp"}}
	params.Set("scope", "")
	params.Set("sdk_extension", "ignored")
	res, raw, err := a.oauthHTTP("begin local approval with long opaque state and empty scope", http.MethodGet, "/authorize?"+params.Encode(), "", nil, nil, 202)
	if err != nil {
		return oauthTokens{}, err
	}
	var pending object
	if err := decodeJSON(raw, &pending); err != nil {
		return oauthTokens{}, err
	}
	requestID := str(pending, "request_id")
	cookies := res.Cookies()
	if err := a.check("GET /authorize", "pending request and browser cookie", "pending with Secure HttpOnly cookie", requestID != "" && str(pending, "status") == "pending_local_approval" && len(cookies) == 1 && validBrowserCookie(cookies[0])); err != nil {
		return oauthTokens{}, err
	}
	continuation := "/authorize/continue?" + url.Values{"request_id": {requestID}}.Encode()
	if _, _, err := a.oauthHTTP("continue without browser cookie rejected", http.MethodGet, continuation, "", nil, nil, 401); err != nil {
		return oauthTokens{}, err
	}
	// This manual header is a loopback protocol probe, not browser HTTPS acceptance.
	headers := http.Header{"Cookie": {cookies[0].Name + "=" + cookies[0].Value}}
	_, raw, err = a.oauthHTTP("unapproved original browser remains pending", http.MethodGet, continuation, "", nil, headers, 202)
	if err != nil {
		return oauthTokens{}, err
	}
	var waiting object
	if decodeJSON(raw, &waiting) != nil || str(waiting, "status") != "pending_local_approval" {
		return oauthTokens{}, errors.New("unapproved request no longer pending")
	}
	browserHeaders := headers.Clone()
	browserHeaders.Set("Accept", "text/html")
	browserResponse, browserPage, err := a.oauthHTTP("browser waits on original approval", http.MethodGet, continuation, "", nil, browserHeaders, 202)
	if err != nil {
		return oauthTokens{}, err
	}
	page := string(browserPage)
	if err := a.check("GET /authorize/continue", "private browser continuation page", "HTML with same request and no credentials", strings.HasPrefix(browserResponse.Header.Get("Content-Type"), "text/html") && strings.Contains(page, requestID) && strings.Contains(page, continuation) && !strings.Contains(page, state) && !strings.Contains(page, cookies[0].Value)); err != nil {
		return oauthTokens{}, err
	}
	raw, err = a.admin(true, "oauth-pending")
	if err != nil {
		return oauthTokens{}, err
	}
	var requests []object
	if err := decodeJSON(raw, &requests); err != nil {
		return oauthTokens{}, err
	}
	found := false
	for _, r := range requests {
		if str(r, "request_id") == requestID {
			found = str(r, "client_id") == clientID && str(r, "resource") == publicURL+"/mcp" && str(r, "redirect_uri") == redirect
		}
	}
	if err := a.check("CLI admin oauth-pending", "pending approval read-back", "matching client, redirect and resource", found); err != nil {
		return oauthTokens{}, err
	}
	raw, err = a.admin(true, "oauth-approve", "--request", requestID, "--principal", "alice")
	if err != nil {
		return oauthTokens{}, err
	}
	var approval object
	if decodeJSON(raw, &approval) != nil || !flagValue(approval, "approved") || str(approval, "principal_id") != "alice" || str(approval, "request_id") != requestID {
		return oauthTokens{}, errors.New("local approval receipt mismatch")
	}
	res, _, err = a.oauthHTTP("approved continuation returns unfollowed redirect", http.MethodGet, continuation, "", nil, headers, 303)
	if err != nil {
		return oauthTokens{}, err
	}
	code, err := authorizationCode(res.Header.Get("Location"), redirect, state)
	if err != nil {
		return oauthTokens{}, err
	}
	if err := a.check("GET /authorize/continue", "redirect state and target", "fixture redirect and original state", true); err != nil {
		return oauthTokens{}, err
	}
	exchange := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {publicURL + "/mcp"}}
	exchange.Set("scope", "")
	exchange.Set("client_secret", "")
	raw, err = a.oauthForm("exchange PKCE code", "/token", exchange, 200)
	if err != nil {
		return oauthTokens{}, err
	}
	tokens, err := parseTokens(raw)
	if err != nil {
		return oauthTokens{}, err
	}
	tokens.client = clientID
	if _, err := a.oauthForm("code is single-use", "/token", exchange, 400); err != nil {
		return oauthTokens{}, err
	}
	oauthActor := actor{id: "alice", token: tokens.access}
	for _, protocol := range []string{"REST", "MCP"} {
		out, err := a.call(protocol, oauthActor, "identity", object{})
		if err != nil {
			return oauthTokens{}, err
		}
		if err := a.check(protocol+" identity", "OAuth access is bound to approved identity", "alice", str(out, "principal_id") == "alice"); err != nil {
			return oauthTokens{}, err
		}
	}
	return tokens, nil
}
func refreshForm(t oauthTokens) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {t.client}, "refresh_token": {t.refresh}, "resource": {publicURL + "/mcp"}, "scope": {""}, "client_secret": {""}}
}
func (a *App) rotate(t oauthTokens) (oauthTokens, error) {
	raw, err := a.oauthForm("rotate refresh token", "/token", refreshForm(t), 200)
	if err != nil {
		return oauthTokens{}, err
	}
	next, err := parseTokens(raw)
	if err != nil {
		return oauthTokens{}, err
	}
	next.client = t.client
	if err := a.check("POST /token", "refresh rotation returns new credentials", "new access and refresh", next.refresh != t.refresh && next.access != t.access); err != nil {
		return oauthTokens{}, err
	}
	out, err := a.rest(actor{id: "alice", token: next.access}, "identity", object{}, 200)
	if err != nil {
		return oauthTokens{}, err
	}
	if err := a.check("REST identity", "successor access works before invalidation", "alice", str(out, "principal_id") == "alice"); err != nil {
		return oauthTokens{}, err
	}
	return next, nil
}
func (a *App) familyInvalid(next oauthTokens) error {
	if _, err := a.rest(actor{id: "alice", token: next.access}, "identity", object{}, 401); err != nil {
		return err
	}
	_, err := a.oauthForm("successor refresh invalid after family revocation", "/token", refreshForm(next), 400)
	return err
}
func (a *App) identity() error {
	if err := a.program("version"); err != nil {
		return err
	}
	if err := a.program("help"); err != nil {
		return err
	}
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		_, raw, err := a.oauthHTTP("OAuth discovery", http.MethodGet, path, "", nil, nil, 200)
		if err != nil {
			return err
		}
		var out object
		if err := decodeJSON(raw, &out); err != nil {
			return err
		}
		valid := str(out, "resource") == publicURL+"/mcp" && len(array(out, "authorization_servers")) == 1 && array(out, "authorization_servers")[0] == publicURL
		if path == "/.well-known/oauth-authorization-server" {
			valid = str(out, "issuer") == publicURL && str(out, "token_endpoint") == publicURL+"/token" && str(out, "authorization_endpoint") == publicURL+"/authorize" && str(out, "registration_endpoint") == publicURL+"/register" && str(out, "revocation_endpoint") == publicURL+"/revoke"
		}
		if err := a.check("GET "+path, "discovery resource binding", "fixture public URL", valid); err != nil {
			return err
		}
	}
	if _, err := a.rest(actor{}, "identity", object{}, 401); err != nil {
		return err
	}
	if _, _, err := a.oauthHTTP("anonymous MCP rejected before initialize", http.MethodPost, "/mcp", "application/json", []byte(`{}`), nil, 401); err != nil {
		return err
	}
	for _, protocol := range []string{"REST", "MCP"} {
		out, err := a.call(protocol, a.actors["alice"], "identity", object{})
		if err != nil {
			return err
		}
		if err := a.check(protocol+" identity", "self identity", "alice", str(out, "principal_id") == "alice"); err != nil {
			return err
		}
	}
	probe, err := a.createActor("probe")
	if err != nil {
		return err
	}
	if _, err := a.admin(true, "principal-disable", "--id", "probe"); err != nil {
		return err
	}
	if _, err := a.rest(probe, "identity", object{}, 401); err != nil {
		return err
	}
	if _, err := a.admin(true, "principal-enable", "--id", "probe"); err != nil {
		return err
	}
	out, err := a.rest(probe, "identity", object{}, 200)
	if err != nil {
		return err
	}
	if err := a.check("REST identity", "reenabled principal can authenticate", "probe", str(out, "principal_id") == "probe"); err != nil {
		return err
	}
	if _, err := a.admin(true, "grant-revoke", "--id", probe.grantID); err != nil {
		return err
	}
	if _, err := a.rest(probe, "identity", object{}, 401); err != nil {
		return err
	}
	original, err := a.oauthFamily()
	if err != nil {
		return err
	}
	next, err := a.rotate(original)
	if err != nil {
		return err
	}
	if _, err := a.oauthForm("replay consumed refresh invalidates family", "/token", refreshForm(original), 400); err != nil {
		return err
	}
	if err := a.familyInvalid(next); err != nil {
		return err
	}
	independent, err := a.oauthFamily()
	if err != nil {
		return err
	}
	next, err = a.rotate(independent)
	if err != nil {
		return err
	}
	raw, err := a.oauthForm("revoke family using old consumed refresh", "/revoke", url.Values{"client_id": {independent.client}, "token": {independent.refresh}, "token_type_hint": {"refresh_token"}}, 200)
	if err != nil {
		return err
	}
	if err := a.check("POST /revoke", "revocation response", "empty body", len(raw) == 0); err != nil {
		return err
	}
	return a.familyInvalid(next)
}
