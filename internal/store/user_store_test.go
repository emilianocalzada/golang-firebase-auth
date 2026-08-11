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

func TestRecordEventIsIdempotent(t *testing.T) {
	events := store.NewRevenueCatEventStore(newTestDB(t))
	ctx := context.Background()

	isNew, err := events.RecordEvent(ctx, "evt-1")
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if !isNew {
		t.Fatal("first RecordEvent should report the event as new")
	}

	isNew, err = events.RecordEvent(ctx, "evt-1")
	if err != nil {
		t.Fatalf("second RecordEvent: %v", err)
	}
	if isNew {
		t.Error("replayed event should not be reported as new")
	}

	// A different id is still new.
	if isNew, err := events.RecordEvent(ctx, "evt-2"); err != nil || !isNew {
		t.Errorf("evt-2: isNew = %v, err = %v", isNew, err)
	}
}

func TestDeleteEventAllowsReprocessing(t *testing.T) {
	events := store.NewRevenueCatEventStore(newTestDB(t))
	ctx := context.Background()

	if _, err := events.RecordEvent(ctx, "evt-retry"); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if err := events.DeleteEvent(ctx, "evt-retry"); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}

	isNew, err := events.RecordEvent(ctx, "evt-retry")
	if err != nil {
		t.Fatalf("RecordEvent after delete: %v", err)
	}
	if !isNew {
		t.Error("event should be processable again after DeleteEvent")
	}

	// Deleting an unknown id is not an error.
	if err := events.DeleteEvent(ctx, "never-seen"); err != nil {
		t.Errorf("DeleteEvent unknown: %v", err)
	}
}
