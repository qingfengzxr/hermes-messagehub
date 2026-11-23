// Package server exposes only authenticated communication operations over REST and MCP.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Options struct {
	Address            string
	PublicBaseURL      string
	AllowedHosts       []string
	AllowedOrigins     []string
	MaxBodyBytes       int64
	RequestTimeout     time.Duration
	RequestsPerSecond  float64
	Burst              int
	MaxConcurrent      int
	MaxConcurrentWaits int
	OAuth              *hub.Store
}
type actorKey struct{}
type boundary struct {
	options        Options
	backend        Backend
	hosts, origins map[string]bool
	slots          chan struct{}
	waitSlots      chan struct{}
	mu             sync.Mutex
	tokens         float64
	last           time.Time
}

func normalize(o Options) (Options, error) {
	if o.Address == "" {
		o.Address = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(o.Address); err != nil {
		return o, hub.ErrInvalid
	}
	if o.MaxBodyBytes == 0 {
		o.MaxBodyBytes = 2 << 20
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 30 * time.Second
	}
	if o.RequestsPerSecond == 0 {
		o.RequestsPerSecond = 100
	}
	if o.Burst == 0 {
		o.Burst = 200
	}
	if o.MaxConcurrent == 0 {
		o.MaxConcurrent = 64
	}
	if o.MaxConcurrentWaits == 0 {
		o.MaxConcurrentWaits = 64
	}
	if o.MaxConcurrentWaits < 1 || o.MaxConcurrentWaits > 10000 {
		return o, hub.ErrInvalid
	}
	if o.MaxBodyBytes < 128 || o.MaxBodyBytes > 16<<20 || o.RequestTimeout < time.Millisecond || o.RequestTimeout > 5*time.Minute || o.RequestsPerSecond < 0 || math.IsNaN(o.RequestsPerSecond) || math.IsInf(o.RequestsPerSecond, 0) || o.Burst < 1 || o.Burst > 100000 || o.MaxConcurrent < 1 || o.MaxConcurrent > 10000 {
		return o, hub.ErrInvalid
	}
	if o.PublicBaseURL != "" {
		u, err := url.Parse(o.PublicBaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || strings.Contains(o.PublicBaseURL, "#") || (u.Path != "" && u.Path != "/") {
			return o, hub.ErrInvalid
		}
		o.PublicBaseURL = strings.TrimSuffix(o.PublicBaseURL, "/")
		if len(o.AllowedHosts) == 0 {
			o.AllowedHosts = []string{u.Host}
		}
		if len(o.AllowedOrigins) == 0 {
			o.AllowedOrigins = []string{o.PublicBaseURL}
		}
	}
	if len(o.AllowedHosts) == 0 {
		o.AllowedHosts = []string{"localhost:8080", "127.0.0.1:8080", "[::1]:8080"}
	}
	for _, host := range o.AllowedHosts {
		u, err := url.Parse("https://" + host)
		if err != nil || u.Host != host || u.User != nil || u.Path != "" || strings.ContainsAny(host, "/*?# \t\r\n") {
			return o, hub.ErrInvalid
		}
	}
	for _, origin := range o.AllowedOrigins {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || strings.Contains(origin, "#") {
			return o, hub.ErrInvalid
		}
	}
	if o.OAuth != nil && o.PublicBaseURL == "" {
		return o, hub.ErrInvalid
	}
	return o, nil
}

func newHandler(backend Backend, options Options) (http.Handler, error) {
	if backend == nil {
		return nil, hub.ErrInvalid
	}
	o, err := normalize(options)
	if err != nil {
		return nil, err
	}
	b := &boundary{options: o, backend: backend, hosts: map[string]bool{}, origins: map[string]bool{}, slots: make(chan struct{}, o.MaxConcurrent), waitSlots: make(chan struct{}, o.MaxConcurrentWaits), tokens: float64(o.Burst), last: time.Now()}
	for _, v := range o.AllowedHosts {
		b.hosts[strings.ToLower(v)] = true
	}
	for _, v := range o.AllowedOrigins {
		b.origins[v] = true
	}
	srv := http.NewServeMux()
	if o.OAuth != nil {
		oauthHandler, err := newOAuthHandler(o.OAuth, o.PublicBaseURL)
		if err != nil {
			return nil, err
		}
		for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp", "/register", "/authorize", "/authorize/continue", "/token", "/revoke"} {
			srv.Handle(path, oauthHandler)
		}
	}
	sdk := mcp.NewServer(&mcp.Implementation{Name: "messagehub", Version: "1"}, &mcp.ServerOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, op := range operations(backend) {
		srv.HandleFunc(op.path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || media != "application/json" {
				writeError(w, 415, "unsupported_media_type")
				return
			}
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				writeError(w, 400, "invalid_request")
				return
			}
			actor, ok := r.Context().Value(actorKey{}).(hub.Actor)
			if !ok {
				writeError(w, 401, "unauthenticated")
				return
			}
			result, err := op.call(r.Context(), actor, raw)
			if err != nil {
				writeHubError(w, err)
				return
			}
			writeJSON(w, 200, result)
		})
		sdk.AddTool(&mcp.Tool{Name: op.name, Description: op.description, InputSchema: op.schema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// SDK transports may detach HTTP contexts for older protocol versions.
			// Bound the operation itself and reauthenticate this request, never a cached session actor.
			ctx, cancel := context.WithTimeout(ctx, o.RequestTimeout)
			defer cancel()
			var actor hub.Actor
			var err error
			if req.Extra == nil {
				err = hub.ErrUnauthenticated
			} else {
				actor, err = authenticate(ctx, backend, req.Extra.Header)
			}
			var result any
			if err == nil {
				raw := req.Params.Arguments
				if len(raw) == 0 {
					raw = []byte("{}")
				}
				result, err = op.call(ctx, actor, raw)
			}
			if err != nil {
				_, code := errorCode(err)
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: code}}}, nil
			}
			data, err := json.Marshal(result)
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "internal_error"}}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}, StructuredContent: result}, nil
		})
	}
	srv.Handle("/v1/inbox/wait", inboxWaitHandler(backend))
	// Our exact Host allowlist replaces the SDK's localhost-only heuristic behind ingress.
	srv.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return sdk }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: o.MaxBodyBytes, DisableLocalhostProtection: true, PropagateRequestCancellation: true}))
	srv.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	return b.filter(srv), nil
}
func authenticate(ctx context.Context, b Backend, h http.Header) (hub.Actor, error) {
	if len(h.Values("Authorization")) != 1 {
		return hub.Actor{}, hub.ErrUnauthenticated
	}
	parts := strings.Fields(h.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return hub.Actor{}, hub.ErrUnauthenticated
	}
	return b.Authenticate(ctx, parts[1])
}
func (b *boundary) allowRate() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = math.Min(float64(b.options.Burst), b.tokens+now.Sub(b.last).Seconds()*b.options.RequestsPerSecond)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
func (b *boundary) filter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = &privateResponseWriter{ResponseWriter: w}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !b.hosts[strings.ToLower(r.Host)] {
			writeError(w, 403, "forbidden_host")
			return
		}
		origin := r.Header.Get("Origin")
		if len(r.Header.Values("Origin")) > 1 || (origin != "" && !b.origins[origin]) {
			writeError(w, 403, "forbidden_origin")
			return
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate, MCP-Protocol-Version")
		}
		if !b.allowRate() {
			w.Header().Set("Retry-After", "1")
			writeError(w, 429, "rate_limited")
			return
		}
		slots := b.slots
		if r.URL.Path == "/v1/inbox/wait" {
			// Sleeping receivers must not consume the capacity needed for Send/ACK.
			slots = b.waitSlots
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeError(w, 429, "capacity_exceeded")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), b.options.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		if r.ContentLength > b.options.MaxBodyBytes {
			writeError(w, 413, "body_too_large")
			return
		}
		if r.Body != nil {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, b.options.MaxBodyBytes))
			r.Body.Close()
			if err != nil {
				var capErr *http.MaxBytesError
				if errors.As(err, &capErr) {
					writeError(w, 413, "body_too_large")
				} else {
					writeError(w, 400, "invalid_request")
				}
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		if r.URL.Path != "/healthz" && !(b.options.OAuth != nil && oauthPublicPath(r.URL.Path)) {
			actor, err := authenticate(ctx, b.backend, r.Header)
			if err != nil {
				challenge := "Bearer"
				if b.options.OAuth != nil {
					challenge += ` resource_metadata="` + b.options.PublicBaseURL + `/.well-known/oauth-protected-resource"`
				}
				w.Header().Set("WWW-Authenticate", challenge)
				writeHubError(w, hub.ErrUnauthenticated)
				return
			}
			r = r.WithContext(context.WithValue(ctx, actorKey{}, actor))
		}
		next.ServeHTTP(w, r)
	})
}
func errorCode(err error) (int, string) {
	switch {
	case errors.Is(err, hub.ErrUnauthenticated):
		return 401, "unauthenticated"
	case errors.Is(err, hub.ErrInvalid):
		return 400, "invalid_request"
	case errors.Is(err, hub.ErrNotFound):
		return 404, "not_found"
	case errors.Is(err, hub.ErrConflict):
		return 409, "conflict"
	case errors.Is(err, hub.ErrQuota):
		return 429, "capacity_exceeded"
	case errors.Is(err, hub.ErrExpired):
		return 410, "expired"
	case errors.Is(err, context.DeadlineExceeded):
		return 504, "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return 408, "request_canceled"
	default:
		return 500, "internal_error"
	}
}
func writeHubError(w http.ResponseWriter, err error) {
	status, code := errorCode(err)
	writeError(w, status, code)
}
func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

type privateResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *privateResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if status >= 200 {
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *privateResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}
func (w *privateResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *privateResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
