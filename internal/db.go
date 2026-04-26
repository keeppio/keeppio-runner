package internal

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
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
