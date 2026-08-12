package config_test

import (
	"aislide/internal/config"
	"encoding/base64"
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
		"FIREBASE_PROJECT_ID", "FIREBASE_CREDENTIALS_FILE", "FIREBASE_CREDENTIALS_JSON",
		"REVENUECAT_SECRET_API_KEY", "REVENUECAT_PROJECT_ID",
		"REVENUECAT_WEBHOOK_AUTH", "REVENUECAT_ENTITLEMENT_ID",
		"TRUSTED_PROXIES", "CLIENT_IP_HEADER",
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

func TestLoadDefaultsToTrustingNoProxy(t *testing.T) {
	clearEnv(t)
	writeEnvFile(t, minimalEnv)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Nothing trusted and no header read: the client IP is the socket peer,
	// which is what a local run without a proxy should see.
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies = %v, want empty", cfg.TrustedProxies)
	}
	if cfg.ClientIPHeader != "" {
		t.Errorf("ClientIPHeader = %q, want empty", cfg.ClientIPHeader)
	}
}

func TestLoadParsesCloudflareProxySetup(t *testing.T) {
	clearEnv(t)
	writeEnvFile(t, minimalEnv+
		"TRUSTED_PROXIES=172.16.0.0/12, 10.0.0.0/8 ,192.168.1.7\n"+
		"CLIENT_IP_HEADER=CF-Connecting-IP\n")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := []string{"172.16.0.0/12", "10.0.0.0/8", "192.168.1.7"}
	if len(cfg.TrustedProxies) != len(want) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, want)
	}
	for i, entry := range want {
		if cfg.TrustedProxies[i] != entry {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, cfg.TrustedProxies[i], entry)
		}
	}
	if cfg.ClientIPHeader != "CF-Connecting-IP" {
		t.Errorf("ClientIPHeader = %q, want CF-Connecting-IP", cfg.ClientIPHeader)
	}
}

func TestLoadRejectsBadProxyTrust(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  string
	}{
		{
			// Trusting every source makes the header spoofable by anyone who
			// reaches the origin directly.
			name:  "trust all ipv4",
			extra: "TRUSTED_PROXIES=0.0.0.0/0\n",
			want:  "0.0.0.0/0",
		},
		{
			name:  "trust all ipv6",
			extra: "TRUSTED_PROXIES=::/0\n",
			want:  "::/0",
		},
		{
			name:  "typo",
			extra: "TRUSTED_PROXIES=172.16.0.0/99\n",
			want:  "not a valid CIDR",
		},
		{
			name:  "not an ip",
			extra: "TRUSTED_PROXIES=traefik\n",
			want:  "not a valid IP address or CIDR",
		},
		{
			// The header would silently never be read.
			name:  "header without proxies",
			extra: "CLIENT_IP_HEADER=CF-Connecting-IP\n",
			want:  "would be ignored",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			writeEnvFile(t, minimalEnv+tc.extra)

			_, err := config.Load()
			if err == nil {
				t.Fatal("Load should have failed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// serviceAccountEnv is a syntactically valid service account document. The key
// is fake but shaped right: parseCredentialsJSON only checks structure.
const serviceAccountEnv = `{"type":"service_account","project_id":"p",` +
	`"private_key":"-----BEGIN PRIVATE KEY-----\nMIIBfake\n-----END PRIVATE KEY-----\n",` +
	`"client_email":"sa@p.iam.gserviceaccount.com"}`

func TestLoadAcceptsInlineCredentialsJSON(t *testing.T) {
	clearEnv(t)
	writeEnvFile(t, minimalEnv)
	t.Setenv("FIREBASE_CREDENTIALS_JSON", serviceAccountEnv)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(cfg.FirebaseCredentialsJSON) != serviceAccountEnv {
		t.Errorf("FirebaseCredentialsJSON = %q, want the document verbatim", cfg.FirebaseCredentialsJSON)
	}
}

func TestLoadAcceptsBase64CredentialsJSON(t *testing.T) {
	clearEnv(t)
	writeEnvFile(t, minimalEnv)
	t.Setenv("FIREBASE_CREDENTIALS_JSON", base64.StdEncoding.EncodeToString([]byte(serviceAccountEnv)))

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Decoded, so the SDK receives the same bytes either way.
	if string(cfg.FirebaseCredentialsJSON) != serviceAccountEnv {
		t.Errorf("FirebaseCredentialsJSON = %q, want the decoded document", cfg.FirebaseCredentialsJSON)
	}
}

func TestLoadRejectsBadCredentialsJSON(t *testing.T) {
	credentials := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(credentials, []byte("{}"), 0600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	cases := []struct {
		name string
		json string
		file string
		want string
	}{
		{
			name: "truncated paste",
			json: `{"type":"service_account","project_id":"p"`,
			want: "not valid JSON",
		},
		{
			name: "not json and not base64",
			json: "paste-went-wrong!!",
			want: "nor valid base64",
		},
		{
			name: "wrong credential type",
			json: `{"type":"external_account","project_id":"p","private_key":"x\ny","client_email":"a@b"}`,
			want: "want service_account",
		},
		{
			name: "missing fields",
			json: `{"type":"service_account"}`,
			want: "missing project_id",
		},
		{
			// The commonest paste failure: \n escapes flattened away.
			name: "private key lost its newlines",
			json: `{"type":"service_account","project_id":"p","private_key":"-----BEGIN PRIVATE KEY-----MIIBfake-----END PRIVATE KEY-----","client_email":"a@b"}`,
			want: "no line breaks",
		},
		{
			name: "both sources set",
			json: serviceAccountEnv,
			file: credentials,
			want: "not both",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			writeEnvFile(t, minimalEnv)
			t.Setenv("FIREBASE_CREDENTIALS_JSON", tc.json)
			if tc.file != "" {
				t.Setenv("FIREBASE_CREDENTIALS_FILE", tc.file)
			}

			_, err := config.Load()
			if err == nil {
				t.Fatal("Load should have failed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			// An error that echoed the document would put a private key in the
			// logs of whatever is watching startup.
			if strings.Contains(err.Error(), "PRIVATE KEY") {
				t.Errorf("error must not include the credential contents: %q", err)
			}
		})
	}
}
