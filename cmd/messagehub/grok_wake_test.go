package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrokWakeServeRequiresCredentialButStatusDoesNotReadIt(t *testing.T) {
	path := commandConfig(t, "127.0.0.1:0")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(filepath.Dir(path), "synthetic-missing-secret.json")
	cfg["grok_wake"] = map[string]string{"generation": "g1", "sender": "alice", "recipient": "bob", "conversation_id": "c_synthetic", "credential_file": missing}
	body, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	var out, logs bytes.Buffer
	if err := run(context.Background(), []string{"admin", "--config", path, "wake-status"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != `{"counts":{}}` || logs.Len() != 0 {
		t.Fatalf("unexpected status: %s", out.String())
	}
	err = run(context.Background(), []string{"serve", "--config", path}, io.Discard, &logs)
	if err == nil || err.Error() != "grok wake credential unavailable or invalid" || strings.Contains(logs.String(), missing) {
		t.Fatalf("serve did not fail closed: %v", err)
	}
}
