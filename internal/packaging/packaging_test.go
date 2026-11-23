package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingfengzxr/hermes-messagehub/internal/config"
)

func TestPinnedStandaloneContainerAndExample(t *testing.T) {
	root := filepath.Join("..", "..")
	body, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, part := range []string{"golang:1.25.12-bookworm@sha256:6359592445455f2dbe2412bed411336035bc019a50017720d77454ffdd6d0f82", "CGO_ENABLED=0", "FROM scratch", "USER 10001:10001", "./cmd/messagehub"} {
		if !strings.Contains(text, part) {
			t.Fatalf("packaging invariant missing: %s", part)
		}
	}
	for _, forbidden := range []string{"latest", "hermes_gateway", "runtime-home", "Obsidian", "COPY . "} {
		if strings.Contains(text, forbidden) {
			t.Fatal("container includes unrelated runtime or broad context")
		}
	}
	ignore, err := os.ReadFile(filepath.Join(root, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(ignore), "**\n") {
		t.Fatal("build context must be allowlisted")
	}
	f, err := os.Open(filepath.Join(root, "configs", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8098" {
		t.Fatal("example defaults to public listener")
	}
}

func TestScratchRuntimeHasSystemTLSRoots(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	_, runtime, _ := strings.Cut(string(body), "FROM scratch\n")
	if !strings.Contains(runtime, "COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\n") {
		t.Fatal("scratch runtime has no system trust bundle for outbound TLS")
	}
}
