package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func emptyRollbackJournal() []byte {
	journal := make([]byte, 512)
	copy(journal, []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7})
	binary.BigEndian.PutUint32(journal[12:16], 0x12345678)
	binary.BigEndian.PutUint32(journal[20:24], 512)
	binary.BigEndian.PutUint32(journal[24:28], 4096)
	return journal
}

func interruptedEmptyDatabase(t *testing.T, foreign bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "interrupted.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA page_size=4096; PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if foreign {
		if _, err := db.Exec("CREATE TABLE foreign_data(value TEXT); INSERT INTO foreign_data VALUES('must survive')"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-journal", emptyRollbackJournal(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEmptyBootstrapHotJournalRecovers(t *testing.T) {
	path := interruptedEmptyDatabase(t, false)
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("empty bootstrap hot journal rejected: %v", err)
	}
	if err := s.AddPrincipal(context.Background(), "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM principals WHERE id='alice'").Scan(&count); err != nil || count != 1 {
		t.Fatal("recovered store did not preserve writes")
	}
}

func TestHotJournalForeignDatabaseRejectedWithoutMutation(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		path := interruptedEmptyDatabase(t, foreign)
		if !foreign {
			journal := emptyRollbackJournal()
			binary.BigEndian.PutUint32(journal[16:20], 1)
			if err := os.WriteFile(path+"-journal", journal, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)
		journalBefore, _ := os.ReadFile(path + "-journal")
		if s, err := Open(path, Options{}); err == nil {
			s.Close()
			t.Fatal("unproven hot journal adopted")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("rejected database changed")
		}
		journalAfter, err := os.ReadFile(path + "-journal")
		if err != nil || !bytes.Equal(journalBefore, journalAfter) {
			t.Fatal("rejected journal changed")
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0644 {
			t.Fatal("rejected database permissions changed")
		}
	}
}

func TestDatabaseJournalLinksRejectedBeforeRecovery(t *testing.T) {
	for _, hard := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "hub.db")
		s, err := Open(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(path)
		other := filepath.Join(dir, "other-journal")
		if err := os.WriteFile(other, emptyRollbackJournal(), 0600); err != nil {
			t.Fatal(err)
		}
		link := os.Symlink
		if hard {
			link = os.Link
		}
		if err := link(other, path+"-journal"); err != nil {
			t.Fatal(err)
		}
		if s, err := Open(path, Options{}); err == nil {
			s.Close()
			t.Fatal("linked journal accepted")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("linked journal caused database mutation")
		}
	}
}
