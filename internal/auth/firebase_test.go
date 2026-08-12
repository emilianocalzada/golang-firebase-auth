package auth_test

import (
	"aislide/internal/auth"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// serviceAccountJSON builds a syntactically real service account document with a
// throwaway key, so the credential-loading path can be exercised without any
// network access or a real Google key.
func serviceAccountJSON(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	document, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "aislide-test",
		"private_key_id": "throwaway",
		"private_key":    string(privateKey),
		"client_email":   "throwaway@aislide-test.iam.gserviceaccount.com",
		"client_id":      "1",
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("marshal service account: %v", err)
	}

	return document
}

// Inline JSON is how the service account arrives on a host whose only secret
// store is the environment.
func TestNewFirebaseVerifierFromInlineJSON(t *testing.T) {
	verifier, err := auth.NewFirebaseVerifier(context.Background(), "aislide-test", auth.Credentials{
		JSON: serviceAccountJSON(t),
	})
	if err != nil {
		t.Fatalf("NewFirebaseVerifier: %v", err)
	}
	if verifier == nil {
		t.Fatal("verifier should not be nil")
	}
}

// The same document on disk must behave identically, so switching between the
// two is only a deployment decision.
func TestNewFirebaseVerifierFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, serviceAccountJSON(t), 0600); err != nil {
		t.Fatalf("write service account: %v", err)
	}

	verifier, err := auth.NewFirebaseVerifier(context.Background(), "aislide-test", auth.Credentials{File: path})
	if err != nil {
		t.Fatalf("NewFirebaseVerifier: %v", err)
	}
	if verifier == nil {
		t.Fatal("verifier should not be nil")
	}
}

func TestNewFirebaseVerifierRejectsBrokenCredentials(t *testing.T) {
	_, err := auth.NewFirebaseVerifier(context.Background(), "aislide-test", auth.Credentials{
		JSON: []byte(`{"type":"service_account","private_key":"not a pem"}`),
	})
	if err == nil {
		t.Error("a service account with an unparseable private key should fail")
	}
}
