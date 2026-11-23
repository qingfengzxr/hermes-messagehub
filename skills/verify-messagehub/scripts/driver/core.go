package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const moduleID = "github.com/qingfengzxr/hermes-messagehub"
const publicURL = "https://message.example.invalid"

var featureNames = []string{"identity", "messages", "inbox", "files", "recovery"}

type options struct{ mode, feature, evidence string }
type operation struct{ Name, Path string }
type object = map[string]any
type actor struct{ id, token, grantID string }
type page struct {
	Items      []object `json:"items"`
	NextCursor string   `json:"next_cursor"`
	HasMore    bool     `json:"has_more"`
}
type chunk struct {
	Offset        int64  `json:"offset"`
	NextOffset    int64  `json:"next_offset"`
	EOF           bool   `json:"eof"`
	ContentBase64 string `json:"content_base64"`
}

// Report owns only allowlisted observations, never transport or fixture state.
type Report struct {
	Mode             string          `json:"mode"`
	RequestedFeature string          `json:"requested_feature"`
	Outcome          string          `json:"outcome"`
	SourceSHA256     string          `json:"source_sha256"`
	BinarySHA256     string          `json:"binary_sha256"`
	Features         []FeatureResult `json:"features"`
	Steps            []Step          `json:"steps"`
	Coverage         Coverage        `json:"coverage"`
	Cleanup          Cleanup         `json:"cleanup"`
	EvidenceDir      string          `json:"evidence_dir"`
}
type FeatureResult struct {
	Feature string `json:"feature"`
	Outcome string `json:"outcome"`
}
type Step struct {
	Feature  string `json:"feature"`
	Entry    string `json:"entry"`
	Action   string `json:"action"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
	Outcome  string `json:"outcome"`
}
type Coverage struct {
	REST     []string `json:"rest"`
	MCP      []string `json:"mcp"`
	Required []string `json:"required"`
	Complete bool     `json:"complete"`
}
type Cleanup struct {
	ProcessReaped     bool `json:"process_reaped"`
	FixtureRemoved    bool `json:"fixture_removed"`
	EvidencePreserved bool `json:"evidence_preserved"`
	SessionsClosed    bool `json:"sessions_closed"`
	ForcedKills       int  `json:"forced_kills"`
	Started           int  `json:"started"`
	Reaped            int  `json:"reaped"`
}

func parseOptions(args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("verify-messagehub", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.mode, "mode", "run", "check, doctor, run")
	fs.StringVar(&o.feature, "feature", "all", "all, identity, messages, inbox, files, recovery")
	fs.StringVar(&o.evidence, "evidence", "/out/verify-messagehub", "absolute evidence parent")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return o, errors.New("invalid arguments; only -mode, -feature and -evidence are accepted")
	}
	if !slices.Contains([]string{"check", "doctor", "run"}, o.mode) || (o.feature != "all" && !slices.Contains(featureNames, o.feature)) || !strings.HasPrefix(o.evidence, "/") {
		return o, errors.New("invalid mode, feature or evidence path")
	}
	return o, nil
}
func newReport(evidence string) *Report {
	return &Report{Outcome: "BLOCKED", Features: []FeatureResult{}, Steps: []Step{}, EvidenceDir: evidence, Coverage: Coverage{REST: []string{}, MCP: []string{}, Required: []string{}}}
}
func (r *Report) step(feature, entry, action, expected, observed string, ok bool) error {
	outcome := "PASS"
	if !ok {
		outcome = "ISSUES"
	}
	r.Steps = append(r.Steps, Step{Feature: feature, Entry: entry, Action: action, Expected: expected, Observed: observed, Outcome: outcome})
	if !ok {
		return fmt.Errorf("%s: %s failed", feature, action)
	}
	return nil
}
func (r *Report) cover(protocol, name string) {
	target := &r.Coverage.REST
	if protocol == "MCP" {
		target = &r.Coverage.MCP
	}
	if !slices.Contains(*target, name) {
		*target = append(*target, name)
		sort.Strings(*target)
	}
}
func (r *Report) finishCoverage(ops []operation, required bool) error {
	r.Coverage.Required = nil
	r.Coverage.Complete = true
	for _, op := range ops {
		r.Coverage.Required = append(r.Coverage.Required, op.Name)
		if !slices.Contains(r.Coverage.REST, op.Name) || !slices.Contains(r.Coverage.MCP, op.Name) {
			r.Coverage.Complete = false
		}
	}
	sort.Strings(r.Coverage.Required)
	if required && !r.Coverage.Complete {
		return errors.New("REST/MCP operation coverage incomplete")
	}
	return nil
}
func decodeJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return errors.New("invalid JSON response")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON response data")
	}
	return nil
}
func sha256Hex(b []byte) string           { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func str(o object, key string) string     { s, _ := o[key].(string); return s }
func number(o object, key string) int64   { n, _ := o[key].(json.Number); v, _ := n.Int64(); return v }
func flagValue(o object, key string) bool { b, _ := o[key].(bool); return b }
func child(o object, key string) object   { v, _ := o[key].(map[string]any); return v }
func objects(o object, key string) []object {
	var out []object
	for _, item := range array(o, key) {
		if v, ok := item.(map[string]any); ok {
			out = append(out, v)
		}
	}
	return out
}
func array(o object, key string) []any { v, _ := o[key].([]any); return v }
func containsID(items []object, id string) bool {
	for _, v := range items {
		if str(v, "id") == id || str(child(v, "message"), "id") == id {
			return true
		}
	}
	return false
}

var objectIDPattern = regexp.MustCompile(`^[cmf]_[A-Za-z0-9_-]{1,128}$`)

func safeObjectID(id string) string {
	if objectIDPattern.MatchString(id) {
		return id
	}
	return "invalid-object-id"
}

func collectPages(fetch func(string) (page, error)) ([]object, error) {
	var out []object
	cursor := ""
	seen := map[string]bool{}
	seenIDs := map[string]bool{}
	for n := 0; n < 1000; n++ {
		p, err := fetch(cursor)
		if err != nil {
			return nil, err
		}
		for _, item := range p.Items {
			id := str(item, "id")
			if id == "" {
				id = str(child(item, "message"), "id")
			}
			if id == "" || seenIDs[id] {
				return nil, errors.New("pagination contains missing or repeated object ID")
			}
			seenIDs[id] = true
		}
		out = append(out, p.Items...)
		if !p.HasMore {
			return out, nil
		}
		if len(p.Items) == 0 || p.NextCursor == "" || seen[p.NextCursor] {
			return nil, errors.New("pagination stalled")
		}
		seen[p.NextCursor] = true
		cursor = p.NextCursor
	}
	return nil, errors.New("pagination exceeded fixture bound")
}
func collectChunks(size int64, hash string, fetch func(int64) (chunk, error)) ([]byte, error) {
	if size < 0 || size > 8<<20 {
		return nil, errors.New("invalid fixture size")
	}
	data := make([]byte, 0, int(size))
	offset := int64(0)
	for n := 0; n < 1024; n++ {
		c, err := fetch(offset)
		if err != nil {
			return nil, err
		}
		part, err := base64.StdEncoding.Strict().DecodeString(c.ContentBase64)
		if err != nil || c.Offset != offset || c.NextOffset != offset+int64(len(part)) || c.NextOffset > size {
			return nil, errors.New("invalid attachment chunk")
		}
		data = append(data, part...)
		if c.EOF {
			if int64(len(data)) != size || sha256Hex(data) != hash {
				return nil, errors.New("attachment size or hash mismatch")
			}
			return data, nil
		}
		if c.NextOffset <= offset {
			return nil, errors.New("attachment cursor stalled")
		}
		offset = c.NextOffset
	}
	return nil, errors.New("attachment chunk limit exceeded")
}

type guardedTransport struct {
	target    string
	transport *http.Transport
	mu        sync.Mutex
	last      time.Time
}

func (g *guardedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "http" || r.URL.Host != g.target || r.URL.User != nil {
		return nil, errors.New("request outside owned loopback endpoint")
	}
	// Pace SDK and REST through the same gate. Never retry a write on 429.
	g.mu.Lock()
	wait := 10*time.Millisecond - time.Since(g.last)
	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-r.Context().Done():
			timer.Stop()
			g.mu.Unlock()
			return nil, errors.New("request canceled during pacing")
		}
	}
	g.last = time.Now()
	g.mu.Unlock()
	return g.transport.RoundTrip(r)
}
func (g *guardedTransport) CloseIdleConnections() { g.transport.CloseIdleConnections() }
func privateHTTPClient(base string) (*http.Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid owned loopback endpoint")
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != u.Host {
			return nil, errors.New("dial outside owned loopback endpoint")
		}
		return dialer.DialContext(ctx, network, address)
	}, MaxIdleConns: 8, IdleConnTimeout: 15 * time.Second, ResponseHeaderTimeout: 5 * time.Second, ExpectContinueTimeout: time.Second}
	return &http.Client{Transport: &guardedTransport{target: u.Host, transport: tr}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

type authenticatedTransport struct {
	token string
	base  http.RoundTripper
}

func (a authenticatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	if a.token != "" {
		clone.Header.Set("Authorization", "Bearer "+a.token)
	}
	return a.base.RoundTrip(clone)
}
func connectSDK(ctx context.Context, c *http.Client, base, token string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "verify-messagehub", Version: "1"}, nil)
	hc := *c
	hc.Transport = authenticatedTransport{token: token, base: c.Transport}
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: &hc, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return nil, errors.New("MCP initialization failed")
	}
	return session, nil
}
func checkTools(ctx context.Context, s *mcp.ClientSession, ops []operation) error {
	expected := map[string]bool{}
	for _, op := range ops {
		expected[op.Name] = true
	}
	seen := map[string]bool{}
	cursor := ""
	for n := 0; n < 100; n++ {
		list, err := s.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil || list == nil {
			return errors.New("MCP tools/list failed")
		}
		for _, tool := range list.Tools {
			if tool == nil || !expected[tool.Name] || seen[tool.Name] {
				return errors.New("MCP tools differ from source")
			}
			seen[tool.Name] = true
		}
		if list.NextCursor == "" {
			if len(seen) != len(expected) {
				return errors.New("MCP tool missing")
			}
			return nil
		}
		if list.NextCursor == cursor {
			return errors.New("MCP tools cursor stalled")
		}
		cursor = list.NextCursor
	}
	return errors.New("MCP tools page bound exceeded")
}
func callSDK(ctx context.Context, s *mcp.ClientSession, name string, in, out any) error {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
	if err != nil {
		return errors.New("MCP call failed")
	}
	if res == nil || res.IsError {
		return errors.New("MCP tool rejected request")
	}
	if len(res.Content) != 1 {
		return errors.New("MCP result shape mismatch")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return errors.New("MCP result is not JSON text")
	}
	return decodeJSON([]byte(text.Text), out)
}
