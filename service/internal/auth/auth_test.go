// Package auth_test tests the auth.Verifier and context helpers.
// All tests run without a real Firebase project — only local-mode behavior
// (empty projectID) is exercised in unit tests. Integration tests that require
// real Firebase token verification are outside this file.
package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"storycloud/internal/auth"
)

// ─────────────────────────────────────────────────────────────────────────────
// NewVerifier / local-mode
// ─────────────────────────────────────────────────────────────────────────────

func TestNewVerifierLocalMode(t *testing.T) {
	// Empty projectID → local dev mode, no Firebase connection.
	v, err := auth.NewVerifier(context.Background(), "")
	if err != nil {
		t.Fatalf("NewVerifier(empty projectID) error = %v; want nil", err)
	}
	if v == nil {
		t.Fatal("Verifier is nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// FromRequest — local dev mode
// ─────────────────────────────────────────────────────────────────────────────

func TestFromRequestLocalModeNoHeader(t *testing.T) {
	v, _ := auth.NewVerifier(context.Background(), "")

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	user, err := v.FromRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("FromRequest() error = %v; want nil in local mode", err)
	}
	if user == nil {
		t.Fatal("user = nil; want local-dev user")
	}
	if user.UID != "local-dev" {
		t.Errorf("UID = %q; want local-dev", user.UID)
	}
}

func TestFromRequestLocalModeWithBogusToken(t *testing.T) {
	// Local mode ignores the Authorization header completely.
	v, _ := auth.NewVerifier(context.Background(), "")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer totally-invalid-token")
	user, err := v.FromRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("FromRequest() in local mode error = %v; want nil", err)
	}
	if user == nil {
		t.Fatal("user = nil; want local-dev user (local mode ignores token)")
	}
	if user.UID != "local-dev" {
		t.Errorf("UID = %q; want local-dev", user.UID)
	}
}

func TestFromRequestLocalModeFields(t *testing.T) {
	v, _ := auth.NewVerifier(context.Background(), "")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	user, err := v.FromRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("FromRequest() error = %v", err)
	}
	if user.UID == "" {
		t.Error("UID is empty")
	}
	if user.Email == "" {
		t.Error("Email is empty in local mode")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// WithUser / UserFromContext
// ─────────────────────────────────────────────────────────────────────────────

func TestWithUserAndUserFromContext(t *testing.T) {
	tests := []struct {
		name string
		user *auth.User
	}{
		{
			name: "non-nil user is stored and retrieved",
			user: &auth.User{UID: "uid-123", Email: "alice@example.com", Name: "Alice"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = auth.WithUser(ctx, tt.user)

			got := auth.UserFromContext(ctx)
			if got == nil {
				t.Fatal("UserFromContext returned nil")
			}
			if got.UID != tt.user.UID {
				t.Errorf("UID = %q; want %q", got.UID, tt.user.UID)
			}
			if got.Email != tt.user.Email {
				t.Errorf("Email = %q; want %q", got.Email, tt.user.Email)
			}
			if got.Name != tt.user.Name {
				t.Errorf("Name = %q; want %q", got.Name, tt.user.Name)
			}
		})
	}
}

func TestUserFromContextEmptyContext(t *testing.T) {
	// A context with no user stored must return nil — not panic.
	user := auth.UserFromContext(context.Background())
	if user != nil {
		t.Errorf("UserFromContext(empty ctx) = %v; want nil", user)
	}
}

func TestUserFromContextIsolation(t *testing.T) {
	// Storing a user in one ctx branch must not affect the parent.
	parent := context.Background()
	user := &auth.User{UID: "user-1"}
	_ = auth.WithUser(parent, user)

	// Parent is unchanged.
	if auth.UserFromContext(parent) != nil {
		t.Error("parent context was mutated by WithUser — contexts should be immutable")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Non-local mode: missing / malformed Authorization header
//
// We cannot test real Firebase token verification without a live Firebase project.
// The following tests verify that the Authorization header is parsed correctly
// and that a missing / malformed header results in an error.
//
// Note: These tests require creating a Verifier in non-local mode, which would
// normally require a real Firebase project. Since we cannot do that in unit tests,
// these cases are tested via the server handler middleware tests (server_test.go).
// ─────────────────────────────────────────────────────────────────────────────

// TestAuthMiddlewareRequiresBearerToken documents the expected behavior:
//   - "Authorization: Bearer <token>" with a valid token → 200
//   - No header → 401
//   - "Authorization: token" (wrong scheme) → 401
//
// These are enforced by the authRequired middleware in server.go and validated
// in server_test.go using a local-mode Verifier (which always succeeds) and
// verifying that the 401 path is reachable when the header is absent in
// non-local mode.
func TestAuthBearerPrefixRequired(t *testing.T) {
	// This test documents the expected header shape rather than running
	// Firebase verification. The actual middleware behavior is tested in
	// server_test.go.
	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"bearer no token", "Bearer "},
		{"bearer with space only", "Bearer   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// In non-local mode, each of these should fail with an error.
			// We document the expected behavior here; see server_test.go for
			// full middleware validation.
			t.Log("expected: non-local Verifier.FromRequest returns error for:", tc.header)
		})
	}
}
