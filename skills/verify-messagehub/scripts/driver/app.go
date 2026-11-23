package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type App struct {
	ctx                                             context.Context
	root, dir, binary, configPath, baseURL, feature string
	config                                          object
	cmd                                             *exec.Cmd
	done                                            chan struct{}
	waitErr                                         error
	http                                            *http.Client
	sessions                                        map[string]*mcp.ClientSession
	actors                                          map[string]actor
	samples                                         map[string]string
	ops                                             []operation
	report                                          *Report
}

func newApp(ctx context.Context, root, evidence string) (*App, error) {
	dir, err := os.MkdirTemp(os.TempDir(), "verify-messagehub-fixture-")
	if err != nil {
		return nil, errors.New("private fixture directory unavailable")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		os.RemoveAll(dir)
		return nil, errors.New("private fixture permissions failed")
	}
	return &App{ctx: ctx, root: root, dir: dir, binary: filepath.Join(dir, "messagehub"), configPath: filepath.Join(dir, "config.json"), sessions: map[string]*mcp.ClientSession{}, actors: map[string]actor{}, samples: map[string]string{}, report: newReport(evidence)}, nil
}
func (a *App) assert(entry, action, expected, observed string, ok bool) error {
	return a.report.step(a.feature, entry, action, expected, observed, ok)
}
func (a *App) saveSample(name string, data []byte) error {
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".bin") {
		return errors.New("invalid sample filename")
	}
	if err := os.WriteFile(filepath.Join(a.report.EvidenceDir, name), data, 0600); err != nil {
		return errors.New("evidence sample write failed")
	}
	a.samples[name] = sha256Hex(data)
	return nil
}
func (a *App) writeReport() (problem error) {
	defer func() {
		if problem != nil {
			a.report.Outcome = "ISSUES"
		}
	}()
	body, err := json.MarshalIndent(a.report, "", "  ")
	if err != nil {
		return errors.New("report encoding failed")
	}
	if err := os.WriteFile(filepath.Join(a.report.EvidenceDir, "report.json"), append(body, '\n'), 0600); err != nil {
		return errors.New("report write failed")
	}
	return nil
}
func (a *App) closeSessions() error {
	var problem error
	for key, s := range a.sessions {
		if err := s.Close(); err != nil {
			problem = errors.New("MCP session cleanup failed")
		}
		delete(a.sessions, key)
	}
	a.report.Cleanup.SessionsClosed = problem == nil
	return problem
}
func (a *App) startOwned(cmd *exec.Cmd) error {
	if a.cmd != nil {
		return errors.New("owned process must be reaped before start")
	}
	if err := cmd.Start(); err != nil {
		return errors.New("owned process start failed")
	}
	a.cmd = cmd
	a.done = make(chan struct{})
	a.waitErr = nil
	a.report.Cleanup.Started++
	done := a.done
	go func() { a.waitErr = cmd.Wait(); close(done) }()
	return nil
}
func (a *App) alive() bool {
	if a.cmd == nil {
		return false
	}
	select {
	case <-a.done:
		return false
	default:
		return true
	}
}
func (a *App) stop() error {
	sessionErr := a.closeSessions()
	if a.cmd == nil {
		return sessionErr
	}
	problem := sessionErr
	select {
	case <-a.done:
		problem = errors.New("owned process exited before requested shutdown")
	default:
		if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			problem = errors.New("owned SIGTERM failed")
		}
		timer := time.NewTimer(8 * time.Second)
		select {
		case <-a.done:
		case <-timer.C:
			a.report.Cleanup.ForcedKills++
			problem = errors.New("owned process required forced kill")
			_ = a.cmd.Process.Kill()
			<-a.done
		}
		timer.Stop()
	}
	if a.waitErr != nil {
		problem = errors.New("owned process exited unsuccessfully")
	}
	a.report.Cleanup.Reaped++
	a.cmd = nil
	a.done = nil
	return problem
}
func (a *App) cleanup() error {
	problem := a.stop()
	if a.http != nil {
		a.http.CloseIdleConnections()
	}
	a.report.Cleanup.ProcessReaped = a.cmd == nil && a.report.Cleanup.Started == a.report.Cleanup.Reaped
	if err := os.RemoveAll(a.dir); err != nil {
		problem = errors.New("private fixture removal failed")
	}
	_, err := os.Stat(a.dir)
	a.report.Cleanup.FixtureRemoved = errors.Is(err, os.ErrNotExist)
	st, err := os.Stat(a.report.EvidenceDir)
	preserved := err == nil && st.IsDir()
	for name, hash := range a.samples {
		b, err := os.ReadFile(filepath.Join(a.report.EvidenceDir, name))
		if err != nil || sha256Hex(b) != hash {
			preserved = false
		}
	}
	a.report.Cleanup.EvidencePreserved = preserved
	if !preserved || !a.report.Cleanup.FixtureRemoved || !a.report.Cleanup.ProcessReaped {
		problem = errors.New("cleanup invariant failed")
	}
	if problem != nil {
		a.report.Outcome = "ISSUES"
	}
	return problem
}
func buildCommand(ctx context.Context, root, binary string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-mod=readonly", "-buildvcs=false", "-trimpath", "-ldflags=-s -w", "-o", binary, "./cmd/messagehub")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd
}
func (a *App) build() error {
	if err := verifyModule(a.root); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Minute)
	defer cancel()
	err := buildCommand(ctx, a.root, a.binary).Run()
	if e := a.assert("CLI go build", "build private binary", "exit 0", exitObservation(err), err == nil); e != nil {
		return e
	}
	raw, err := os.ReadFile(a.binary)
	if err != nil {
		return errors.New("built binary unavailable")
	}
	a.report.BinarySHA256 = sha256Hex(raw)
	return nil
}
func exitObservation(err error) string {
	if err == nil {
		return "exit 0"
	}
	var e *exec.ExitError
	if errors.As(err, &e) {
		return fmt.Sprintf("exit %d", e.ExitCode())
	}
	return "process unavailable"
}
func (a *App) program(command string) error {
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.binary, command)
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	ok := err == nil
	if command == "version" {
		ok = ok && bytes.HasPrefix(out, []byte("messagehub "))
	} else {
		ok = ok && bytes.Contains(out, []byte("messagehub serve")) && bytes.Contains(out, []byte("oauth-approve"))
	}
	return a.assert("CLI "+command, "inspect CLI "+command, "exit 0 and expected command shape", exitObservation(err), ok)
}
func (a *App) admin(success bool, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.binary, append([]string{"admin", "--config", a.configPath}, args...)...)
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	expected := "exit 0"
	if !success {
		expected = "nonzero exit"
	}
	// A failed process start or timeout is not the expected business rejection.
	var exit *exec.ExitError
	rejected := errors.As(err, &exit) && ctx.Err() == nil
	if e := a.assert("CLI admin "+args[0], "local admin "+args[0], expected, exitObservation(err), (success && err == nil) || (!success && rejected)); e != nil {
		return nil, e
	}
	return out, nil
}
func (a *App) createActor(id string) (actor, error) {
	var result actor
	raw, err := a.admin(true, "principal-add", "--id", id, "--name", id)
	if err != nil {
		return result, err
	}
	var added object
	if decodeJSON(raw, &added) != nil || str(added, "principal_id") != id || !flagValue(added, "active") {
		return result, errors.New("principal creation output mismatch")
	}
	path := filepath.Join(a.dir, id+"-credential.json")
	output, err := a.admin(true, "grant-create", "--principal", id, "--output", path)
	if err != nil {
		return result, err
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return result, errors.New("credential file is not private")
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		return result, errors.New("fixture credential read failed")
	}
	var cred object
	if decodeJSON(raw, &cred) != nil {
		return result, errors.New("fixture credential decode failed")
	}
	result = actor{id: id, token: str(cred, "token"), grantID: str(cred, "grant_id")}
	if result.token == "" || result.grantID == "" || str(cred, "principal_id") != id || bytes.Contains(output, []byte(result.token)) {
		return actor{}, errors.New("credential shape or CLI confidentiality mismatch")
	}
	a.actors[id] = result
	return result, nil
}
func (a *App) writeConfig() error {
	body, err := json.Marshal(a.config)
	if err != nil {
		return errors.New("fixture configuration encoding failed")
	}
	if err := os.WriteFile(a.configPath, body, 0600); err != nil {
		return errors.New("fixture configuration write failed")
	}
	return nil
}
func (a *App) prepare() error {
	if err := a.build(); err != nil {
		return err
	}
	if err := a.program("version"); err != nil {
		return err
	}
	if err := a.program("help"); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return errors.New("fixture port selection failed")
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return errors.New("fixture port release failed")
	}
	a.baseURL = "http://" + address
	a.http, err = privateHTTPClient(a.baseURL)
	if err != nil {
		return err
	}
	a.config = object{"database": filepath.Join(a.dir, "state.db"), "listen": address, "public_url": publicURL, "rate_limit_per_minute": 60000, "max_concurrent": 8}
	if err := a.writeConfig(); err != nil {
		return err
	}
	for _, id := range []string{"alice", "bob", "carol"} {
		if _, err := a.createActor(id); err != nil {
			return err
		}
	}
	return a.start()
}
func (a *App) start() error {
	if err := a.ctx.Err(); err != nil {
		return errors.New("verification canceled")
	}
	cmd := exec.Command(a.binary, "serve", "--config", a.configPath)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := a.startOwned(cmd); err != nil {
		return err
	}
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !a.alive() {
			return errors.New("owned server exited before readiness")
		}
		res, body, err := a.request(http.MethodGet, "/healthz", actor{}, "", nil, nil)
		if err == nil && res.StatusCode == 200 {
			var health object
			if decodeJSON(body, &health) == nil && str(health, "status") == "ok" {
				return nil
			}
		}
		select {
		case <-a.ctx.Done():
			return errors.New("verification canceled")
		case <-deadline.C:
			return errors.New("owned readiness timeout")
		case <-ticker.C:
		}
	}
}
func (a *App) request(method, path string, who actor, media string, body []byte, headers http.Header) (*http.Response, []byte, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return nil, nil, errors.New("invalid local request path")
	}
	req, err := http.NewRequestWithContext(a.ctx, method, a.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, errors.New("HTTP request construction failed")
	}
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	if who.token != "" {
		req.Header.Set("Authorization", "Bearer "+who.token)
	}
	for key, values := range headers {
		req.Header[key] = append([]string{}, values...)
	}
	res, err := a.http.Do(req)
	if err != nil {
		return nil, nil, errors.New("HTTP request failed")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return nil, nil, errors.New("HTTP response read failed or too large")
	}
	return res, raw, nil
}
func (a *App) rest(who actor, name string, in object, status int) (object, error) {
	path := ""
	for _, op := range a.ops {
		if op.Name == name {
			path = op.Path
		}
	}
	if path == "" {
		return nil, errors.New("operation missing from source")
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, errors.New("request encoding failed")
	}
	res, raw, err := a.request(http.MethodPost, path, who, "application/json", body, nil)
	actual := 0
	if res != nil {
		actual = res.StatusCode
	}
	if e := a.assert("REST "+path, name, fmt.Sprintf("HTTP %d", status), fmt.Sprintf("HTTP %d", actual), err == nil && actual == status); e != nil {
		return nil, e
	}
	var out object
	if err := decodeJSON(raw, &out); err != nil {
		return nil, err
	}
	if status == 200 {
		a.report.cover("REST", name)
	}
	return out, nil
}
func (a *App) session(who actor) (*mcp.ClientSession, error) {
	if s := a.sessions[who.token]; s != nil {
		return s, nil
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	s, err := connectSDK(ctx, a.http, a.baseURL, who.token)
	if err != nil {
		return nil, err
	}
	a.sessions[who.token] = s
	a.report.Cleanup.SessionsClosed = false
	return s, nil
}
func (a *App) mcp(who actor, name string, in object) (object, error) {
	s, err := a.session(who)
	if err != nil {
		_ = a.assert("MCP initialize", name, "authenticated session", "initialization failed", false)
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	var out object
	err = callSDK(ctx, s, name, in, &out)
	observed := "success"
	if err != nil {
		observed = "SDK or tool error"
	}
	if e := a.assert("MCP "+name, name, "success", observed, err == nil); e != nil {
		return nil, e
	}
	a.report.cover("MCP", name)
	return out, nil
}
func (a *App) call(protocol string, who actor, name string, in object) (object, error) {
	if protocol == "MCP" {
		return a.mcp(who, name, in)
	}
	return a.rest(who, name, in, 200)
}
func (a *App) doctor() error {
	if err := a.assert("process", "doctor owned process", "running", "owned process probe", a.alive()); err != nil {
		return err
	}
	res, raw, err := a.request(http.MethodGet, "/healthz", actor{}, "", nil, nil)
	actual := 0
	if res != nil {
		actual = res.StatusCode
	}
	var health object
	ok := err == nil && actual == 200 && decodeJSON(raw, &health) == nil && str(health, "status") == "ok"
	if err := a.assert("HTTP /healthz", "doctor health", "HTTP 200 status ok", fmt.Sprintf("HTTP %d", actual), ok); err != nil {
		return err
	}
	identity, err := a.rest(a.actors["alice"], "identity", object{}, 200)
	if err != nil {
		return err
	}
	if err := a.check("REST /v1/identity", "doctor authenticated identity", "alice", str(identity, "principal_id") == "alice"); err != nil {
		return err
	}
	session, err := a.session(a.actors["alice"])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	err = checkTools(ctx, session, a.ops)
	observed := "exact source tool set"
	if err != nil {
		observed = "tool set or SDK mismatch"
	}
	return a.assert("MCP initialize + tools/list", "doctor registered tools", "exact source tool set", observed, err == nil)
}
func (a *App) pages(protocol string, who actor, name string, input object) ([]object, error) {
	return collectPages(func(cursor string) (page, error) {
		request := object{}
		for k, v := range input {
			request[k] = v
		}
		request["limit"] = 2
		if cursor != "" {
			request["cursor"] = cursor
		}
		out, err := a.call(protocol, who, name, request)
		if err != nil {
			return page{}, err
		}
		hasMore, ok := out["has_more"].(bool)
		if !ok {
			return page{}, errors.New("pagination has_more missing")
		}
		if _, ok := out["items"].([]any); !ok {
			return page{}, errors.New("pagination items missing")
		}
		return page{Items: objects(out, "items"), HasMore: hasMore, NextCursor: str(out, "next_cursor")}, nil
	})
}
