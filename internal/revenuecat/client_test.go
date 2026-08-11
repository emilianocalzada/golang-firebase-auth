package revenuecat_test

import (
	"aislide/internal/revenuecat"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testProjectID = "proj1ab2c3d4"
	testAPIKey    = "sk_v2_test_secret"
)

// stubAPI serves a canned v2 response.
func stubAPI(t *testing.T, status int, body string) *revenuecat.Client {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return revenuecat.New(testAPIKey, testProjectID, "premium").WithBaseURL(server.URL)
}

// entitlementList builds a CustomerActiveEntitlementList payload. A nil
// expiresAt renders as JSON null, i.e. a non-expiring entitlement.
func entitlementList(entitlementID string, expiresAt *time.Time) string {
	expires := "null"
	if expiresAt != nil {
		expires = strconv.FormatInt(expiresAt.UnixMilli(), 10)
	}

	return `{"object":"list","items":[{"object":"customer.active_entitlement","entitlement_id":"` +
		entitlementID + `","expires_at":` + expires + `}],"next_page":null,"url":"/v2/..."}`
}

func TestPremiumStatusActiveSubscription(t *testing.T) {
	expires := time.Now().Add(48 * time.Hour)
	client := stubAPI(t, http.StatusOK, entitlementList("premium", &expires))

	active, expiresAt, err := client.PremiumStatus(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("PremiumStatus: %v", err)
	}
	if !active {
		t.Error("active = false, want true")
	}
	if expiresAt == nil {
		t.Fatal("expiresAt should be set")
	}
	// expires_at arrives as ms since epoch and must survive the conversion.
	if expiresAt.UnixMilli() != expires.UnixMilli() {
		t.Errorf("expiresAt = %v, want %v", expiresAt, expires)
	}
}

func TestPremiumStatusLifetimeEntitlement(t *testing.T) {
	client := stubAPI(t, http.StatusOK, entitlementList("premium", nil))

	active, expiresAt, err := client.PremiumStatus(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("PremiumStatus: %v", err)
	}
	if !active {
		t.Error("lifetime entitlement should be active")
	}
	if expiresAt != nil {
		t.Errorf("expiresAt = %v, want nil for lifetime", expiresAt)
	}
}

func TestPremiumStatusExpiredDateIsNotActive(t *testing.T) {
	// v2 should not list a lapsed entitlement, but if it does we trust the date
	// so a missed webhook cannot keep premium switched on.
	past := time.Now().Add(-time.Hour)
	client := stubAPI(t, http.StatusOK, entitlementList("premium", &past))

	active, expiresAt, err := client.PremiumStatus(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("PremiumStatus: %v", err)
	}
	if active {
		t.Error("a past expires_at must not be active")
	}
	if expiresAt == nil {
		t.Error("expiresAt should still be reported")
	}
}

func TestPremiumStatusEmptyList(t *testing.T) {
	client := stubAPI(t, http.StatusOK, `{"object":"list","items":[],"next_page":null}`)

	active, expiresAt, err := client.PremiumStatus(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("PremiumStatus: %v", err)
	}
	if active || expiresAt != nil {
		t.Errorf("active = %v, expiresAt = %v, want false/nil", active, expiresAt)
	}
}

func TestPremiumStatusIgnoresOtherEntitlements(t *testing.T) {
	client := stubAPI(t, http.StatusOK, entitlementList("pro_annual", nil))

	active, _, err := client.PremiumStatus(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("PremiumStatus: %v", err)
	}
	if active {
		t.Error("a different entitlement id must not grant premium")
	}
}

func TestPremiumStatusFollowsPagination(t *testing.T) {
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("starting_after")
		cursors = append(cursors, cursor)

		if cursor == "" {
			// First page has no premium and points at a second page.
			w.Write([]byte(`{"object":"list","items":[{"object":"customer.active_entitlement","entitlement_id":"other","expires_at":null}],` +
				`"next_page":"/v2/projects/` + testProjectID + `/customers/uid-1/active_entitlements?starting_after=ent_cursor_2"}`))
			return
		}
		w.Write([]byte(entitlementList("premium", nil)))
	}))
	defer server.Close()

	client := revenuecat.New(testAPIKey, testProjectID, "premium").WithBaseURL(server.URL)

	active, _, err := client.PremiumStatus(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("PremiumStatus: %v", err)
	}
	if !active {
		t.Error("entitlement on the second page should be found")
	}
	if len(cursors) != 2 {
		t.Fatalf("made %d requests, want 2", len(cursors))
	}
	if cursors[1] != "ent_cursor_2" {
		t.Errorf("second request cursor = %q, want ent_cursor_2", cursors[1])
	}
}

func TestPremiumStatusStopsWhenNextPageIsNull(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Write([]byte(`{"object":"list","items":[],"next_page":null}`))
	}))
	defer server.Close()

	client := revenuecat.New(testAPIKey, testProjectID, "premium").WithBaseURL(server.URL)
	if _, _, err := client.PremiumStatus(context.Background(), "uid-1"); err != nil {
		t.Fatalf("PremiumStatus: %v", err)
	}
	if calls != 1 {
		t.Errorf("made %d requests, want 1", calls)
	}
}

func TestPremiumStatusStopsEarlyWhenFound(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		// Premium is on this page even though a next page exists.
		w.Write([]byte(`{"object":"list","items":[{"object":"customer.active_entitlement","entitlement_id":"premium","expires_at":null}],` +
			`"next_page":"/v2/whatever?starting_after=more"}`))
	}))
	defer server.Close()

	client := revenuecat.New(testAPIKey, testProjectID, "premium").WithBaseURL(server.URL)
	active, _, err := client.PremiumStatus(context.Background(), "uid-1")
	if err != nil {
		t.Fatalf("PremiumStatus: %v", err)
	}
	if !active {
		t.Error("active = false, want true")
	}
	if calls != 1 {
		t.Errorf("made %d requests, want 1 (should stop once found)", calls)
	}
}

func TestPremiumStatus404IsATypedError(t *testing.T) {
	// A wrong project id also answers 404, so treating it as "no entitlement"
	// would silently downgrade every paying customer. It is reported as a
	// sentinel instead, so only the callers that can act on it do.
	client := stubAPI(t, http.StatusNotFound, `{"type":"resource_missing","message":"Resource not found","object":"error"}`)

	_, _, err := client.PremiumStatus(context.Background(), "uid-1")
	if err == nil {
		t.Fatal("404 should be an error")
	}
	if !errors.Is(err, revenuecat.ErrCustomerNotFound) {
		t.Errorf("err = %v, want it to match ErrCustomerNotFound", err)
	}
	// The RevenueCat error detail should make it into the message.
	if !strings.Contains(err.Error(), "resource_missing") || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want it to mention the status and type", err)
	}
}

func TestPremiumStatusOtherErrorsAreNotCustomerNotFound(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		client := stubAPI(t, status, `{"message":"nope"}`)

		_, _, err := client.PremiumStatus(context.Background(), "uid-1")
		if err == nil {
			t.Fatalf("status %d should be an error", status)
		}
		if errors.Is(err, revenuecat.ErrCustomerNotFound) {
			t.Errorf("status %d must not look like a missing customer, or a transient outage would revoke premium", status)
		}
	}
}

func TestPremiumStatusErrorMentionsAuthenticationFailure(t *testing.T) {
	client := stubAPI(t, http.StatusUnauthorized,
		`{"doc_url":"https://errors.rev.cat/authentication-error","message":"Invalid API key.","object":"error","retryable":false,"type":"authentication_error"}`)

	_, _, err := client.PremiumStatus(context.Background(), "uid-1")
	if err == nil {
		t.Fatal("401 should be an error")
	}
	if !strings.Contains(err.Error(), "authentication_error") || !strings.Contains(err.Error(), "Invalid API key.") {
		t.Errorf("err = %v, want the RevenueCat message", err)
	}
}

func TestPremiumStatusServerErrors(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusLocked,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		client := stubAPI(t, status, `{"message":"nope"}`)

		if _, _, err := client.PremiumStatus(context.Background(), "uid-1"); err == nil {
			t.Errorf("status %d should be an error", status)
		}
	}
}

func TestPremiumStatusRequestShape(t *testing.T) {
	var gotAuth, gotAccept, gotPath, gotLimit string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotPath = r.URL.EscapedPath()
		gotLimit = r.URL.Query().Get("limit")
		w.Write([]byte(entitlementList("premium", nil)))
	}))
	defer server.Close()

	client := revenuecat.New(testAPIKey, testProjectID, "premium").WithBaseURL(server.URL)
	if _, _, err := client.PremiumStatus(context.Background(), "uid with/slash"); err != nil {
		t.Fatalf("PremiumStatus: %v", err)
	}

	if gotAuth != "Bearer "+testAPIKey {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if want := "/projects/" + testProjectID + "/customers/uid%20with%2Fslash/active_entitlements"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotLimit != "100" {
		t.Errorf("limit = %q, want 100", gotLimit)
	}
}
