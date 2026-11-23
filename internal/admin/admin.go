package admin

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

// Run exposes local operator actions only. It is not registered on HTTP or MCP.
func Run(ctx context.Context, store *hub.Store, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("administrative operation required")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch args[0] {
	case "wake-status":
		if len(args) != 1 {
			return hub.ErrInvalid
		}
		counts, err := store.WakeCounts(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"counts": counts})
	case "principal-add":
		id := fs.String("id", "", "principal ID")
		name := fs.String("name", "", "display name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return hub.ErrInvalid
		}
		if err := store.AddPrincipal(ctx, *id, *name); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"principal_id": *id, "active": true})
	case "principal-disable", "principal-enable":
		id := fs.String("id", "", "principal ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *id == "" {
			return hub.ErrInvalid
		}
		active := args[0] == "principal-enable"
		if err := store.SetPrincipalActive(ctx, *id, active); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"principal_id": *id, "active": active})
	case "grant-revoke":
		id := fs.String("id", "", "grant ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *id == "" {
			return hub.ErrInvalid
		}
		if err := store.RevokeGrant(ctx, *id); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"grant_id": *id, "revoked": true})
	case "grant-create":
		principal := fs.String("principal", "", "principal ID")
		output := fs.String("output", "", "new private credential file")
		ttl := fs.Duration("ttl", 30*24*time.Hour, "credential lifetime")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *principal == "" || !filepath.IsAbs(*output) {
			return hub.ErrInvalid
		}
		parent, err := os.Lstat(filepath.Dir(*output))
		if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
			return errors.New("credential parent must be an existing private directory")
		}
		fd, err := syscall.Open(*output, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return errors.New("credential output must be a new private file")
		}
		file := os.NewFile(uintptr(fd), *output)
		keep := false
		defer func() {
			_ = file.Close()
			if !keep {
				_ = os.Remove(*output)
			}
		}()
		grant, token, err := store.IssueGrant(ctx, *principal, *ttl)
		if err != nil {
			return err
		}
		if err := json.NewEncoder(file).Encode(map[string]any{"token": token, "grant_id": grant.ID, "principal_id": grant.PrincipalID, "expires_at": grant.ExpiresAt}); err != nil {
			_ = store.RevokeGrant(ctx, grant.ID)
			return errors.New("credential could not be persisted; grant revoked")
		}
		if err := file.Sync(); err != nil {
			_ = store.RevokeGrant(ctx, grant.ID)
			return errors.New("credential could not be synced; grant revoked")
		}
		keep = true
		return json.NewEncoder(out).Encode(grant)
	case "backup":
		output := fs.String("output", "", "new private backup file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return hub.ErrInvalid
		}
		if err := store.Backup(ctx, *output); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"verified": true, "path": *output})
	case "oauth-pending":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return hub.ErrInvalid
		}
		pending, err := store.ListPendingAuthorizations(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(pending)
	case "oauth-approve":
		request := fs.String("request", "", "authorization request ID")
		principal := fs.String("principal", "", "explicit principal ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *request == "" || *principal == "" {
			return hub.ErrInvalid
		}
		if err := store.ApproveAuthorization(ctx, *request, *principal); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"request_id": *request, "principal_id": *principal, "approved": true})
	case "conversation-close":
		id := fs.String("id", "", "conversation ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *id == "" {
			return hub.ErrInvalid
		}
		if err := store.CloseConversation(ctx, *id); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"conversation_id": *id, "closed": true})
	default:
		return errors.New("unknown administrative operation")
	}
}
