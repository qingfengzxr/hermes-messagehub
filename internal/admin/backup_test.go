package admin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupCommandCreatesVerifiedSnapshot(t *testing.T) {
	s := adminStore(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "snapshot.db")
	var out bytes.Buffer
	if err := Run(context.Background(), s, []string{"backup", "--output", path}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"verified":true`)) {
		t.Fatal("verified backup receipt absent")
	}
}
