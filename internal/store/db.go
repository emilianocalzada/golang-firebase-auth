package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound lets the layers above stay free of database/sql details.
var ErrNotFound = errors.New("record not found")

// Open opens the SQLite database and applies the pragmas we want for a
// server workload: WAL so reads do not block on writes, a busy timeout so
// concurrent writes retry instead of failing, and foreign key enforcement.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply %q: %w", p, err)
		}
	}

	return db, nil
}

// Migrate creates the tables if they do not exist yet.
func Migrate(db *sql.DB) error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS users (
			firebase_uid TEXT PRIMARY KEY,
			is_premium INTEGER NOT NULL DEFAULT 0,
			premium_until TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS books (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL,
			author TEXT NOT NULL
		)`,
		// Processed webhook ids, so RevenueCat retries are idempotent.
		`CREATE TABLE IF NOT EXISTS revenuecat_events (
			event_id TEXT PRIMARY KEY,
			received_at TEXT NOT NULL
		)`,
	}

	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	return nil
}
