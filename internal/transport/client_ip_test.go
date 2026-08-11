package transport_test

import (
	"aislide/internal/transport"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// The deployed topology: Cloudflare -> Traefik (Docker) -> this process. The
// peer is always Traefik's address on the Docker network, so that is what gets
// trusted, and CF-Connecting-IP is the only header believed.
const (
	traefikPeer = "172.18.0.4:54321"
	strangePeer = "203.0.113.9:54321"
	clientIP    = "198.51.100.23"
)

func clientIPFor(t *testing.T, engine *gin.Engine, peer string, headers map[string]string) string {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/ip", nil)
	req.RemoteAddr = peer
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	return w.Body.String()
}

func newIPEngine(t *testing.T, trustedProxies []string, header string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	if err := transport.ConfigureClientIP(engine, trustedProxies, header); err != nil {
		t.Fatalf("ConfigureClientIP: %v", err)
	}
	engine.GET("/ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})

	return engine
}

func TestClientIPFromCloudflareHeader(t *testing.T) {
	engine := newIPEngine(t, []string{"172.16.0.0/12"}, "CF-Connecting-IP")

	got := clientIPFor(t, engine, traefikPeer, map[string]string{
		"CF-Connecting-IP": clientIP,
		// Cloudflare appends the real client IP to whatever the caller sent, so
		// the front of this chain is attacker controlled. It must not be read.
		"X-Forwarded-For": "1.2.3.4, " + clientIP,
		"X-Real-IP":       "1.2.3.4",
	})

	if got != clientIP {
		t.Errorf("ClientIP = %q, want %q", got, clientIP)
	}
}

func TestClientIPIgnoresHeaderFromUntrustedPeer(t *testing.T) {
	engine := newIPEngine(t, []string{"172.16.0.0/12"}, "CF-Connecting-IP")

	// Somebody reaching the container from outside the Docker network, e.g. a
	// published port. Their header is worthless and must be ignored.
	got := clientIPFor(t, engine, strangePeer, map[string]string{
		"CF-Connecting-IP": clientIP,
	})

	if got != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want the socket peer 203.0.113.9", got)
	}
}

func TestClientIPIgnoresAllHeadersWhenUnconfigured(t *testing.T) {
	// The local-development default: no proxies, no header.
	engine := newIPEngine(t, nil, "")

	got := clientIPFor(t, engine, traefikPeer, map[string]string{
		"CF-Connecting-IP": clientIP,
		"X-Forwarded-For":  clientIP,
		"X-Real-IP":        clientIP,
	})

	if got != "172.18.0.4" {
		t.Errorf("ClientIP = %q, want the socket peer 172.18.0.4", got)
	}
}

func TestConfigureClientIPRejectsBadCIDR(t *testing.T) {
	gin.SetMode(gin.TestMode)

	if err := transport.ConfigureClientIP(gin.New(), []string{"nonsense"}, "CF-Connecting-IP"); err == nil {
		t.Error("ConfigureClientIP should reject an unparseable proxy entry")
	}
}
