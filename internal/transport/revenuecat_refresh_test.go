package transport_test

import (
	"aislide/internal/service"
	"aislide/internal/store"
	"aislide/internal/transport"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
)

const refreshPath = "/v1/me/premium/refresh"

// newRefreshServer mounts the refresh endpoint behind the real auth middleware,
// with no webhook ever delivered: this is the state of a customer whose
// entitlement RevenueCat knows about and we do not.
func newRefreshServer(t *testing.T, provider *fakeProvider) (*gin.Engine, *service.UserService) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	users := service.NewUserService(store.NewUserStore(db))
	rcService := service.NewRevenueCatService(users, store.NewRevenueCatEventStore(db), provider)
	middleware := transport.NewAuthMiddleware(&fakeVerifier{validToken: "good-token", uid: "uid-123"}, users)

	r := gin.New()
	v1 := r.Group("/v1", middleware.RequireAuth())
	transport.NewRevenueCatHandler(rcService, webhookSecret).RegisterUserRoutes(v1)
	v1.POST("/premium-only", middleware.RequirePremium(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	return r, users
}

func TestRefreshRecoversPremiumWhenTheWebhookNeverArrived(t *testing.T) {
	provider := &fakeProvider{perUID: map[string]entitlement{
		"uid-123": {active: true}, // a lifetime purchase: no expiry, no later event
	}}
	r, users := newRefreshServer(t, provider)

	// The gate is closed: nothing has told us about the purchase.
	if w := do(t, r, http.MethodPost, "/v1/premium-only", "Bearer good-token"); w.Code != http.StatusForbidden {
		t.Fatalf("before refresh status = %d, want 403", w.Code)
	}

	w := do(t, r, http.MethodPost, refreshPath, "Bearer good-token")
	if w.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"is_premium":true`) {
		t.Errorf("body = %s, want is_premium true", w.Body.String())
	}
	if provider.lastUID != "uid-123" {
		t.Errorf("provider asked about %q, want the authenticated uid", provider.lastUID)
	}

	user, err := users.GetUser(context.Background(), "uid-123")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if !user.HasActivePremium() {
		t.Error("refresh should have granted premium")
	}

	if w := do(t, r, http.MethodPost, "/v1/premium-only", "Bearer good-token"); w.Code != http.StatusOK {
		t.Errorf("after refresh status = %d, want 200", w.Code)
	}
}

func TestRefreshRevokesPremiumWhenTheEntitlementIsGone(t *testing.T) {
	// The other direction: a refund whose webhook was lost.
	provider := &fakeProvider{perUID: map[string]entitlement{"uid-123": {active: true}}}
	r, users := newRefreshServer(t, provider)

	if w := do(t, r, http.MethodPost, refreshPath, "Bearer good-token"); w.Code != http.StatusOK {
		t.Fatalf("first refresh status = %d", w.Code)
	}

	provider.perUID = map[string]entitlement{}

	w := do(t, r, http.MethodPost, refreshPath, "Bearer good-token")
	if w.Code != http.StatusOK {
		t.Fatalf("second refresh status = %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"is_premium":false`) {
		t.Errorf("body = %s, want is_premium false", w.Body.String())
	}

	user, err := users.GetUser(context.Background(), "uid-123")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if user.HasActivePremium() {
		t.Error("refresh should have revoked premium")
	}
}

func TestRefreshIgnoresAnyUIDInTheBody(t *testing.T) {
	// The token says uid-123. A body naming someone else must not be honoured,
	// or one customer could grant themselves another's entitlement.
	provider := &fakeProvider{perUID: map[string]entitlement{
		"uid-123":    {active: false},
		"uid-victim": {active: true},
	}}
	r, users := newRefreshServer(t, provider)

	req := httptest.NewRequest(http.MethodPost, refreshPath, strings.NewReader(`{"firebase_uid":"uid-victim","app_user_id":"uid-victim"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"is_premium":false`) {
		t.Errorf("body = %s, want is_premium false for the authenticated uid", w.Body.String())
	}
	if provider.lastUID != "uid-123" {
		t.Errorf("provider asked about %q, want uid-123 (the token's uid)", provider.lastUID)
	}
	if _, err := users.GetUser(context.Background(), "uid-victim"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the uid from the body must never be touched, err = %v", err)
	}
}

func TestRefreshRequiresAuthentication(t *testing.T) {
	provider := &fakeProvider{perUID: map[string]entitlement{"uid-123": {active: true}}}
	r, _ := newRefreshServer(t, provider)

	for _, token := range []string{"", "Bearer wrong-token"} {
		w := do(t, r, http.MethodPost, refreshPath, token)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", token, w.Code)
		}
	}
	if provider.calls != 0 {
		t.Errorf("provider called %d times, want 0 for unauthenticated requests", provider.calls)
	}
}

func TestRefreshReturns502WhenRevenueCatFails(t *testing.T) {
	provider := &fakeProvider{err: errors.New("revenuecat down")}
	r, _ := newRefreshServer(t, provider)

	w := do(t, r, http.MethodPost, refreshPath, "Bearer good-token")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", w.Code, w.Body.String())
	}
}

func TestRefreshKeepsLocalExpiryWhenRevenueCatReportsIt(t *testing.T) {
	// An expiry in the past must not be reported as premium even though the
	// entitlement was returned as active, matching the webhook path.
	past := time.Now().Add(-time.Hour)
	provider := &fakeProvider{perUID: map[string]entitlement{
		"uid-123": {active: true, expiresAt: &past},
	}}
	r, _ := newRefreshServer(t, provider)

	w := do(t, r, http.MethodPost, refreshPath, "Bearer good-token")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"is_premium":false`) {
		t.Errorf("body = %s, want is_premium false for an elapsed expiry", w.Body.String())
	}
}
