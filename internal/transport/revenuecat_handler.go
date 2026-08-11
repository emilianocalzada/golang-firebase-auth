package transport

import (
	"aislide/internal/service"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// maxWebhookBody caps how much we read from an unauthenticated-by-Firebase
// endpoint. RevenueCat payloads are a few kilobytes.
const maxWebhookBody = 1 << 20 // 1 MiB

type RevenueCatHandler struct {
	service *service.RevenueCatService
	// expectedAuthorization is the full Authorization header value configured
	// in the RevenueCat dashboard, e.g. "Bearer <random secret>".
	expectedAuthorization string
}

func NewRevenueCatHandler(s *service.RevenueCatService, expectedAuthorization string) *RevenueCatHandler {
	return &RevenueCatHandler{
		service:               s,
		expectedAuthorization: expectedAuthorization,
	}
}

// RegisterRoutes mounts the webhook. It must NOT sit behind the Firebase auth
// middleware: RevenueCat's servers authenticate with the shared secret below,
// not with a user token.
func (h *RevenueCatHandler) RegisterRoutes(r gin.IRouter) {
	r.POST("/webhooks/revenuecat", h.HandleWebhook)
}

// webhookPayload is RevenueCat's wire format.
type webhookPayload struct {
	APIVersion string `json:"api_version"`
	Event      struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		AppUserID string `json:"app_user_id"`
	} `json:"event"`
}

func (h *RevenueCatHandler) HandleWebhook(c *gin.Context) {
	if !h.authorized(c.GetHeader("Authorization")) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook authorization"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBody)

	var payload webhookPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
		return
	}
	if payload.Event.ID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing event id"})
		return
	}

	user, err := h.service.ProcessEvent(c.Request.Context(), service.RevenueCatEvent{
		ID:        payload.Event.ID,
		Type:      payload.Event.Type,
		AppUserID: payload.Event.AppUserID,
	})

	switch {
	case errors.Is(err, service.ErrDuplicateEvent):
		// A retry of something we already handled. Ack so RevenueCat stops.
		c.JSON(http.StatusOK, gin.H{"status": "duplicate"})
	case err != nil:
		// 502 tells RevenueCat to retry; the event id was released so the
		// retry is processed instead of being skipped as a duplicate.
		log.Printf("revenuecat webhook %s (%s): %v", payload.Event.ID, payload.Event.Type, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not synchronize entitlement"})
	case user == nil:
		// Test event, or an event with no app user id to sync.
		c.JSON(http.StatusOK, gin.H{"status": "ignored"})
	default:
		c.JSON(http.StatusOK, gin.H{
			"status":     "synced",
			"is_premium": user.HasActivePremium(),
		})
	}
}

// authorized compares the shared secret in constant time so the response time
// does not leak how much of the secret was correct.
func (h *RevenueCatHandler) authorized(provided string) bool {
	return subtle.ConstantTimeCompare([]byte(provided), []byte(h.expectedAuthorization)) == 1
}
