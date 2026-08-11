package store

import (
	"aislide/internal/model"
	"context"
	"database/sql"
	"errors"
	"time"
)

type UserStore interface {
	// EnsureExists inserts the user when it is the first time we see the UID,
	// then returns the stored row either way.
	EnsureExists(ctx context.Context, firebaseUID string) (*model.User, error)
	GetByFirebaseUID(ctx context.Context, firebaseUID string) (*model.User, error)
	// UpsertPremium writes the entitlement state, creating the user when the
	// RevenueCat webhook arrives before the app's first authenticated call.
	UpsertPremium(ctx context.Context, firebaseUID string, isPremium bool, premiumUntil *time.Time) (*model.User, error)
}

type userStore struct {
	db *sql.DB
}

func NewUserStore(db *sql.DB) UserStore {
	return &userStore{db: db}
}

const userColumns = `firebase_uid, is_premium, premium_until, created_at, updated_at`

func (s *userStore) EnsureExists(ctx context.Context, firebaseUID string) (*model.User, error) {
	now := formatTime(time.Now().UTC())

	q := `INSERT INTO users (firebase_uid, is_premium, created_at, updated_at)
		VALUES (?, 0, ?, ?)
		ON CONFLICT(firebase_uid) DO NOTHING`

	if _, err := s.db.ExecContext(ctx, q, firebaseUID, now, now); err != nil {
		return nil, err
	}

	return s.GetByFirebaseUID(ctx, firebaseUID)
}

func (s *userStore) GetByFirebaseUID(ctx context.Context, firebaseUID string) (*model.User, error) {
	q := `SELECT ` + userColumns + ` FROM users WHERE firebase_uid = ?`

	return scanUser(s.db.QueryRowContext(ctx, q, firebaseUID))
}

func (s *userStore) UpsertPremium(ctx context.Context, firebaseUID string, isPremium bool, premiumUntil *time.Time) (*model.User, error) {
	q := `INSERT INTO users (firebase_uid, is_premium, premium_until, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(firebase_uid) DO UPDATE SET
			is_premium = excluded.is_premium,
			premium_until = excluded.premium_until,
			updated_at = excluded.updated_at`

	var until any
	if premiumUntil != nil {
		until = formatTime(premiumUntil.UTC())
	}

	now := formatTime(time.Now().UTC())

	if _, err := s.db.ExecContext(ctx, q, firebaseUID, isPremium, until, now, now); err != nil {
		return nil, err
	}

	return s.GetByFirebaseUID(ctx, firebaseUID)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (*model.User, error) {
	var (
		u            model.User
		premiumUntil sql.NullString
		createdAt    string
		updatedAt    string
	)

	err := row.Scan(&u.FirebaseUID, &u.IsPremium, &premiumUntil, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	if premiumUntil.Valid && premiumUntil.String != "" {
		t, err := parseTime(premiumUntil.String)
		if err != nil {
			return nil, err
		}
		u.PremiumUntil = &t
	}

	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if u.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}

	return &u, nil
}

// Timestamps are stored as RFC3339 UTC strings so they stay readable and sortable.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}
