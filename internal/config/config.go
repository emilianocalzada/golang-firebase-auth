package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// minWebhookSecretLength guards against a shared secret short enough to guess.
const minWebhookSecretLength = 16

// Config holds everything the server needs to boot.
//
// Values come from the real environment first, then from the .env file for
// anything still unset. Set ENV_FILE to load a different file.
//
//	PORT                       HTTP port                        (default 8000)
//	DATABASE_PATH              SQLite file                      (default ./aislide.db)
//	FIREBASE_PROJECT_ID        Firebase project id              (required)
//	FIREBASE_CREDENTIALS_FILE  Service account JSON path        (optional)
//	FIREBASE_CREDENTIALS_JSON  Service account JSON inline,     (optional)
//	                           raw or base64. Use this instead
//	                           of the file on hosts whose only
//	                           secret store is the environment.
//	                           With neither, Application Default
//	                           Credentials are used.
//	REVENUECAT_SECRET_API_KEY  RevenueCat v2 secret API key     (required)
//	REVENUECAT_PROJECT_ID      RevenueCat project id            (required)
//	REVENUECAT_WEBHOOK_AUTH    Full Authorization header value  (required)
//	                           configured in the RevenueCat dashboard
//	REVENUECAT_ENTITLEMENT_ID  Entitlement to gate on           (default premium)
//	TRUSTED_PROXIES            Comma-separated CIDRs/IPs the    (default none)
//	                           reverse proxy connects from
//	CLIENT_IP_HEADER           Header holding the real client    (default none)
//	                           IP, e.g. CF-Connecting-IP
type Config struct {
	Port                    string
	DatabasePath            string
	FirebaseProjectID       string
	FirebaseCredentialsFile string
	// FirebaseCredentialsJSON is the service account document itself, for
	// deployments that can only inject secrets as environment variables. Never
	// log it: it holds a private key.
	FirebaseCredentialsJSON []byte
	RevenueCatSecretAPIKey  string
	RevenueCatProjectID     string
	RevenueCatWebhookAuth   string
	RevenueCatEntitlementID string
	// TrustedProxies are the addresses a forwarded-client-IP header is
	// believed from. Empty means trust nobody, so ClientIP is the socket peer:
	// wrong behind a proxy, but never spoofable, which is the right default for
	// local development.
	TrustedProxies []string
	// ClientIPHeader is the single header carrying the original client IP.
	// Behind Cloudflare that is CF-Connecting-IP: Cloudflare sets it on every
	// request it proxies and it holds exactly one address, unlike
	// X-Forwarded-For whose leading entries are whatever the client sent.
	// Empty means no header is trusted at all.
	ClientIPHeader string
}

func Load() (Config, error) {
	if err := loadEnvFile(); err != nil {
		return Config{}, err
	}

	cfg := Config{
		Port:                    env("PORT", "8000"),
		DatabasePath:            env("DATABASE_PATH", "./aislide.db"),
		FirebaseProjectID:       os.Getenv("FIREBASE_PROJECT_ID"),
		FirebaseCredentialsFile: os.Getenv("FIREBASE_CREDENTIALS_FILE"),
		RevenueCatSecretAPIKey:  os.Getenv("REVENUECAT_SECRET_API_KEY"),
		RevenueCatProjectID:     os.Getenv("REVENUECAT_PROJECT_ID"),
		RevenueCatWebhookAuth:   os.Getenv("REVENUECAT_WEBHOOK_AUTH"),
		RevenueCatEntitlementID: env("REVENUECAT_ENTITLEMENT_ID", "premium"),
		ClientIPHeader:          strings.TrimSpace(os.Getenv("CLIENT_IP_HEADER")),
	}

	if cfg.FirebaseProjectID == "" {
		return Config{}, fmt.Errorf("FIREBASE_PROJECT_ID is required (set it in .env or the environment)")
	}

	credentialsJSON, err := parseCredentialsJSON(os.Getenv("FIREBASE_CREDENTIALS_JSON"))
	if err != nil {
		return Config{}, err
	}
	cfg.FirebaseCredentialsJSON = credentialsJSON

	// Both set is ambiguous, and picking one silently is how you end up
	// authenticating as a project you did not mean to.
	if len(cfg.FirebaseCredentialsJSON) > 0 && cfg.FirebaseCredentialsFile != "" {
		return Config{}, fmt.Errorf("set FIREBASE_CREDENTIALS_FILE or FIREBASE_CREDENTIALS_JSON, not both")
	}

	if cfg.FirebaseCredentialsFile != "" {
		if _, err := os.Stat(cfg.FirebaseCredentialsFile); err != nil {
			return Config{}, fmt.Errorf("FIREBASE_CREDENTIALS_FILE %q is not readable: %w", cfg.FirebaseCredentialsFile, err)
		}
	}

	if cfg.RevenueCatSecretAPIKey == "" {
		return Config{}, fmt.Errorf("REVENUECAT_SECRET_API_KEY is required")
	}

	// The v2 API is scoped per project: /v2/projects/{project_id}/...
	if cfg.RevenueCatProjectID == "" {
		return Config{}, fmt.Errorf("REVENUECAT_PROJECT_ID is required")
	}

	// An empty or short secret would leave the webhook effectively open, so
	// this is a hard failure rather than a warning.
	if len(cfg.RevenueCatWebhookAuth) < minWebhookSecretLength {
		return Config{}, fmt.Errorf("REVENUECAT_WEBHOOK_AUTH is required and must be at least %d characters", minWebhookSecretLength)
	}

	trustedProxies, err := parseTrustedProxies(os.Getenv("TRUSTED_PROXIES"))
	if err != nil {
		return Config{}, err
	}
	cfg.TrustedProxies = trustedProxies

	// A header without a proxy list would never be read, which looks like it
	// works while every rate limit and log line records the wrong IP.
	if cfg.ClientIPHeader != "" && len(cfg.TrustedProxies) == 0 {
		return Config{}, fmt.Errorf("CLIENT_IP_HEADER is set but TRUSTED_PROXIES is empty: the header would be ignored")
	}

	return cfg, nil
}

// parseCredentialsJSON accepts the service account document either raw or
// base64 encoded, because pasting a multi-line JSON blob with an embedded
// private key into a dashboard field mangles it in ways that are hard to see.
// Base64 is one line and survives every UI.
//
// It is validated here so a bad paste fails at boot with a clear message
// instead of an opaque SDK error on the first request. No error mentions the
// value: it contains a private key.
func parseCredentialsJSON(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	document := []byte(raw)
	if !strings.HasPrefix(raw, "{") {
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("FIREBASE_CREDENTIALS_JSON is neither JSON (it must start with {) nor valid base64")
		}
		document = decoded
	}

	var parsed struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		PrivateKey  string `json:"private_key"`
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal(document, &parsed); err != nil {
		return nil, fmt.Errorf("FIREBASE_CREDENTIALS_JSON is not valid JSON: check for a truncated paste")
	}

	// The SDK only accepts a service account, so reject anything else here
	// where the message can say why.
	if parsed.Type != "service_account" {
		return nil, fmt.Errorf("FIREBASE_CREDENTIALS_JSON has type %q, want service_account", parsed.Type)
	}
	if parsed.PrivateKey == "" || parsed.ClientEmail == "" || parsed.ProjectID == "" {
		return nil, fmt.Errorf("FIREBASE_CREDENTIALS_JSON is missing project_id, private_key or client_email")
	}

	// A literal "\n" instead of a newline means the escaping was eaten
	// somewhere, which the SDK reports only as a PEM parse failure.
	if !strings.Contains(parsed.PrivateKey, "\n") {
		return nil, fmt.Errorf("FIREBASE_CREDENTIALS_JSON private_key has no line breaks: the \\n escapes were lost, use the base64 form instead")
	}

	return document, nil
}

// parseTrustedProxies validates the CIDR list at boot, so a typo fails loudly
// here instead of silently making every client look like the proxy.
func parseTrustedProxies(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	var proxies []string

	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// Trusting everything means anyone who reaches the origin directly can
		// claim any client IP, which is worse than not trusting the header at
		// all because it looks like it is working.
		if entry == "0.0.0.0/0" || entry == "::/0" {
			return nil, fmt.Errorf("TRUSTED_PROXIES must not contain %s: that trusts a forwarded IP from any source", entry)
		}

		if err := validateProxyEntry(entry); err != nil {
			return nil, err
		}

		proxies = append(proxies, entry)
	}

	return proxies, nil
}

// validateProxyEntry accepts the same forms gin does: a bare IP or a CIDR.
func validateProxyEntry(entry string) error {
	if strings.Contains(entry, "/") {
		if _, _, err := net.ParseCIDR(entry); err != nil {
			return fmt.Errorf("TRUSTED_PROXIES entry %q is not a valid CIDR: %w", entry, err)
		}

		return nil
	}

	if net.ParseIP(entry) == nil {
		return fmt.Errorf("TRUSTED_PROXIES entry %q is not a valid IP address or CIDR", entry)
	}

	return nil
}

// loadEnvFile reads the .env file for local development. A missing file is
// fine (deployed environments inject real env vars), but a malformed one is
// an error so we never boot with half the config.
//
// Values already present in the environment win. An exported but empty
// variable counts as unset, otherwise a stray `export PORT=` in a shell would
// silently beat the .env file.
func loadEnvFile() error {
	path := env("ENV_FILE", ".env")

	values, err := godotenv.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}

	for key, value := range values {
		if os.Getenv(key) != "" {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}

	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}
