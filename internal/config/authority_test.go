package config

import (
	"slices"
	"strings"
	"testing"
)

func TestAllowedAuthoritiesMatchRealHTTPHosts(t *testing.T) {
	cfg, err := Load(strings.NewReader(`{"database":"/data/hub.db","public_url":"https://message.example.test:9443"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"message.example.test:9443", "127.0.0.1:8098", "localhost:8098", "[::1]:8098"} {
		if !slices.Contains(cfg.AllowedHosts, want) {
			t.Fatalf("missing HTTP authority %s", want)
		}
	}
	if _, err := Load(strings.NewReader(`{"database":"/data/hub.db","public_url":"https://message.example.test","allowed_hosts":["message.example.test","127.0.0.1:8098"]}`)); err != nil {
		t.Fatal("explicit local authority rejected")
	}
}
