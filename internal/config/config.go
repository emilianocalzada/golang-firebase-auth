package config

import (
	"errors"
	"fmt"
	"os"

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
//	FIREBASE_CREDENTIALS_FILE  Service account JSON path        (optional, falls
//	                           back to Application Default Credentials)
//	REVENUECAT_SECRET_API_KEY  RevenueCat v2 secret API key     (required)
//	REVENUECAT_PROJECT_ID      RevenueCat project id            (required)
//	REVENUECAT_WEBHOOK_AUTH    Full Authorization header value  (required)
//	                           configured in the RevenueCat dashboard
//	REVENUECAT_ENTITLEMENT_ID  Entitlement to gate on           (default premium)
type Config struct {
	Port                    string
	DatabasePath            string
	FirebaseProjectID       string
	FirebaseCredentialsFile string
	RevenueCatSecretAPIKey  string
	RevenueCatProjectID     string
	RevenueCatWebhookAuth   string
	RevenueCatEntitlementID string
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
	}

	if cfg.FirebaseProjectID == "" {
		return Config{}, fmt.Errorf("FIREBASE_PROJECT_ID is required (set it in .env or the environment)")
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

	return cfg, nil
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
