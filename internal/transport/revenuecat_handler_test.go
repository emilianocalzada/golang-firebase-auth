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

const webhookSecret = "Bearer test-secret-value-long-enough"

// fakeProvider stands in for the RevenueCat API.
type fakeProvider struct {
	active    bool
	expiresAt *time.Time
	err       error
	calls     int
	lastUID   string
}

func (f *fakeProvider) PremiumStatus(_ context.Context, appUserID string) (bool, *time.Time, error) {
	f.calls++
	f.lastUID = appUserID

	if f.err != nil {
		return false, nil, f.err
	}

	return f.active, f.expiresAt, nil
}

func newWebhookServer(t *testing.T, provider *fakeProvider) (*gin.Engine, *service.UserService) {
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

	r := gin.New()
	transport.NewRevenueCatHandler(rcService, webhookSecret).RegisterRoutes(r)

	return r, users
}

func postWebhook(t *testing.T, r *gin.Engine, authHeader, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/webhooks/revenuecat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

func eventBody(id, eventType, appUserID string) string {
	return `{"api_version":"1.0","event":{"id":"` + id + `","type":"` + eventType + `","app_user_id":"` + appUserID + `"}}`
}

func TestWebhookRejectsBadAuthorization(t *testing.T) {
	provider := &fakeProvider{active: true}
	r, _ := newWebhookServer(t, provider)

	cases := []struct {
		name   string
		header string
	}{
		{"missing", ""},
		{"wrong secret", "Bearer wrong-secret-value-here"},
		{"prefix only", "Bearer"},
		{"secret without scheme", strings.TrimPrefix(webhookSecret, "Bearer ")},
		{"trailing space", webhookSecret + " "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postWebhook(t, r, tc.header, eventBody("evt-1", "INITIAL_PURCHASE", "uid-1"))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body: %s)", w.Code, w.Body.String())
			}
		})
	}

	if provider.calls != 0 {
		t.Errorf("provider called %d times, want 0 for unauthorized requests", provider.calls)
	}
}

func TestWebhookSyncsPremium(t *testing.T) {
	expires := time.Now().Add(30 * 24 * time.Hour)
	provider := &fakeProvider{active: true, expiresAt: &expires}
	r, users := newWebhookServer(t, provider)

	w := postWebhook(t, r, webhookSecret, eventBody("evt-1", "INITIAL_PURCHASE", "uid-buyer"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"is_premium":true`) {
		t.Errorf("body = %s, want is_premium true", w.Body.String())
	}
	if provider.lastUID != "uid-buyer" {
		t.Errorf("provider asked about %q, want uid-buyer", provider.lastUID)
	}

	// The user row was created by the webhook alone.
	user, err := users.GetUser(context.Background(), "uid-buyer")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if !user.HasActivePremium() {
		t.Error("user should have active premium")
	}
}

func TestWebhookRevokesWhenEntitlementIsGone(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	provider := &fakeProvider{active: true, expiresAt: &expires}
	r, users := newWebhookServer(t, provider)

	if w := postWebhook(t, r, webhookSecret, eventBody("evt-1", "INITIAL_PURCHASE", "uid-churn")); w.Code != http.StatusOK {
		t.Fatalf("purchase status = %d", w.Code)
	}

	// RevenueCat now reports no entitlement, e.g. after a refund.
	provider.active = false
	provider.expiresAt = nil

	if w := postWebhook(t, r, webhookSecret, eventBody("evt-2", "CANCELLATION", "uid-churn")); w.Code != http.StatusOK {
		t.Fatalf("cancellation status = %d", w.Code)
	}

	user, err := users.GetUser(context.Background(), "uid-churn")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if user.HasActivePremium() {
		t.Error("premium should be revoked")
	}
}

func TestWebhookIsIdempotent(t *testing.T) {
	provider := &fakeProvider{active: true}
	r, _ := newWebhookServer(t, provider)
	body := eventBody("evt-same", "RENEWAL", "uid-1")

	if w := postWebhook(t, r, webhookSecret, body); w.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d", w.Code)
	}

	w := postWebhook(t, r, webhookSecret, body)
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "duplicate") {
		t.Errorf("body = %s, want duplicate", w.Body.String())
	}
	if provider.calls != 1 {
		t.Errorf("provider called %d times, want 1 (retry must not re-sync)", provider.calls)
	}
}

func TestWebhookIgnoresTestEvents(t *testing.T) {
	provider := &fakeProvider{active: true}
	r, users := newWebhookServer(t, provider)

	w := postWebhook(t, r, webhookSecret, eventBody("evt-test", "TEST", "dashboard-user"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if provider.calls != 0 {
		t.Errorf("provider called %d times, want 0 for a TEST event", provider.calls)
	}
	if _, err := users.GetUser(context.Background(), "dashboard-user"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("TEST event must not create a user, err = %v", err)
	}
}

func TestWebhookRejectsInvalidPayloads(t *testing.T) {
	provider := &fakeProvider{active: true}
	r, _ := newWebhookServer(t, provider)

	cases := []struct {
		name string
		body string
	}{
		{"broken json", `{"event":`},
		{"missing event id", `{"event":{"type":"RENEWAL","app_user_id":"uid-1"}}`},
		{"empty object", `{}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postWebhook(t, r, webhookSecret, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body: %s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestWebhookReturns502AndAllowsRetryWhenRevenueCatFails(t *testing.T) {
	provider := &fakeProvider{err: errors.New("revenuecat down")}
	r, users := newWebhookServer(t, provider)
	body := eventBody("evt-flaky", "RENEWAL", "uid-1")

	w := postWebhook(t, r, webhookSecret, body)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", w.Code, w.Body.String())
	}

	// The retry must be processed, not skipped as a duplicate.
	provider.err = nil
	provider.active = true

	if w := postWebhook(t, r, webhookSecret, body); w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if provider.calls != 2 {
		t.Errorf("provider called %d times, want 2", provider.calls)
	}

	user, err := users.GetUser(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if !user.HasActivePremium() {
		t.Error("retry should have granted premium")
	}
}

func TestWebhookGrantsAccessToPremiumRoute(t *testing.T) {
	// End to end: webhook grants premium, then RequirePremium lets the user in.
	provider := &fakeProvider{active: true}
	gin.SetMode(gin.TestMode)

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	users := service.NewUserService(store.NewUserStore(db))
	rcService := service.NewRevenueCatService(users, store.NewRevenueCatEventStore(db), provider)
	middleware := transport.NewAuthMiddleware(&fakeVerifier{validToken: "good-token", uid: "uid-123"}, users)

	r := gin.New()
	transport.NewRevenueCatHandler(rcService, webhookSecret).RegisterRoutes(r)
	v1 := r.Group("/v1", middleware.RequireAuth())
	v1.POST("/premium-only", middleware.RequirePremium(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	if w := do(t, r, http.MethodPost, "/v1/premium-only", "Bearer good-token"); w.Code != http.StatusForbidden {
		t.Fatalf("before purchase status = %d, want 403", w.Code)
	}

	if w := postWebhook(t, r, webhookSecret, eventBody("evt-1", "INITIAL_PURCHASE", "uid-123")); w.Code != http.StatusOK {
		t.Fatalf("webhook status = %d (body: %s)", w.Code, w.Body.String())
	}

	if w := do(t, r, http.MethodPost, "/v1/premium-only", "Bearer good-token"); w.Code != http.StatusOK {
		t.Errorf("after purchase status = %d, want 200", w.Code)
	}
}
