package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadRequiresOnlyCommunicationConfiguration(t *testing.T) {
	cfg, err := Load(strings.NewReader(`{"database":"/data/hub.db","public_url":"https://message.example.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8098" {
		t.Fatal("unsafe default listener")
	}
	if cfg.Options.MessageTTL != 30*24*time.Hour || cfg.Options.AttachmentTTL != 7*24*time.Hour {
		t.Fatal("missing retention defaults")
	}
	if len(cfg.AllowedHosts) != 4 || cfg.MaxConcurrent != 8 {
		t.Fatal("missing bounded configuration")
	}
}

func TestLoadRejectsUnsafeOrUnknownConfiguration(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"database":"relative.db","public_url":"https://message.example.test"}`,
		`{"database":"/data/hub.db","public_url":"http://public.example.test"}`,
		`{"database":"/data/hub.db","public_url":"https://user:password@example.test"}`,
		`{"database":"/data/hub.db","public_url":"https://example.test/?token=x"}`,
		`{"database":"/data/hub.db","public_url":"https://example.test","api_key":"not-allowed"}`,
		`{"database":"/data/hub.db","public_url":"https://example.test","max_concurrent":-1}`,
		`{"database":"/data/hub.db","public_url":"https://example.test","message_retention":"1h","attachment_retention":"2h"}`,
		`{"database":"/data/hub.db","public_url":"https://example.test","allowed_origins":["*"]}`,
		`{"database":"/data/hub.db","public_url":"https://example.test"} {}`,
	} {
		if _, err := Load(strings.NewReader(body)); err == nil {
			t.Fatalf("accepted invalid config: %s", body)
		}
	}
}

func TestLoadAcceptsExplicitContainerListenerAndLimits(t *testing.T) {
	cfg, err := Load(strings.NewReader(`{"database":"/data/hub.db","public_url":"https://message.example.test","listen":"0.0.0.0:8098","message_retention":"48h","attachment_retention":"24h","max_message_bytes":1024,"max_attachment_bytes":4096,"max_store_bytes":16384,"request_body_bytes":65536,"request_timeout_seconds":10,"rate_limit_per_minute":100,"max_concurrent":2,"allowed_hosts":["message.example.test"],"allowed_origins":["https://message.example.test"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Options.MaxFileBytes != 4096 || cfg.Options.MessageTTL != 48*time.Hour || cfg.MaxConcurrent != 2 {
		t.Fatal("explicit limits ignored")
	}
}

func TestLoadBoundsConfiguredGrokReplyDepth(t *testing.T) {
	const prefix = `{"database":"/data/hub.db","public_url":"https://message.example.test","grok_wake":{"generation":"g1","sender":"alice","recipient":"bob","conversation_id":"dm","credential_file":"/private/wake.json"`
	for _, suffix := range []string{"", `,"reply_depth_limit":32`} {
		if _, err := Load(strings.NewReader(prefix + suffix + `}}`)); err != nil {
			t.Fatalf("valid reply depth %q: %v", suffix, err)
		}
	}
	for _, invalid := range []string{`-1`, `3`, `33`, `true`, `"32"`, `32.5`} {
		if _, err := Load(strings.NewReader(prefix + `,"reply_depth_limit":` + invalid + `}}`)); err == nil {
			t.Fatalf("accepted invalid reply depth %s", invalid)
		}
	}
}
