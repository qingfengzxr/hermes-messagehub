# Authenticated inbox wait

`POST /v1/inbox/wait` is a REST transport operation, not a model tool. The MCP surface remains the existing twelve tools.

Send the usual Bearer credential and `Content-Type: application/json`:

```json
{"wait_ms":20000,"limit":50}
```

`wait_ms` is required and must be an integer from 1 through 20000. `cursor` and `limit` have the same meaning as `/v1/inbox/pull`; a cursor is scoped to its recipient. Unknown or duplicate JSON fields, null values, invalid field types, and out-of-range values are rejected. Existing Host, Origin, body size, shared request-rate, authentication and no-store checks apply.

The response is the existing `InboxPage`:

- Pending messages return immediately, with the usual `items`, `next_cursor` and `has_more` fields.
- An empty timeout returns HTTP 200 with `items: []` and `has_more: false`.
- Waiting never acknowledges delivery. Persist the full page before advancing a client checkpoint, and use the separate ACK operation when appropriate. On reconnect, an empty cursor starts again from all unacknowledged deliveries.
- Revoked or expired authority fails authentication on a subsequent scan, including the final timeout scan. Request cancellation and `RequestTimeout` take precedence over waiting. A shorter configured request deadline can therefore return 504 before `wait_ms` elapses.

## Store and server boundaries

`hub.WaitInboxRequest` embeds `PageRequest` and adds `WaitMS int`. `Store.WaitInbox(ctx, actor, request)` returns `InboxPage`. The server `Backend` contract includes this method, but `operations()` does not register it with MCP.

Each loop captures the store's broadcast generation before calling `PullInbox`. `Send` broadcasts only after a successful transaction commit. Broadcasts carry no data and grant no authority; even an unrelated or repeated send only triggers another authenticated database scan. Every 250ms the wait also rescans, covering commits from another Store instance, grant revocation, disabled principals and token/grant expiry. A final scan checks authority when the requested wait expires. No database connection or transaction remains held during sleep. The broadcast uses constant store state, not a retained subscriber map.

`server.Options.MaxConcurrentWaits` defaults to 64 and accepts 1..10000. The application file setting `max_concurrent_waits` defaults to 64, is restricted to 1..128, and is passed explicitly into service construction. It is a separate bounded pool from `MaxConcurrent`, so sleeping receivers do not take Send or ACK slots. Exhaustion returns 429. Cancellation, completion and errors release the slot.

The wait duration does not override the existing request deadline. The default request timeout is 30 seconds; `server.New` already sets socket read/write timeouts to `RequestTimeout + 5s`. The bounded sleep and short database scans remain within that existing request envelope. Public OAuth routes and behavior are unchanged.

## Offline verification

From the repository root, use the pinned, network-disabled, non-root wrapper with a prepared cache:

```sh
export MESSAGEHUB_BUILD_CACHE=/absolute/prepared-cache
bash scripts/go test ./... -count=1 -v
bash scripts/go test -race ./... -count=1 -v
bash scripts/go vet ./...
bash scripts/go test -race ./internal/hub ./internal/server -run TestInboxWait -short -count=20 -v
```

`internal/hub/inbox_wait_test.go` covers pending messages, scoped cursors, commit wakeups, independent Store commits, authority changes, cancel/deadline, final authority checks, subscribe-before-scan, commit-only notifications and concurrent reconnects. `internal/server/inbox_wait_test.go` exercises the real SQLite store and Kratos handler over HTTP, including independent wait quotas, Send/ACK progress, strict JSON, rate limits, revocation and the real 20-second timeout with matching socket timeouts. The short repeat run skips only that 20-second test. Existing SDK tests continue to assert the exact twelve-tool MCP surface.

These tests use fixtures and do not connect to a deployed service.
