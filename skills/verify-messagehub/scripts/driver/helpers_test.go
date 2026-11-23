package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This child is the test binary, never the product binary.
func TestOwnedFixtureChild(t *testing.T) {
	if os.Getenv("VERIFY_UNIT_CHILD") != "1" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	os.Stdout.WriteString("ready\n")
	<-signals
}

func TestOwnedProcessIsReapedAndEvidenceKept(t *testing.T) {
	a, err := newApp(context.Background(), featureFixture(t), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.cleanup()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnedFixtureChild$")
	cmd.Env = append(os.Environ(), "VERIFY_UNIT_CHILD=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.startOwned(cmd); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatal("child not ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child timeout")
	}
	if !a.alive() {
		t.Fatal("owned fixture child not alive")
	}
	if err := a.stop(); err != nil {
		t.Fatal(err)
	}
	if a.alive() || a.report.Cleanup.Started != 1 || a.report.Cleanup.Reaped != 1 || a.report.Cleanup.ForcedKills != 0 {
		t.Fatal("child not reaped precisely")
	}
	if err := a.stop(); err != nil {
		t.Fatal("stop is not idempotent")
	}
}

func TestOptionsRejectExternalTargets(t *testing.T) {
	o, err := parseOptions(nil)
	if err != nil || o.mode != "run" || o.feature != "all" || o.evidence != "/out/verify-messagehub" {
		t.Fatalf("defaults: %#v %v", o, err)
	}
	for _, args := range [][]string{{"-mode", "bad"}, {"-feature", "bad"}, {"-base-url", "http://127.0.0.1"}, {"-database", "/db"}, {"-pid", "1"}, {"-config", "/config"}, {"-evidence", "relative"}, {"extra"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestDecodeJSONPreservesNumbersAndRejectsTrailingData(t *testing.T) {
	var got map[string]any
	if err := decodeJSON([]byte(`{"exact":9007199254740993}`), &got); err != nil {
		t.Fatal(err)
	}
	if got["exact"] != json.Number("9007199254740993") {
		t.Fatal("integer precision lost")
	}
	for _, raw := range []string{`{} {}`, `{}x`, ``} {
		if decodeJSON([]byte(raw), &got) == nil {
			t.Fatal("invalid JSON accepted")
		}
	}
}

func featureFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(name, data string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module github.com/qingfengzxr/hermes-messagehub\n\ngo 1.25.0\n")
	write("go.sum", "fixture\n")
	write("internal/server/operations.go", `package server
func operations() { typedOperation("identity", "/v1/identity", "description", handler); typedOperation[X,Y]("messages_send", "/v1/messages/send", "description", handler) }`)
	write("internal/admin/admin.go", `package admin
func Run() { switch args[0] { case "principal-add": case "principal-disable", "principal-enable": } }`)
	for _, p := range []string{"cmd/messagehub/main.go", "internal/server/handler.go", "internal/server/oauth.go", "internal/config/config.go"} {
		write(p, "package fixture\n")
	}
	write("scripts/go", "#!/bin/bash\n")
	var index strings.Builder
	for _, name := range []string{"identity-oauth.md", "conversations-messages.md", "inbox.md", "files.md", "lifecycle.md"} {
		index.WriteString("- [Feature](" + name + ")\n")
		write("skills/verify-messagehub/features/"+name, "# Feature\n\n## Sub-features\nidentity /v1/identity messages_send /v1/messages/send principal-add principal-disable principal-enable\n\n## How to get to it (user POV)\nEntry\n\n## Driving it with verify-messagehub\nDrive\n\n## Gotchas\nLimits\n")
	}
	write("skills/verify-messagehub/features/README.md", "# Features\n"+index.String())
	return root
}

func TestFeatureMapValidatesSourcePairsAndIndex(t *testing.T) {
	root := featureFixture(t)
	ops, err := checkFeatureMap(root)
	if err != nil || len(ops) != 2 || ops[1].Name != "messages_send" {
		t.Fatalf("valid map: %v %v", ops, err)
	}
	tests := []struct {
		name   string
		mutate func(string)
	}{
		{"duplicate", func(r string) {
			p := filepath.Join(r, "skills/verify-messagehub/features/README.md")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, append(b, []byte("[again](inbox.md)\n")...), 0600)
		}},
		{"dead", func(r string) { os.Remove(filepath.Join(r, "skills/verify-messagehub/features/inbox.md")) }},
		{"extra", func(r string) {
			os.WriteFile(filepath.Join(r, "skills/verify-messagehub/features/extra.md"), []byte("# Extra"), 0600)
		}},
		{"heading order", func(r string) {
			p := filepath.Join(r, "skills/verify-messagehub/features/inbox.md")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, bytes.Replace(b, []byte("## Sub-features"), []byte("## Gotchas"), 1), 0600)
		}},
		{"unmapped route", func(r string) {
			p := filepath.Join(r, "internal/server/operations.go")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, bytes.ReplaceAll(b, []byte("/v1/identity"), []byte("/v1/new")), 0600)
		}},
		{"unmapped cli", func(r string) {
			p := filepath.Join(r, "internal/admin/admin.go")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, bytes.ReplaceAll(b, []byte("principal-add"), []byte("new-command")), 0600)
		}},
		{"wrong module", func(r string) { os.WriteFile(filepath.Join(r, "go.mod"), []byte("module other\n"), 0600) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := featureFixture(t)
			tc.mutate(r)
			if _, err := checkFeatureMap(r); err == nil {
				t.Fatal("invalid map accepted")
			}
		})
	}
}

func TestSourceHashIgnoresSkillButDetectsProductChanges(t *testing.T) {
	root := featureFixture(t)
	first, err := sourceHash(root)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "skills/verify-messagehub/features/inbox.md"), []byte("doc"), 0600)
	same, err := sourceHash(root)
	if err != nil || same != first {
		t.Fatal("documentation changed source hash")
	}
	os.WriteFile(filepath.Join(root, "internal/server/handler.go"), []byte("package changed"), 0600)
	changed, err := sourceHash(root)
	if err != nil || changed == first {
		t.Fatal("product mutation undetected")
	}
}

func TestCoverageRequiresEveryOperationOnBothProtocols(t *testing.T) {
	r := newReport(t.TempDir())
	ops := []operation{{Name: "identity", Path: "/v1/identity"}, {Name: "messages_send", Path: "/v1/messages/send"}}
	r.cover("REST", "identity")
	r.cover("MCP", "identity")
	r.cover("REST", "messages_send")
	r.cover("REST", "identity")
	if err := r.finishCoverage(ops, true); err == nil {
		t.Fatal("partial coverage accepted")
	}
	r.cover("MCP", "messages_send")
	if err := r.finishCoverage(ops, true); err != nil {
		t.Fatal(err)
	}
	if !r.Coverage.Complete || len(r.Coverage.REST) != 2 {
		t.Fatal("coverage duplicate or incomplete")
	}
}

func TestPaginationUsesHasMoreNotLastCursor(t *testing.T) {
	calls := 0
	items, err := collectPages(func(cursor string) (page, error) {
		calls++
		if calls == 1 {
			if cursor != "" {
				t.Fatal("first cursor not empty")
			}
			return page{Items: []object{{"id": "m_1"}}, HasMore: true, NextCursor: "next"}, nil
		}
		if cursor != "next" {
			t.Fatal("cursor not forwarded")
		}
		return page{Items: []object{{"id": "m_2"}}, NextCursor: "last-nonempty"}, nil
	})
	if err != nil || calls != 2 || len(items) != 2 {
		t.Fatal("pagination did not stop at has_more=false")
	}
	_, err = collectPages(func(string) (page, error) { return page{HasMore: true}, nil })
	if err == nil {
		t.Fatal("stalled page accepted")
	}
	_, err = collectPages(func(string) (page, error) {
		return page{Items: []object{{"id": "same"}}, HasMore: true, NextCursor: "stuck"}, nil
	})
	if err == nil {
		t.Fatal("repeated cursor accepted")
	}
}

func TestDownloadAssemblesBytesAndChecksOffsetsHashAndEOF(t *testing.T) {
	raw := bytes.Repeat([]byte{0, 255, 1, 2}, 20000)
	calls := 0
	fetch := func(offset int64) (chunk, error) {
		calls++
		end := min(int(offset)+32768, len(raw))
		return chunk{Offset: offset, NextOffset: int64(end), EOF: end == len(raw), ContentBase64: base64.StdEncoding.EncodeToString(raw[int(offset):end])}, nil
	}
	got, err := collectChunks(int64(len(raw)), sha256Hex(raw), fetch)
	if err != nil || !bytes.Equal(got, raw) || calls < 2 {
		t.Fatalf("download failed: %v", err)
	}
	for _, bad := range []chunk{{Offset: 1, NextOffset: 1}, {NextOffset: 2, ContentBase64: "AA=="}, {NextOffset: 1, EOF: true, ContentBase64: "AA=="}, {ContentBase64: "not-base64"}} {
		if _, err := collectChunks(2, sha256Hex([]byte{0, 1}), func(int64) (chunk, error) { return bad, nil }); err == nil {
			t.Fatal("invalid chunk accepted")
		}
	}
	if _, err := collectChunks(int64(len(raw)), strings.Repeat("0", 64), fetch); err == nil {
		t.Fatal("hash mismatch accepted")
	}
}

func TestPrivateHTTPClientDoesNotFollowRedirectOrEscape(t *testing.T) {
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Location", "https://example.invalid/?code=secret")
		w.WriteHeader(303)
	}))
	defer ts.Close()
	c, err := privateHTTPClient(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	res, err := c.Get(ts.URL + "/authorize/continue")
	if err != nil || res.StatusCode != 303 || requests != 1 {
		t.Fatal("redirect was followed")
	}
	res.Body.Close()
	if c.Timeout <= 0 {
		t.Fatal("missing timeout")
	}
	for _, target := range []string{"http://example.invalid", "http://127.0.0.1:1", "https://127.0.0.1:1"} {
		if _, err := c.Get(target); err == nil {
			t.Fatal("escaped owned endpoint")
		}
	}
	if _, err := privateHTTPClient("http://localhost:80"); err == nil {
		t.Fatal("ambiguous DNS accepted")
	}
}

func TestSDKUsesOfficialClientAndPreservesJSON(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "helper-fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "identity", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"principal_id":"alice","exact":9007199254740993}`}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "denied", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "not_found"}}}, nil
	})
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer unit-only-token" {
			w.WriteHeader(401)
			return
		}
		sdk.ServeHTTP(w, r)
	}))
	defer ts.Close()
	c, err := privateHTTPClient(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := connectSDK(ctx, c, ts.URL, "unit-only-token")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := checkTools(ctx, s, []operation{{Name: "identity"}, {Name: "denied"}}); err != nil {
		t.Fatal(err)
	}
	if err := checkTools(ctx, s, []operation{{Name: "identity"}}); err == nil {
		t.Fatal("unexpected tool accepted")
	}
	var out object
	if err := callSDK(ctx, s, "identity", object{}, &out); err != nil {
		t.Fatal(err)
	}
	if out["exact"] != json.Number("9007199254740993") {
		t.Fatal("SDK precision lost")
	}
	if err := callSDK(ctx, s, "denied", object{}, &out); err == nil || strings.Contains(err.Error(), "unit-only-token") {
		t.Fatal("IsError ignored or secret exposed")
	}
	if err := callSDK(ctx, s, "unknown", object{}, &out); err == nil {
		t.Fatal("SDK error ignored")
	}
}

func TestEvidenceSurvivesFixtureCleanupWithoutPrivateState(t *testing.T) {
	evidence := t.TempDir()
	a, err := newApp(context.Background(), featureFixture(t), evidence)
	if err != nil {
		t.Fatal(err)
	}
	dir := a.dir
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0700 {
		t.Fatal("fixture permissions")
	}
	a.actors["alice"] = actor{id: "alice", token: "NEVER-ARCHIVE", grantID: "grant-private"}
	sample := []byte{0, 255, 3}
	if err := a.saveSample("unit-source.bin", sample); err != nil {
		t.Fatal(err)
	}
	if err := a.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture not removed")
	}
	if err := a.writeReport(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(evidence, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("NEVER-ARCHIVE")) || bytes.Contains(raw, []byte("grant-private")) || bytes.Contains(raw, []byte(dir)) {
		t.Fatal("private fixture state leaked")
	}
	if !a.report.Cleanup.FixtureRemoved || !a.report.Cleanup.EvidencePreserved || !a.report.Cleanup.ProcessReaped {
		t.Fatal("cleanup not proven")
	}
	kept, err := os.ReadFile(filepath.Join(evidence, "unit-source.bin"))
	if err != nil || !reflect.DeepEqual(kept, sample) {
		t.Fatal("evidence lost")
	}
}

func TestScriptForcesReadOnlySourceAndCGODisabled(t *testing.T) {
	b, err := os.ReadFile("../verify")
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"/../../..", "MESSAGEHUB_SOURCE_MODE=ro", "CGO_ENABLED=0", `exec bash "$module/scripts/go" run ./skills/verify-messagehub/scripts/driver "$@"`} {
		if !strings.Contains(string(b), part) {
			t.Fatalf("missing safe wrapper %s", part)
		}
	}
	st, err := os.Stat("../verify")
	if err != nil || st.Mode().Perm()&0111 == 0 {
		t.Fatal("entrypoint is not executable")
	}
}

func TestReportNeverEmbedsRawHTTPFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, `{"access_token":"DO-NOT-LOG"}`)
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
	a.feature = "identity"
	a.ops = []operation{{Name: "identity", Path: "/v1/identity"}}
	_, err = a.rest(actor{}, "identity", object{}, 200)
	if err == nil {
		t.Fatal("unexpected status accepted")
	}
	data, marshalErr := json.Marshal(a.report)
	if marshalErr != nil || strings.Contains(string(data), "DO-NOT-LOG") || strings.Contains(err.Error(), "DO-NOT-LOG") {
		t.Fatal("raw response leaked")
	}
}
