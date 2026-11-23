package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func (a *App) restartForRecovery() error {
	err := a.stop()
	if e := a.assert("owned process SIGTERM + Wait", "orderly recovery shutdown", "reaped without forced kill", fmt.Sprintf("started %d reaped %d forced_kills %d", a.report.Cleanup.Started, a.report.Cleanup.Reaped, a.report.Cleanup.ForcedKills), err == nil && a.report.Cleanup.Started == a.report.Cleanup.Reaped && a.report.Cleanup.ForcedKills == 0); e != nil {
		return e
	}
	if err := a.start(); err != nil {
		return err
	}
	return a.doctor()
}
func (a *App) recovery() error {
	alice, bob := a.actors["alice"], a.actors["bob"]
	conv, err := a.conversation("REST", "recovery-thread", []string{"alice", "bob"})
	if err != nil {
		return err
	}
	request := object{"conversation_id": conv, "idempotency_key": "recovery-pending", "text": "before snapshot"}
	original, err := a.send("REST", alice, request)
	if err != nil {
		return err
	}
	id := str(child(original, "message"), "id")
	file, err := a.uploadFile("REST", conv, "recovery-file", false)
	if err != nil {
		return err
	}
	if _, err := a.checkDelivery("REST", alice, id, "bob", "pending"); err != nil {
		return err
	}
	if err := a.restartForRecovery(); err != nil {
		return err
	}
	retry, err := a.send("MCP", alice, request)
	if err != nil {
		return err
	}
	if err := a.check("MCP messages_send", "same-database restart idempotency", "same message ID and sequence", sameMessage(original, retry)); err != nil {
		return err
	}
	fileRetry, err := a.send("MCP", alice, file.request)
	if err != nil {
		return err
	}
	if err := a.check("MCP messages_send", "same-database file binding retry", "same file message ID", str(child(fileRetry, "message"), "id") == file.messageID); err != nil {
		return err
	}
	incoming, err := a.pages("MCP", bob, "inbox_pull", object{})
	if err != nil {
		return err
	}
	if err := a.check("MCP inbox_pull", "pending messages survive process restart", "both original IDs pending", containsID(incoming, id) && containsID(incoming, file.messageID)); err != nil {
		return err
	}
	recovered, err := a.download("MCP", bob, file)
	if err != nil {
		return err
	}
	if err := a.saveSample("recovery-restarted-download.bin", recovered); err != nil {
		return err
	}
	ack, err := a.call("REST", bob, "inbox_ack", object{"message_ids": []string{id, file.messageID}})
	if err != nil {
		return err
	}
	if err := a.check("REST inbox_ack", "pre-snapshot ACK receipt", "both messages acknowledged", flagValue(ack, "acknowledged") && len(array(ack, "message_ids")) == 2); err != nil {
		return err
	}
	stamps := map[string]int64{}
	for _, mid := range []string{id, file.messageID} {
		stamp, err := a.checkDelivery("MCP", alice, mid, "bob", "acked")
		if err != nil {
			return err
		}
		stamps[mid] = stamp
	}
	snapshot := filepath.Join(a.dir, "snapshot.db")
	raw, err := a.admin(true, "backup", "--output", snapshot)
	if err != nil {
		return err
	}
	var backup object
	if decodeJSON(raw, &backup) != nil || !flagValue(backup, "verified") || str(backup, "path") != snapshot {
		return errors.New("backup not verified by CLI")
	}
	st, err := os.Lstat(snapshot)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() == 0 {
		return errors.New("backup permissions or content invalid")
	}
	snapshotBytes, err := os.ReadFile(snapshot)
	if err != nil {
		return errors.New("owned snapshot unreadable")
	}
	snapshotHash := sha256Hex(snapshotBytes)
	if _, err := a.admin(false, "backup", "--output", snapshot); err != nil {
		return err
	}
	unchanged, err := os.ReadFile(snapshot)
	if err := a.check("CLI admin backup", "existing backup is never overwritten", "snapshot unchanged after rejected overwrite", err == nil && sha256Hex(unchanged) == snapshotHash); err != nil {
		return err
	}
	after, err := a.send("REST", alice, object{"conversation_id": conv, "idempotency_key": "recovery-after-snapshot", "text": "must disappear on snapshot restore"})
	if err != nil {
		return err
	}
	afterID := str(child(after, "message"), "id")
	if _, err := a.status("REST", alice, afterID); err != nil {
		return err
	}
	if _, err := a.admin(true, "grant-revoke", "--id", bob.grantID); err != nil {
		return err
	}
	if _, err := a.rest(bob, "identity", object{}, 401); err != nil {
		return err
	}
	if err := a.stop(); err != nil {
		return err
	}
	if err := a.check("owned process SIGTERM + Wait", "stop before snapshot switch", "all earlier instances reaped", a.report.Cleanup.Started == a.report.Cleanup.Reaped && a.report.Cleanup.ForcedKills == 0); err != nil {
		return err
	}
	restore := filepath.Join(a.dir, "restore.db")
	target, err := os.OpenFile(restore, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("private restore target unavailable")
	}
	_, writeErr := target.Write(snapshotBytes)
	syncErr := target.Sync()
	closeErr := target.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("snapshot copy failed")
	}
	restored, err := os.ReadFile(restore)
	if err != nil || sha256Hex(restored) != snapshotHash {
		return errors.New("snapshot copy differs")
	}
	a.config["database"] = restore
	if err := a.writeConfig(); err != nil {
		return err
	}
	if err := a.start(); err != nil {
		return err
	}
	if err := a.doctor(); err != nil {
		return err
	}
	identity, err := a.rest(bob, "identity", object{}, 200)
	if err != nil {
		return err
	}
	if err := a.check("REST identity", "snapshot rolls authorization back", "bob's pre-snapshot grant becomes usable again", str(identity, "principal_id") == "bob"); err != nil {
		return err
	}
	history, err := a.pages("MCP", bob, "messages_read", object{"conversation_id": conv})
	if err != nil {
		return err
	}
	beforeText := false
	for _, m := range history {
		if str(m, "id") == id {
			beforeText = str(m, "text") == "before snapshot" && number(m, "sequence") == number(child(original, "message"), "sequence")
		}
	}
	if err := a.check("MCP messages_read", "restored history is the snapshot view", "pre-snapshot messages only; original text and sequence", len(history) == 2 && containsID(history, id) && containsID(history, file.messageID) && !containsID(history, afterID) && beforeText); err != nil {
		return err
	}
	if _, err := a.rest(alice, "messages_status", object{"message_id": afterID}, 404); err != nil {
		return err
	}
	for _, mid := range []string{id, file.messageID} {
		stamp, err := a.checkDelivery("REST", alice, mid, "bob", "acked")
		if err != nil {
			return err
		}
		if err := a.check("REST messages_status", "snapshot preserves ACK timestamp", "original acked_at", stamp == stamps[mid]); err != nil {
			return err
		}
	}
	incoming, err = a.pages("REST", bob, "inbox_pull", object{})
	if err != nil {
		return err
	}
	if err := a.check("REST inbox_pull", "restored ACK excludes pending IDs", "both original IDs absent from fresh inbox", !containsID(incoming, id) && !containsID(incoming, file.messageID)); err != nil {
		return err
	}
	recovered, err = a.download("REST", bob, file)
	if err != nil {
		return err
	}
	return a.saveSample("recovery-restored-download.bin", recovered)
}
