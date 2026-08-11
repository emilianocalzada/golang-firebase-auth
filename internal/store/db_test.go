package store_test

import (
	"aislide/internal/store"
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestOpenAppliesSettingsToEveryConnection holds several connections open at
// once and checks each one. busy_timeout and foreign_keys are per-connection
// state, so configuring them with an Exec after opening would only reach
// whichever connection the pool handed out first.
func TestOpenAppliesSettingsToEveryConnection(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Distinct connections: each is checked out before any is released.
	conns := make([]*sql.Conn, 0, 3)
	for i := 0; i < 3; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		t.Cleanup(func() { conn.Close() })
		conns = append(conns, conn)
	}

	for i, conn := range conns {
		var busyTimeout int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i, err)
		}
		if busyTimeout != 5000 {
			t.Errorf("conn %d busy_timeout = %d, want 5000 (a concurrent write would fail instead of waiting)", i, busyTimeout)
		}

		var foreignKeys int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("conn %d foreign_keys: %v", i, err)
		}
		if foreignKeys != 1 {
			t.Errorf("conn %d foreign_keys = %d, want 1", i, foreignKeys)
		}

		var journalMode string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatalf("conn %d journal_mode: %v", i, err)
		}
		if journalMode != "wal" {
			t.Errorf("conn %d journal_mode = %q, want wal", i, journalMode)
		}
	}
}

func TestOpenFailsOnAnUnusablePath(t *testing.T) {
	// A directory that does not exist: sql.Open alone would not notice.
	if _, err := store.Open(filepath.Join(t.TempDir(), "missing-dir", "test.db")); err == nil {
		t.Error("Open should fail when the database file cannot be created")
	}
}
