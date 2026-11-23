package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/admin"
	"github.com/qingfengzxr/hermes-messagehub/internal/config"
	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
	"github.com/qingfengzxr/hermes-messagehub/internal/server"
	"github.com/go-kratos/kratos/v3"
)

var version = "dev"

const usage = `messagehub serve --config /absolute/config.json
messagehub admin --config /absolute/config.json <operation> [flags]
messagehub version

Local administrative operations:
  principal-add --id ID --name NAME
  principal-disable --id ID
  principal-enable --id ID
  grant-create --principal ID --output /private/new-credential.json [--ttl 720h]
  grant-revoke --id GRANT_ID
  oauth-pending
  oauth-approve --request REQUEST_ID --principal ID
  conversation-close --id CONVERSATION_ID
  wake-status
  backup --output /private/new-snapshot.db

Credentials are written only to a new 0600 file, never stdout.
`

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "messagehub:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, logs io.Writer) error {
	if len(args) == 0 {
		return errors.New("command required; use messagehub help")
	}
	if args[0] == "help" || args[0] == "--help" {
		_, err := io.WriteString(out, usage)
		return err
	}
	if args[0] == "version" {
		_, err := fmt.Fprintln(out, "messagehub", version)
		return err
	}
	if args[0] != "serve" && args[0] != "admin" {
		return errors.New("unknown command")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", "", "communication configuration")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || (args[0] == "serve" && fs.NArg() != 0) || (args[0] == "admin" && fs.NArg() == 0) {
		return errors.New("explicit configuration and valid operation required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(*path)
	if err != nil {
		return errors.New("configuration unavailable")
	}
	cfg, err := config.Load(file)
	_ = file.Close()
	if err != nil {
		return err
	}
	store, err := hub.Open(cfg.Database, cfg.Options)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.InitOAuth(ctx, hub.OAuthOptions{Resource: cfg.PublicURL + "/mcp"}); err != nil {
		return err
	}
	if args[0] == "admin" {
		return admin.Run(ctx, store, fs.Args(), out)
	}
	srv, err := server.New(store, server.Options{Address: cfg.Listen, PublicBaseURL: cfg.PublicURL, AllowedHosts: cfg.AllowedHosts, AllowedOrigins: cfg.AllowedOrigins, MaxBodyBytes: cfg.RequestBodyBytes, RequestTimeout: cfg.RequestTimeout, RequestsPerSecond: float64(cfg.RateLimitPerMinute) / 60, Burst: min(cfg.RateLimitPerMinute, 60), MaxConcurrent: cfg.MaxConcurrent, MaxConcurrentWaits: cfg.MaxConcurrentWaits, OAuth: store})
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	var stopWake func()
	defer func() {
		if stopWake != nil {
			stopWake()
		}
	}()
	app := kratos.New(kratos.Name("messagehub"), kratos.Version(version), kratos.Context(ctx), kratos.Server(srv), kratos.Logger(logger), kratos.StopTimeout(5*time.Second),
		kratos.BeforeStart(func(ctx context.Context) error {
			var err error
			stopWake, err = store.StartWake(ctx, logger)
			return err
		}))
	return app.Run()
}
