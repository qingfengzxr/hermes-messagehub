package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestFeatureExecutionDoctorsBeforeEachAndStopsAfterFailure(t *testing.T) {
	report := newReport(t.TempDir())
	var calls []string
	doctor := func() error { calls = append(calls, "doctor"); return nil }
	drive := func(name string) error {
		calls = append(calls, name)
		if name == "inbox" {
			return errors.New("fixture failure")
		}
		return nil
	}
	if err := executeFeatures(report, []string{"identity", "inbox", "files"}, doctor, drive); err == nil {
		t.Fatal("failure swallowed")
	}
	want := []string{"doctor", "identity", "doctor", "inbox", "doctor"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v", calls)
	}
	if len(report.Features) != 2 || report.Features[1].Outcome != "ISSUES" {
		t.Fatal("false coverage")
	}
}
func TestFeatureExecutionNeverDrivesAfterDoctorFailure(t *testing.T) {
	report := newReport(t.TempDir())
	calls := 0
	doctors := 0
	err := executeFeatures(report, []string{"identity"}, func() error { doctors++; return errors.New("failed") }, func(string) error { calls++; return nil })
	if err == nil || calls != 0 || doctors != 2 {
		t.Fatal("doctor failure did not fail closed with diagnostic doctor")
	}
}
func TestFeatureRegistryAndScopeAreExplicit(t *testing.T) {
	a := &App{}
	if !slices.Equal(scenarioNames(a), featureNames) {
		t.Fatal("feature registry drift")
	}
	r := newReport(t.TempDir())
	r.Mode = "doctor"
	r.RequestedFeature = "identity"
	b, err := json.Marshal(r)
	if err != nil || !bytes.Contains(b, []byte(`"mode":"doctor"`)) || !bytes.Contains(b, []byte(`"requested_feature":"identity"`)) {
		t.Fatal("ambiguous report scope")
	}
}
func TestBuildUsesPinnedReadonlyToolchainWithoutRunning(t *testing.T) {
	cmd := buildCommand(context.Background(), "/module", "/fixture/messagehub")
	for _, arg := range []string{"-mod=readonly", "-trimpath", "-ldflags=-s -w", "./cmd/messagehub"} {
		if !slices.Contains(cmd.Args, arg) {
			t.Fatalf("missing %s", arg)
		}
	}
	if cmd.Dir != "/module" || !slices.Contains(cmd.Env, "CGO_ENABLED=0") {
		t.Fatal("unsafe build")
	}
}
func TestDriverDoesNotImportProductInternals(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		tree, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range tree.Imports {
			if strings.Contains(imp.Path.Value, "/internal/") || imp.Path.Value == `"database/sql"` {
				t.Fatalf("black box violated in %s", p)
			}
		}
	}
}
func TestMessageAndDeliveryChecksRejectFalseSuccess(t *testing.T) {
	first := object{"message": object{"id": "m_fixture", "sequence": json.Number("1")}}
	retry := object{"message": object{"id": "m_fixture", "sequence": json.Number("2")}}
	if sameMessage(first, retry) {
		t.Fatal("sequence mismatch ignored")
	}
	child(retry, "message")["sequence"] = json.Number("1")
	if !sameMessage(first, retry) {
		t.Fatal("same logical message rejected")
	}
	if sameMessage(object{}, object{}) {
		t.Fatal("empty receipts accepted")
	}
	status := object{"deliveries": []any{object{"recipient": "bob", "state": "acked", "acked_at": json.Number("42")}}}
	state, stamp, ok := deliveryState(status, "bob")
	if !ok || state != "acked" || stamp != 42 {
		t.Fatal("receipt not decoded")
	}
	if _, _, ok := deliveryState(status, "carol"); ok {
		t.Fatal("unknown receiver accepted")
	}
}
func TestOAuthRedirectAndTokenParsingRemainPrivate(t *testing.T) {
	redirect := "https://client.example.invalid/callback"
	code, err := authorizationCode(redirect+"?code=unit-code&state=unit-state", redirect, "unit-state")
	if err != nil || code != "unit-code" {
		t.Fatal("redirect not decoded")
	}
	for _, location := range []string{"https://evil.invalid/?code=unit-code&state=unit-state", redirect + "?code=a&code=b&state=unit-state", redirect + "?code=a&state=wrong", redirect + "?state=unit-state"} {
		if _, err := authorizationCode(location, redirect, "unit-state"); err == nil {
			t.Fatal("bad redirect accepted")
		}
	}
	tokens, err := parseTokens([]byte(`{"access_token":"PRIVATE-ACCESS","refresh_token":"PRIVATE-REFRESH","token_type":"Bearer","expires_in":3600}`))
	if err != nil || tokens.access == "" || tokens.refresh == "" {
		t.Fatal("tokens missing")
	}
	raw, err := json.Marshal(tokens)
	if err != nil || strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("token struct serializes secrets")
	}
	for _, raw := range []string{`{}`, `{"access_token":"a","refresh_token":"b","token_type":"bad"}`, `{"access_token":"a","refresh_token":"b","token_type":"Bearer","expires_in":0}`} {
		if _, err := parseTokens([]byte(raw)); err == nil {
			t.Fatal("invalid token response accepted")
		}
	}
	cookie := &http.Cookie{Name: "__Host-messagehub_oauth", Value: "private-cookie", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if !validBrowserCookie(cookie) {
		t.Fatal("valid cookie rejected")
	}
	cookie.Secure = false
	if validBrowserCookie(cookie) {
		t.Fatal("insecure cookie accepted")
	}
}
func TestSyntheticPayloadIsMultichunkOriginalBinary(t *testing.T) {
	b := filePayload()
	if len(b) <= 65536 || !bytes.Contains(b, []byte{0}) || !bytes.Contains(b, []byte{255}) {
		t.Fatal("fixture not binary or multichunk")
	}
}
func TestCheckModeNeverBuildsOrStartsProduct(t *testing.T) {
	root := featureFixture(t)
	parent := t.TempDir()
	code, err := runVerification(context.Background(), root, options{mode: "check", feature: "all", evidence: parent})
	if err != nil || code != 0 {
		t.Fatalf("check failed: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 {
		t.Fatal("unique evidence missing")
	}
	b, err := os.ReadFile(filepath.Join(parent, entries[0].Name(), "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.Mode != "check" || r.Outcome != "PASS" || r.BinarySHA256 != "" || r.Cleanup.Started != 0 || len(r.Features) != 0 || r.Coverage.Complete {
		t.Fatal("check overclaimed live verification")
	}
	// main may select an exit status, but only after runVerification's deferred cleanup.
	tree, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range tree.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runVerification" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if ok {
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "Exit" {
					t.Error("cleanup bypass")
				}
			}
			return true
		})
	}
}
