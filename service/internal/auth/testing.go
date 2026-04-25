// Package auth — testing helpers.
//
// This file adds VerifierIface and MockVerifier so server tests can exercise
// the firebaseAuthRequired middleware without a live Firebase project.
// It is safe to import from production code; MockVerifier is used exclusively
// from _test.go files in practice.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// VerifierIface is the minimal interface implemented by both *Verifier and
// *MockVerifier, enabling dependency injection in server tests without a live
// Firebase project.
//
// Methods:
//   - FromRequest: extracts and verifies the token from Authorization header.
//   - VerifyToken: verifies a raw token string (used by firebaseAuthRequired for
//     the sendBeacon suspend ?token= query-param path, §22.5.4).
type VerifierIface interface {
	FromRequest(ctx context.Context, r *http.Request) (*User, error)
	VerifyToken(ctx context.Context, idToken string) (*User, error)
}

// Compile-time assertion: *Verifier must satisfy VerifierIface.
var _ VerifierIface = (*Verifier)(nil)

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
//	mv.SetUser("token-b", &auth.User{UID: "uid-b", Email: "b@example.com"})
//
// Requests carrying "Authorization: Bearer token-a" are authenticated as uid-a.
// Requests with no token or an unregistered token receive an error, which the
// firebaseAuthRequired middleware translates to HTTP 401.
type MockVerifier struct {
	users map[string]*User
}

// NewMockVerifier creates a MockVerifier with no registered users.
func NewMockVerifier() *MockVerifier {
	return &MockVerifier{users: make(map[string]*User)}
}

// SetUser registers a mapping from token to user. A request carrying
// "Authorization: Bearer <token>" will be authenticated as u.
func (m *MockVerifier) SetUser(token string, u *User) {
	m.users[token] = u
}

// FromRequest extracts the bearer token from the Authorization header and
// returns the pre-configured User. Returns an error when the token is absent
// or unregistered, simulating an invalid/expired Firebase ID token.
func (m *MockVerifier) FromRequest(_ context.Context, r *http.Request) (*User, error) {
	bearer := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(bearer, "Bearer ")
	if !ok || token == "" {
		return nil, fmt.Errorf("auth: missing or malformed Authorization header")
	}
	return m.VerifyToken(r.Context(), token)
}

// VerifyToken looks up the raw token in the registered map and returns the
// corresponding User. Returns an error if the token is not registered.
// Called directly by firebaseAuthRequired for the sendBeacon suspend path.
func (m *MockVerifier) VerifyToken(_ context.Context, idToken string) (*User, error) {
	if idToken == "" {
		return nil, fmt.Errorf("auth: empty token")
	}
	u, found := m.users[idToken]
	if !found {
		return nil, fmt.Errorf("auth: unknown or invalid token")
	}
	return u, nil
}

// Compile-time assertion: *MockVerifier must satisfy VerifierIface.
var _ VerifierIface = (*MockVerifier)(nil)
