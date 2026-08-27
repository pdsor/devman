// Package storage is the SQLite persistence layer.
//
// The database holds registration and runtime state only. devman.yaml remains
// the single source of truth for what a service is; nothing here duplicates a
// service definition.
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/devman-project/devman/pkg/errs"
	_ "modernc.org/sqlite"
)

// schemaVersion is bumped whenever migrate() gains a step.
const schemaVersion = 1

// DB is the DevMan database handle.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if needed) the database at path.
//
// WAL plus a busy timeout lets the daemon's concurrent starts share the
// connection pool without spurious "database is locked" failures. Port
// reservation correctness does not rely on that: it is enforced by a partial
// unique index, so two concurrent allocations can never claim one port.
func Open(path string) (*DB, error) {
	dsn := path + "?" + strings.Join([]string{
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(WAL)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
	}, "&")

	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot open database")
	}
	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)
	handle.SetConnMaxLifetime(0)

	db := &DB{sql: handle}
	if err := db.migrate(); err != nil {
		handle.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the database.
func (db *DB) Close() error { return db.sql.Close() }

// SQL exposes the raw handle for tests.
func (db *DB) SQL() *sql.DB { return db.sql }

func (db *DB) migrate() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    path                TEXT NOT NULL UNIQUE,
    config_path         TEXT NOT NULL,
    trusted_fingerprint TEXT,
    favorite            INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    last_started_at     TEXT
);

CREATE TABLE IF NOT EXISTS service_runtime (
    project_id          TEXT NOT NULL,
    service_name        TEXT NOT NULL,
    desired_state       TEXT NOT NULL,
    actual_state        TEXT NOT NULL,
    pid                 INTEGER,
    spawned_at          TEXT,
    executable          TEXT,
    command_fingerprint TEXT,
    instance_id         TEXT,
    restart_count       INTEGER NOT NULL DEFAULT 0,
    last_exit_code      INTEGER,
    log_capture         TEXT NOT NULL DEFAULT 'none',
    adopted             INTEGER NOT NULL DEFAULT 0,
    updated_at          TEXT NOT NULL,
    PRIMARY KEY (project_id, service_name),
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS process_instances (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    service_name  TEXT NOT NULL,
    pid           INTEGER NOT NULL,
    status        TEXT NOT NULL,
    runtime       TEXT NOT NULL,
    command_line  TEXT,
    cwd           TEXT,
    started_at    TEXT NOT NULL,
    stopped_at    TEXT,
    exit_code     INTEGER,
    restart_count INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS ix_instances_service
    ON process_instances(project_id, service_name, started_at DESC);

CREATE TABLE IF NOT EXISTS port_allocations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    port         INTEGER NOT NULL,
    project_id   TEXT NOT NULL,
    service_name TEXT NOT NULL,
    port_name    TEXT NOT NULL,
    env_var      TEXT,
    state        TEXT NOT NULL,
    allocated_at TEXT NOT NULL,
    released_at  TEXT
);

-- The partial unique index is what makes port reservation atomic: an active
-- allocation for a port can only exist once, enforced by the database rather
-- than by application-level locking.
CREATE UNIQUE INDEX IF NOT EXISTS ux_port_active
    ON port_allocations(port)
    WHERE state IN ('RESERVED', 'BOUND', 'UNVERIFIED');

CREATE INDEX IF NOT EXISTS ix_ports_service
    ON port_allocations(project_id, service_name);

CREATE TABLE IF NOT EXISTS events (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    type         TEXT NOT NULL,
    project_id   TEXT,
    service_name TEXT,
    message      TEXT,
    data         TEXT,
    created_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_events_created ON events(created_at DESC);
`
	if _, err := db.sql.Exec(ddl); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot create schema")
	}
	if err := db.SetMeta("schema_version", fmt.Sprint(schemaVersion)); err != nil {
		return err
	}
	return nil
}

// SetMeta stores an internal key/value pair.
func (db *DB) SetMeta(key, value string) error {
	_, err := db.sql.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot write meta %q", key)
	}
	return nil
}

// GetMeta reads an internal key. A missing key returns "".
func (db *DB) GetMeta(key string) (string, error) {
	var value string
	err := db.sql.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", errs.Wrap(errs.CodeInternal, err, "cannot read meta %q", key)
	}
	return value, nil
}

// --- time helpers -----------------------------------------------------------

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func scanTime(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	parsed := parseTime(value.String)
	if parsed.IsZero() {
		return nil
	}
	return &parsed
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func scanInt(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	converted := int(value.Int64)
	return &converted
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "constraint failed: unique")
}
