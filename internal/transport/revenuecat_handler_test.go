package transport_test

import (
	"aislide/internal/revenuecat"
	"aislide/internal/service"
	"aislide/internal/store"
	"aislide/internal/transport"
	"context"
	"errors"
	"fmt"
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

// entitlement is one customer's state in the fake RevenueCat API.
type entitlement struct {
	active    bool
	expiresAt *time.Time
	// err makes the lookup for this one App User ID fail, which is what the
	// real API does for an id it does not know (it answers 404).
	err error
}

// fakeProvider stands in for the RevenueCat API.
type fakeProvider struct {
	active    bool
	expiresAt *time.Time
	err       error
	calls     int
	lastUID   string
	askedFor  []string
	// perUID, when set, answers per App User ID instead of using the single
	// active/expiresAt pair. A UID missing from the map has no entitlement,
	// which is what RevenueCat reports for the losing side of a transfer.
	perUID map[string]entitlement
}

func (f *fakeProvider) PremiumStatus(_ context.Context, appUserID string) (bool, *time.Time, error) {
	f.calls++
	f.lastUID = appUserID
	f.askedFor = append(f.askedFor, appUserID)

	if f.err != nil {
		return false, nil, f.err
	}

	if f.perUID != nil {
		if e := f.perUID[appUserID]; e.err != nil {
			return false, nil, e.err
		}

		return f.perUID[appUserID].active, f.perUID[appUserID].expiresAt, nil
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

// transferBody mirrors a real TRANSFER delivery: no app_user_id field at all,
// only the two ends of the move.
func transferBody(id string, from, to []string) string {
	quote := func(uids []string) string {
		quoted := make([]string, 0, len(uids))
		for _, uid := range uids {
			quoted = append(quoted, `"`+uid+`"`)
		}

		return "[" + strings.Join(quoted, ",") + "]"
	}

	return `{"api_version":"1.0","event":{"id":"` + id + `","type":"TRANSFER",` +
		`"store":"APP_STORE","environment":"PRODUCTION",` +
		`"transferred_from":` + quote(from) + `,"transferred_to":` + quote(to) + `}}`
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

func TestWebhookTransfersPremiumBetweenFirebaseUIDs(t *testing.T) {
	// The scenario: an existing subscriber updates the app, gets a brand new
	// Firebase UID from anonymous sign-in, and taps "Restore purchases".
	// RevenueCat transfers the entitlement and sends a TRANSFER event, which
	// carries no app_user_id.
	expires := time.Now().Add(30 * 24 * time.Hour)
	provider := &fakeProvider{perUID: map[string]entitlement{
		"uid-old": {active: true, expiresAt: &expires},
	}}
	r, users := newWebhookServer(t, provider)

	if w := postWebhook(t, r, webhookSecret, eventBody("evt-1", "INITIAL_PURCHASE", "uid-old")); w.Code != http.StatusOK {
		t.Fatalf("purchase status = %d (body: %s)", w.Code, w.Body.String())
	}
	if user, err := users.GetUser(context.Background(), "uid-old"); err != nil || !user.HasActivePremium() {
		t.Fatalf("uid-old should start premium (err = %v)", err)
	}

	// RevenueCat has moved the entitlement to the new UID.
	provider.perUID = map[string]entitlement{
		"uid-new": {active: true, expiresAt: &expires},
	}
	provider.askedFor = nil

	body := transferBody("evt-transfer", []string{"uid-old"}, []string{"uid-new"})
	w := postWebhook(t, r, webhookSecret, body)
	if w.Code != http.StatusOK {
		t.Fatalf("transfer status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"synced"`) {
		t.Errorf("body = %s, want synced (a TRANSFER must not be ignored)", w.Body.String())
	}

	oldUser, err := users.GetUser(context.Background(), "uid-old")
	if err != nil {
		t.Fatalf("GetUser(uid-old): %v", err)
	}
	if oldUser.HasActivePremium() {
		t.Error("uid-old must lose premium after the transfer")
	}

	newUser, err := users.GetUser(context.Background(), "uid-new")
	if err != nil {
		t.Fatalf("GetUser(uid-new): %v", err)
	}
	if !newUser.HasActivePremium() {
		t.Error("uid-new must gain premium after the transfer")
	}

	// Both ends were reconciled, and the destination before the source so that
	// nothing about the old id can stand between the customer and their access.
	if want := []string{"uid-new", "uid-old"}; !equalStrings(provider.askedFor, want) {
		t.Errorf("provider asked for %v, want %v", provider.askedFor, want)
	}

	// A redelivery of the same transfer must not re-sync.
	calls := provider.calls
	if w := postWebhook(t, r, webhookSecret, body); w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", w.Code)
	} else if !strings.Contains(w.Body.String(), "duplicate") {
		t.Errorf("retry body = %s, want duplicate", w.Body.String())
	}
	if provider.calls != calls {
		t.Errorf("provider called %d more times on redelivery, want 0", provider.calls-calls)
	}
}

func TestWebhookTransferFromForeignAppUserIDStillGrantsDestination(t *testing.T) {
	// Before this backend, purchases were keyed on PocketBase user ids. Those
	// are not Firebase UIDs, so there is no local row to revoke: the source is
	// reported as a warning and must not stop the destination from being synced
	// or turn the delivery into a failure.
	const pocketbaseID = "8b0b1a4e1f4c4c9"

	provider := &fakeProvider{perUID: map[string]entitlement{
		"uid-new": {active: true},
	}}
	r, users := newWebhookServer(t, provider)

	w := postWebhook(t, r, webhookSecret, transferBody("evt-transfer", []string{pocketbaseID}, []string{"uid-new"}))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"synced"`) {
		t.Errorf("body = %s, want synced", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), pocketbaseID) {
		t.Errorf("body = %s, want the skipped source reported as a warning", w.Body.String())
	}

	newUser, err := users.GetUser(context.Background(), "uid-new")
	if err != nil {
		t.Fatalf("GetUser(uid-new): %v", err)
	}
	if !newUser.HasActivePremium() {
		t.Error("uid-new must gain premium, it is the restoring customer")
	}

	if _, err := users.GetUser(context.Background(), pocketbaseID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("foreign source must not become a user row, err = %v", err)
	}
	if want := []string{"uid-new"}; !equalStrings(provider.askedFor, want) {
		t.Errorf("provider asked for %v, want %v (unknown source needs no lookup)", provider.askedFor, want)
	}
}

func TestWebhookTransferRetriesWhenTheSourceCannotBeReconciled(t *testing.T) {
	// The source is a known Firebase uid whose lookup fails. Leaving it premium
	// would mean two accounts entitled at once, so the delivery must fail and be
	// retried. The destination keeps the access it was already granted.
	provider := &fakeProvider{perUID: map[string]entitlement{
		"uid-old": {active: true},
	}}
	r, users := newWebhookServer(t, provider)

	if w := postWebhook(t, r, webhookSecret, eventBody("evt-1", "INITIAL_PURCHASE", "uid-old")); w.Code != http.StatusOK {
		t.Fatalf("purchase status = %d (body: %s)", w.Code, w.Body.String())
	}

	provider.perUID = map[string]entitlement{
		"uid-old": {err: errors.New("revenuecat returned status 503")},
		"uid-new": {active: true},
	}

	body := transferBody("evt-transfer", []string{"uid-old"}, []string{"uid-new"})
	if w := postWebhook(t, r, webhookSecret, body); w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", w.Code, w.Body.String())
	}

	// The destination was written before the source was attempted, so the
	// customer who restored is not left waiting on the retry.
	newUser, err := users.GetUser(context.Background(), "uid-new")
	if err != nil {
		t.Fatalf("GetUser(uid-new): %v", err)
	}
	if !newUser.HasActivePremium() {
		t.Error("uid-new should already have premium despite the failed delivery")
	}

	// The redelivery must be processed, not skipped as a duplicate.
	provider.perUID = map[string]entitlement{"uid-new": {active: true}}

	if w := postWebhook(t, r, webhookSecret, body); w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	oldUser, err := users.GetUser(context.Background(), "uid-old")
	if err != nil {
		t.Fatalf("GetUser(uid-old): %v", err)
	}
	if oldUser.HasActivePremium() {
		t.Error("uid-old must lose premium once the retry succeeds")
	}
}

func TestWebhookTransferRevokesASourceRevenueCatDoesNotKnow(t *testing.T) {
	// A known Firebase uid that RevenueCat has no customer for cannot own an
	// entitlement, so it is revoked instead of retried forever.
	provider := &fakeProvider{perUID: map[string]entitlement{
		"uid-old": {active: true},
	}}
	r, users := newWebhookServer(t, provider)

	if w := postWebhook(t, r, webhookSecret, eventBody("evt-1", "INITIAL_PURCHASE", "uid-old")); w.Code != http.StatusOK {
		t.Fatalf("purchase status = %d (body: %s)", w.Code, w.Body.String())
	}

	provider.perUID = map[string]entitlement{
		"uid-old": {err: fmt.Errorf("%w: uid-old: status 404", revenuecat.ErrCustomerNotFound)},
		"uid-new": {active: true},
	}

	w := postWebhook(t, r, webhookSecret, transferBody("evt-transfer", []string{"uid-old"}, []string{"uid-new"}))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "warnings") {
		t.Errorf("body = %s, want the revoked source reported as a warning", w.Body.String())
	}

	oldUser, err := users.GetUser(context.Background(), "uid-old")
	if err != nil {
		t.Fatalf("GetUser(uid-old): %v", err)
	}
	if oldUser.HasActivePremium() {
		t.Error("a source unknown to revenuecat must be revoked, not left premium")
	}

	newUser, err := users.GetUser(context.Background(), "uid-new")
	if err != nil {
		t.Fatalf("GetUser(uid-new): %v", err)
	}
	if !newUser.HasActivePremium() {
		t.Error("uid-new must still gain premium")
	}
}

func TestWebhookTransferRetriesWhenTheDestinationFails(t *testing.T) {
	provider := &fakeProvider{err: errors.New("revenuecat down")}
	r, users := newWebhookServer(t, provider)
	body := transferBody("evt-transfer", []string{"uid-old"}, []string{"uid-new"})

	if w := postWebhook(t, r, webhookSecret, body); w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body: %s)", w.Code, w.Body.String())
	}

	provider.err = nil
	provider.perUID = map[string]entitlement{"uid-new": {active: true}}

	if w := postWebhook(t, r, webhookSecret, body); w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	user, err := users.GetUser(context.Background(), "uid-new")
	if err != nil {
		t.Fatalf("GetUser(uid-new): %v", err)
	}
	if !user.HasActivePremium() {
		t.Error("the retry should have granted premium to the destination")
	}
}

func TestWebhookTransferToMultipleDestinations(t *testing.T) {
	// transferred_to is a list: an aliased customer can have several ids.
	provider := &fakeProvider{perUID: map[string]entitlement{
		"uid-new-a": {active: true},
		"uid-new-b": {active: true},
	}}
	r, users := newWebhookServer(t, provider)

	w := postWebhook(t, r, webhookSecret,
		transferBody("evt-transfer", []string{"uid-old-a", "uid-old-b"}, []string{"uid-new-a", "uid-new-b"}))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	for _, uid := range []string{"uid-new-a", "uid-new-b"} {
		user, err := users.GetUser(context.Background(), uid)
		if err != nil {
			t.Fatalf("GetUser(%s): %v", uid, err)
		}
		if !user.HasActivePremium() {
			t.Errorf("%s must gain premium", uid)
		}
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
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
