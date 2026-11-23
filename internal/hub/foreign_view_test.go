package hub

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestForeignDatabaseWithOnlyViewIsNotAnEmptyBootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE VIEW unrelated AS SELECT 1 AS value"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, Options{})
	if s != nil {
		_ = s.Close()
	}
	if err == nil {
		t.Fatal("foreign view-only database was adopted")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("rejected foreign database permissions changed")
	}
}
