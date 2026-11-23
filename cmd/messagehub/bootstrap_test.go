package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

func TestBinaryBootstrapCrashRecovery(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "messagehub")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-trimpath", "-o", binary, "./cmd/messagehub")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v %s", err, output)
	}
	interrupted := 0
	for range 12 {
		cfg := commandConfig(t, "127.0.0.1:0")
		path := filepath.Join(filepath.Dir(cfg), "state.db")
		cmd := exec.Command(binary, "admin", "--config", cfg, "principal-add", "--id", "alice", "--name", "Alice")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		observed := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if info, err := os.Stat(path); err == nil && info.Size() > 0 {
				observed = true
				break
			}
			runtime.Gosched()
		}
		_ = cmd.Process.Kill()
		waitErr := cmd.Wait()
		if !observed {
			t.Fatal("bootstrap never wrote a database header")
		}
		if exit, ok := waitErr.(*exec.ExitError); ok {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signal() == syscall.SIGKILL {
				interrupted++
			}
		}
		s, err := hub.Open(path, hub.Options{})
		if err != nil {
			t.Fatalf("cannot recover interrupted bootstrap: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if interrupted == 0 {
		t.Fatal("test did not interrupt a bootstrap process")
	}
	t.Logf("recovered %d forcibly interrupted bootstrap processes", interrupted)
}
