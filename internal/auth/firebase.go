package auth

import (
	"context"
	"errors"
	"fmt"

	firebase "firebase.google.com/go/v4"
	firebaseauth "firebase.google.com/go/v4/auth"
	"google.golang.org/api/option"
)

// ErrInvalidToken is returned for any token we refuse: malformed, expired,
// wrong audience or wrong signature. The transport layer maps it to 401.
var ErrInvalidToken = errors.New("invalid id token")

// Token is the subset of the Firebase ID token the app cares about.
type Token struct {
	UID string
	// SignInProvider is "anonymous" for anonymous sign-in, "google.com",
	// "apple.com", etc. Useful later if we let users upgrade an anonymous
	// account to a permanent one.
	SignInProvider string
}

// Verifier decouples handlers from the Firebase SDK so they can be tested
// with a fake.
type Verifier interface {
	VerifyIDToken(ctx context.Context, idToken string) (*Token, error)
}

type firebaseVerifier struct {
	client *firebaseauth.Client
}

// Credentials says where the service account comes from. Exactly one field
// should be set, or neither to fall back to Application Default Credentials.
//
// JSON exists for hosts whose only way to inject a secret is an environment
// variable. It is the same document as the file, just carried inline.
type Credentials struct {
	File string
	JSON []byte
}

// NewFirebaseVerifier builds a verifier from a service account, supplied either
// as a file path or as the JSON itself. Pass an empty Credentials to fall back
// to Application Default Credentials (only available on a Google cloud host).
func NewFirebaseVerifier(ctx context.Context, projectID string, creds Credentials) (Verifier, error) {
	var opts []option.ClientOption

	// Both variants pin the accepted credential type instead of loading
	// whatever the document happens to contain. The plain WithCredentialsFile
	// and WithCredentialsJSON are deprecated because they also accept
	// external_account configs, which can point at arbitrary URLs or local
	// executables to fetch a token.
	switch {
	case len(creds.JSON) > 0:
		opts = append(opts, option.WithAuthCredentialsJSON(option.ServiceAccount, creds.JSON))
	case creds.File != "":
		opts = append(opts, option.WithAuthCredentialsFile(option.ServiceAccount, creds.File))
	}

	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: projectID}, opts...)
	if err != nil {
		return nil, fmt.Errorf("init firebase app: %w", err)
	}

	client, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("init firebase auth client: %w", err)
	}

	return &firebaseVerifier{client: client}, nil
}

func (v *firebaseVerifier) VerifyIDToken(ctx context.Context, idToken string) (*Token, error) {
	token, err := v.client.VerifyIDToken(ctx, idToken)
	if err != nil {
		// Wrapped without the token value so it never reaches the logs.
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	return &Token{
		UID:            token.UID,
		SignInProvider: token.Firebase.SignInProvider,
	}, nil
}
