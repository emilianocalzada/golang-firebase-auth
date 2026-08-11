package transport

import (
	"aislide/internal/auth"
	"aislide/internal/model"
	"aislide/internal/service"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// contextUserKey holds the *model.User for the current request.
const contextUserKey = "currentUser"

type AuthMiddleware struct {
	verifier auth.Verifier
	users    *service.UserService
}

func NewAuthMiddleware(v auth.Verifier, users *service.UserService) *AuthMiddleware {
	return &AuthMiddleware{
		verifier: v,
		users:    users,
	}
}

// RequireAuth verifies the Firebase ID token from the Authorization header and
// provisions the local user row, so any handler behind it can rely on
// CurrentUser being present.
func (m *AuthMiddleware) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		idToken := bearerToken(c.GetHeader("Authorization"))
		if idToken == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}

		token, err := m.verifier.VerifyIDToken(c.Request.Context(), idToken)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		user, err := m.users.EnsureUser(c.Request.Context(), token.UID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "could not load user"})
			return
		}

		c.Set(contextUserKey, user)
		c.Next()
	}
}

// RequirePremium must be chained after RequireAuth. It is the gate for
// presentation creation.
func (m *AuthMiddleware) RequirePremium() gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := CurrentUser(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}

		if !user.HasActivePremium() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "premium_required"})
			return
		}

		c.Next()
	}
}

// CurrentUser returns the authenticated user stored by RequireAuth.
func CurrentUser(c *gin.Context) (*model.User, bool) {
	value, exists := c.Get(contextUserKey)
	if !exists {
		return nil, false
	}

	user, ok := value.(*model.User)

	return user, ok
}

// bearerToken pulls the credential out of "Authorization: Bearer <token>".
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}

	return strings.TrimSpace(header[len(prefix):])
}
