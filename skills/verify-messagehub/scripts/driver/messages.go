package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func sameMessage(first, retry object) bool {
	a, b := child(first, "message"), child(retry, "message")
	return str(a, "id") != "" && str(a, "id") == str(b, "id") && number(a, "sequence") > 0 && number(a, "sequence") == number(b, "sequence")
}
func deliveryState(receipt object, recipient string) (string, int64, bool) {
	found := false
	state := ""
	var stamp int64
	for _, d := range objects(receipt, "deliveries") {
		if str(d, "recipient") == recipient {
			if found {
				return "", 0, false
			}
			found = true
			state = str(d, "state")
			stamp = number(d, "acked_at")
		}
	}
	return state, stamp, found
}
func (a *App) check(entry, action, expected string, ok bool) error {
	observed := "matched"
	if !ok {
		observed = "mismatch"
	}
	return a.assert(entry, action, expected, observed, ok)
}
func (a *App) conversation(protocol, key string, members []string) (string, error) {
	out, err := a.call(protocol, a.actors["alice"], "conversations_create", object{"members": members, "idempotency_key": key})
	if err != nil {
		return "", err
	}
	id := str(out, "id")
	if err := a.assert(protocol+" conversations_create", "conversation created", "nonempty conversation ID", safeObjectID(id), objectIDPattern.MatchString(id)); err != nil {
		return "", err
	}
	return id, nil
}
func (a *App) send(protocol string, who actor, request object) (object, error) {
	out, err := a.call(protocol, who, "messages_send", request)
	if err != nil {
		return nil, err
	}
	msg := child(out, "message")
	id := str(msg, "id")
	if err := a.assert(protocol+" messages_send", "stored message", "stored with authenticated sender and positive sequence", safeObjectID(id), flagValue(out, "stored") && objectIDPattern.MatchString(id) && str(msg, "sender") == who.id && number(msg, "sequence") > 0); err != nil {
		return nil, err
	}
	return out, nil
}
func (a *App) status(protocol string, who actor, id string) (object, error) {
	return a.call(protocol, who, "messages_status", object{"message_id": id})
}
func (a *App) checkDelivery(protocol string, who actor, id, recipient, want string) (int64, error) {
	out, err := a.status(protocol, who, id)
	if err != nil {
		return 0, err
	}
	state, stamp, ok := deliveryState(out, recipient)
	err = a.check(protocol+" messages_status", "delivery state", want, ok && state == want && (want != "acked" || stamp > 0))
	return stamp, err
}
func (a *App) rejectMCP(who actor, name string, in object, code string) error {
	s, err := a.session(who)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	out, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: in})
	ok := err == nil && out != nil && out.IsError && len(out.Content) == 1
	if ok {
		text, yes := out.Content[0].(*mcp.TextContent)
		ok = yes && text.Text == code
	}
	return a.check("MCP "+name, "reject unauthorized or conflicting request", code, ok)
}
func (a *App) messages() error {
	alice, bob := a.actors["alice"], a.actors["bob"]
	conv, err := a.conversation("REST", "messages-thread", []string{"alice", "bob"})
	if err != nil {
		return err
	}
	retry, err := a.call("MCP", alice, "conversations_create", object{"members": []string{"bob", "alice"}, "idempotency_key": "messages-thread"})
	if err != nil {
		return err
	}
	if err := a.check("REST + MCP conversations_create", "reordered conversation retry", "same conversation ID", str(retry, "id") == conv); err != nil {
		return err
	}
	closed, err := a.conversation("MCP", "messages-closed-thread", []string{"alice", "bob"})
	if err != nil {
		return err
	}
	for _, protocol := range []string{"REST", "MCP"} {
		list, err := a.pages(protocol, bob, "conversations_list", object{})
		if err != nil {
			return err
		}
		if err := a.check(protocol+" conversations_list", "both conversation entry points visible", "both created IDs", containsID(list, conv) && containsID(list, closed)); err != nil {
			return err
		}
	}
	request := object{"conversation_id": conv, "idempotency_key": "messages-first", "text": "private text 你好", "data": json.RawMessage(`{"exact":9007199254740993,"kind":"fixture"}`)}
	first, err := a.send("REST", alice, request)
	if err != nil {
		return err
	}
	second, err := a.send("MCP", alice, request)
	if err != nil {
		return err
	}
	if err := a.check("REST + MCP messages_send", "cross-protocol message retry", "same ID and sequence", sameMessage(first, second)); err != nil {
		return err
	}
	conflict := object{}
	for k, v := range request {
		conflict[k] = v
	}
	conflict["text"] = "different body"
	if _, err := a.rest(alice, "messages_send", conflict, 409); err != nil {
		return err
	}
	if err := a.rejectMCP(alice, "messages_send", conflict, "conflict"); err != nil {
		return err
	}
	originalID := str(child(first, "message"), "id")
	reply, err := a.send("MCP", bob, object{"conversation_id": conv, "idempotency_key": "messages-reply", "text": "reply", "reply_to": originalID})
	if err != nil {
		return err
	}
	if err := a.check("MCP messages_send", "reply links real message", "actual returned parent ID", str(child(reply, "message"), "reply_to") == originalID); err != nil {
		return err
	}
	if _, err := a.send("REST", alice, object{"conversation_id": conv, "idempotency_key": "messages-third", "text": "pagination fixture"}); err != nil {
		return err
	}
	for _, protocol := range []string{"REST", "MCP"} {
		history, err := a.pages(protocol, bob, "messages_read", object{"conversation_id": conv})
		if err != nil {
			return err
		}
		exact := false
		replySeen := false
		for _, m := range history {
			if str(m, "id") == originalID {
				exact = str(m, "text") == "private text 你好" && child(m, "data")["exact"] == json.Number("9007199254740993")
			}
			if str(m, "id") == str(child(reply, "message"), "id") {
				replySeen = str(m, "reply_to") == originalID
			}
		}
		if err := a.assert(protocol+" messages_read", "history pagination and lossless JSON", "3 messages; exact integer and reply preserved", fmt.Sprintf("count %d", len(history)), len(history) == 3 && exact && replySeen); err != nil {
			return err
		}
		status, err := a.status(protocol, alice, originalID)
		if err != nil {
			return err
		}
		if err := a.check(protocol+" messages_status", "stored message status", "same message ID and sequence", sameMessage(first, status)); err != nil {
			return err
		}
	}
	for _, probe := range []struct {
		who    actor
		status int
	}{{a.actors["carol"], 404}, {actor{}, 401}} {
		if _, err := a.rest(probe.who, "messages_read", object{"conversation_id": conv}, probe.status); err != nil {
			return err
		}
		if _, err := a.rest(probe.who, "messages_status", object{"message_id": originalID}, probe.status); err != nil {
			return err
		}
	}
	if err := a.rejectMCP(a.actors["carol"], "messages_read", object{"conversation_id": conv}, "not_found"); err != nil {
		return err
	}
	if _, err := a.rest(alice, "messages_send", object{"conversation_id": conv, "idempotency_key": "forged-sender", "text": "fixture", "sender": "bob"}, 400); err != nil {
		return err
	}
	closeRequest := object{"conversation_id": closed, "idempotency_key": "before-close", "text": "history survives closure"}
	before, err := a.send("REST", alice, closeRequest)
	if err != nil {
		return err
	}
	raw, err := a.admin(true, "conversation-close", "--id", closed)
	if err != nil {
		return err
	}
	var receipt object
	if decodeJSON(raw, &receipt) != nil || !flagValue(receipt, "closed") || str(receipt, "conversation_id") != closed {
		return errors.New("conversation close receipt mismatch")
	}
	history, err := a.pages("MCP", bob, "messages_read", object{"conversation_id": closed})
	if err != nil {
		return err
	}
	if err := a.check("MCP messages_read", "closed conversation history", "previous message still readable", containsID(history, str(child(before, "message"), "id"))); err != nil {
		return err
	}
	if _, err := a.rest(alice, "messages_send", object{"conversation_id": closed, "idempotency_key": "after-close-new", "text": "must fail"}, 409); err != nil {
		return err
	}
	after, err := a.send("MCP", alice, closeRequest)
	if err != nil {
		return err
	}
	return a.check("MCP messages_send", "closed conversation idempotent retry", "old ID and sequence", sameMessage(before, after))
}
func (a *App) inbox() error {
	alice, bob, carol := a.actors["alice"], a.actors["bob"], a.actors["carol"]
	conv, err := a.conversation("REST", "inbox-thread", []string{"alice", "bob", "carol"})
	if err != nil {
		return err
	}
	ids := []string{}
	for i := 0; i < 3; i++ {
		out, err := a.send("REST", alice, object{"conversation_id": conv, "idempotency_key": fmt.Sprintf("inbox-%d", i), "text": "ack fixture"})
		if err != nil {
			return err
		}
		ids = append(ids, str(child(out, "message"), "id"))
	}
	for _, protocol := range []string{"REST", "MCP", "REST"} {
		incoming, err := a.pages(protocol, bob, "inbox_pull", object{})
		if err != nil {
			return err
		}
		if err := a.check(protocol+" inbox_pull", "pull returns pending fixtures", "three known pending messages", containsID(incoming, ids[0]) && containsID(incoming, ids[1]) && containsID(incoming, ids[2])); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if _, err := a.checkDelivery("MCP", alice, id, "bob", "pending"); err != nil {
			return err
		}
	}
	ack, err := a.call("REST", bob, "inbox_ack", object{"message_ids": []string{ids[0], ids[0]}})
	if err != nil {
		return err
	}
	if err := a.check("REST inbox_ack", "duplicate ACK IDs deduplicated", "one acknowledged ID", flagValue(ack, "acknowledged") && len(array(ack, "message_ids")) == 1 && array(ack, "message_ids")[0] == ids[0]); err != nil {
		return err
	}
	firstStamp, err := a.checkDelivery("REST", bob, ids[0], "bob", "acked")
	if err != nil {
		return err
	}
	// Cross a wall-clock second so replacing acked_at cannot pass accidentally.
	select {
	case <-time.After(1100 * time.Millisecond):
	case <-a.ctx.Done():
		return errors.New("verification canceled")
	}
	ack, err = a.call("MCP", bob, "inbox_ack", object{"message_ids": []string{ids[0], ids[0]}})
	if err != nil {
		return err
	}
	if err := a.check("MCP inbox_ack", "repeat ACK receipt", "one acknowledged ID", flagValue(ack, "acknowledged") && len(array(ack, "message_ids")) == 1); err != nil {
		return err
	}
	repeatStamp, err := a.checkDelivery("MCP", bob, ids[0], "bob", "acked")
	if err != nil {
		return err
	}
	if err := a.check("REST + MCP inbox_ack", "ACK timestamp immutable", "same nonzero acked_at", firstStamp > 0 && firstStamp == repeatStamp); err != nil {
		return err
	}
	sender, err := a.status("REST", alice, ids[0])
	if err != nil {
		return err
	}
	receiver, err := a.status("MCP", bob, ids[0])
	if err != nil {
		return err
	}
	cs, _, co := deliveryState(sender, "carol")
	bs, _, bo := deliveryState(sender, "bob")
	if err := a.check("REST + MCP messages_status", "receipt visibility", "sender sees both; receiver sees only self", len(objects(sender, "deliveries")) == 2 && len(objects(receiver, "deliveries")) == 1 && str(objects(receiver, "deliveries")[0], "recipient") == "bob" && co && bo && cs == "pending" && bs == "acked"); err != nil {
		return err
	}
	// messageValidKey accepts zzzz-missing; it sorts after real m_* IDs.
	missing := "zzzz-missing"
	if _, err := a.rest(bob, "messages_status", object{"message_id": missing}, 404); err != nil {
		return err
	}
	if err := a.check("fixture", "ACK valid item precedes missing item", "valid item sorts first", ids[1] < missing && ids[2] < missing); err != nil {
		return err
	}
	if _, err := a.rest(bob, "inbox_ack", object{"message_ids": []string{ids[1], missing}}, 404); err != nil {
		return err
	}
	if err := a.rejectMCP(bob, "inbox_ack", object{"message_ids": []string{ids[2], missing}}, "not_found"); err != nil {
		return err
	}
	for _, id := range ids[1:] {
		if _, err := a.checkDelivery("REST", alice, id, "bob", "pending"); err != nil {
			return err
		}
	}
	fresh, err := a.pages("MCP", bob, "inbox_pull", object{})
	if err != nil {
		return err
	}
	if err := a.check("MCP inbox_pull", "failed batch rolled back from fresh cursor", "both valid IDs remain pending", containsID(fresh, ids[1]) && containsID(fresh, ids[2])); err != nil {
		return err
	}
	if _, err := a.call("MCP", bob, "inbox_ack", object{"message_ids": ids[1:]}); err != nil {
		return err
	}
	fresh, err = a.pages("REST", bob, "inbox_pull", object{})
	if err != nil {
		return err
	}
	for _, id := range ids {
		if containsID(fresh, id) {
			return a.check("REST inbox_pull", "ACK removes pending entry", "known ACKed IDs absent from fresh inbox", false)
		}
	}
	if err := a.check("REST inbox_pull", "fresh inbox reflects ACK", "known ACKed IDs absent", true); err != nil {
		return err
	}
	history, err := a.pages("REST", bob, "messages_read", object{"conversation_id": conv})
	if err != nil {
		return err
	}
	if err := a.check("REST messages_read", "ACK preserves history", "three original messages", len(history) == 3 && containsID(history, ids[0]) && containsID(history, ids[1]) && containsID(history, ids[2])); err != nil {
		return err
	}
	_, err = a.checkDelivery("MCP", carol, ids[0], "carol", "pending")
	return err
}
