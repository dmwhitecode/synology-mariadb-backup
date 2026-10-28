// Package store persists connections, settings, run history and the admin
// account in a single SQLite file.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/model"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/naming"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/schedule"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS admin (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	username      TEXT NOT NULL,
	password_hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS connections (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	name           TEXT NOT NULL,
	slug           TEXT NOT NULL UNIQUE,
	host           TEXT NOT NULL,
	port           INTEGER NOT NULL DEFAULT 3306,
	database_name  TEXT NOT NULL,
	username       TEXT NOT NULL,
	password_enc   TEXT NOT NULL,
	tls            INTEGER NOT NULL DEFAULT 0,
	enabled        INTEGER NOT NULL DEFAULT 1,
	schedule_json  TEXT,
	retention_keep INTEGER,
	naming_json    TEXT,
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	connection_id INTEGER NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
	started_at    INTEGER NOT NULL,
	finished_at   INTEGER,
	status        TEXT NOT NULL,
	trigger       TEXT NOT NULL,
	file          TEXT NOT NULL DEFAULT '',
	size          INTEGER NOT NULL DEFAULT 0,
	error         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runs_conn_idx ON runs(connection_id, id DESC);
`

// Open opens (and creates if needed) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	// SQLite is single-writer; keep one connection to avoid lock churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: init schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// --- Admin -----------------------------------------------------------------

// HasAdmin reports whether first-run setup has been completed.
func (s *Store) HasAdmin(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin`).Scan(&n)
	return n > 0, err
}

// CreateAdmin stores the single admin account. Fails if one exists.
func (s *Store) CreateAdmin(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin (id, username, password_hash) VALUES (1, ?, ?)`, username, passwordHash)
	return err
}

// GetAdmin returns the admin username and password hash.
func (s *Store) GetAdmin(ctx context.Context) (username, passwordHash string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT username, password_hash FROM admin WHERE id = 1`).Scan(&username, &passwordHash)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// UpdateAdminPassword replaces the stored hash.
func (s *Store) UpdateAdminPassword(ctx context.Context, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE admin SET password_hash = ? WHERE id = 1`, passwordHash)
	return err
}

// --- Sessions --------------------------------------------------------------

// CreateSession records a login session token.
func (s *Store) CreateSession(ctx context.Context, token string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token, created_at, expires_at) VALUES (?, ?, ?)`,
		token, time.Now().Unix(), expires.Unix())
	return err
}

// SessionValid reports whether token exists and has not expired.
func (s *Store) SessionValid(ctx context.Context, token string) (bool, error) {
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM sessions WHERE token = ?`, token).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Now().Unix() < exp, nil
}

// DeleteSession logs a session out.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// DeleteAllSessions logs everyone out (used after a password change).
func (s *Store) DeleteAllSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions`)
	return err
}

// PurgeExpiredSessions removes dead sessions.
func (s *Store) PurgeExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	return err
}

// --- Defaults --------------------------------------------------------------

// GetDefaults returns the global default settings.
func (s *Store) GetDefaults(ctx context.Context) (model.Defaults, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'defaults'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return model.DefaultDefaults(), nil
	}
	if err != nil {
		return model.Defaults{}, err
	}
	var d model.Defaults
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return model.Defaults{}, fmt.Errorf("store: corrupt defaults: %w", err)
	}
	return d, nil
}

// SaveDefaults replaces the global default settings.
func (s *Store) SaveDefaults(ctx context.Context, d model.Defaults) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('defaults', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(raw))
	return err
}

// --- Connections -----------------------------------------------------------

const connCols = `id, name, slug, host, port, database_name, username, password_enc, tls, enabled,
	schedule_json, retention_keep, naming_json, created_at, updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanConn(r rowScanner) (*model.Connection, error) {
	var c model.Connection
	var tls, enabled int
	var sched, nm sql.NullString
	var keep sql.NullInt64
	var created, updated int64
	err := r.Scan(&c.ID, &c.Name, &c.Slug, &c.Host, &c.Port, &c.Database, &c.Username, &c.PasswordEnc,
		&tls, &enabled, &sched, &keep, &nm, &created, &updated)
	if err != nil {
		return nil, err
	}
	c.TLS = tls == 1
	c.Enabled = enabled == 1
	c.CreatedAt = time.Unix(created, 0)
	c.UpdatedAt = time.Unix(updated, 0)
	if sched.Valid {
		var sc schedule.Schedule
		if err := json.Unmarshal([]byte(sched.String), &sc); err != nil {
			return nil, fmt.Errorf("store: corrupt schedule for connection %d: %w", c.ID, err)
		}
		c.Schedule = &sc
	}
	if keep.Valid {
		k := int(keep.Int64)
		c.RetentionKeep = &k
	}
	if nm.Valid {
		var no naming.Options
		if err := json.Unmarshal([]byte(nm.String), &no); err != nil {
			return nil, fmt.Errorf("store: corrupt naming for connection %d: %w", c.ID, err)
		}
		c.Naming = &no
	}
	return &c, nil
}

func connArgs(c *model.Connection) (sched, nm sql.NullString, keep sql.NullInt64, err error) {
	if c.Schedule != nil {
		b, e := json.Marshal(c.Schedule)
		if e != nil {
			return sched, nm, keep, e
		}
		sched = sql.NullString{String: string(b), Valid: true}
	}
	if c.Naming != nil {
		b, e := json.Marshal(c.Naming)
		if e != nil {
			return sched, nm, keep, e
		}
		nm = sql.NullString{String: string(b), Valid: true}
	}
	if c.RetentionKeep != nil {
		keep = sql.NullInt64{Int64: int64(*c.RetentionKeep), Valid: true}
	}
	return sched, nm, keep, nil
}

// ListConnections returns all connections ordered by name.
func (s *Store) ListConnections(ctx context.Context) ([]*model.Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+connCols+` FROM connections ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Connection
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConnection fetches one connection by id.
func (s *Store) GetConnection(ctx context.Context, id int64) (*model.Connection, error) {
	c, err := scanConn(s.db.QueryRowContext(ctx, `SELECT `+connCols+` FROM connections WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// CreateConnection inserts c, assigning a unique slug from its name, and
// sets c.ID, c.Slug and timestamps.
func (s *Store) CreateConnection(ctx context.Context, c *model.Connection) error {
	sched, nm, keep, err := connArgs(c)
	if err != nil {
		return err
	}
	base := naming.Slug(c.Name)
	now := time.Now()
	for i := 0; ; i++ {
		slug := base
		if i > 0 {
			slug = fmt.Sprintf("%s-%d", base, i+1)
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM connections WHERE slug = ?`, slug).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			c.Slug = slug
			break
		}
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO connections
		(name, slug, host, port, database_name, username, password_enc, tls, enabled,
		 schedule_json, retention_keep, naming_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Slug, c.Host, c.Port, c.Database, c.Username, c.PasswordEnc, b2i(c.TLS), b2i(c.Enabled),
		sched, keep, nm, now.Unix(), now.Unix())
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	c.CreatedAt, c.UpdatedAt = now, now
	return nil
}

// UpdateConnection saves changes to an existing connection. The slug is never
// changed so the backup directory stays stable. If c.PasswordEnc is empty the
// stored password is kept.
func (s *Store) UpdateConnection(ctx context.Context, c *model.Connection) error {
	sched, nm, keep, err := connArgs(c)
	if err != nil {
		return err
	}
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `UPDATE connections SET
		name = ?, host = ?, port = ?, database_name = ?, username = ?,
		password_enc = CASE WHEN ? = '' THEN password_enc ELSE ? END,
		tls = ?, enabled = ?, schedule_json = ?, retention_keep = ?, naming_json = ?, updated_at = ?
		WHERE id = ?`,
		c.Name, c.Host, c.Port, c.Database, c.Username,
		c.PasswordEnc, c.PasswordEnc,
		b2i(c.TLS), b2i(c.Enabled), sched, keep, nm, now.Unix(), c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	c.UpdatedAt = now
	return nil
}

// SetConnectionEnabled toggles scheduling for a connection.
func (s *Store) SetConnectionEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE connections SET enabled = ?, updated_at = ? WHERE id = ?`,
		b2i(enabled), time.Now().Unix(), id)
	return err
}

// DeleteConnection removes a connection and its run history. Backup files
// on disk are left alone.
func (s *Store) DeleteConnection(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE id = ?`, id)
	return err
}

// --- Runs ------------------------------------------------------------------

// CreateRun records the start of a backup and returns its id.
func (s *Store) CreateRun(ctx context.Context, connID int64, trigger string, started time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs (connection_id, started_at, status, trigger)
		VALUES (?, ?, ?, ?)`, connID, started.Unix(), model.StatusRunning, trigger)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishRun records the outcome of a run.
func (s *Store) FinishRun(ctx context.Context, id int64, status model.RunStatus, file string, size int64, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET finished_at = ?, status = ?, file = ?, size = ?, error = ? WHERE id = ?`,
		time.Now().Unix(), status, file, size, errMsg, id)
	return err
}

// RecordSkipped logs a scheduled run that was skipped because another was
// still in progress.
func (s *Store) RecordSkipped(ctx context.Context, connID int64, reason string) error {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs (connection_id, started_at, finished_at, status, trigger, error)
		VALUES (?, ?, ?, ?, ?, ?)`, connID, now, now, model.StatusSkipped, model.TriggerScheduled, reason)
	return err
}

// MarkInterruptedRuns flips any "running" rows to failed. Called at startup
// so a crash mid-dump is not shown as running forever.
func (s *Store) MarkInterruptedRuns(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET finished_at = ?, status = ?, error = 'interrupted by restart'
		WHERE status = ?`, time.Now().Unix(), model.StatusFailed, model.StatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const runCols = `id, connection_id, started_at, finished_at, status, trigger, file, size, error`

func scanRun(r rowScanner) (*model.Run, error) {
	var run model.Run
	var started int64
	var finished sql.NullInt64
	if err := r.Scan(&run.ID, &run.ConnectionID, &started, &finished, &run.Status, &run.Trigger, &run.File, &run.Size, &run.Error); err != nil {
		return nil, err
	}
	run.StartedAt = time.Unix(started, 0)
	if finished.Valid {
		t := time.Unix(finished.Int64, 0)
		run.FinishedAt = &t
	}
	return &run, nil
}

// ListRuns returns the most recent runs for a connection, newest first.
func (s *Store) ListRuns(ctx context.Context, connID int64, limit int) ([]*model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs WHERE connection_id = ? ORDER BY id DESC LIMIT ?`, connID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastRuns returns the most recent run for every connection, keyed by
// connection id.
func (s *Store) LastRuns(ctx context.Context) (map[int64]*model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs
		WHERE id IN (SELECT MAX(id) FROM runs GROUP BY connection_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*model.Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out[r.ConnectionID] = r
	}
	return out, rows.Err()
}

// PruneRuns keeps the newest `keep` run rows per connection.
func (s *Store) PruneRuns(ctx context.Context, connID int64, keep int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE connection_id = ? AND id NOT IN
		(SELECT id FROM runs WHERE connection_id = ? ORDER BY id DESC LIMIT ?)`, connID, connID, keep)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
