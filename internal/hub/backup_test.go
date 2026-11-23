package hub

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupIsPrivateConsistentAndDoesNotReplaceExistingFile(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, token := provision(t, s, "alice")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backup.db")
	if err := s.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("backup not private")
	}
	copy, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if _, err := copy.Authenticate(ctx, token); err != nil {
		t.Fatal("backup did not preserve committed identity")
	}
	if err := s.Backup(ctx, path); err == nil {
		t.Fatal("backup replaced existing data")
	}
	if _, err := s.Authenticate(ctx, token); err != nil {
		t.Fatal("backup changed source")
	}
}

func TestBackupRejectsUnsafeDestination(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Backup(ctx, "relative.db"); err == nil {
		t.Fatal("relative backup accepted")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(s.path, link); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, link); err == nil {
		t.Fatal("backup symlink accepted")
	}
}
