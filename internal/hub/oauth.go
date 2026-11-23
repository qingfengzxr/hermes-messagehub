package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// OAuthOptions is persisted by InitOAuth. Resource must be the exact public MCP URL.
type OAuthOptions struct {
	Resource   string `json:"resource"`
	MaxClients int    `json:"max_clients"`
	MaxPending int    `json:"max_pending"`
}
type ClientRegistration struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name,omitempty"`
	ApplicationType         string   `json:"application_type,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
}
type OAuthClient struct {
	ClientID string `json:"client_id"`
	ClientRegistration
}
type AuthorizationRequest struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
}
type PendingAuthorization struct {
	RequestID   string `json:"request_id"`
	ClientID    string `json:"client_id"`
	ClientName  string `json:"client_name"`
	RedirectURI string `json:"redirect_uri"`
	Resource    string `json:"resource"`
	ExpiresAt   int64  `json:"expires_at"`
}
type AuthorizationResult struct {
	Pending     bool   `json:"pending"`
	RedirectURI string `json:"-"`
}
type TokenRequest struct {
	GrantType    string
	ClientID     string
	Code         string
	RedirectURI  string
	CodeVerifier string
	Resource     string
	RefreshToken string
}
type OAuthTokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

const oauthAccessTTL = 15 * time.Minute
const oauthGrantTTL = 30 * 24 * time.Hour

var oauthVerifierPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// InitOAuth is explicit so opening a non-OAuth store never changes its schema.
// Repeated calls must use the same configuration; resource changes require migration.
func (s *Store) InitOAuth(ctx context.Context, o OAuthOptions) error {
	if o.MaxClients == 0 {
		o.MaxClients = 128
	}
	if o.MaxPending == 0 {
		o.MaxPending = 256
	}
	u, err := url.Parse(o.Resource)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || strings.Contains(o.Resource, "#") || o.MaxClients < 1 || o.MaxClients > 10000 || o.MaxPending < 1 || o.MaxPending > 10000 {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS oauth_config(id INTEGER PRIMARY KEY CHECK(id=1),resource TEXT NOT NULL,max_clients INTEGER NOT NULL,max_pending INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS oauth_clients(id TEXT PRIMARY KEY,metadata TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS oauth_requests(id TEXT PRIMARY KEY,client_id TEXT NOT NULL REFERENCES oauth_clients(id),redirect_uri TEXT NOT NULL,state TEXT NOT NULL,challenge TEXT NOT NULL,resource TEXT NOT NULL,browser_hash TEXT NOT NULL,expires_at INTEGER NOT NULL,principal_id TEXT REFERENCES principals(id),code_hash TEXT UNIQUE,code_expires INTEGER,consumed INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS oauth_grants(grant_id TEXT PRIMARY KEY REFERENCES grants(id),client_id TEXT NOT NULL REFERENCES oauth_clients(id),resource TEXT NOT NULL);`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO oauth_config VALUES(1,?,?,?)", o.Resource, o.MaxClients, o.MaxPending); err != nil {
		return err
	}
	stored, err := oauthConfigTx(ctx, tx)
	if err != nil {
		return err
	}
	if stored != o {
		return ErrConflict
	}
	return tx.Commit()
}
func oauthConfigTx(ctx context.Context, tx *sql.Tx) (OAuthOptions, error) {
	var o OAuthOptions
	err := tx.QueryRowContext(ctx, "SELECT resource,max_clients,max_pending FROM oauth_config WHERE id=1").Scan(&o.Resource, &o.MaxClients, &o.MaxPending)
	return o, err
}
func (s *Store) OAuthResource(ctx context.Context) (string, error) {
	var resource string
	err := s.db.QueryRowContext(ctx, "SELECT resource FROM oauth_config WHERE id=1").Scan(&resource)
	return resource, err
}
func oauthSafeText(v string, max int) bool {
	if len(v) > max || !utf8.ValidString(v) {
		return false
	}
	for _, r := range v {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func oauthValidRedirect(raw string) bool {
	if !oauthSafeText(raw, 2048) || strings.Contains(raw, "#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" {
		return false
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
			return false
		}
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for _, key := range []string{"code", "state", "error"} {
		if q.Has(key) {
			return false
		}
	}
	return true
}
func (s *Store) RegisterOAuthClient(ctx context.Context, r ClientRegistration) (OAuthClient, error) {
	if len(r.RedirectURIs) == 0 || len(r.RedirectURIs) > 8 || !oauthSafeText(r.ClientName, 200) || (r.ApplicationType != "" && r.ApplicationType != "native" && r.ApplicationType != "web") {
		return OAuthClient{}, ErrInvalid
	}
	if r.TokenEndpointAuthMethod == "" {
		r.TokenEndpointAuthMethod = "none"
	}
	if r.TokenEndpointAuthMethod != "none" {
		return OAuthClient{}, ErrInvalid
	}
	if len(r.GrantTypes) == 0 {
		r.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	grants := map[string]bool{}
	for _, v := range r.GrantTypes {
		if v != "authorization_code" && v != "refresh_token" {
			return OAuthClient{}, ErrInvalid
		}
		grants[v] = true
	}
	if len(r.GrantTypes) != 2 || len(grants) != 2 {
		return OAuthClient{}, ErrInvalid
	}
	if len(r.ResponseTypes) == 0 {
		r.ResponseTypes = []string{"code"}
	}
	if len(r.ResponseTypes) != 1 || r.ResponseTypes[0] != "code" || len(r.GrantTypes) > 2 {
		return OAuthClient{}, ErrInvalid
	}
	seen := map[string]bool{}
	for _, v := range r.RedirectURIs {
		if !oauthValidRedirect(v) || seen[v] {
			return OAuthClient{}, ErrInvalid
		}
		seen[v] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthClient{}, err
	}
	defer tx.Rollback()
	cfg, err := oauthConfigTx(ctx, tx)
	if err != nil {
		return OAuthClient{}, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM oauth_clients").Scan(&count); err != nil {
		return OAuthClient{}, err
	}
	if count >= cfg.MaxClients {
		return OAuthClient{}, ErrQuota
	}
	c := OAuthClient{ClientID: newID("oc"), ClientRegistration: r}
	data, err := json.Marshal(r)
	if err != nil {
		return OAuthClient{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO oauth_clients(id,metadata) VALUES(?,?)", c.ClientID, string(data)); err != nil {
		return OAuthClient{}, err
	}
	if err = tx.Commit(); err != nil {
		return OAuthClient{}, err
	}
	return c, nil
}
func oauthClientTx(ctx context.Context, tx *sql.Tx, id string) (ClientRegistration, error) {
	var data string
	var r ClientRegistration
	if err := tx.QueryRowContext(ctx, "SELECT metadata FROM oauth_clients WHERE id=?", id).Scan(&data); err != nil {
		return r, ErrInvalid
	}
	err := json.Unmarshal([]byte(data), &r)
	return r, err
}
func (s *Store) BeginAuthorization(ctx context.Context, r AuthorizationRequest) (PendingAuthorization, string, error) {
	challenge, err := base64.RawURLEncoding.DecodeString(r.CodeChallenge)
	if r.ResponseType != "code" || r.State == "" || !oauthSafeText(r.State, 8<<10) || r.CodeChallengeMethod != "S256" || err != nil || len(challenge) != 32 || base64.RawURLEncoding.EncodeToString(challenge) != r.CodeChallenge {
		return PendingAuthorization{}, "", ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PendingAuthorization{}, "", err
	}
	defer tx.Rollback()
	cfg, err := oauthConfigTx(ctx, tx)
	if err != nil {
		return PendingAuthorization{}, "", err
	}
	if r.Resource != cfg.Resource {
		return PendingAuthorization{}, "", ErrInvalid
	}
	c, err := oauthClientTx(ctx, tx, r.ClientID)
	if err != nil {
		return PendingAuthorization{}, "", err
	}
	matched := false
	for _, v := range c.RedirectURIs {
		if v == r.RedirectURI {
			matched = true
		}
	}
	if !matched {
		return PendingAuthorization{}, "", ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM oauth_requests WHERE expires_at<=? AND (code_expires IS NULL OR code_expires<=?)", s.now(), s.now()); err != nil {
		return PendingAuthorization{}, "", err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM oauth_requests").Scan(&count); err != nil {
		return PendingAuthorization{}, "", err
	}
	if count >= cfg.MaxPending {
		return PendingAuthorization{}, "", ErrQuota
	}
	p := PendingAuthorization{RequestID: newID("oa"), ClientID: r.ClientID, ClientName: c.ClientName, RedirectURI: r.RedirectURI, Resource: r.Resource, ExpiresAt: s.now() + 600}
	cookie := rand.Text() + rand.Text()
	if _, err = tx.ExecContext(ctx, "INSERT INTO oauth_requests(id,client_id,redirect_uri,state,challenge,resource,browser_hash,expires_at) VALUES(?,?,?,?,?,?,?,?)", p.RequestID, r.ClientID, r.RedirectURI, r.State, r.CodeChallenge, r.Resource, tokenDigest(cookie), p.ExpiresAt); err != nil {
		return PendingAuthorization{}, "", err
	}
	if err = tx.Commit(); err != nil {
		return PendingAuthorization{}, "", err
	}
	return p, cookie, nil
}

// ListPendingAuthorizations and ApproveAuthorization are local-admin APIs only.
func (s *Store) ListPendingAuthorizations(ctx context.Context) ([]PendingAuthorization, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT r.id,r.client_id,c.metadata,r.redirect_uri,r.resource,r.expires_at FROM oauth_requests r JOIN oauth_clients c ON c.id=r.client_id WHERE r.principal_id IS NULL AND r.expires_at>? ORDER BY r.id", s.now())
	if err != nil {
		return nil, err
	}
	result := []PendingAuthorization{}
	for rows.Next() {
		var p PendingAuthorization
		var metadata string
		if err = rows.Scan(&p.RequestID, &p.ClientID, &metadata, &p.RedirectURI, &p.Resource, &p.ExpiresAt); err != nil {
			rows.Close()
			return nil, err
		}
		var c ClientRegistration
		if err = json.Unmarshal([]byte(metadata), &c); err != nil {
			rows.Close()
			return nil, err
		}
		p.ClientName = c.ClientName
		result = append(result, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *Store) ApproveAuthorization(ctx context.Context, requestID, principalID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var active bool
	if err = tx.QueryRowContext(ctx, "SELECT active FROM principals WHERE id=?", principalID).Scan(&active); err != nil || !active {
		return ErrNotFound
	}
	result, err := tx.ExecContext(ctx, "UPDATE oauth_requests SET principal_id=? WHERE id=? AND principal_id IS NULL AND expires_at>?", principalID, requestID, s.now())
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return tx.Commit()
}
func (s *Store) ContinueAuthorization(ctx context.Context, requestID, cookie string) (AuthorizationResult, error) {
	if len(cookie) < 32 || len(cookie) > 256 {
		return AuthorizationResult{}, ErrUnauthenticated
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthorizationResult{}, err
	}
	defer tx.Rollback()
	var redirect, state, browser string
	var expires int64
	var principal, codeHash sql.NullString
	if err = tx.QueryRowContext(ctx, "SELECT redirect_uri,state,browser_hash,expires_at,principal_id,code_hash FROM oauth_requests WHERE id=?", requestID).Scan(&redirect, &state, &browser, &expires, &principal, &codeHash); err != nil || tokenDigest(cookie) != browser {
		return AuthorizationResult{}, ErrUnauthenticated
	}
	if expires <= s.now() {
		return AuthorizationResult{}, ErrExpired
	}
	if codeHash.Valid {
		return AuthorizationResult{}, ErrConflict
	}
	if !principal.Valid {
		if err = tx.Commit(); err != nil {
			return AuthorizationResult{}, err
		}
		return AuthorizationResult{Pending: true}, nil
	}
	var active bool
	if err = tx.QueryRowContext(ctx, "SELECT active FROM principals WHERE id=?", principal.String).Scan(&active); err != nil || !active {
		return AuthorizationResult{}, ErrUnauthenticated
	}
	code := rand.Text() + rand.Text()
	if _, err = tx.ExecContext(ctx, "UPDATE oauth_requests SET code_hash=?,code_expires=? WHERE id=?", tokenDigest(code), s.now()+60, requestID); err != nil {
		return AuthorizationResult{}, err
	}
	u, err := url.Parse(redirect)
	if err != nil {
		return AuthorizationResult{}, err
	}
	q := u.Query()
	q.Set("code", code)
	q.Set("state", state)
	u.RawQuery = q.Encode()
	if err = tx.Commit(); err != nil {
		return AuthorizationResult{}, err
	}
	return AuthorizationResult{RedirectURI: u.String()}, nil
}
func (s *Store) ExchangeOAuthToken(ctx context.Context, r TokenRequest) (OAuthTokens, error) {
	if r.GrantType != "authorization_code" && r.GrantType != "refresh_token" {
		return OAuthTokens{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return OAuthTokens{}, err
	}
	defer tx.Rollback()
	var grantID, access string
	var grantExpires int64
	if r.GrantType == "authorization_code" {
		if !oauthVerifierPattern.MatchString(r.CodeVerifier) || len(r.Code) < 32 || len(r.Code) > 256 {
			return OAuthTokens{}, ErrUnauthenticated
		}
		var requestID, client, redirect, challenge, resource, principal string
		var expires int64
		var consumed bool
		err = tx.QueryRowContext(ctx, "SELECT id,client_id,redirect_uri,challenge,resource,principal_id,code_expires,consumed FROM oauth_requests WHERE code_hash=?", tokenDigest(r.Code)).Scan(&requestID, &client, &redirect, &challenge, &resource, &principal, &expires, &consumed)
		sum := sha256.Sum256([]byte(r.CodeVerifier))
		if err != nil || consumed || expires <= s.now() || client != r.ClientID || redirect != r.RedirectURI || resource != r.Resource || challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
			return OAuthTokens{}, ErrUnauthenticated
		}
		var grant Grant
		grant, access, err = s.issueGrantTx(ctx, tx, principal, oauthGrantTTL)
		if err != nil {
			return OAuthTokens{}, ErrUnauthenticated
		}
		grantID = grant.ID
		grantExpires = grant.ExpiresAt
		if _, err = tx.ExecContext(ctx, "UPDATE tokens SET expires_at=? WHERE token_hash=?", s.now()+int64(oauthAccessTTL/time.Second), tokenDigest(access)); err != nil {
			return OAuthTokens{}, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO oauth_grants(grant_id,client_id,resource) VALUES(?,?,?)", grantID, client, resource); err != nil {
			return OAuthTokens{}, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE oauth_requests SET consumed=1 WHERE id=?", requestID); err != nil {
			return OAuthTokens{}, err
		}
	} else {
		if len(r.RefreshToken) < 32 || len(r.RefreshToken) > 1024 {
			return OAuthTokens{}, ErrUnauthenticated
		}
		var kind string
		err = tx.QueryRowContext(ctx, `SELECT g.id,g.expires_at,t.kind FROM tokens t JOIN grants g ON g.id=t.grant_id JOIN oauth_grants o ON o.grant_id=g.id JOIN principals p ON p.id=g.principal_id WHERE t.token_hash=? AND t.kind IN('refresh','refresh_used') AND t.expires_at>? AND g.expires_at>? AND g.revoked=0 AND p.active=1 AND o.client_id=? AND o.resource=?`, tokenDigest(r.RefreshToken), s.now(), s.now(), r.ClientID, r.Resource).Scan(&grantID, &grantExpires, &kind)
		if err != nil {
			return OAuthTokens{}, ErrUnauthenticated
		}
		if kind == "refresh_used" {
			if _, err = tx.ExecContext(ctx, "UPDATE grants SET revoked=1 WHERE id=?", grantID); err != nil {
				return OAuthTokens{}, err
			}
			if err = tx.Commit(); err != nil {
				return OAuthTokens{}, err
			}
			return OAuthTokens{}, ErrUnauthenticated
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM tokens WHERE kind='access' AND expires_at<=?", s.now()); err != nil {
			return OAuthTokens{}, err
		}
		var history, liveAccess int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM tokens WHERE grant_id=? AND kind='refresh_used'", grantID).Scan(&history); err != nil {
			return OAuthTokens{}, err
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM tokens WHERE grant_id=? AND kind='access'", grantID).Scan(&liveAccess); err != nil {
			return OAuthTokens{}, err
		}
		if history >= 4096 || liveAccess >= 64 {
			return OAuthTokens{}, ErrQuota
		}
		// Keep consumed-token hashes until grant expiry so any replay or old
		// refresh revocation still identifies the complete authorization family.
		if _, err = tx.ExecContext(ctx, "UPDATE tokens SET kind='refresh_used' WHERE token_hash=? AND kind='refresh'", tokenDigest(r.RefreshToken)); err != nil {
			return OAuthTokens{}, err
		}
		access = "mh_" + rand.Text() + rand.Text()
		expires := min(s.now()+int64(oauthAccessTTL/time.Second), grantExpires)
		if _, err = tx.ExecContext(ctx, "INSERT INTO tokens(token_hash,grant_id,kind,expires_at) VALUES(?,?,'access',?)", tokenDigest(access), grantID, expires); err != nil {
			return OAuthTokens{}, err
		}
	}
	refresh := "mr_" + rand.Text() + rand.Text()
	if _, err = tx.ExecContext(ctx, "INSERT INTO tokens(token_hash,grant_id,kind,expires_at) VALUES(?,?,'refresh',?)", tokenDigest(refresh), grantID, grantExpires); err != nil {
		return OAuthTokens{}, err
	}
	if err = tx.Commit(); err != nil {
		return OAuthTokens{}, err
	}
	ttl := int64(oauthAccessTTL / time.Second)
	if remaining := grantExpires - s.now(); remaining < ttl {
		ttl = remaining
	}
	return OAuthTokens{AccessToken: access, TokenType: "Bearer", ExpiresIn: ttl, RefreshToken: refresh}, nil
}

// RevokeOAuthToken requires possession of a token bound to this public client.
// Unknown tokens deliberately succeed, as required for non-enumerating revocation.
func (s *Store) RevokeOAuthToken(ctx context.Context, clientID, token string) error {
	if len(clientID) > 256 || len(token) > 1024 {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE grants SET revoked=1 WHERE id IN(SELECT t.grant_id FROM tokens t JOIN oauth_grants o ON o.grant_id=t.grant_id WHERE t.token_hash=? AND o.client_id=?)`, tokenDigest(token), clientID); err != nil {
		return err
	}
	return tx.Commit()
}
