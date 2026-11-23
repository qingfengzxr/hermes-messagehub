# Grok wake

Omit `grok_wake` to disable outbound wakes. The three route IDs fix one direction of one existing two-member conversation. Use a new, never-reused `generation` when changing the route, replacing the routine credentials, or re-enabling the worker. Run one `serve` process for the database. Do not overlap two instances during a restart.

See [Grok Routine setup](GROK-ROUTINE.md) for recipient-side configuration and acceptance checks.

Example public configuration fragment (synthetic IDs and path):

```json
{
  "grok_wake": {
    "generation": "wake-1",
    "sender": "alice",
    "recipient": "bob",
    "conversation_id": "c_example",
    "credential_file": "/private/grok-wake.json",
    "reply_depth_limit": 32
  }
}
```

Only `serve` with an enabled route opens the credential file. Give the service user read access to a private regular file (mode `0400` or `0600`) with exactly two JSON strings, `url` and `token`. The URL must be the literal HTTPS `api2.cursor.sh` endpoint at `/automations/webhook/<lowercase-uuid>` used by the old routine adapter. Do not put either value in the public config. An unreadable or invalid file blocks enabled startup without printing its path or contents. The `scratch` runtime includes the pinned builder's CA bundle. Certificate and hostname verification remain on.

A matching authenticated send inserts its delivery and its wake together. Idempotent send retries do not enqueue again, and enabling does not backfill history. A wake sends only `event: "messagehub.new_message"` and `message_id`; it does not carry conversation text, metadata, or an inbox credential. The recipient still authenticates, pulls, and ACKs through MessageHub. Terminal replies also wake the recipient so it can pull and ACK them.

Only HTTP `200` becomes `accepted`; this does not mean the recipient has read or ACKed the message. Identified temporary dial failures get at most three attempts total, with backoffs of 30 and 60 seconds. Unknown transport results, HTTP `408` or `5xx`, and claims left in `sending` by a stopped process become `uncertain` without replay. Other non-200 responses become `rejected`. The worker waits at least 10 seconds after an attempt finishes before issuing the next for the same directed conversation. An interrupted attempt reserves the interval from restart. Queued items from old routes or generations, closed conversations, disabled principals, and ACKed or expired deliveries become `skipped`. Starting without the route retires pending wakes without using credentials. Eligibility is checked in the claim transaction; an ACK that commits after the claim can still race with an in-flight wake.

`messagehub admin --config /absolute/config.json wake-status` shows aggregate outbox counts across all generations. It does not read the credential file or show message IDs, route IDs, URLs, or tokens. Treat `uncertain` as possibly delivered; use normal inbox tooling to check the message instead of resending a webhook. To roll back to a pre-wake binary, remove `grok_wake` from its config. The optional table does not change SQLite `user_version`.

Both authenticated members see top-level `reply_depth` on live messages in the bound DM. A root is `0`; replies count database links up to `grok_wake.reply_depth_limit`. Set the limit to an integer from `4` to `32`. Omission or `0` preserves the legacy limit of `4` and its response shape. For higher limits the message also contains the top-level `reply_depth_limit`. Missing, foreign, non-earlier, or already-expired ancestors return the limit. Ancestor expiry is evaluated at message creation, so links expiring later do not change the depth under the same limit. `data.reply_requested` is client intent, not authorization or proof of identity. Consumers may answer legacy roots without the flag. For a reply, require boolean `true` and depth below the limit. ACK at the limit without answering. Put `reply_requested: false` on any reply that reaches the limit.

Keep the limit at `4` until the installed Hermes plugin and the actual Grok Routine can both consume the new top-level field and preserve the old fallback. Then deploy the compatible server with the new setting. Changing the limit alone does not change the route or generation. To roll back to an older server, remove the key before starting that binary and keep compatible consumers until old deliveries settle. Consumers keep the limits and actions already persisted for each admitted message.
