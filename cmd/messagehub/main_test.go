package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func commandConfig(t *testing.T, listen string) string {
	t.Helper()
	dir := t.TempDir()
	body, err := json.Marshal(map[string]any{"database": filepath.Join(dir, "state.db"), "public_url": "https://message.example.test", "listen": listen})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestCommandVersionAndLocalAdministration(t *testing.T) {
	var out, logs bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &out, &logs); err != nil || !strings.Contains(out.String(), "messagehub") {
		t.Fatal("version unavailable")
	}
	cfg := commandConfig(t, "127.0.0.1:8098")
	if err := run(context.Background(), []string{"admin", "--config", cfg, "principal-add", "--id", "alice", "--name", "Alice"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"principal_id":"alice"`) {
		t.Fatal("administrative receipt missing")
	}
	for _, args := range [][]string{nil, {"unknown"}, {"serve"}, {"serve", "--config", cfg, "extra"}} {
		if err := run(context.Background(), args, &out, &logs); err == nil {
			t.Fatal("invalid command accepted")
		}
	}
}
func TestKratosCommandStartsAndStopsWithoutBusinessConfiguration(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	cfg := commandConfig(t, addr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve", "--config", cfg}, io.Discard, io.Discard) }()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("service stopped before ready: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("Kratos never became ready")
	}
	resp, err := client.Get("http://" + addr + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("OAuth discovery not wired")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("service did not stop")
	}
}
