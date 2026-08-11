package service

import (
	"aislide/internal/model"
	"aislide/internal/store"
	"context"
	"errors"
	"time"
)

type UserService struct {
	store store.UserStore
}

func NewUserService(s store.UserStore) *UserService {
	return &UserService{
		store: s,
	}
}

// EnsureUser provisions the local row for a Firebase account. It runs on every
// authenticated request, so the first call from a fresh anonymous sign-in
// creates the user and later calls just read it back.
func (s *UserService) EnsureUser(ctx context.Context, firebaseUID string) (*model.User, error) {
	if firebaseUID == "" {
		return nil, errors.New("firebase uid is required")
	}

	return s.store.EnsureExists(ctx, firebaseUID)
}

func (s *UserService) GetUser(ctx context.Context, firebaseUID string) (*model.User, error) {
	return s.store.GetByFirebaseUID(ctx, firebaseUID)
}

// SyncPremium writes the entitlement state we read from RevenueCat. It is the
// single place premium is granted or revoked, so the RevenueCat response stays
// the only source of truth.
func (s *UserService) SyncPremium(ctx context.Context, firebaseUID string, active bool, until *time.Time) (*model.User, error) {
	if firebaseUID == "" {
		return nil, errors.New("firebase uid is required")
	}

	return s.store.UpsertPremium(ctx, firebaseUID, active, until)
}

// GrantPremium marks the user as premium until the given time. A nil until
// means the entitlement does not expire.
func (s *UserService) GrantPremium(ctx context.Context, firebaseUID string, until *time.Time) (*model.User, error) {
	return s.SyncPremium(ctx, firebaseUID, true, until)
}

// RevokePremium clears the entitlement, e.g. on expiration or refund.
func (s *UserService) RevokePremium(ctx context.Context, firebaseUID string) (*model.User, error) {
	return s.SyncPremium(ctx, firebaseUID, false, nil)
}
