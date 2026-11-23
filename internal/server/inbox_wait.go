package server

import (
	"context"
	"io"
	"mime"
	"net/http"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

// This transport operation is deliberately not registered as an MCP tool.
func inboxWaitHandler(backend Backend) http.Handler {
	op := typedOperation("inbox_wait", "/v1/inbox/wait", "", func(ctx context.Context, actor hub.Actor, req hub.WaitInboxRequest) (hub.InboxPage, error) {
		if !validPage(req.PageRequest) || req.WaitMS < 1 || req.WaitMS > 20000 {
			return hub.InboxPage{}, hub.ErrInvalid
		}
		return backend.WaitInbox(ctx, actor, req)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeHubError(w, hub.ErrInvalid)
			return
		}
		actor, ok := r.Context().Value(actorKey{}).(hub.Actor)
		if !ok {
			writeHubError(w, hub.ErrUnauthenticated)
			return
		}
		page, err := op.call(r.Context(), actor, raw)
		if err != nil {
			writeHubError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, page)
	})
}
