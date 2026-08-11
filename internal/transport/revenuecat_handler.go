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

// RegisterUserRoutes mounts the client-facing refresh. This one must sit behind
// RequireAuth: it acts on the caller's own Firebase UID. Extra middleware is
// applied to this route only, which is where the rate limiter goes: every call
// costs a RevenueCat API request.
func (h *RevenueCatHandler) RegisterUserRoutes(r gin.IRouter, middleware ...gin.HandlerFunc) {
	handlers := append(append([]gin.HandlerFunc{}, middleware...), h.RefreshPremium)
	r.POST("/me/premium/refresh", handlers...)
}

// RefreshPremium re-reads the caller's entitlement state from RevenueCat and
// returns the updated user. The app calls it right after a purchase or a
// restore, which is also what recovers a webhook that never arrived.
//
// The UID comes from the verified Firebase token and never from the request
// body, so a caller can only ever refresh their own entitlement.
func (h *RevenueCatHandler) RefreshPremium(c *gin.Context) {
	current, ok := CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		return
	}

	user, err := h.service.RefreshPremium(c.Request.Context(), current.FirebaseUID)
	if err != nil {
		// 502, not 500: the failure is upstream and the client should retry.
		log.Printf("premium refresh for %s: %v", current.FirebaseUID, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not refresh entitlement"})

		return
	}

	c.JSON(http.StatusOK, gin.H{
		"user":       user,
		"is_premium": user.HasActivePremium(),
	})
}

// webhookPayload is RevenueCat's wire format.
type webhookPayload struct {
	APIVersion string `json:"api_version"`
	Event      struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		AppUserID string `json:"app_user_id"`
		// TRANSFER events omit app_user_id and name both ends of the move here
		// instead, so these must be parsed for a transfer to sync at all.
		TransferredFrom []string `json:"transferred_from"`
		TransferredTo   []string `json:"transferred_to"`
	} `json:"event"`
}

// syncedCustomer is what we report back per customer. RevenueCat shows the
// response body in its delivery log, which makes this the quickest way to see
// which side of a transfer ended up with the entitlement.
type syncedCustomer struct {
	FirebaseUID string `json:"firebase_uid"`
	IsPremium   bool   `json:"is_premium"`
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

	result, err := h.service.ProcessEvent(c.Request.Context(), service.RevenueCatEvent{
		ID:              payload.Event.ID,
		Type:            payload.Event.Type,
		AppUserID:       payload.Event.AppUserID,
		TransferredFrom: payload.Event.TransferredFrom,
		TransferredTo:   payload.Event.TransferredTo,
	})

	if result != nil {
		for _, warning := range result.Warnings {
			log.Printf("revenuecat webhook %s (%s): %s", payload.Event.ID, payload.Event.Type, warning)
		}
	}

	switch {
	case errors.Is(err, service.ErrDuplicateEvent):
		// A retry of something we already handled. Ack so RevenueCat stops.
		c.JSON(http.StatusOK, gin.H{"status": "duplicate"})
	case err != nil:
		// 502 tells RevenueCat to retry. Because the event was not recorded as
		// completed, the retry will be processed normally.
		log.Printf("revenuecat webhook %s (%s): %v", payload.Event.ID, payload.Event.Type, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "could not synchronize entitlement"})
	case len(result.Synced) == 0:
		// Test event, or an event naming no customer we could sync.
		c.JSON(http.StatusOK, describeResult("ignored", result))
	default:
		c.JSON(http.StatusOK, describeResult("synced", result))
	}
}

// describeResult builds the response body. RevenueCat shows it in the delivery
// log, which makes it the quickest place to see which side of a transfer ended
// up with the entitlement and which ids were skipped.
func describeResult(status string, result *service.SyncResult) gin.H {
	body := gin.H{"status": status}

	if len(result.Synced) > 0 {
		customers := make([]syncedCustomer, 0, len(result.Synced))
		for _, user := range result.Synced {
			customers = append(customers, syncedCustomer{
				FirebaseUID: user.FirebaseUID,
				IsPremium:   user.HasActivePremium(),
			})
		}
		body["customers"] = customers
	}

	if len(result.Warnings) > 0 {
		body["warnings"] = result.Warnings
	}

	return body
}

// authorized compares the shared secret in constant time so the response time
// does not leak how much of the secret was correct.
func (h *RevenueCatHandler) authorized(provided string) bool {
	return subtle.ConstantTimeCompare([]byte(provided), []byte(h.expectedAuthorization)) == 1
}
