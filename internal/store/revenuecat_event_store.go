package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// RevenueCatEventStore gives the webhook idempotency: RevenueCat retries
// deliveries, so we remember which event ids we have finished processing.
type RevenueCatEventStore interface {
	// HasEvent reports whether this event id was already processed to
	// completion.
	HasEvent(ctx context.Context, eventID string) (bool, error)
	// RecordEvent marks the event id as processed. It is called only after the
	// work it acknowledges succeeded, and is safe to call more than once.
	RecordEvent(ctx context.Context, eventID string) error
}

type revenueCatEventStore struct {
	db *sql.DB
}

func NewRevenueCatEventStore(db *sql.DB) RevenueCatEventStore {
	return &revenueCatEventStore{db: db}
}

func (s *revenueCatEventStore) HasEvent(ctx context.Context, eventID string) (bool, error) {
	q := `SELECT 1 FROM revenuecat_events WHERE event_id = ?`

	var found int
	err := s.db.QueryRowContext(ctx, q, eventID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
}

func (s *revenueCatEventStore) RecordEvent(ctx context.Context, eventID string) error {
	// OR IGNORE: two deliveries of the same event racing each other both do the
	// work and both try to record it, which is safe rather than an error.
	q := `INSERT OR IGNORE INTO revenuecat_events (event_id, received_at) VALUES (?, ?)`

	_, err := s.db.ExecContext(ctx, q, eventID, formatTime(time.Now().UTC()))

	return err
}
