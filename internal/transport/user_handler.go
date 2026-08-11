package transport

import (
	"aislide/internal/service"
	"net/http"

	"github.com/gin-gonic/gin"
)

type UserHandler struct {
	service *service.UserService
}

func NewUserHandler(s *service.UserService) *UserHandler {
	return &UserHandler{
		service: s,
	}
}

// RegisterRoutes mounts the current-user endpoints. The caller decides which
// group they live on, so the auth middleware is applied in one place.
func (h *UserHandler) RegisterRoutes(r gin.IRouter) {
	r.GET("/me", h.GetMe)
}

// GetMe is what the Expo app calls right after Firebase anonymous sign-in:
// it confirms the token works and returns the premium state to drive the UI.
func (h *UserHandler) GetMe(c *gin.Context) {
	user, ok := CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user":       user,
		"is_premium": user.HasActivePremium(),
	})
}
