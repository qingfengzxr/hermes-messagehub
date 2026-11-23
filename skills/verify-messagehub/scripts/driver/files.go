package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type fileFixture struct {
	conversationID, attachmentID, messageID string
	source                                  []byte
	request                                 object
}

func filePayload() []byte { return bytes.Repeat([]byte{0, 255, 1, 2, 65, 128, 42, 254}, 20000) }
func opposite(protocol string) string {
	if protocol == "REST" {
		return "MCP"
	}
	return "REST"
}
func (a *App) reject(protocol string, who actor, name string, input object, status int, code string) error {
	if protocol == "MCP" {
		return a.rejectMCP(who, name, input, code)
	}
	_, err := a.rest(who, name, input, status)
	return err
}
func (a *App) download(protocol string, who actor, f fileFixture) ([]byte, error) {
	out, err := collectChunks(int64(len(f.source)), sha256Hex(f.source), func(offset int64) (chunk, error) {
		result, err := a.call(protocol, who, "attachments_read", object{"message_id": f.messageID, "attachment_id": f.attachmentID, "offset": offset})
		if err != nil {
			return chunk{}, err
		}
		metadata := child(result, "attachment")
		if str(metadata, "id") != f.attachmentID || number(metadata, "size_bytes") != int64(len(f.source)) || str(metadata, "sha256") != sha256Hex(f.source) || str(metadata, "state") != "ready" {
			return chunk{}, errors.New("download attachment metadata mismatch")
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return chunk{}, errors.New("download response shape mismatch")
		}
		var c chunk
		if err := decodeJSON(raw, &c); err != nil {
			return chunk{}, err
		}
		return c, nil
	})
	if err != nil {
		return nil, err
	}
	if err := a.assert(protocol+" attachments_read", "original file byte comparison", "size, SHA256 and every byte equal", fmt.Sprintf("bytes %d sha256 %s", len(out), sha256Hex(out)), bytes.Equal(out, f.source)); err != nil {
		return nil, err
	}
	return out, nil
}
func (a *App) uploadFile(protocol, conv, key string, negative bool) (fileFixture, error) {
	alice, bob := a.actors["alice"], a.actors["bob"]
	f := fileFixture{conversationID: conv, source: filePayload()}
	if err := a.saveSample(key+"-source.bin", f.source); err != nil {
		return f, err
	}
	anchorID := ""
	if negative {
		anchor, err := a.send(protocol, alice, object{"conversation_id": conv, "idempotency_key": key + "-anchor", "text": "unlinked file read probe"})
		if err != nil {
			return f, err
		}
		anchorID = str(child(anchor, "message"), "id")
	}
	begin := object{"conversation_id": conv, "idempotency_key": key + "-upload", "filename": "fixture.bin", "media_type": "application/octet-stream", "size_bytes": len(f.source), "sha256": sha256Hex(f.source)}
	file, err := a.call(protocol, alice, "attachments_begin", begin)
	if err != nil {
		return f, err
	}
	f.attachmentID = str(file, "id")
	if err := a.assert(protocol+" attachments_begin", "upload metadata", "uploading, exact size/hash, zero received", safeObjectID(f.attachmentID), objectIDPattern.MatchString(f.attachmentID) && str(file, "state") == "uploading" && number(file, "size_bytes") == int64(len(f.source)) && number(file, "received_bytes") == 0 && str(file, "sha256") == sha256Hex(f.source)); err != nil {
		return f, err
	}
	chunkSize := 64 << 10
	for offset := 0; offset < len(f.source); offset += chunkSize {
		end := min(offset+chunkSize, len(f.source))
		payload := base64.StdEncoding.EncodeToString(f.source[offset:end])
		request := object{"attachment_id": f.attachmentID, "offset": offset, "content_base64": payload}
		file, err = a.call(protocol, alice, "attachments_write", request)
		if err != nil {
			return f, err
		}
		if err := a.assert(protocol+" attachments_write", "byte offset append", "exact cumulative byte count", fmt.Sprintf("received %d", number(file, "received_bytes")), number(file, "received_bytes") == int64(end)); err != nil {
			return f, err
		}
		if offset == 0 && negative {
			retry, err := a.call(protocol, alice, "attachments_write", request)
			if err != nil {
				return f, err
			}
			if err := a.check(protocol+" attachments_write", "identical chunk retry", "received_bytes unchanged", number(retry, "received_bytes") == int64(end)); err != nil {
				return f, err
			}
			if err := a.reject(protocol, alice, "attachments_write", object{"attachment_id": f.attachmentID, "offset": end + 1, "content_base64": "AA=="}, 409, "conflict"); err != nil {
				return f, err
			}
			changed := append([]byte{}, f.source[:end]...)
			changed[0] ^= 255
			if err := a.reject(protocol, alice, "attachments_write", object{"attachment_id": f.attachmentID, "offset": 0, "content_base64": base64.StdEncoding.EncodeToString(changed)}, 409, "conflict"); err != nil {
				return f, err
			}
			if err := a.reject(protocol, alice, "attachments_commit", object{"attachment_id": f.attachmentID}, 409, "conflict"); err != nil {
				return f, err
			}
			if err := a.reject(protocol, bob, "attachments_read", object{"message_id": anchorID, "attachment_id": f.attachmentID, "offset": 0}, 404, "not_found"); err != nil {
				return f, err
			}
		}
	}
	file, err = a.call(protocol, alice, "attachments_commit", object{"attachment_id": f.attachmentID})
	if err != nil {
		return f, err
	}
	if err := a.check(protocol+" attachments_commit", "verified commit", "ready with expected bytes and SHA256", str(file, "state") == "ready" && number(file, "received_bytes") == int64(len(f.source)) && str(file, "sha256") == sha256Hex(f.source)); err != nil {
		return f, err
	}
	if negative {
		if err := a.reject(protocol, bob, "attachments_read", object{"message_id": anchorID, "attachment_id": f.attachmentID, "offset": 0}, 404, "not_found"); err != nil {
			return f, err
		}
	}
	f.request = object{"conversation_id": conv, "idempotency_key": key + "-message", "text": "binary fixture attachment", "attachment_ids": []string{f.attachmentID}}
	receipt, err := a.send(protocol, alice, f.request)
	if err != nil {
		return f, err
	}
	f.messageID = str(child(receipt, "message"), "id")
	linked := array(child(receipt, "message"), "attachment_ids")
	if err := a.check(protocol+" messages_send", "public message binds file", "single expected attachment", len(linked) == 1 && linked[0] == f.attachmentID); err != nil {
		return f, err
	}
	got, err := a.download(protocol, bob, f)
	if err != nil {
		return f, err
	}
	if err := a.saveSample(key+"-download.bin", got); err != nil {
		return f, err
	}
	if negative {
		cross, err := a.download(opposite(protocol), bob, f)
		if err != nil {
			return f, err
		}
		if err := a.saveSample(key+"-cross-download.bin", cross); err != nil {
			return f, err
		}
		read := object{"message_id": f.messageID, "attachment_id": f.attachmentID, "offset": 0}
		if _, err := a.rest(a.actors["carol"], "attachments_read", read, 404); err != nil {
			return f, err
		}
		if _, err := a.rest(actor{}, "attachments_read", read, 401); err != nil {
			return f, err
		}
		if err := a.rejectMCP(a.actors["carol"], "attachments_read", read, "not_found"); err != nil {
			return f, err
		}
		if err := a.reject(protocol, bob, "attachments_read", object{"message_id": anchorID, "attachment_id": f.attachmentID, "offset": 0}, 404, "not_found"); err != nil {
			return f, err
		}
	}
	return f, nil
}
func (a *App) files() error {
	for _, protocol := range []string{"REST", "MCP"} {
		key := "files-" + strings.ToLower(protocol)
		conv, err := a.conversation(protocol, key+"-thread", []string{"alice", "bob"})
		if err != nil {
			return err
		}
		if _, err := a.uploadFile(protocol, conv, key, true); err != nil {
			return err
		}
	}
	return nil
}
