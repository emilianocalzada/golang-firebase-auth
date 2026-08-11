package service

import (
	"aislide/internal/model"
	"aislide/internal/revenuecat"
	"aislide/internal/store"
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrDuplicateEvent means we already processed this event id. RevenueCat
// retries deliveries, so the transport layer treats it as success.
var ErrDuplicateEvent = errors.New("revenuecat event already processed")

// EntitlementProvider is the part of the RevenueCat API we depend on. An
// implementation must report a customer RevenueCat does not have as
// revenuecat.ErrCustomerNotFound, which is the one error the transfer path can
// act on rather than retry.
type EntitlementProvider interface {
	PremiumStatus(ctx context.Context, appUserID string) (bool, *time.Time, error)
}

const (
	// EventTypeTest is what the "Send test event" button in the RevenueCat
	// dashboard sends. Its app_user_id is not a real Firebase UID.
	EventTypeTest = "TEST"
	// EventTypeTransfer is sent when entitlements move between App User IDs,
	// e.g. after a user with a fresh Firebase UID taps "Restore purchases".
	EventTypeTransfer = "TRANSFER"
)

// maxAffectedCustomers bounds how many RevenueCat lookups one delivery can
// cost us. A real transfer names one or two App User IDs per side, so anything
// beyond this is malformed input rather than a customer we should chase.
const maxAffectedCustomers = 100

// RevenueCatEvent is the subset of a webhook payload the service needs.
type RevenueCatEvent struct {
	ID   string
	Type string
	// AppUserID identifies the customer on every event type except TRANSFER,
	// which omits the field entirely and uses the two lists below instead.
	AppUserID string
	// TransferredFrom are the App User IDs that lost the transactions. These
	// are not necessarily Firebase UIDs: a customer who bought before this
	// backend existed can be identified by an id from the previous system.
	TransferredFrom []string
	// TransferredTo are the App User IDs that received them.
	TransferredTo []string
}

// SyncResult is the outcome of one webhook delivery.
type SyncResult struct {
	// Synced holds every user whose entitlement state was written.
	Synced []*model.User
	// Warnings describe customers that were deliberately not reconciled: ids
	// from a previous backend, or source lookups that failed without being
	// worth failing the delivery. The transport layer logs them.
	Warnings []string
}

func (r *SyncResult) warnf(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

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

// ProcessEvent handles one webhook delivery and reports every user whose
// entitlement state it reconciled.
//
// Rather than interpreting every event type (purchase, renewal, cancellation,
// expiration, transfer, refund), it asks RevenueCat for the current entitlement
// state of every customer the event names. One code path covers every event type
// and always converges on the truth.
//
// It returns ErrDuplicateEvent for a redelivery we already completed, and an
// empty result for events that carry no entitlement to sync (test events).
func (s *RevenueCatService) ProcessEvent(ctx context.Context, event RevenueCatEvent) (*SyncResult, error) {
	if event.ID == "" {
		return nil, errors.New("event id is required")
	}

	sources, destinations := event.affectedCustomers()
	if total := len(sources) + len(destinations); total > maxAffectedCustomers {
		return nil, fmt.Errorf("event names %d customers, refusing to sync more than %d", total, maxAffectedCustomers)
	}

	processed, err := s.events.HasEvent(ctx, event.ID)
	if err != nil {
		return nil, fmt.Errorf("look up event: %w", err)
	}
	if processed {
		return nil, ErrDuplicateEvent
	}

	result := &SyncResult{}

	// Destinations first: granting the entitlement to the customer who just
	// restored is what the app is waiting on, and doing it before anything else
	// means a later failure cannot take it away again. A 404 here stays a hard
	// error, which is also what stops a misconfigured project id from ever
	// reaching the revoke path in releaseSources.
	for _, appUserID := range destinations {
		user, err := s.reconcile(ctx, appUserID)
		if err != nil {
			return nil, err
		}
		result.Synced = append(result.Synced, user)
	}

	if err := s.releaseSources(ctx, sources, result); err != nil {
		return nil, err
	}

	// Recorded last, and only on success. Doing it first would let a crash
	// between recording and syncing turn the redelivery into an acknowledged
	// duplicate, leaving the customer stale forever. Two deliveries racing may
	// now both do the work; every write is idempotent, so they converge.
	if err := s.events.RecordEvent(ctx, event.ID); err != nil {
		return nil, fmt.Errorf("record event: %w", err)
	}

	return result, nil
}

// releaseSources revokes premium from the losing side of a transfer.
//
// An id we have never stored is reported and skipped: purchases made before this
// backend used Firebase are keyed on ids from the previous system, and there is
// nothing to revoke for one of those. A customer RevenueCat itself does not know
// is revoked, since it cannot be entitled to anything.
//
// Anything else is returned as an error so the delivery fails and RevenueCat
// retries, instead of leaving the entitlement active on two accounts with only a
// log line to show for it. The destinations are already written by then and
// re-granting them on the retry is idempotent, so the customer who restored
// keeps their access either way.
func (s *RevenueCatService) releaseSources(ctx context.Context, sources []string, result *SyncResult) error {
	for _, appUserID := range sources {
		if _, err := s.users.GetUser(ctx, appUserID); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("look up transfer source %s: %w", appUserID, err)
			}

			result.warnf("transfer source %q is not a known firebase uid, nothing to revoke", appUserID)

			continue
		}

		user, err := s.reconcile(ctx, appUserID)

		switch {
		case errors.Is(err, revenuecat.ErrCustomerNotFound):
			result.warnf("transfer source %q is unknown to revenuecat, revoking: %v", appUserID, err)

			user, err = s.users.RevokePremium(ctx, appUserID)
			if err != nil {
				return fmt.Errorf("revoke transfer source %s: %w", appUserID, err)
			}
		case err != nil:
			return fmt.Errorf("transfer source %s: %w", appUserID, err)
		}

		result.Synced = append(result.Synced, user)
	}

	return nil
}

// RefreshPremium re-reads one customer's entitlement state on demand and is the
// recovery path for a webhook that never arrived. The app calls it after a
// purchase or a restore.
//
// Without it some states stay wrong forever. An expired subscription fails
// closed on its own because HasActivePremium checks the stored expiry, but a
// lifetime purchase, a refund, or a transfer whose deliveries were all lost has
// nothing that would ever re-read it.
//
// The caller must pass the UID from the verified Firebase token, never one taken
// from a request body.
func (s *RevenueCatService) RefreshPremium(ctx context.Context, firebaseUID string) (*model.User, error) {
	if firebaseUID == "" {
		return nil, errors.New("firebase uid is required")
	}

	return s.reconcile(ctx, firebaseUID)
}

// reconcile reads one customer's entitlement state from RevenueCat and writes
// it locally, creating the user row when the webhook beats the app's first
// authenticated call. RevenueCat is configured with appUserID = Firebase UID.
func (s *RevenueCatService) reconcile(ctx context.Context, appUserID string) (*model.User, error) {
	active, expiresAt, err := s.provider.PremiumStatus(ctx, appUserID)
	if err != nil {
		return nil, fmt.Errorf("read entitlement for %s: %w", appUserID, err)
	}

	user, err := s.users.SyncPremium(ctx, appUserID, active, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("sync premium for %s: %w", appUserID, err)
	}

	return user, nil
}

// affectedCustomers splits the event into the customers that may have lost
// entitlements and the ones that may have gained them.
//
// TRANSFER events carry no app_user_id at all: RevenueCat only names the two
// ends of the move in transferred_from and transferred_to. Keying the sync on
// app_user_id alone therefore skips both sides, leaving premium switched on for
// the old id and off for the new one.
func (e RevenueCatEvent) affectedCustomers() (sources, destinations []string) {
	if e.Type == EventTypeTest {
		return nil, nil
	}

	seen := make(map[string]bool)

	// Destinations are collected first so an id appearing on both sides counts
	// as a gainer, which is the half that must not be skipped.
	destinations = appendUnique(destinations, seen, e.AppUserID)
	for _, appUserID := range e.TransferredTo {
		destinations = appendUnique(destinations, seen, appUserID)
	}

	for _, appUserID := range e.TransferredFrom {
		sources = appendUnique(sources, seen, appUserID)
	}

	return sources, destinations
}

func appendUnique(list []string, seen map[string]bool, appUserID string) []string {
	if appUserID == "" || seen[appUserID] {
		return list
	}
	seen[appUserID] = true

	return append(list, appUserID)
}
