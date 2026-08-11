package revenuecat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	defaultBaseURL = "https://api.revenuecat.com/v2"
	// entitlementsPerPage is the maximum the API allows; values above it are
	// clamped. One request covers any realistic number of entitlements.
	entitlementsPerPage = 100
	// maxPages stops us from following a pagination cursor forever.
	maxPages = 20
)

// Client reads the authoritative entitlement state from the RevenueCat v2 API.
// The webhook only says something changed; this says what is true.
//
// Requires a v2 secret API key with the customer_information:customers:read
// scope. v1 keys do not work against v2.
type Client struct {
	secretAPIKey  string
	projectID     string
	entitlementID string
	baseURL       string
	httpClient    *http.Client
}

func New(secretAPIKey, projectID, entitlementID string) *Client {
	return &Client{
		secretAPIKey:  secretAPIKey,
		projectID:     projectID,
		entitlementID: entitlementID,
		baseURL:       defaultBaseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// WithBaseURL points the client at another host. Used by tests.
func (c *Client) WithBaseURL(baseURL string) *Client {
	c.baseURL = baseURL

	return c
}

// activeEntitlementsResponse is the CustomerActiveEntitlementList schema.
type activeEntitlementsResponse struct {
	Items []struct {
		EntitlementID string `json:"entitlement_id"`
		// ExpiresAt is ms since epoch, null for a non-expiring entitlement.
		ExpiresAt *int64 `json:"expires_at"`
	} `json:"items"`
	NextPage *string `json:"next_page"`
}

// PremiumStatus reports whether appUserID currently owns the premium
// entitlement. appUserID is the Firebase UID, which is what the mobile app
// configures as the RevenueCat app user id.
//
// A nil expiresAt with active true means a non-expiring (lifetime) purchase.
//
// The endpoint only returns entitlements RevenueCat considers active, so
// presence is the signal. We still re-check the expiry date locally: if it has
// passed, the entitlement is reported inactive so a missed webhook cannot leave
// premium switched on forever.
func (c *Client) PremiumStatus(ctx context.Context, appUserID string) (bool, *time.Time, error) {
	startingAfter := ""

	for page := 0; page < maxPages; page++ {
		payload, err := c.fetchActiveEntitlements(ctx, appUserID, startingAfter)
		if err != nil {
			return false, nil, err
		}

		for _, item := range payload.Items {
			if item.EntitlementID != c.entitlementID {
				continue
			}

			if item.ExpiresAt == nil {
				return true, nil, nil
			}

			expiresAt := time.UnixMilli(*item.ExpiresAt).UTC()

			return expiresAt.After(time.Now()), &expiresAt, nil
		}

		startingAfter = nextPageCursor(payload.NextPage)
		if startingAfter == "" {
			break
		}
	}

	return false, nil, nil
}

func (c *Client) fetchActiveEntitlements(ctx context.Context, appUserID, startingAfter string) (*activeEntitlementsResponse, error) {
	endpoint := fmt.Sprintf("%s/projects/%s/customers/%s/active_entitlements",
		c.baseURL,
		url.PathEscape(c.projectID),
		url.PathEscape(appUserID),
	)

	query := url.Values{}
	query.Set("limit", fmt.Sprint(entitlementsPerPage))
	if startingAfter != "" {
		query.Set("starting_after", startingAfter)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretAPIKey)
	req.Header.Set("Accept", "application/json")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call revenuecat: %w", err)
	}
	defer res.Body.Close()

	// Every non-200 is an error, including 404. A webhook always names a
	// customer RevenueCat knows, so a 404 here means the project id is wrong
	// rather than "no entitlement" - failing loudly beats silently downgrading
	// paying customers to free.
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("revenuecat returned status %d%s", res.StatusCode, describeError(res.Body))
	}

	var payload activeEntitlementsResponse
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode revenuecat response: %w", err)
	}

	return &payload, nil
}

// describeError pulls RevenueCat's structured error out of the body so the log
// line says what went wrong, e.g. "authentication_error: Invalid API key.".
func describeError(body io.Reader) string {
	var apiError struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}

	if err := json.NewDecoder(io.LimitReader(body, 4096)).Decode(&apiError); err != nil {
		return ""
	}
	if apiError.Type == "" && apiError.Message == "" {
		return ""
	}

	return fmt.Sprintf(" (%s: %s)", apiError.Type, apiError.Message)
}

// nextPageCursor takes the starting_after value out of the next_page URL.
// Reusing the cursor instead of following the returned URL keeps us from
// issuing requests to whatever host that field happens to contain.
func nextPageCursor(nextPage *string) string {
	if nextPage == nil || *nextPage == "" {
		return ""
	}

	parsed, err := url.Parse(*nextPage)
	if err != nil {
		return ""
	}

	return parsed.Query().Get("starting_after")
}
