package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/qingfengzxr/hermes-messagehub/internal/hub"
)

func TestStrictOperationRejectsDuplicateFieldsBeforeDispatch(t *testing.T) {
	called := false
	op := typedOperation("test", "/test", "test", func(_ context.Context, _ hub.Actor, in hub.SendRequest) (hub.SendRequest, error) {
		called = true
		return in, nil
	})
	for _, raw := range []string{
		`{"conversation_id":"foreign","conversation_id":"c","idempotency_key":"k","text":"x"}`,
		`{"conversation_id":"c","idempotency_key":"k","data":{"recipient":"a","recipient":"b"}}`,
	} {
		if _, err := op.call(context.Background(), hub.Actor{}, []byte(raw)); !errors.Is(err, hub.ErrInvalid) {
			t.Fatalf("duplicate JSON accepted: %v", err)
		}
	}
	if called {
		t.Fatal("ambiguous JSON reached backend")
	}
}
func TestStrictOperationTypesAndSchemaMatch(t *testing.T) {
	called := false
	op := typedOperation("read", "/read", "read", func(_ context.Context, _ hub.Actor, in readAttachment) (readAttachment, error) {
		called = true
		return in, nil
	})
	for _, raw := range []string{`null`, `{}`, `{"message_id":"m","attachment_id":"f","offset":null}`, `{"message_id":"m","attachment_id":"f","offset":true}`, `{"message_id":"m","attachment_id":"f","offset":0.5}`, `{"message_id":"m","attachment_id":"f","offset":0,"sender":"bob"}`, `{"message_id":"m","attachment_id":"f","offset":0} {}`} {
		if _, err := op.call(context.Background(), hub.Actor{}, []byte(raw)); !errors.Is(err, hub.ErrInvalid) {
			t.Fatalf("invalid JSON accepted %q", raw)
		}
	}
	if called {
		t.Fatal("invalid call dispatched")
	}
	result, err := op.call(context.Background(), hub.Actor{}, []byte(`{"message_id":"m","attachment_id":"f","offset":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if string(encoded) != `{"message_id":"m","attachment_id":"f","offset":9007199254740993}` {
		t.Fatal("integer lost precision", string(encoded))
	}
}
