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

// NewFirebaseVerifier builds a verifier from a service account JSON file.
// Pass an empty credentialsFile to fall back to Application Default
// Credentials (how this will run on a cloud host).
func NewFirebaseVerifier(ctx context.Context, projectID, credentialsFile string) (Verifier, error) {
	var opts []option.ClientOption
	if credentialsFile != "" {
		// WithAuthCredentialsFile pins the accepted credential type instead of
		// loading whatever the file happens to contain. option.WithCredentialsFile
		// is deprecated because it also accepts external_account configs, which
		// can point at arbitrary URLs or local executables to fetch a token.
		opts = append(opts, option.WithAuthCredentialsFile(option.ServiceAccount, credentialsFile))
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
