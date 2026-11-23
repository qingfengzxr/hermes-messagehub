package hub

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrInvalid         = errors.New("invalid request")
	ErrUnauthenticated = errors.New("authentication required")
	ErrNotFound        = errors.New("resource unavailable")
	ErrConflict        = errors.New("conflicting request")
	ErrQuota           = errors.New("capacity exceeded")
	ErrExpired         = errors.New("resource expired")
)

type Options struct {
	MessageTTL      time.Duration
	AttachmentTTL   time.Duration
	MaxMessageBytes int
	MaxFileBytes    int64
	MaxChunkBytes   int
	MaxStoreBytes   int64
	MaxMembers      int
	MaxMessages     int
	Now             func() time.Time
	GrokWake        *GrokWakeConfig
}

type Actor struct {
	principalID string
	grantID     string
	tokenHash   string
}

func (a Actor) PrincipalID() string { return a.principalID }
func (a Actor) GrantID() string     { return a.grantID }

type Principal struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

type Grant struct {
	ID          string `json:"id"`
	PrincipalID string `json:"principal_id"`
	ExpiresAt   int64  `json:"expires_at"`
	Revoked     bool   `json:"revoked"`
}

type Identity struct {
	PrincipalID     string      `json:"principal_id"`
	Peers           []Principal `json:"peers"`
	MaxMessageBytes int         `json:"max_message_bytes"`
	MaxFileBytes    int64       `json:"max_file_bytes"`
	MaxChunkBytes   int         `json:"max_chunk_bytes"`
}

type PageRequest struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// WaitInboxRequest bounds an unacknowledged inbox wait to 1..20000 milliseconds.
type WaitInboxRequest struct {
	PageRequest
	WaitMS int `json:"wait_ms"`
}

type CreateConversationRequest struct {
	Members        []string `json:"members"`
	IdempotencyKey string   `json:"idempotency_key"`
}

type Conversation struct {
	ID        string   `json:"id"`
	Members   []string `json:"members"`
	CreatedAt int64    `json:"created_at"`
	Closed    bool     `json:"closed"`
}

type ConversationPage struct {
	Items      []Conversation `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
	HasMore    bool           `json:"has_more"`
}

type SendRequest struct {
	ConversationID string          `json:"conversation_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Text           string          `json:"text,omitempty"`
	Data           json.RawMessage `json:"data,omitempty"`
	ReplyTo        string          `json:"reply_to,omitempty"`
	AttachmentIDs  []string        `json:"attachment_ids,omitempty"`
}

type Message struct {
	ID              string          `json:"id"`
	ConversationID  string          `json:"conversation_id"`
	Sequence        int64           `json:"sequence"`
	Sender          string          `json:"sender"`
	Text            string          `json:"text,omitempty"`
	Data            json.RawMessage `json:"data,omitempty"`
	ReplyTo         string          `json:"reply_to,omitempty"`
	AttachmentIDs   []string        `json:"attachment_ids,omitempty"`
	CreatedAt       int64           `json:"created_at"`
	ExpiresAt       int64           `json:"expires_at"`
	Expired         bool            `json:"expired"`
	ReplyDepth      *int            `json:"reply_depth,omitempty"`
	ReplyDepthLimit int             `json:"reply_depth_limit,omitempty"`
}

type MessagePage struct {
	Items      []Message `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
	HasMore    bool      `json:"has_more"`
}

type Delivery struct {
	MessageID string `json:"message_id"`
	Recipient string `json:"recipient"`
	Sequence  int64  `json:"sequence"`
	State     string `json:"state"`
	AckedAt   int64  `json:"acked_at,omitempty"`
}

type SendReceipt struct {
	Stored     bool       `json:"stored"`
	Message    Message    `json:"message"`
	Deliveries []Delivery `json:"deliveries"`
}

type InboxItem struct {
	Message  Message  `json:"message"`
	Delivery Delivery `json:"delivery"`
}

type InboxPage struct {
	Items      []InboxItem `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
	HasMore    bool        `json:"has_more"`
}

type AckReceipt struct {
	MessageIDs   []string `json:"message_ids"`
	Acknowledged bool     `json:"acknowledged"`
}

type BeginAttachmentRequest struct {
	ConversationID string `json:"conversation_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Filename       string `json:"filename"`
	MediaType      string `json:"media_type"`
	SizeBytes      int64  `json:"size_bytes"`
	SHA256         string `json:"sha256"`
}

type Attachment struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Filename       string `json:"filename"`
	MediaType      string `json:"media_type"`
	SizeBytes      int64  `json:"size_bytes"`
	SHA256         string `json:"sha256"`
	State          string `json:"state"`
	ReceivedBytes  int64  `json:"received_bytes"`
	ExpiresAt      int64  `json:"expires_at"`
}

type AttachmentChunk struct {
	Attachment    Attachment `json:"attachment"`
	Offset        int64      `json:"offset"`
	NextOffset    int64      `json:"next_offset"`
	EOF           bool       `json:"eof"`
	ContentBase64 string     `json:"content_base64"`
}
