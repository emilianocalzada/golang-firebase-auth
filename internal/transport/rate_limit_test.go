package transport

import (
	"aislide/internal/model"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// In-package test: the limiter keys on the user RequireAuth puts on the
// context, and fakeAuth below sets it directly rather than standing up
// Firebase. Behaviour of the real chain is covered in auth_middleware_test.go.

// fakeAuth stands in for RequireAuth: the X-Test-Uid header becomes the
// authenticated user, so tests can act as several users against one limiter.
func fakeAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		uid := c.GetHeader("X-Test-Uid")
		if uid == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}

		c.Set(contextUserKey, &model.User{FirebaseUID: uid})
		c.Next()
	}
}

// newLimitedServer mounts the limiter the way main.go does: behind auth, so it
// keys on a verified UID.
func newLimitedServer(t *testing.T, cfg RateLimitConfig) (*gin.Engine, *UserRateLimiter) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	limiter := NewUserRateLimiter(cfg)

	engine := gin.New()
	v1 := engine.Group("/v1", fakeAuth(), limiter.Middleware())
	v1.GET("/thing", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	return engine, limiter
}

func callAs(t *testing.T, r *gin.Engine, uid string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/v1/thing", nil)
	req.Header.Set("X-Test-Uid", uid)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	return w
}

func TestRateLimiterAllowsBurstThenRejects(t *testing.T) {
	r, _ := newLimitedServer(t, RateLimitConfig{
		Rate:  rate.Every(time.Hour), // No meaningful refill during the test.
		Burst: 3,
	})

	for i := 1; i <= 3; i++ {
		if w := callAs(t, r, "uid-a"); w.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (body: %s)", i, w.Code, w.Body.String())
		}
	}

	w := callAs(t, r, "uid-a")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body: %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got == "" || got == "0" {
		t.Errorf("Retry-After = %q, want a positive number of seconds", got)
	}
}

func TestRateLimiterIsPerUser(t *testing.T) {
	r, _ := newLimitedServer(t, RateLimitConfig{
		Rate:  rate.Every(time.Hour),
		Burst: 1,
	})

	// uid-a spends its whole budget.
	if w := callAs(t, r, "uid-a"); w.Code != http.StatusOK {
		t.Fatalf("first call status = %d, want 200", w.Code)
	}
	if w := callAs(t, r, "uid-a"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d, want 429", w.Code)
	}

	// uid-b must be unaffected, which is the whole point of keying per user.
	if w := callAs(t, r, "uid-b"); w.Code != http.StatusOK {
		t.Errorf("other user status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

func TestRateLimiterRefills(t *testing.T) {
	r, _ := newLimitedServer(t, RateLimitConfig{
		Rate:  rate.Every(20 * time.Millisecond),
		Burst: 1,
	})

	if w := callAs(t, r, "uid-a"); w.Code != http.StatusOK {
		t.Fatalf("first call status = %d, want 200", w.Code)
	}
	if w := callAs(t, r, "uid-a"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("immediate retry status = %d, want 429", w.Code)
	}

	time.Sleep(50 * time.Millisecond)

	if w := callAs(t, r, "uid-a"); w.Code != http.StatusOK {
		t.Errorf("after refill status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
}

func TestRateLimiterEvictsIdleUsers(t *testing.T) {
	// A fast bucket so the TTL floor (one full refill) stays tiny.
	r, limiter := newLimitedServer(t, RateLimitConfig{
		Rate:    rate.Every(10 * time.Millisecond),
		Burst:   1,
		IdleTTL: 20 * time.Millisecond,
	})

	for _, uid := range []string{"uid-a", "uid-b", "uid-c"} {
		if w := callAs(t, r, uid); w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", uid, w.Code)
		}
	}
	if got := limiter.Tracked(); got != 3 {
		t.Fatalf("tracked = %d, want 3", got)
	}

	// Past the TTL, the next request sweeps the idle buckets and keeps only
	// its own. Without this the map grows for every UID ever seen.
	time.Sleep(40 * time.Millisecond)
	if w := callAs(t, r, "uid-d"); w.Code != http.StatusOK {
		t.Fatalf("uid-d status = %d, want 200", w.Code)
	}
	if got := limiter.Tracked(); got != 1 {
		t.Errorf("tracked after sweep = %d, want 1", got)
	}
}

func TestRateLimiterIdleTTLNeverShorterThanARefill(t *testing.T) {
	// Asking for a 1s TTL on a bucket that takes 50s to refill would let a
	// user reset a spent budget by pausing for a second.
	limiter := NewUserRateLimiter(RateLimitConfig{
		Rate:    rate.Every(10 * time.Second),
		Burst:   5,
		IdleTTL: time.Second,
	})

	if limiter.ttl != 50*time.Second {
		t.Errorf("ttl = %s, want 50s (one full refill)", limiter.ttl)
	}
}

func TestRateLimiterFailsClosedWithoutAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	limiter := NewUserRateLimiter(RateLimitConfig{Rate: 1, Burst: 1})

	// Mounted without RequireAuth in front, which is a wiring mistake. The
	// request must be refused rather than pass through unlimited.
	engine := gin.New()
	engine.GET("/unguarded", limiter.Middleware(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/unguarded", nil)
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (body: %s)", w.Code, w.Body.String())
	}
}
