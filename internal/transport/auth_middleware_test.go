package transport_test

import (
	"aislide/internal/auth"
	"aislide/internal/service"
	"aislide/internal/store"
	"aislide/internal/transport"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
)

// fakeVerifier accepts one known token so tests never touch Firebase.
type fakeVerifier struct {
	validToken string
	uid        string
}

func (f *fakeVerifier) VerifyIDToken(_ context.Context, idToken string) (*auth.Token, error) {
	if idToken != f.validToken {
		return nil, auth.ErrInvalidToken
	}

	return &auth.Token{UID: f.uid, SignInProvider: "anonymous"}, nil
}

func newTestServer(t *testing.T) (*gin.Engine, *service.UserService) {
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
	middleware := transport.NewAuthMiddleware(&fakeVerifier{validToken: "good-token", uid: "uid-123"}, users)

	r := gin.New()
	v1 := r.Group("/v1", middleware.RequireAuth())
	transport.NewUserHandler(users).RegisterRoutes(v1)

	// Stand-in for the future POST /v1/presentations route.
	v1.POST("/premium-only", middleware.RequirePremium(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	return r, users
}

func do(t *testing.T, r *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

func TestRequireAuthRejectsMissingAndBadTokens(t *testing.T) {
	r, _ := newTestServer(t)

	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"not bearer", "Basic abc"},
		{"empty bearer", "Bearer "},
		{"wrong token", "Bearer nope"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, r, http.MethodGet, "/v1/me", tc.header)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (body: %s)", w.Code, w.Body.String())
			}
		})
	}
}

func TestRequireAuthProvisionsUserOnFirstCall(t *testing.T) {
	r, users := newTestServer(t)

	if _, err := users.GetUser(context.Background(), "uid-123"); err == nil {
		t.Fatal("user should not exist before the first request")
	}

	w := do(t, r, http.MethodGet, "/v1/me", "Bearer good-token")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	var body struct {
		User struct {
			FirebaseUID string `json:"firebase_uid"`
			IsPremium   bool   `json:"is_premium"`
		} `json:"user"`
		IsPremium bool `json:"is_premium"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.User.FirebaseUID != "uid-123" {
		t.Errorf("firebase_uid = %q, want uid-123", body.User.FirebaseUID)
	}
	if body.IsPremium {
		t.Error("fresh anonymous user should not be premium")
	}

	// The row now exists, and a lowercase scheme still authenticates.
	if _, err := users.GetUser(context.Background(), "uid-123"); err != nil {
		t.Fatalf("user should have been provisioned: %v", err)
	}
	if w := do(t, r, http.MethodGet, "/v1/me", "bearer good-token"); w.Code != http.StatusOK {
		t.Errorf("lowercase scheme status = %d, want 200", w.Code)
	}
}

func TestRequirePremiumGatesFreeUsers(t *testing.T) {
	r, users := newTestServer(t)
	ctx := context.Background()

	w := do(t, r, http.MethodPost, "/v1/premium-only", "Bearer good-token")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != `{"error":"premium_required"}` {
		t.Errorf("body = %s, want premium_required", body)
	}

	until := time.Now().Add(24 * time.Hour)
	if _, err := users.GrantPremium(ctx, "uid-123", &until); err != nil {
		t.Fatalf("GrantPremium: %v", err)
	}

	if w := do(t, r, http.MethodPost, "/v1/premium-only", "Bearer good-token"); w.Code != http.StatusOK {
		t.Errorf("premium user status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}

	// An expired entitlement is treated as free.
	expired := time.Now().Add(-time.Hour)
	if _, err := users.GrantPremium(ctx, "uid-123", &expired); err != nil {
		t.Fatalf("GrantPremium expired: %v", err)
	}
	if w := do(t, r, http.MethodPost, "/v1/premium-only", "Bearer good-token"); w.Code != http.StatusForbidden {
		t.Errorf("expired premium status = %d, want 403", w.Code)
	}
}

func TestGrantPremiumProvisionsUnknownUser(t *testing.T) {
	_, users := newTestServer(t)

	// The RevenueCat webhook can arrive before the user ever calls the API.
	user, err := users.GrantPremium(context.Background(), "uid-from-webhook", nil)
	if err != nil {
		t.Fatalf("GrantPremium: %v", err)
	}
	if !user.HasActivePremium() {
		t.Error("webhook user should be premium")
	}
}
