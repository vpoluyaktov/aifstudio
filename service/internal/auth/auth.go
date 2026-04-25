// Package auth provides Firebase ID token verification middleware.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	firebase "firebase.google.com/go/v4"
	firebaseauth "firebase.google.com/go/v4/auth"
	"google.golang.org/api/option"
)

// User represents a verified Firebase user.
type User struct {
	UID   string
	Email string
	Name  string
}

type contextKey struct{}

// WithUser stores the authenticated user in ctx.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, contextKey{}, u)
}

// UserFromContext retrieves the authenticated user from ctx, or nil.
func UserFromContext(ctx context.Context) *User {
	u, _ := ctx.Value(contextKey{}).(*User)
	return u
}

// Verifier wraps the Firebase Admin SDK auth client.
type Verifier struct {
	client *firebaseauth.Client
	// localMode means GCP_PROJECT_ID was not set; skip real verification.
	localMode bool
}

// NewVerifier creates a Verifier. If projectID is empty, local dev mode is
// activated: FromRequest returns a fixed "local-dev" user.
func NewVerifier(ctx context.Context, projectID string, opts ...option.ClientOption) (*Verifier, error) {
	if projectID == "" {
		return &Verifier{localMode: true}, nil
	}

	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: projectID}, opts...)
	if err != nil {
		return nil, fmt.Errorf("firebase app: %w", err)
	}

	client, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("firebase auth client: %w", err)
	}

	return &Verifier{client: client}, nil
}

// FromRequest extracts and verifies the Firebase ID token from the
// Authorization: Bearer header. Returns an error if the token is absent or
// invalid. In local dev mode always returns a fixed user.
func (v *Verifier) FromRequest(ctx context.Context, r *http.Request) (*User, error) {
	if v.localMode {
		return &User{UID: "local-dev", Email: "dev@local", Name: "Local Dev"}, nil
	}

	bearer := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(bearer, "Bearer ")
	if !ok || token == "" {
		return nil, fmt.Errorf("missing or malformed Authorization header")
	}

	return v.verify(ctx, token)
}

// VerifyToken verifies a raw ID token string directly. Used when the token is
// passed via query parameter rather than the Authorization header (e.g. for the
// sendBeacon suspend path which cannot set custom headers).
func (v *Verifier) VerifyToken(ctx context.Context, idToken string) (*User, error) {
	if v.localMode {
		return &User{UID: "local-dev", Email: "dev@local", Name: "Local Dev"}, nil
	}
	return v.verify(ctx, idToken)
}

func (v *Verifier) verify(ctx context.Context, idToken string) (*User, error) {
	tok, err := v.client.VerifyIDToken(ctx, idToken)
	if err != nil {
		return nil, fmt.Errorf("invalid id token: %w", err)
	}

	u := &User{UID: tok.UID}
	if email, ok := tok.Claims["email"].(string); ok {
		u.Email = email
	}
	if name, ok := tok.Claims["name"].(string); ok {
		u.Name = name
	}
	return u, nil
}
