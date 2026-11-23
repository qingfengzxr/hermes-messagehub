package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"reflect"
	"unicode/utf8"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
	"github.com/google/jsonschema-go/jsonschema"
)

// Backend freezes the Store contract. Only the twelve communication operations
// are MCP tools. WaitInbox is REST-only; CloseConversation has no public route.
type Backend interface {
	CloseConversation(context.Context, string) error
	Authenticate(context.Context, string) (hub.Actor, error)
	Identity(context.Context, hub.Actor) (hub.Identity, error)
	CreateConversation(context.Context, hub.Actor, hub.CreateConversationRequest) (hub.Conversation, error)
	ListConversations(context.Context, hub.Actor, hub.PageRequest) (hub.ConversationPage, error)
	Send(context.Context, hub.Actor, hub.SendRequest) (hub.SendReceipt, error)
	ReadMessages(context.Context, hub.Actor, string, hub.PageRequest) (hub.MessagePage, error)
	ReadReplies(context.Context, hub.Actor, string, string, hub.PageRequest) (hub.MessagePage, error)
	MessageStatus(context.Context, hub.Actor, string) (hub.SendReceipt, error)
	PullInbox(context.Context, hub.Actor, hub.PageRequest) (hub.InboxPage, error)
	WaitInbox(context.Context, hub.Actor, hub.WaitInboxRequest) (hub.InboxPage, error)
	Ack(context.Context, hub.Actor, []string) (hub.AckReceipt, error)
	BeginAttachment(context.Context, hub.Actor, hub.BeginAttachmentRequest) (hub.Attachment, error)
	WriteAttachment(context.Context, hub.Actor, string, int64, []byte) (hub.Attachment, error)
	CommitAttachment(context.Context, hub.Actor, string) (hub.Attachment, error)
	ReadAttachment(context.Context, hub.Actor, string, string, int64) (hub.AttachmentChunk, error)
}
type readMessages struct {
	ConversationID string      `json:"conversation_id"`
	ReplyTo        replyParent `json:"reply_to,omitempty"`
	hub.PageRequest
}
type replyParent string

func (id *replyParent) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return hub.ErrInvalid
	}
	*id = replyParent(value)
	return nil
}

type messageID struct {
	MessageID string `json:"message_id"`
}
type ackMessages struct {
	MessageIDs []string `json:"message_ids"`
}
type attachmentID struct {
	AttachmentID string `json:"attachment_id"`
}
type writeAttachment struct {
	AttachmentID  string `json:"attachment_id"`
	Offset        int64  `json:"offset"`
	ContentBase64 string `json:"content_base64"`
}
type readAttachment struct {
	MessageID    string `json:"message_id"`
	AttachmentID string `json:"attachment_id"`
	Offset       int64  `json:"offset"`
}
type operation struct {
	name, path, description string
	schema                  *jsonschema.Schema
	call                    func(context.Context, hub.Actor, []byte) (any, error)
}

func typedOperation[In, Out any](name, path, description string, fn func(context.Context, hub.Actor, In) (Out, error)) operation {
	schema, err := jsonschema.For[In](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{reflect.TypeFor[json.RawMessage](): {Type: "object"}}})
	if err != nil {
		panic(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		panic(err)
	}
	return operation{name: name, path: path, description: description, schema: schema, call: func(ctx context.Context, a hub.Actor, raw []byte) (any, error) {
		var input In
		if err := strictJSON(raw, &input); err != nil {
			return nil, err
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil || resolved.Validate(value) != nil {
			return nil, hub.ErrInvalid
		}
		return fn(ctx, a, input)
	}}
}
func strictJSON(raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return hub.ErrInvalid
	}
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 || trim[0] != '{' {
		return hub.ErrInvalid
	}
	tokens := json.NewDecoder(bytes.NewReader(raw))
	tokens.UseNumber()
	if rejectDuplicateKeys(tokens, 0) != nil {
		return hub.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return hub.ErrInvalid
	}
	if d.Decode(new(any)) != io.EOF {
		return hub.ErrInvalid
	}
	return nil
}
func rejectDuplicateKeys(d *json.Decoder, depth int) error {
	if depth > 64 {
		return hub.ErrInvalid
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return hub.ErrInvalid
			}
			keys[name] = true
			if err := rejectDuplicateKeys(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := rejectDuplicateKeys(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return hub.ErrInvalid
	}
	_, err = d.Token()
	return err
}
func validPage(p hub.PageRequest) bool {
	return p.Limit >= 0 && p.Limit <= 100 && len(p.Cursor) <= 1024
}
func operations(b Backend) []operation {
	return []operation{
		typedOperation("identity", "/v1/identity", "Read your authenticated identity and private-team peers.", func(ctx context.Context, a hub.Actor, _ struct{}) (hub.Identity, error) { return b.Identity(ctx, a) }),
		typedOperation("conversations_create", "/v1/conversations/create", "Create a private fixed-members conversation.", b.CreateConversation),
		typedOperation("conversations_list", "/v1/conversations/list", "List your conversations using a scoped cursor.", func(ctx context.Context, a hub.Actor, r hub.PageRequest) (hub.ConversationPage, error) {
			if !validPage(r) {
				return hub.ConversationPage{}, hub.ErrInvalid
			}
			return b.ListConversations(ctx, a, r)
		}),
		typedOperation("messages_send", "/v1/messages/send", "Store an idempotent message as your authenticated identity. Does not execute tools.", b.Send),
		typedOperation("messages_read", "/v1/messages/read", "Read authorized conversation history or direct replies to reply_to without acknowledging delivery.", func(ctx context.Context, a hub.Actor, r readMessages) (hub.MessagePage, error) {
			if !validPage(r.PageRequest) {
				return hub.MessagePage{}, hub.ErrInvalid
			}
			if r.ReplyTo != "" {
				return b.ReadReplies(ctx, a, r.ConversationID, string(r.ReplyTo), r.PageRequest)
			}
			return b.ReadMessages(ctx, a, r.ConversationID, r.PageRequest)
		}),
		typedOperation("messages_status", "/v1/messages/status", "Read authorized message delivery receipts.", func(ctx context.Context, a hub.Actor, r messageID) (hub.SendReceipt, error) {
			return b.MessageStatus(ctx, a, r.MessageID)
		}),
		typedOperation("inbox_pull", "/v1/inbox/pull", "Pull your pending inbox without acknowledging it.", func(ctx context.Context, a hub.Actor, r hub.PageRequest) (hub.InboxPage, error) {
			if !validPage(r) {
				return hub.InboxPage{}, hub.ErrInvalid
			}
			return b.PullInbox(ctx, a, r)
		}),
		typedOperation("inbox_ack", "/v1/inbox/ack", "Explicitly acknowledge your deliveries.", func(ctx context.Context, a hub.Actor, r ackMessages) (hub.AckReceipt, error) {
			return b.Ack(ctx, a, r.MessageIDs)
		}),
		typedOperation("attachments_begin", "/v1/attachments/begin", "Begin a private inert attachment upload.", b.BeginAttachment),
		typedOperation("attachments_write", "/v1/attachments/write", "Append a bounded Base64 chunk at an exact byte offset.", func(ctx context.Context, a hub.Actor, r writeAttachment) (hub.Attachment, error) {
			if r.Offset < 0 {
				return hub.Attachment{}, hub.ErrInvalid
			}
			data, err := base64.StdEncoding.Strict().DecodeString(r.ContentBase64)
			if err != nil {
				return hub.Attachment{}, hub.ErrInvalid
			}
			return b.WriteAttachment(ctx, a, r.AttachmentID, r.Offset, data)
		}),
		typedOperation("attachments_commit", "/v1/attachments/commit", "Verify and commit an attachment upload.", func(ctx context.Context, a hub.Actor, r attachmentID) (hub.Attachment, error) {
			return b.CommitAttachment(ctx, a, r.AttachmentID)
		}),
		typedOperation("attachments_read", "/v1/attachments/read", "Read a bounded inert chunk of an attachment linked to an authorized message.", func(ctx context.Context, a hub.Actor, r readAttachment) (hub.AttachmentChunk, error) {
			if r.Offset < 0 {
				return hub.AttachmentChunk{}, hub.ErrInvalid
			}
			return b.ReadAttachment(ctx, a, r.MessageID, r.AttachmentID, r.Offset)
		}),
	}
}
