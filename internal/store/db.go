package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
)

// ErrNotFound lets the layers above stay free of database/sql details.
var ErrNotFound = errors.New("record not found")

// maxOpenConns bounds the connection pool. SQLite serialises writers, so a
// large pool buys nothing but contention; a small warm pool avoids repeating
// the per-connection pragma setup on every query.
const maxOpenConns = 4

// Open opens the SQLite database with the settings we want for a server
// workload: WAL so reads do not block on a write in flight, a busy timeout so a
// concurrent write waits instead of failing with SQLITE_BUSY, and foreign key
// enforcement, which SQLite leaves off by default.
//
// The pragmas are passed in the DSN, not executed after opening. database/sql
// hands out pooled connections and busy_timeout and foreign_keys are
// per-connection state, so an Exec here would only configure whichever
// connection the pool happened to give us. Later connections would have
// foreign key enforcement disabled, while the busy timeout would implicitly
// depend on the driver's current default. In the DSN the driver applies both
// settings to every connection it opens. (journal_mode is the exception: it is
// recorded in the database file, so it holds for every connection either way.)
//
// Deployment constraint: this ties the service to a single instance. The file
// must live on persistent storage with backups, never a container's ephemeral
// layer, and only one replica may run against it. Horizontal scaling means
// moving to PostgreSQL.
func Open(path string) (*sql.DB, error) {
	params := url.Values{
		"_journal_mode": {"WAL"},
		"_busy_timeout": {"5000"},
		"_foreign_keys": {"on"},
	}

	db, err := sql.Open("sqlite3", path+"?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxOpenConns)

	// sql.Open is lazy, so ping to fail at startup rather than on first use.
	if err := db.Ping(); err != nil {
		db.Close()

		return nil, fmt.Errorf("open database %q: %w", path, err)
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
