package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

type Config struct {
	Listen             string
	Database           string
	PublicURL          string
	AllowedHosts       []string
	AllowedOrigins     []string
	RequestBodyBytes   int64
	RequestTimeout     time.Duration
	RateLimitPerMinute int
	MaxConcurrent      int
	MaxConcurrentWaits int
	Options            hub.Options
}

type fileConfig struct {
	Listen                string              `json:"listen"`
	Database              string              `json:"database"`
	PublicURL             string              `json:"public_url"`
	AllowedHosts          []string            `json:"allowed_hosts"`
	AllowedOrigins        []string            `json:"allowed_origins"`
	MessageRetention      string              `json:"message_retention"`
	AttachmentRetention   string              `json:"attachment_retention"`
	MaxMessageBytes       int                 `json:"max_message_bytes"`
	MaxAttachmentBytes    int64               `json:"max_attachment_bytes"`
	MaxStoreBytes         int64               `json:"max_store_bytes"`
	RequestBodyBytes      int64               `json:"request_body_bytes"`
	RequestTimeoutSeconds int                 `json:"request_timeout_seconds"`
	RateLimitPerMinute    int                 `json:"rate_limit_per_minute"`
	MaxConcurrent         int                 `json:"max_concurrent"`
	MaxConcurrentWaits    int                 `json:"max_concurrent_waits"`
	GrokWake              *hub.GrokWakeConfig `json:"grok_wake"`
}

var errConfig = errors.New("invalid communication configuration")
var hostPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$`)

func Load(r io.Reader) (Config, error) {
	body, err := io.ReadAll(io.LimitReader(r, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		return Config{}, errConfig
	}
	f := fileConfig{Listen: "127.0.0.1:8098", MessageRetention: "720h", AttachmentRetention: "168h", MaxMessageBytes: 64 << 10, MaxAttachmentBytes: 8 << 20, MaxStoreBytes: 1 << 30, RequestBodyBytes: 1 << 20, RequestTimeoutSeconds: 30, RateLimitPerMinute: 600, MaxConcurrent: 8, MaxConcurrentWaits: 64}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f); err != nil {
		return Config{}, errConfig
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, errConfig
	}
	if f.GrokWake != nil && !f.GrokWake.Valid() {
		return Config{}, errConfig
	}
	if !filepath.IsAbs(f.Database) || filepath.Clean(f.Database) == "/" {
		return Config{}, errConfig
	}
	u, err := url.Parse(f.PublicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return Config{}, errConfig
	}
	host, port, err := net.SplitHostPort(f.Listen)
	if err != nil || (host != "localhost" && net.ParseIP(host) == nil) {
		return Config{}, errConfig
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return Config{}, errConfig
	}
	messageTTL, err := time.ParseDuration(f.MessageRetention)
	if err != nil {
		return Config{}, errConfig
	}
	fileTTL, err := time.ParseDuration(f.AttachmentRetention)
	if err != nil || messageTTL < time.Second || messageTTL > 365*24*time.Hour || fileTTL < time.Second || fileTTL > messageTTL {
		return Config{}, errConfig
	}
	if f.MaxMessageBytes < 1 || f.MaxMessageBytes > 1<<20 || f.MaxAttachmentBytes < 1 || f.MaxAttachmentBytes > 64<<20 || f.MaxStoreBytes < f.MaxAttachmentBytes || f.RequestBodyBytes < 16<<10 || f.RequestBodyBytes > 4<<20 || f.RequestTimeoutSeconds < 1 || f.RequestTimeoutSeconds > 300 || f.RateLimitPerMinute < 1 || f.RateLimitPerMinute > 100000 || f.MaxConcurrent < 1 || f.MaxConcurrent > 128 {
		return Config{}, errConfig
	}
	if f.MaxConcurrentWaits < 1 || f.MaxConcurrentWaits > 128 {
		return Config{}, errConfig
	}
	chunkBytes := min(int64(64<<10), f.MaxAttachmentBytes)
	if f.RequestBodyBytes < 2*chunkBytes+4096 || f.RequestBodyBytes < int64(f.MaxMessageBytes)+4096 {
		return Config{}, errConfig
	}
	if len(f.AllowedHosts) == 0 {
		f.AllowedHosts = []string{u.Host, net.JoinHostPort("localhost", port), net.JoinHostPort("127.0.0.1", port), net.JoinHostPort("::1", port)}
	}
	publicAllowed := false
	for _, h := range f.AllowedHosts {
		authority, err := url.Parse("https://" + h)
		if err != nil || authority.Host != h || authority.User != nil || authority.Path != "" || strings.ContainsAny(h, "/*?# 	\r\n") || (net.ParseIP(authority.Hostname()) == nil && !hostPattern.MatchString(authority.Hostname())) {
			return Config{}, errConfig
		}
		if strings.EqualFold(h, u.Host) {
			publicAllowed = true
		}
	}
	if !publicAllowed {
		return Config{}, errConfig
	}
	for _, origin := range f.AllowedOrigins {
		v, err := url.Parse(origin)
		if err != nil || v.Scheme != "https" || v.Hostname() == "" || v.User != nil || v.Path != "" || v.RawQuery != "" || v.Fragment != "" {
			return Config{}, errConfig
		}
	}
	return Config{Listen: f.Listen, Database: filepath.Clean(f.Database), PublicURL: strings.TrimSuffix(f.PublicURL, "/"), AllowedHosts: f.AllowedHosts, AllowedOrigins: f.AllowedOrigins, RequestBodyBytes: f.RequestBodyBytes, RequestTimeout: time.Duration(f.RequestTimeoutSeconds) * time.Second, RateLimitPerMinute: f.RateLimitPerMinute, MaxConcurrent: f.MaxConcurrent, MaxConcurrentWaits: f.MaxConcurrentWaits, Options: hub.Options{MessageTTL: messageTTL, AttachmentTTL: fileTTL, MaxMessageBytes: f.MaxMessageBytes, MaxFileBytes: f.MaxAttachmentBytes, MaxChunkBytes: int(chunkBytes), MaxStoreBytes: f.MaxStoreBytes, GrokWake: f.GrokWake}}, nil
}
