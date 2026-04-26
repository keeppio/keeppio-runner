package internal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// Schema migrations. Apply in order. Idempotent: each runs inside its
// own transaction and is a no-op if `applied_migrations` already has
// its name. Plain SQL keeps the dependency surface tiny — no migration
// framework needed for a tool with this many tables.
var migrations = []struct {
	name string
	sql  string
}{
	{
		name: "0001_init",
		sql: `
CREATE TABLE IF NOT EXISTS applied_migrations (
  name        TEXT PRIMARY KEY,
  applied_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  id          TEXT PRIMARY KEY,
  username    TEXT NOT NULL,
  expires_at  DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  action_id     TEXT NOT NULL,
  action_label  TEXT NOT NULL,
  status        TEXT NOT NULL,
  args_json     TEXT NOT NULL,
  commit_hash   TEXT,
  username      TEXT NOT NULL,
  log_path      TEXT,
  created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  started_at    DATETIME,
  ended_at      DATETIME,
  exit_code     INTEGER
);
CREATE INDEX IF NOT EXISTS tasks_status_idx ON tasks(status);
CREATE INDEX IF NOT EXISTS tasks_created_idx ON tasks(created_at);
`,
	},
	{
		// Read-only API for external observers (CI helpers, debug
		// scripts, etc). Token format: kr_pat_<64 hex chars>. We store
		// only the SHA-256 of the token — leak ⇒ revoke. SHA-256 is
		// fine here (vs bcrypt) because the secret has 256 bits of
		// entropy; bcrypt's slowness is meant for low-entropy human
		// passwords, not random tokens.
		name: "0002_api_tokens",
		sql: `
CREATE TABLE IF NOT EXISTS api_tokens (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  name          TEXT NOT NULL,
  hash          TEXT NOT NULL UNIQUE,
  created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_used_at  DATETIME,
  revoked_at    DATETIME
);
`,
	},
}

func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// SQLite + Go: limit to 1 writer to sidestep "database is locked"
	// when multiple goroutines do small writes during a task run.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return db, nil
}

func Migrate(ctx context.Context, db *sql.DB) error {
	for _, m := range migrations {
		var exists string
		err := db.QueryRowContext(ctx, `SELECT name FROM applied_migrations WHERE name=?`, m.name).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) && err.Error() != "no such table: applied_migrations" {
			// First-run: applied_migrations doesn't exist yet. Fall
			// through to the migration body which creates it.
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO applied_migrations(name) VALUES(?)`, m.name); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// EnsureAdmin creates the admin user on first boot. Idempotent. The
// initial password is the value of RUNNER_ADMIN_PASSWORD; once set,
// changing the env var has no effect (the user must rotate via UI).
func EnsureAdmin(ctx context.Context, db *sql.DB, initialPassword string) error {
	var existing string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='admin_password_hash'`).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if initialPassword == "" {
		return errors.New("no admin password set: provide RUNNER_ADMIN_PASSWORD on first boot")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(initialPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('admin_password_hash',?)`, string(hash))
	return err
}

// EnsureSessionKey returns a stable session-signing key, generating
// one on first call and persisting it in settings.
func EnsureSessionKey(ctx context.Context, db *sql.DB) ([]byte, error) {
	var b64 string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='session_key'`).Scan(&b64)
	if err == nil {
		return base64.StdEncoding.DecodeString(b64)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('session_key',?)`, base64.StdEncoding.EncodeToString(buf))
	return buf, err
}

// VerifyAdminPassword returns true iff the supplied password matches
// the stored bcrypt hash. Constant-time-ish at the bcrypt layer.
func VerifyAdminPassword(ctx context.Context, db *sql.DB, password string) (bool, error) {
	var hash string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='admin_password_hash'`).Scan(&hash); err != nil {
		return false, err
	}
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, nil
	}
	return false, err
}

// SetAdminPassword overwrites the stored hash. Used by the UI's
// settings page.
func SetAdminPassword(ctx context.Context, db *sql.DB, password string) error {
	if len(password) < 8 {
		return errors.New("password must be ≥ 8 chars")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT OR REPLACE INTO settings(key,value) VALUES('admin_password_hash',?)`, string(hash))
	return err
}

// ──────────────────────────────────────────────────────────────────────
// Sessions
// ──────────────────────────────────────────────────────────────────────

const sessionTTL = 12 * time.Hour

func NewSession(ctx context.Context, db *sql.DB, username string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(buf)
	_, err := db.ExecContext(ctx,
		`INSERT INTO sessions(id, username, expires_at) VALUES(?,?,?)`,
		id, username, time.Now().Add(sessionTTL))
	return id, err
}

func LookupSession(ctx context.Context, db *sql.DB, id string) (string, error) {
	var (
		username string
		exp      time.Time
	)
	err := db.QueryRowContext(ctx,
		`SELECT username, expires_at FROM sessions WHERE id=?`, id).
		Scan(&username, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if time.Now().After(exp) {
		return "", ErrNotFound
	}
	return username, nil
}

func DeleteSession(ctx context.Context, db *sql.DB, id string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, id)
	return err
}

// PurgeExpiredSessions runs every hour from main; cheap with the
// session-id index. Keeps the table from growing forever.
func PurgeExpiredSessions(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < CURRENT_TIMESTAMP`)
	return err
}

// ──────────────────────────────────────────────────────────────────────
// API tokens (read-only Authorization: Bearer access)
// ──────────────────────────────────────────────────────────────────────

const apiTokenPrefix = "kr_pat_"

// APIToken is the metadata side of a token. The actual secret is only
// returned once, at creation time, by CreateAPIToken.
type APIToken struct {
	ID         int64
	Name       string
	CreatedAt  time.Time
	LastUsedAt sql.NullTime
	RevokedAt  sql.NullTime
}

func hashAPIToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// CreateAPIToken mints a new bearer token and stores its hash. The
// token string is only returned here — surface it once to the operator
// and never again. 32 random bytes = 256 bits of entropy.
func CreateAPIToken(ctx context.Context, db *sql.DB, name string) (token string, id int64, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", 0, errors.New("token name is required")
	}
	if len(name) > 64 {
		return "", 0, errors.New("token name must be ≤ 64 chars")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", 0, err
	}
	token = apiTokenPrefix + hex.EncodeToString(buf)
	res, err := db.ExecContext(ctx,
		`INSERT INTO api_tokens(name, hash) VALUES(?, ?)`,
		name, hashAPIToken(token))
	if err != nil {
		return "", 0, err
	}
	id, _ = res.LastInsertId()
	return token, id, nil
}

func ListAPITokens(ctx context.Context, db *sql.DB) ([]APIToken, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name, created_at, last_used_at, revoked_at
		 FROM api_tokens ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func RevokeAPIToken(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = CURRENT_TIMESTAMP
		 WHERE id=? AND revoked_at IS NULL`, id)
	return err
}

// VerifyAPIToken returns the token's id+name if accepted. ErrNotFound
// for unknown OR revoked tokens (never distinguish the two — same
// information leakage class as login). Updates last_used_at so the
// settings page can show idle tokens.
func VerifyAPIToken(ctx context.Context, db *sql.DB, token string) (id int64, name string, err error) {
	if !strings.HasPrefix(token, apiTokenPrefix) {
		return 0, "", ErrNotFound
	}
	var revoked sql.NullTime
	err = db.QueryRowContext(ctx,
		`SELECT id, name, revoked_at FROM api_tokens WHERE hash=?`,
		hashAPIToken(token)).Scan(&id, &name, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	if err != nil {
		return 0, "", err
	}
	if revoked.Valid {
		return 0, "", ErrNotFound
	}
	_, _ = db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = CURRENT_TIMESTAMP WHERE id=?`, id)
	return id, name, nil
}
