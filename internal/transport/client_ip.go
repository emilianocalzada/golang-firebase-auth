package transport

import (
	"fmt"

	"github.com/gin-gonic/gin"
)

// ConfigureClientIP decides what c.ClientIP() is allowed to believe.
//
// gin's default is to trust every proxy and read X-Forwarded-For, which lets
// any caller name their own IP in the access log and in anything keyed on it.
// This replaces that with an explicit pair: a header is read only when the
// request arrives from one of trustedProxies, and only that one header is read.
//
// Behind Cloudflare the header to use is CF-Connecting-IP. Cloudflare sets it
// on every request it proxies and it always holds exactly one address, whereas
// X-Forwarded-For has the caller's own value in front of the real client IP.
// Traefik discards inbound X-Forwarded-* from untrusted peers anyway, so by the
// time a request reaches us that header holds only the Cloudflare edge IP.
//
// Passing an empty header, or no proxies, leaves ClientIP as the socket peer:
// wrong behind a proxy, never spoofable, and the right default for a local run.
//
// None of this is worth anything unless the origin can only be reached through
// the proxy. Anyone able to open a connection to the container directly can set
// the header themselves, so the network path is what makes the value true.
func ConfigureClientIP(engine *gin.Engine, trustedProxies []string, header string) error {
	if err := engine.SetTrustedProxies(trustedProxies); err != nil {
		return fmt.Errorf("trusted proxies: %w", err)
	}

	// nil, not an empty slice: gin skips header inspection entirely.
	engine.RemoteIPHeaders = nil
	if header != "" {
		engine.RemoteIPHeaders = []string{header}
	}

	return nil
}
