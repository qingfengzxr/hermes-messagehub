package hub

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

// Backup creates an application-consistent SQLite snapshot, never overwriting a target.
func (s *Store) Backup(ctx context.Context, destination string) error {
	if !filepath.IsAbs(destination) || filepath.Clean(destination) == s.path {
		return ErrInvalid
	}
	parent, err := os.Lstat(filepath.Dir(destination))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0077 != 0 {
		return ErrInvalid
	}
	fd, err := syscall.Open(destination, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrConflict
	}
	_ = syscall.Close(fd)
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(destination)
		}
	}()
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", destination); err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: destination, RawQuery: "mode=ro"}
	copy, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer copy.Close()
	var status string
	if err := copy.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&status); err != nil {
		return err
	}
	if status != "ok" {
		return errors.New("backup integrity check failed")
	}
	file, err := os.OpenFile(destination, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = file.Sync()
	_ = file.Close()
	if err != nil {
		return err
	}
	keep = true
	return nil
}
