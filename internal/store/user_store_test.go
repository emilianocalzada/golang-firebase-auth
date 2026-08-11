package store_test

import (
	"aislide/internal/store"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return db
}

func newTestStore(t *testing.T) store.UserStore {
	t.Helper()

	return store.NewUserStore(newTestDB(t))
}

func TestEnsureExistsCreatesThenReturnsSameUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created, err := s.EnsureExists(ctx, "uid-abc")
	if err != nil {
		t.Fatalf("first EnsureExists: %v", err)
	}
	if created.FirebaseUID != "uid-abc" {
		t.Errorf("uid = %q, want uid-abc", created.FirebaseUID)
	}
	if created.IsPremium {
		t.Error("new user should not be premium")
	}
	if created.PremiumUntil != nil {
		t.Errorf("premium_until = %v, want nil", created.PremiumUntil)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Error("timestamps should be set")
	}

	// Second call must be idempotent and keep the original created_at.
	again, err := s.EnsureExists(ctx, "uid-abc")
	if err != nil {
		t.Fatalf("second EnsureExists: %v", err)
	}
	if !again.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at changed: %v -> %v", created.CreatedAt, again.CreatedAt)
	}
}

func TestGetByFirebaseUIDNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetByFirebaseUID(context.Background(), "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpsertPremiumRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.EnsureExists(ctx, "uid-premium"); err != nil {
		t.Fatalf("EnsureExists: %v", err)
	}

	until := time.Now().Add(30 * 24 * time.Hour)
	granted, err := s.UpsertPremium(ctx, "uid-premium", true, &until)
	if err != nil {
		t.Fatalf("UpsertPremium: %v", err)
	}
	if !granted.IsPremium {
		t.Error("is_premium should be true")
	}
	if granted.PremiumUntil == nil {
		t.Fatal("premium_until should be set")
	}
	// Stored with second precision in RFC3339.
	if diff := granted.PremiumUntil.Sub(until); diff > time.Second || diff < -time.Second {
		t.Errorf("premium_until = %v, want ~%v", granted.PremiumUntil, until)
	}
	if !granted.HasActivePremium() {
		t.Error("future expiry should count as active premium")
	}

	revoked, err := s.UpsertPremium(ctx, "uid-premium", false, nil)
	if err != nil {
		t.Fatalf("UpsertPremium revoke: %v", err)
	}
	if revoked.IsPremium || revoked.PremiumUntil != nil {
		t.Errorf("revoke left %+v", revoked)
	}
	if revoked.HasActivePremium() {
		t.Error("revoked user must not have active premium")
	}
}

func TestUpsertPremiumCreatesUnknownUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// The RevenueCat webhook can arrive before the app's first API call.
	user, err := s.UpsertPremium(ctx, "uid-from-webhook", true, nil)
	if err != nil {
		t.Fatalf("UpsertPremium: %v", err)
	}
	if !user.IsPremium || !user.HasActivePremium() {
		t.Errorf("user = %+v, want active premium", user)
	}
	if user.CreatedAt.IsZero() {
		t.Error("created_at should be set for the new row")
	}
}

func TestUpsertPremiumPreservesCreatedAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	created, err := s.EnsureExists(ctx, "uid-keep")
	if err != nil {
		t.Fatalf("EnsureExists: %v", err)
	}

	updated, err := s.UpsertPremium(ctx, "uid-keep", true, nil)
	if err != nil {
		t.Fatalf("UpsertPremium: %v", err)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at changed: %v -> %v", created.CreatedAt, updated.CreatedAt)
	}
}

func TestHasEventReportsWhatWasRecorded(t *testing.T) {
	events := store.NewRevenueCatEventStore(newTestDB(t))
	ctx := context.Background()

	seen, err := events.HasEvent(ctx, "evt-1")
	if err != nil {
		t.Fatalf("HasEvent: %v", err)
	}
	if seen {
		t.Fatal("an unrecorded event must not be reported as processed")
	}

	if err := events.RecordEvent(ctx, "evt-1"); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	if seen, err := events.HasEvent(ctx, "evt-1"); err != nil || !seen {
		t.Errorf("evt-1: seen = %v, err = %v, want true", seen, err)
	}
	// An unrelated id is unaffected.
	if seen, err := events.HasEvent(ctx, "evt-2"); err != nil || seen {
		t.Errorf("evt-2: seen = %v, err = %v, want false", seen, err)
	}
}

func TestRecordEventTwiceIsNotAnError(t *testing.T) {
	// Two deliveries of the same event can race and both reach the end of the
	// work, so recording an id that is already there must not fail.
	events := store.NewRevenueCatEventStore(newTestDB(t))
	ctx := context.Background()

	if err := events.RecordEvent(ctx, "evt-same"); err != nil {
		t.Fatalf("first RecordEvent: %v", err)
	}
	if err := events.RecordEvent(ctx, "evt-same"); err != nil {
		t.Errorf("second RecordEvent: %v", err)
	}
}
