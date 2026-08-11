package config_test

import (
	"aislide/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// minimalEnv is everything Load requires, so each test only states what it cares about.
const minimalEnv = "FIREBASE_PROJECT_ID=proj\n" +
	"REVENUECAT_SECRET_API_KEY=sk_test\n" +
	"REVENUECAT_PROJECT_ID=proj1ab2c3d4\n" +
	"REVENUECAT_WEBHOOK_AUTH=Bearer test-secret-value-long-enough\n"

// writeEnvFile drops a .env in a temp dir and points ENV_FILE at it.
func writeEnvFile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	t.Setenv("ENV_FILE", path)

	return path
}

// clearEnv keeps a stray shell environment from leaking into the assertions.
func clearEnv(t *testing.T) {
	t.Helper()

	keys := []string{
		"PORT", "DATABASE_PATH",
		"FIREBASE_PROJECT_ID", "FIREBASE_CREDENTIALS_FILE",
		"REVENUECAT_SECRET_API_KEY", "REVENUECAT_PROJECT_ID",
		"REVENUECAT_WEBHOOK_AUTH", "REVENUECAT_ENTITLEMENT_ID",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}
}

func TestLoadReadsEnvFile(t *testing.T) {
	clearEnv(t)
	credentials := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(credentials, []byte("{}"), 0600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	writeEnvFile(t, "PORT=9100\n"+
		"DATABASE_PATH=/tmp/from-env.db\n"+
		"FIREBASE_PROJECT_ID=proj-from-file\n"+
		"FIREBASE_CREDENTIALS_FILE="+credentials+"\n"+
		"REVENUECAT_SECRET_API_KEY=sk_from_file\n"+
		"REVENUECAT_PROJECT_ID=proj_from_file\n"+
		"REVENUECAT_WEBHOOK_AUTH=Bearer secret-from-file-long\n"+
		"REVENUECAT_ENTITLEMENT_ID=pro\n")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "9100" {
		t.Errorf("Port = %q, want 9100", cfg.Port)
	}
	if cfg.DatabasePath != "/tmp/from-env.db" {
		t.Errorf("DatabasePath = %q", cfg.DatabasePath)
	}
	if cfg.FirebaseProjectID != "proj-from-file" {
		t.Errorf("FirebaseProjectID = %q", cfg.FirebaseProjectID)
	}
	if cfg.FirebaseCredentialsFile != credentials {
		t.Errorf("FirebaseCredentialsFile = %q", cfg.FirebaseCredentialsFile)
	}
	if cfg.RevenueCatSecretAPIKey != "sk_from_file" {
		t.Errorf("RevenueCatSecretAPIKey = %q", cfg.RevenueCatSecretAPIKey)
	}
	if cfg.RevenueCatProjectID != "proj_from_file" {
		t.Errorf("RevenueCatProjectID = %q", cfg.RevenueCatProjectID)
	}
	if cfg.RevenueCatWebhookAuth != "Bearer secret-from-file-long" {
		t.Errorf("RevenueCatWebhookAuth = %q", cfg.RevenueCatWebhookAuth)
	}
	if cfg.RevenueCatEntitlementID != "pro" {
		t.Errorf("RevenueCatEntitlementID = %q, want pro", cfg.RevenueCatEntitlementID)
	}
}

func TestRealEnvironmentWinsOverEnvFile(t *testing.T) {
	clearEnv(t)
	writeEnvFile(t, minimalEnv+"PORT=9100\n")
	t.Setenv("PORT", "7000")
	t.Setenv("FIREBASE_PROJECT_ID", "proj-from-shell")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "7000" {
		t.Errorf("Port = %q, want the shell value 7000", cfg.Port)
	}
	if cfg.FirebaseProjectID != "proj-from-shell" {
		t.Errorf("FirebaseProjectID = %q, want proj-from-shell", cfg.FirebaseProjectID)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	clearEnv(t)
	writeEnvFile(t, minimalEnv)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "8000" {
		t.Errorf("Port = %q, want default 8000", cfg.Port)
	}
	if cfg.DatabasePath != "./aislide.db" {
		t.Errorf("DatabasePath = %q, want default ./aislide.db", cfg.DatabasePath)
	}
	if cfg.FirebaseCredentialsFile != "" {
		t.Errorf("FirebaseCredentialsFile = %q, want empty (ADC)", cfg.FirebaseCredentialsFile)
	}
	if cfg.RevenueCatEntitlementID != "premium" {
		t.Errorf("RevenueCatEntitlementID = %q, want default premium", cfg.RevenueCatEntitlementID)
	}
}

func TestMissingEnvFileIsNotAnError(t *testing.T) {
	clearEnv(t)
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("FIREBASE_PROJECT_ID", "proj")
	t.Setenv("REVENUECAT_SECRET_API_KEY", "sk_test")
	t.Setenv("REVENUECAT_PROJECT_ID", "proj1ab2c3d4")
	t.Setenv("REVENUECAT_WEBHOOK_AUTH", "Bearer test-secret-value-long-enough")

	if _, err := config.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestRequiredValues(t *testing.T) {
	cases := []struct {
		name        string
		envFile     string
		wantMessage string
	}{
		{
			name:        "missing project id",
			envFile:     "REVENUECAT_SECRET_API_KEY=sk_test\nREVENUECAT_WEBHOOK_AUTH=Bearer test-secret-value-long-enough\n",
			wantMessage: "FIREBASE_PROJECT_ID",
		},
		{
			name:        "unreadable credentials file",
			envFile:     minimalEnv + "FIREBASE_CREDENTIALS_FILE=/nope/missing.json\n",
			wantMessage: "FIREBASE_CREDENTIALS_FILE",
		},
		{
			name:        "missing revenuecat api key",
			envFile:     "FIREBASE_PROJECT_ID=proj\nREVENUECAT_PROJECT_ID=proj1ab2c3d4\nREVENUECAT_WEBHOOK_AUTH=Bearer test-secret-value-long-enough\n",
			wantMessage: "REVENUECAT_SECRET_API_KEY",
		},
		{
			name:        "missing revenuecat project id",
			envFile:     "FIREBASE_PROJECT_ID=proj\nREVENUECAT_SECRET_API_KEY=sk_test\nREVENUECAT_WEBHOOK_AUTH=Bearer test-secret-value-long-enough\n",
			wantMessage: "REVENUECAT_PROJECT_ID",
		},
		{
			name:        "missing webhook secret",
			envFile:     "FIREBASE_PROJECT_ID=proj\nREVENUECAT_SECRET_API_KEY=sk_test\nREVENUECAT_PROJECT_ID=proj1ab2c3d4\n",
			wantMessage: "REVENUECAT_WEBHOOK_AUTH",
		},
		{
			name:        "webhook secret too short",
			envFile:     "FIREBASE_PROJECT_ID=proj\nREVENUECAT_SECRET_API_KEY=sk_test\nREVENUECAT_PROJECT_ID=proj1ab2c3d4\nREVENUECAT_WEBHOOK_AUTH=Bearer x\n",
			wantMessage: "REVENUECAT_WEBHOOK_AUTH",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			writeEnvFile(t, tc.envFile)

			_, err := config.Load()
			if err == nil {
				t.Fatalf("expected an error mentioning %s", tc.wantMessage)
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Errorf("err = %v, want it to mention %s", err, tc.wantMessage)
			}
		})
	}
}
