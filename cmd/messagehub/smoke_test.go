package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

type smokeProcess struct {
	cmd  *exec.Cmd
	done chan error
	logs bytes.Buffer
}

func startSmoke(t *testing.T, binary, configPath, base string) *smokeProcess {
	t.Helper()
	p := &smokeProcess{done: make(chan error, 1)}
	p.cmd = exec.Command(binary, "serve", "--config", configPath)
	p.cmd.Stdout = &p.logs
	p.cmd.Stderr = &p.logs
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.done <- p.cmd.Wait() }()
	t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return p
			}
		}
		select {
		case err := <-p.done:
			t.Fatalf("binary failed before readiness: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("binary readiness timeout")
	return nil
}
func stopSmoke(t *testing.T, p *smokeProcess) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Fatal("binary shutdown timeout")
	}
}
func smokeCLI(t *testing.T, binary, configPath string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(binary, append([]string{"admin", "--config", configPath}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("admin command %s failed", args[0])
	}
	return out
}
func smokeHTTP(t *testing.T, base, token, path string, in, out any, status int) {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		t.Fatalf("binary %s status=%d want=%d", path, resp.StatusCode, status)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
}

func TestBinarySmokeRestartFilesAndBackup(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "messagehub")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-trimpath", "-o", binary, "./cmd/messagehub")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("binary build: %v: %s", err, out)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	base := "http://" + address
	configPath := filepath.Join(dir, "config.json")
	configBody, _ := json.Marshal(map[string]any{"database": filepath.Join(dir, "state.db"), "public_url": "https://message.example.test", "listen": address, "rate_limit_per_minute": 6000})
	if err := os.WriteFile(configPath, configBody, 0600); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	grants := map[string]string{}
	for _, id := range []string{"alice", "bob", "charlie"} {
		smokeCLI(t, binary, configPath, "principal-add", "--id", id, "--name", id)
		path := filepath.Join(dir, id+".json")
		out := smokeCLI(t, binary, configPath, "grant-create", "--principal", id, "--output", path)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cred struct {
			Token   string `json:"token"`
			GrantID string `json:"grant_id"`
		}
		if err := json.Unmarshal(body, &cred); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte(cred.Token)) {
			t.Fatal("CLI exposed token")
		}
		tokens[id] = cred.Token
		grants[id] = cred.GrantID
	}
	process := startSmoke(t, binary, configPath, base)
	var conv hub.Conversation
	smokeHTTP(t, base, tokens["alice"], "/v1/conversations/create", hub.CreateConversationRequest{Members: []string{"alice", "bob"}, IdempotencyKey: "smoke-thread"}, &conv, 200)
	var first hub.SendReceipt
	request := hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "smoke-message", Text: "first message", Data: json.RawMessage(`{"kind":"fixture"}`)}
	smokeHTTP(t, base, tokens["alice"], "/v1/messages/send", request, &first, 200)
	raw := bytes.Repeat([]byte{0, 255, 65, 42}, 40000)
	sum := sha256.Sum256(raw)
	var file hub.Attachment
	smokeHTTP(t, base, tokens["alice"], "/v1/attachments/begin", hub.BeginAttachmentRequest{ConversationID: conv.ID, IdempotencyKey: "smoke-file", Filename: "fixture.bin", MediaType: "application/octet-stream", SizeBytes: int64(len(raw)), SHA256: hex.EncodeToString(sum[:])}, &file, 200)
	for offset := 0; offset < len(raw); offset += 64 << 10 {
		end := min(offset+(64<<10), len(raw))
		smokeHTTP(t, base, tokens["alice"], "/v1/attachments/write", map[string]any{"attachment_id": file.ID, "offset": offset, "content_base64": base64.StdEncoding.EncodeToString(raw[offset:end])}, nil, 200)
	}
	smokeHTTP(t, base, tokens["alice"], "/v1/attachments/commit", map[string]any{"attachment_id": file.ID}, &file, 200)
	var fileMessage hub.SendReceipt
	smokeHTTP(t, base, tokens["alice"], "/v1/messages/send", hub.SendRequest{ConversationID: conv.ID, IdempotencyKey: "smoke-file-message", AttachmentIDs: []string{file.ID}}, &fileMessage, 200)
	var incoming hub.InboxPage
	smokeHTTP(t, base, tokens["bob"], "/v1/inbox/pull", map[string]any{}, &incoming, 200)
	if len(incoming.Items) != 2 {
		t.Fatal("inbox missing before restart")
	}
	stopSmoke(t, process)
	for _, token := range tokens {
		if strings.Contains(process.logs.String(), token) {
			t.Fatal("service log exposed credential")
		}
	}
	process = startSmoke(t, binary, configPath, base)
	var retry hub.SendReceipt
	smokeHTTP(t, base, tokens["alice"], "/v1/messages/send", request, &retry, 200)
	if retry.Message.ID != first.Message.ID {
		t.Fatal("restart duplicated message")
	}
	smokeHTTP(t, base, tokens["bob"], "/v1/inbox/pull", map[string]any{}, &incoming, 200)
	if len(incoming.Items) != 2 {
		t.Fatal("restart lost pending inbox")
	}
	var recovered []byte
	offset := int64(0)
	for {
		var chunk hub.AttachmentChunk
		smokeHTTP(t, base, tokens["bob"], "/v1/attachments/read", map[string]any{"message_id": fileMessage.Message.ID, "attachment_id": file.ID, "offset": offset}, &chunk, 200)
		part, err := base64.StdEncoding.DecodeString(chunk.ContentBase64)
		if err != nil {
			t.Fatal(err)
		}
		recovered = append(recovered, part...)
		if chunk.EOF {
			break
		}
		if chunk.NextOffset <= offset {
			t.Fatal("file cursor stalled")
		}
		offset = chunk.NextOffset
	}
	if !bytes.Equal(raw, recovered) || sha256.Sum256(recovered) != sum {
		t.Fatal("binary file mismatch")
	}
	for _, token := range []string{"", tokens["charlie"]} {
		want := 404
		if token == "" {
			want = 401
		}
		smokeHTTP(t, base, token, "/v1/messages/read", map[string]any{"conversation_id": conv.ID}, nil, want)
		smokeHTTP(t, base, token, "/v1/attachments/read", map[string]any{"message_id": fileMessage.Message.ID, "attachment_id": file.ID, "offset": 0}, nil, want)
	}
	smokeHTTP(t, base, tokens["bob"], "/v1/inbox/ack", map[string]any{"message_ids": []string{first.Message.ID, fileMessage.Message.ID}}, nil, 200)
	var status hub.SendReceipt
	smokeHTTP(t, base, tokens["alice"], "/v1/messages/status", map[string]any{"message_id": first.Message.ID}, &status, 200)
	if status.Deliveries[0].State != "acked" {
		t.Fatal("binary ACK missing")
	}
	backup := filepath.Join(dir, "backup.db")
	smokeCLI(t, binary, configPath, "backup", "--output", backup)
	smokeCLI(t, binary, configPath, "grant-revoke", "--id", grants["bob"])
	smokeHTTP(t, base, tokens["bob"], "/v1/inbox/pull", map[string]any{}, nil, 401)
	stopSmoke(t, process)
	copy, err := hub.Open(backup, hub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	actor, err := copy.Authenticate(t.Context(), tokens["alice"])
	if err != nil {
		t.Fatal(err)
	}
	copied, err := copy.ReadAttachment(t.Context(), actor, fileMessage.Message.ID, file.ID, 0)
	if err != nil || copied.Attachment.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("backup did not preserve file state")
	}
	t.Log(fmt.Sprintf("real binary: restart, idempotency, private text/JSON, %d-byte original file, ACK, anonymous/nonmember denial, revocation, backup read-back PASS", len(raw)))
}
