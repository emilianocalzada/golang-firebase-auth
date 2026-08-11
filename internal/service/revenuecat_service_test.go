package service_test

import (
	"aislide/internal/service"
	"aislide/internal/store"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type stubProvider struct {
	active bool
	err    error
	calls  int
}

func (s *stubProvider) PremiumStatus(_ context.Context, _ string) (bool, *time.Time, error) {
	s.calls++

	if s.err != nil {
		return false, nil, s.err
	}

	return s.active, nil, nil
}

// flakyEvents wraps the real store so a write can be made to fail, standing in
// for the process dying between syncing and recording the event.
type flakyEvents struct {
	store.RevenueCatEventStore
	failRecord bool
	records    int
}

func (f *flakyEvents) RecordEvent(ctx context.Context, eventID string) error {
	f.records++

	if f.failRecord {
		return errors.New("disk full")
	}

	return f.RevenueCatEventStore.RecordEvent(ctx, eventID)
}

func newService(t *testing.T, provider service.EntitlementProvider) (*service.RevenueCatService, *service.UserService, *flakyEvents) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	users := service.NewUserService(store.NewUserStore(db))
	events := &flakyEvents{RevenueCatEventStore: store.NewRevenueCatEventStore(db)}

	return service.NewRevenueCatService(users, events, provider), users, events
}

func purchase(id, appUserID string) service.RevenueCatEvent {
	return service.RevenueCatEvent{ID: id, Type: "INITIAL_PURCHASE", AppUserID: appUserID}
}

func TestProcessEventRecordsTheEventOnlyAfterSyncing(t *testing.T) {
	// Recording first would mean a failure anywhere in the sync leaves the id
	// marked as done, and the redelivery is then acknowledged as a duplicate
	// while the customer stays stale forever.
	provider := &stubProvider{err: errors.New("revenuecat down")}
	rc, users, _ := newService(t, provider)
	ctx := context.Background()

	if _, err := rc.ProcessEvent(ctx, purchase("evt-1", "uid-1")); err == nil {
		t.Fatal("ProcessEvent should fail while RevenueCat is down")
	}

	provider.err = nil
	provider.active = true

	result, err := rc.ProcessEvent(ctx, purchase("evt-1", "uid-1"))
	if err != nil {
		t.Fatalf("redelivery must be reprocessed, got: %v", err)
	}
	if len(result.Synced) != 1 {
		t.Fatalf("synced %d users, want 1", len(result.Synced))
	}

	user, err := users.GetUser(ctx, "uid-1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if !user.HasActivePremium() {
		t.Error("the redelivery should have granted premium")
	}
}

func TestProcessEventReportsAFailureToRecord(t *testing.T) {
	// A crash after syncing but before recording only costs repeated work: the
	// writes are idempotent, so reprocessing converges on the same state.
	provider := &stubProvider{active: true}
	rc, users, events := newService(t, provider)
	ctx := context.Background()

	events.failRecord = true

	if _, err := rc.ProcessEvent(ctx, purchase("evt-1", "uid-1")); err == nil {
		t.Fatal("ProcessEvent should surface a failure to record the event")
	}

	// The sync itself already happened.
	user, err := users.GetUser(ctx, "uid-1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if !user.HasActivePremium() {
		t.Error("premium should have been written before recording was attempted")
	}

	events.failRecord = false

	if _, err := rc.ProcessEvent(ctx, purchase("evt-1", "uid-1")); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if provider.calls != 2 {
		t.Errorf("provider called %d times, want 2 (the redelivery redoes the work)", provider.calls)
	}
}

func TestProcessEventSkipsAnAlreadyRecordedEvent(t *testing.T) {
	provider := &stubProvider{active: true}
	rc, _, events := newService(t, provider)
	ctx := context.Background()

	if err := events.RecordEvent(ctx, "evt-done"); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	_, err := rc.ProcessEvent(ctx, purchase("evt-done", "uid-1"))
	if !errors.Is(err, service.ErrDuplicateEvent) {
		t.Errorf("err = %v, want ErrDuplicateEvent", err)
	}
	if provider.calls != 0 {
		t.Errorf("provider called %d times, want 0 for a completed event", provider.calls)
	}
}
