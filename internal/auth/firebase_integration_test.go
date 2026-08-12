package auth_test

import (
	"aislide/internal/auth"
	"context"
	"errors"
	"os"
	"testing"
)

// Opt-in integration test: it talks to Firebase, so it only runs when the
// credentials are present in the environment.
//
//	FIREBASE_PROJECT_ID=... FIREBASE_CREDENTIALS_FILE=... go test ./internal/auth/
//
// Set FIREBASE_TEST_ID_TOKEN to a real ID token to also assert the happy path.
func TestFirebaseVerifier(t *testing.T) {
	projectID := os.Getenv("FIREBASE_PROJECT_ID")
	credentialsFile := os.Getenv("FIREBASE_CREDENTIALS_FILE")
	if projectID == "" || credentialsFile == "" {
		t.Skip("FIREBASE_PROJECT_ID / FIREBASE_CREDENTIALS_FILE not set")
	}

	ctx := context.Background()

	verifier, err := auth.NewFirebaseVerifier(ctx, projectID, auth.Credentials{File: credentialsFile})
	if err != nil {
		t.Fatalf("NewFirebaseVerifier: %v", err)
	}

	if _, err := verifier.VerifyIDToken(ctx, "not.a.real.token"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}

	idToken := os.Getenv("FIREBASE_TEST_ID_TOKEN")
	if idToken == "" {
		t.Log("FIREBASE_TEST_ID_TOKEN not set, skipping happy path")
		return
	}

	token, err := verifier.VerifyIDToken(ctx, idToken)
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if token.UID == "" {
		t.Error("uid should not be empty")
	}
	t.Logf("verified uid=%s provider=%s", token.UID, token.SignInProvider)
}
