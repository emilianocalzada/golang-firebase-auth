package service

import (
	"aislide/internal/model"
	"aislide/internal/store"
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrDuplicateEvent means we already processed this event id. RevenueCat
// retries deliveries, so the transport layer treats it as success.
var ErrDuplicateEvent = errors.New("revenuecat event already processed")

// EntitlementProvider is the part of the RevenueCat API we depend on.
type EntitlementProvider interface {
	PremiumStatus(ctx context.Context, appUserID string) (bool, *time.Time, error)
}

// RevenueCatEvent is the subset of a webhook payload the service needs.
type RevenueCatEvent struct {
	ID        string
	Type      string
	AppUserID string
}

// EventTypeTest is what the "Send test event" button in the RevenueCat
// dashboard sends. Its app_user_id is not a real Firebase UID.
const EventTypeTest = "TEST"

type RevenueCatService struct {
	users    *UserService
	events   store.RevenueCatEventStore
	provider EntitlementProvider
}

func NewRevenueCatService(users *UserService, events store.RevenueCatEventStore, provider EntitlementProvider) *RevenueCatService {
	return &RevenueCatService{
		users:    users,
		events:   events,
		provider: provider,
	}
}

// ProcessEvent handles one webhook delivery.
//
// Rather than interpreting every event type (purchase, renewal, cancellation,
// expiration, transfer, refund), it records the event id for idempotency and
// then asks RevenueCat for the customer's current entitlement state. One code
// path covers every event type and always converges on the truth.
//
// It returns ErrDuplicateEvent for a retry we already handled, and a nil user
// for events that carry no entitlement to sync (test events).
func (s *RevenueCatService) ProcessEvent(ctx context.Context, event RevenueCatEvent) (*model.User, error) {
	if event.ID == "" {
		return nil, errors.New("event id is required")
	}

	isNew, err := s.events.RecordEvent(ctx, event.ID)
	if err != nil {
		return nil, fmt.Errorf("record event: %w", err)
	}
	if !isNew {
		return nil, ErrDuplicateEvent
	}

	if event.Type == EventTypeTest || event.AppUserID == "" {
		return nil, nil
	}

	// RevenueCat is configured with appUserID = Firebase UID.
	active, expiresAt, err := s.provider.PremiumStatus(ctx, event.AppUserID)
	if err != nil {
		s.forget(ctx, event.ID)

		return nil, fmt.Errorf("read entitlement: %w", err)
	}

	user, err := s.users.SyncPremium(ctx, event.AppUserID, active, expiresAt)
	if err != nil {
		s.forget(ctx, event.ID)

		return nil, fmt.Errorf("sync premium: %w", err)
	}

	return user, nil
}

// forget drops the event id so RevenueCat's retry gets another chance. Losing
// this delete is not fatal: the customer state is re-read on the next event,
// and it is better than permanently swallowing a failed delivery.
func (s *RevenueCatService) forget(ctx context.Context, eventID string) {
	_ = s.events.DeleteEvent(ctx, eventID)
}
