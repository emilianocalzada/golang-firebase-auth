package transport

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// defaultIdleTTL is how long an untouched bucket is kept when the config does
// not say. Long enough that a user cannot reset their budget by pausing,
// short enough that churned anonymous UIDs do not accumulate.
const defaultIdleTTL = 10 * time.Minute

// maxRetryAfterSeconds bounds the Retry-After we advertise, so a
// misconfigured bucket cannot tell a client to come back in a year.
const maxRetryAfterSeconds = 3600

// RateLimitConfig describes one token bucket, applied per authenticated user.
type RateLimitConfig struct {
	// Rate is the sustained refill rate. rate.Every reads better than a raw
	// float for the slow buckets: rate.Every(10*time.Second) is one request
	// every ten seconds.
	Rate rate.Limit
	// Burst is how many requests may arrive back to back before throttling
	// starts. Values below 1 are clamped to 1, since a bucket that never
	// admits anything would take the endpoint offline.
	Burst int
	// IdleTTL is how long a user's bucket survives after their last request.
	// It is raised to the time a full refill takes when that is longer, so
	// eviction can never hand back a budget the user had already spent.
	IdleTTL time.Duration
}

// UserRateLimiter keeps one token bucket per Firebase UID.
//
// Per user rather than one bucket for the whole endpoint: a single shared
// limiter lets one noisy client consume the allowance and 429 everybody else,
// which turns a rate limiter into a denial of service vector.
//
// Buckets live in memory, so each process limits independently. That is the
// right tradeoff while this runs as a single instance; behind more than one
// replica the effective limit multiplies by the replica count, and a shared
// store (Redis) is the way to make it exact.
type UserRateLimiter struct {
	rate  rate.Limit
	burst int
	ttl   time.Duration

	mu        sync.Mutex
	buckets   map[string]*userBucket
	lastSweep time.Time
}

type userBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewUserRateLimiter(cfg RateLimitConfig) *UserRateLimiter {
	burst := cfg.Burst
	if burst < 1 {
		burst = 1
	}

	ttl := cfg.IdleTTL
	if ttl <= 0 {
		ttl = defaultIdleTTL
	}
	if refill := fullRefill(cfg.Rate, burst); refill > ttl {
		ttl = refill
	}

	return &UserRateLimiter{
		rate:      cfg.Rate,
		burst:     burst,
		ttl:       ttl,
		buckets:   make(map[string]*userBucket),
		lastSweep: time.Now(),
	}
}

// Middleware limits the caller identified by RequireAuth. It must be chained
// after RequireAuth: the key is the verified Firebase UID, never anything the
// client sends in the request.
func (l *UserRateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := CurrentUser(c)
		if !ok {
			// Only reachable if this is mounted without RequireAuth in front.
			// Fail closed instead of letting the request through unlimited.
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}

		retryAfter, allowed := l.allow(user.FirebaseUID, time.Now())
		if !allowed {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":       "rate_limited",
				"retry_after": retryAfter,
			})

			return
		}

		c.Next()
	}
}

// Tracked reports how many buckets are currently held. Useful as a gauge, and
// it is what the eviction test asserts on.
func (l *UserRateLimiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.buckets)
}

// allow consumes one token for key and, when it refuses, reports how many
// seconds until the next one is available.
func (l *UserRateLimiter) allow(key string, now time.Time) (retryAfterSeconds int, allowed bool) {
	l.mu.Lock()
	bucket, exists := l.buckets[key]
	if !exists {
		bucket = &userBucket{limiter: rate.NewLimiter(l.rate, l.burst)}
		l.buckets[key] = bucket
	}
	bucket.lastSeen = now
	l.sweep(now)
	l.mu.Unlock()

	if bucket.limiter.AllowN(now, 1) {
		return 0, true
	}

	// Rejected. Reserve only to read when the next token lands, then cancel it
	// straight away: this request is refused outright, never queued, so the
	// token must stay in the bucket for whoever asks next.
	reservation := bucket.limiter.ReserveN(now, 1)
	delay := reservation.DelayFrom(now)
	reservation.CancelAt(now)

	if !reservation.OK() {
		return maxRetryAfterSeconds, false
	}

	return clampRetryAfter(delay), false
}

// sweep drops buckets nobody has touched in a full TTL. Without it the map
// grows for every UID ever seen, and Firebase anonymous sign-in makes fresh
// UIDs free to mint.
//
// Called with l.mu held, and at most once per TTL so a burst of requests does
// not walk the whole map on every call.
func (l *UserRateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.ttl {
		return
	}
	l.lastSweep = now

	for key, bucket := range l.buckets {
		if now.Sub(bucket.lastSeen) >= l.ttl {
			delete(l.buckets, key)
		}
	}
}

// fullRefill is how long an empty bucket takes to return to full.
func fullRefill(limit rate.Limit, burst int) time.Duration {
	if limit <= 0 || math.IsInf(float64(limit), 1) {
		return 0
	}

	return time.Duration(float64(burst) / float64(limit) * float64(time.Second))
}

func clampRetryAfter(delay time.Duration) int {
	seconds := int(math.Ceil(delay.Seconds()))
	if seconds < 1 {
		// The client must wait, so never advertise 0 and invite an instant retry.
		return 1
	}
	if seconds > maxRetryAfterSeconds {
		return maxRetryAfterSeconds
	}

	return seconds
}
