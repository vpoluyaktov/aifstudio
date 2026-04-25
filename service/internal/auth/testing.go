// Package auth — testing helpers.
//
// VerifierIface and MockVerifier allow server tests to exercise the
// sessionAuthRequired middleware without a live database or real sessions.
// SessionAuth also satisfies VerifierIface so the same server.New signature
// accepts both in production and in tests.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// VerifierIface is the minimal interface implemented by both *SessionAuth and
// *MockVerifier, enabling dependency injection in server tests.
//
//   - FromRequest: extracts and verifies auth from the request.
//     *SessionAuth reads the aifstudio_session cookie.
//     *MockVerifier reads the Authorization: Bearer header (test only).
//   - VerifyToken: verifies a raw token string.
//     *SessionAuth always returns an error (cookie-only auth).
//     *MockVerifier looks up the token in a pre-registered map.
type VerifierIface interface {
	FromRequest(ctx context.Context, r *http.Request) (*User, error)
	VerifyToken(ctx context.Context, idToken string) (*User, error)
}

// Compile-time assertions: both concrete types must satisfy VerifierIface.
var _ VerifierIface = (*SessionAuth)(nil)
var _ VerifierIface = (*MockVerifier)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// MockVerifier
// ─────────────────────────────────────────────────────────────────────────────

// MockVerifier is a test double for VerifierIface. It resolves tokens from a
// pre-configured map and returns an error for any token not registered.
//
// Usage:
//
//	mv := auth.NewMockVerifier()
//	mv.SetUser("token-a", &auth.User{UID: "uid-a", Email: "a@example.com"})
//
// Requests carrying "Authorization: Bearer token-a" are authenticated as uid-a.
// Requests with no token or an unregistered token receive an error, which the
// sessionAuthRequired middleware translates to HTTP 401.
type MockVerifier struct {
	users    map[string]*User
	localDev bool // if true, always return a fixed local-dev user
}

// NewMockVerifier creates a MockVerifier with no registered users.
func NewMockVerifier() *MockVerifier {
	return &MockVerifier{users: make(map[string]*User)}
}

// NewLocalDevVerifier returns a MockVerifier that always authenticates with a
// fixed "local-dev" user regardless of the token or cookie. Use in tests that
// just need an authenticated server without real auth wiring.
func NewLocalDevVerifier() *MockVerifier {
	mv := NewMockVerifier()
	mv.localDev = true
	return mv
}

// SetUser registers a mapping from token to user. A request carrying
// "Authorization: Bearer <token>" will be authenticated as u.
func (m *MockVerifier) SetUser(token string, u *User) {
	m.users[token] = u
}

// FromRequest extracts the bearer token from the Authorization header and
// returns the pre-configured User. Returns an error when the token is absent
// or unregistered, simulating an invalid or expired session.
func (m *MockVerifier) FromRequest(_ context.Context, r *http.Request) (*User, error) {
	if m.localDev {
		return &User{UID: "local-dev", Email: "dev@local", Name: "Local Dev"}, nil
	}
	bearer := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(bearer, "Bearer ")
	if !ok || token == "" {
		return nil, fmt.Errorf("auth: missing or malformed Authorization header")
	}
	return m.VerifyToken(r.Context(), token)
}

// VerifyToken looks up the raw token in the registered map and returns the
// corresponding User. Returns an error if the token is not registered.
func (m *MockVerifier) VerifyToken(_ context.Context, idToken string) (*User, error) {
	if m.localDev {
		return &User{UID: "local-dev", Email: "dev@local", Name: "Local Dev"}, nil
	}
	if idToken == "" {
		return nil, fmt.Errorf("auth: empty token")
	}
	u, found := m.users[idToken]
	if !found {
		return nil, fmt.Errorf("auth: unknown or invalid token")
	}
	return u, nil
}
