package hub

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const applicationID = 0x4d484231

var principalPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type Store struct {
	db        *sql.DB
	path      string
	opts      Options
	cursorKey []byte

	inboxMu      sync.Mutex
	inboxChanged chan struct{}
}

func optionsWithDefaults(o Options) (Options, error) {
	if o.GrokWake != nil {
		wake := *o.GrokWake
		if !wake.Valid() {
			return o, ErrInvalid
		}
		o.GrokWake = &wake
	}
	if o.MessageTTL == 0 {
		o.MessageTTL = 30 * 24 * time.Hour
	}
	if o.AttachmentTTL == 0 {
		o.AttachmentTTL = 7 * 24 * time.Hour
	}
	if o.MaxMessageBytes == 0 {
		o.MaxMessageBytes = 64 << 10
	}
	if o.MaxFileBytes == 0 {
		o.MaxFileBytes = 8 << 20
	}
	if o.MaxChunkBytes == 0 {
		o.MaxChunkBytes = 64 << 10
	}
	if o.MaxStoreBytes == 0 {
		o.MaxStoreBytes = 1 << 30
	}
	if o.MaxMembers == 0 {
		o.MaxMembers = 16
	}
	if o.MaxMessages == 0 {
		o.MaxMessages = 100000
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MessageTTL < time.Second || o.MessageTTL > 365*24*time.Hour || o.AttachmentTTL < time.Second || o.AttachmentTTL > o.MessageTTL || o.MaxMessageBytes < 1 || o.MaxMessageBytes > 1<<20 || o.MaxFileBytes < 1 || o.MaxFileBytes > 64<<20 || o.MaxChunkBytes < 1 || o.MaxChunkBytes > 1<<20 || o.MaxStoreBytes < o.MaxFileBytes || o.MaxMembers < 2 || o.MaxMembers > 100 || o.MaxMessages < 1 {
		return o, ErrInvalid
	}
	return o, nil
}

func Open(path string, options Options) (*Store, error) {
	opts, err := optionsWithDefaults(options)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) || path == string(filepath.Separator) {
		return nil, ErrInvalid
	}
	path = filepath.Clean(path)
	for p := path; p != filepath.Dir(p); p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrInvalid
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, ErrInvalid
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
			return nil, ErrInvalid
		}
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, ErrInvalid
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
		_ = f.Close()
		return nil, ErrInvalid
	}
	fresh := info.Size() == 0
	kind := ""
	if !fresh {
		header := make([]byte, 100)
		n, err := f.ReadAt(header, 0)
		if err != nil && err != io.EOF {
			_ = f.Close()
			return nil, err
		}
		if n < 100 || string(header[:16]) != "SQLite format 3\x00" {
			_ = f.Close()
			return nil, ErrInvalid
		}
		headerApp := int(binary.BigEndian.Uint32(header[68:72]))
		headerVersion := int(binary.BigEndian.Uint32(header[60:64]))
		switch {
		case headerApp == applicationID && headerVersion == 1:
			kind = "ours"
		case headerApp == 0 && headerVersion == 0:
			// The main-file header can be stale while committed identity lives in
			// the WAL, so ask SQLite itself. Never classify a foreign database by
			// an uncommitted header.
			identity, scanErr := readDatabaseIdentity(path, false)
			if emptyBootstrapRollback(path, scanErr) {
				// Only an empty new-file transaction may bypass journal recovery
				// during classification. The original stays untouched until verified.
				identity, scanErr = readDatabaseIdentity(path, true)
			}
			if scanErr != nil {
				_ = f.Close()
				return nil, ErrInvalid
			}
			switch {
			case identity.app == applicationID && identity.version == 1:
				kind = "ours"
			case identity.app == 0 && identity.version == 0 && identity.objects == 0:
				kind = "adopt"
			default:
				_ = f.Close()
				return nil, fmt.Errorf("database is not a supported message hub")
			}
		default:
			_ = f.Close()
			return nil, fmt.Errorf("database is not a supported message hub")
		}
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	_ = f.Close()
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	for _, p := range []string{"foreign_keys(1)", "busy_timeout(5000)", "synchronous(FULL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path, opts: opts}
	ok := false
	defer func() {
		if !ok {
			_ = db.Close()
		}
	}()
	var app, version int
	if err := db.QueryRow("PRAGMA application_id").Scan(&app); err != nil {
		return nil, err
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if kind == "ours" && (app != applicationID || version != 1) {
		return nil, fmt.Errorf("database is not a supported message hub")
	}
	if kind == "adopt" {
		var objects int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&objects); err != nil {
			return nil, err
		}
		if app != 0 || version != 0 || objects != 0 {
			return nil, fmt.Errorf("database changed during bootstrap recovery")
		}
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, err
	}
	if fresh || kind == "adopt" {
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(schema); err != nil {
			return nil, err
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO settings(key,value) VALUES('cursor_key',?)", rand.Text()); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	var key string
	if err := db.QueryRow("SELECT value FROM settings WHERE key='cursor_key'").Scan(&key); err != nil {
		return nil, err
	}
	s.cursorKey = []byte(key)
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS message_replies
		ON messages(conversation_id,reply_to,seq) WHERE reply_to IS NOT NULL`); err != nil {
		return nil, err
	}
	if opts.GrokWake != nil {
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(wakeSchema); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

func (s *Store) Close() error    { return s.db.Close() }
func (s *Store) now() int64      { return s.opts.Now().UTC().Unix() }
func newID(prefix string) string { return prefix + "_" + rand.Text() }
func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) AddPrincipal(ctx context.Context, id, name string) error {
	if !principalPattern.MatchString(id) || !utf8.ValidString(name) || strings.TrimSpace(name) == "" || len(name) > 200 {
		return ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO principals(id,name,active) VALUES(?,?,1)", id, name)
	if err != nil {
		return ErrConflict
	}
	return nil
}

func (s *Store) SetPrincipalActive(ctx context.Context, id string, active bool) error {
	result, err := s.db.ExecContext(ctx, "UPDATE principals SET active=? WHERE id=?", active, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) IssueGrant(ctx context.Context, principal string, ttl time.Duration) (Grant, string, error) {
	if ttl < time.Second || ttl > 365*24*time.Hour {
		return Grant{}, "", ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Grant{}, "", err
	}
	defer tx.Rollback()
	grant, token, err := s.issueGrantTx(ctx, tx, principal, ttl)
	if err != nil {
		return Grant{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Grant{}, "", err
	}
	return grant, token, nil
}

func (s *Store) issueGrantTx(ctx context.Context, tx *sql.Tx, principal string, ttl time.Duration) (Grant, string, error) {
	var active bool
	if err := tx.QueryRowContext(ctx, "SELECT active FROM principals WHERE id=?", principal).Scan(&active); err != nil || !active {
		return Grant{}, "", ErrNotFound
	}
	grant := Grant{ID: newID("g"), PrincipalID: principal, ExpiresAt: s.opts.Now().Add(ttl).Unix()}
	token := "mh_" + rand.Text() + rand.Text()
	if _, err := tx.ExecContext(ctx, "INSERT INTO grants(id,principal_id,expires_at,revoked) VALUES(?,?,?,0)", grant.ID, principal, grant.ExpiresAt); err != nil {
		return Grant{}, "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO tokens(token_hash,grant_id,kind,expires_at) VALUES(?,?,'access',?)", tokenDigest(token), grant.ID, grant.ExpiresAt); err != nil {
		return Grant{}, "", err
	}
	return grant, token, nil
}

func (s *Store) Authenticate(ctx context.Context, token string) (Actor, error) {
	if len(token) < 32 || len(token) > 1024 {
		return Actor{}, ErrUnauthenticated
	}
	a := Actor{tokenHash: tokenDigest(token)}
	err := s.db.QueryRowContext(ctx, `SELECT p.id,g.id FROM tokens t JOIN grants g ON g.id=t.grant_id JOIN principals p ON p.id=g.principal_id
		WHERE t.token_hash=? AND t.kind='access' AND t.expires_at>? AND g.expires_at>? AND g.revoked=0 AND p.active=1`, a.tokenHash, s.now(), s.now()).Scan(&a.principalID, &a.grantID)
	if err != nil {
		return Actor{}, ErrUnauthenticated
	}
	return a, nil
}

func (s *Store) RevokeGrant(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE grants SET revoked=1 WHERE id=?", id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) transaction(ctx context.Context, actor Actor, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tokens t JOIN grants g ON g.id=t.grant_id JOIN principals p ON p.id=g.principal_id
		WHERE t.token_hash=? AND t.kind='access' AND t.expires_at>? AND g.id=? AND g.principal_id=? AND g.expires_at>? AND g.revoked=0 AND p.active=1`, actor.tokenHash, s.now(), actor.grantID, actor.principalID, s.now()).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrUnauthenticated
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) requireMember(ctx context.Context, tx *sql.Tx, actor Actor, conversationID string) error {
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM members WHERE conversation_id=? AND principal_id=?", conversationID, actor.principalID).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) sealCursor(scope string, sequence int64) string {
	body, _ := json.Marshal(struct {
		Scope    string
		Sequence int64
	}{scope, sequence})
	enc := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.cursorKey)
	_, _ = mac.Write([]byte(enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Store) openCursor(scope, cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	if len(cursor) > 1024 {
		return 0, ErrInvalid
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 {
		return 0, ErrInvalid
	}
	mac := hmac.New(sha256.New, s.cursorKey)
	_, _ = mac.Write([]byte(parts[0]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(mac.Sum(nil), sig) {
		return 0, ErrInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, ErrInvalid
	}
	var value struct {
		Scope    string
		Sequence int64
	}
	if err := json.Unmarshal(body, &value); err != nil || value.Scope != scope || value.Sequence < 0 {
		return 0, ErrInvalid
	}
	return value.Sequence, nil
}

const schema = `
CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE principals(id TEXT PRIMARY KEY,name TEXT NOT NULL,active INTEGER NOT NULL CHECK(active IN(0,1)));
CREATE TABLE grants(id TEXT PRIMARY KEY,principal_id TEXT NOT NULL REFERENCES principals(id),expires_at INTEGER NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE tokens(token_hash TEXT PRIMARY KEY,grant_id TEXT NOT NULL REFERENCES grants(id),kind TEXT NOT NULL,expires_at INTEGER NOT NULL);
CREATE TABLE conversations(id TEXT PRIMARY KEY,creator TEXT NOT NULL REFERENCES principals(id),idem_key TEXT NOT NULL,digest TEXT NOT NULL,created_at INTEGER NOT NULL,closed INTEGER NOT NULL DEFAULT 0,next_seq INTEGER NOT NULL DEFAULT 1,UNIQUE(creator,idem_key));
CREATE TABLE members(conversation_id TEXT NOT NULL REFERENCES conversations(id),principal_id TEXT NOT NULL REFERENCES principals(id),PRIMARY KEY(conversation_id,principal_id));
CREATE TABLE messages(id TEXT PRIMARY KEY,conversation_id TEXT NOT NULL REFERENCES conversations(id),seq INTEGER NOT NULL,sender TEXT NOT NULL REFERENCES principals(id),idem_key TEXT NOT NULL,digest TEXT NOT NULL,text TEXT NOT NULL,data TEXT NOT NULL,reply_to TEXT,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,UNIQUE(conversation_id,seq),UNIQUE(sender,conversation_id,idem_key));
CREATE TABLE mailbox_sequences(principal_id TEXT PRIMARY KEY REFERENCES principals(id),next_seq INTEGER NOT NULL);
CREATE TABLE deliveries(message_id TEXT NOT NULL REFERENCES messages(id),recipient TEXT NOT NULL REFERENCES principals(id),seq INTEGER NOT NULL,acked_at INTEGER,PRIMARY KEY(message_id,recipient),UNIQUE(recipient,seq));
CREATE TABLE attachments(id TEXT PRIMARY KEY,conversation_id TEXT NOT NULL REFERENCES conversations(id),uploader TEXT NOT NULL REFERENCES principals(id),idem_key TEXT NOT NULL,digest TEXT NOT NULL,filename TEXT NOT NULL,media_type TEXT NOT NULL,size_bytes INTEGER NOT NULL,sha256 TEXT NOT NULL,state TEXT NOT NULL,received_bytes INTEGER NOT NULL DEFAULT 0,content BLOB,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,message_id TEXT REFERENCES messages(id),UNIQUE(uploader,conversation_id,idem_key));
CREATE TABLE attachment_chunks(attachment_id TEXT NOT NULL REFERENCES attachments(id),offset INTEGER NOT NULL,data BLOB NOT NULL,PRIMARY KEY(attachment_id,offset));
CREATE TABLE message_attachments(message_id TEXT NOT NULL REFERENCES messages(id),attachment_id TEXT NOT NULL UNIQUE REFERENCES attachments(id),PRIMARY KEY(message_id,attachment_id));
CREATE INDEX inbox_pending ON deliveries(recipient,acked_at,seq);
CREATE INDEX message_history ON messages(conversation_id,seq);
PRAGMA application_id=0x4d484231;
PRAGMA user_version=1;
`
