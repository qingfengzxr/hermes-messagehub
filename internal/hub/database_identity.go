package hub

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"net/url"
	"os"
	"syscall"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type databaseIdentity struct {
	app, version, objects int
}

func readDatabaseIdentity(path string, immutable bool) (databaseIdentity, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {"ro"}}
	if immutable {
		q.Set("immutable", "1")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return databaseIdentity{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var identity databaseIdentity
	if err := db.QueryRow("PRAGMA application_id").Scan(&identity.app); err != nil {
		return identity, err
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&identity.version); err != nil {
		return identity, err
	}
	err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&identity.objects)
	return identity, err
}

func emptyBootstrapRollback(path string, cause error) bool {
	var sqliteError *sqlite.Error
	if !errors.As(cause, &sqliteError) || sqliteError.Code() != sqlite3.SQLITE_READONLY_ROLLBACK {
		return false
	}
	if info, err := os.Lstat(path + "-wal"); err == nil {
		if info.Size() != 0 {
			return false
		}
	} else if !os.IsNotExist(err) {
		return false
	}
	fd, err := syscall.Open(path+"-journal", syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	journal := os.NewFile(uintptr(fd), path+"-journal")
	defer journal.Close()
	info, err := journal.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return false
	}
	var header [28]byte
	if _, err := io.ReadFull(journal, header[:]); err != nil {
		return false
	}
	sector := binary.BigEndian.Uint32(header[20:24])
	page := binary.BigEndian.Uint32(header[24:28])
	return string(header[:8]) == "\xd9\xd5\x05\xf9\x20\xa1\x63\xd7" &&
		binary.BigEndian.Uint32(header[8:12]) == 0 &&
		binary.BigEndian.Uint32(header[16:20]) == 0 &&
		sector >= 512 && sector <= 65536 && sector&(sector-1) == 0 &&
		page >= 512 && page <= 65536 && page&(page-1) == 0 &&
		info.Size() == int64(sector)
}
