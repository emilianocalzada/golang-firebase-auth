package store

import (
	"context"
	"database/sql"
	"time"
)

// RevenueCatEventStore gives the webhook idempotency: RevenueCat retries
// deliveries, so we remember which event ids we already handled.
type RevenueCatEventStore interface {
	// RecordEvent stores the event id and reports whether it was new. A false
	// return means this is a retry of an event we already processed.
	RecordEvent(ctx context.Context, eventID string) (bool, error)
	// DeleteEvent forgets an event so a RevenueCat retry can process it again.
	DeleteEvent(ctx context.Context, eventID string) error
}

type revenueCatEventStore struct {
	db *sql.DB
}

func NewRevenueCatEventStore(db *sql.DB) RevenueCatEventStore {
	return &revenueCatEventStore{db: db}
}

func (s *revenueCatEventStore) RecordEvent(ctx context.Context, eventID string) (bool, error) {
	q := `INSERT OR IGNORE INTO revenuecat_events (event_id, received_at) VALUES (?, ?)`

	resp, err := s.db.ExecContext(ctx, q, eventID, formatTime(time.Now().UTC()))
	if err != nil {
		return false, err
	}

	inserted, err := resp.RowsAffected()
	if err != nil {
		return false, err
	}

	return inserted > 0, nil
}

func (s *revenueCatEventStore) DeleteEvent(ctx context.Context, eventID string) error {
	q := `DELETE FROM revenuecat_events WHERE event_id = ?`

	_, err := s.db.ExecContext(ctx, q, eventID)

	return err
}
