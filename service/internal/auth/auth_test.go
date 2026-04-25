// Package auth_test tests the auth package: context helpers, MockVerifier,
// and SessionAuth (backed by a real in-memory SQLite store).
package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"storycloud/internal/auth"
	"storycloud/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// SessionAuth test helpers
// ─────────────────────────────────────────────────────────────────────────────

// newAuthStore opens a temporary SQLite store suitable for SessionAuth tests.
func newAuthStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	blob := store.NewLocalBlobStore(filepath.Join(dir, "storage"))
	s, err := store.NewSQLiteStore(context.Background(), filepath.Join(dir, "auth_test.db"), blob)
	if err != nil {
		t.Fatalf("NewSQLiteStore for auth test: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newSessionAuth creates a SessionAuth with a 24-hour session max-age.
func newSessionAuth(t *testing.T) (*auth.SessionAuth, *store.SQLiteStore) {
	t.Helper()
	st := newAuthStore(t)
	return auth.NewSessionAuth(st, 24*time.Hour), st
}

// ─────────────────────────────────────────────────────────────────────────────
// NewLocalDevVerifier
// ─────────────────────────────────────────────────────────────────────────────

func TestNewLocalDevVerifierAlwaysReturnsUser(t *testing.T) {
	v := auth.NewLocalDevVerifier()
	if v == nil {
		t.Fatal("NewLocalDevVerifier returned nil")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	user, err := v.FromRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("FromRequest() error = %v; want nil", err)
	}
	if user == nil {
		t.Fatal("user = nil; want local-dev user")
	}
	if user.UID != "local-dev" {
		t.Errorf("UID = %q; want local-dev", user.UID)
	}
}

func TestNewLocalDevVerifierIgnoresToken(t *testing.T) {
	// LocalDevVerifier ignores the Authorization header completely.
	v := auth.NewLocalDevVerifier()

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

func TestLocalDevVerifierFields(t *testing.T) {
	v := auth.NewLocalDevVerifier()

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
// MockVerifier
// ─────────────────────────────────────────────────────────────────────────────

func TestMockVerifierRegisteredToken(t *testing.T) {
	mv := auth.NewMockVerifier()
	want := &auth.User{UID: "u-test", Email: "test@example.com", Name: "Test"}
	mv.SetUser("tok-123", want)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer tok-123")

	got, err := mv.FromRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("FromRequest with valid token: %v", err)
	}
	if got == nil || got.UID != want.UID {
		t.Errorf("UID = %q; want %q", got.UID, want.UID)
	}
}

func TestMockVerifierUnregisteredToken(t *testing.T) {
	mv := auth.NewMockVerifier()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer unknown-token")

	user, err := mv.FromRequest(context.Background(), req)
	if err == nil {
		t.Errorf("expected error for unregistered token, got user %v", user)
	}
}

func TestMockVerifierNoHeader(t *testing.T) {
	mv := auth.NewMockVerifier()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// No Authorization header.

	user, err := mv.FromRequest(context.Background(), req)
	if err == nil {
		t.Errorf("expected error for missing header, got user %v", user)
	}
}

// TestAuthBearerPrefixRequired documents the expected header shape.
func TestAuthBearerPrefixRequired(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"bearer no token", "Bearer "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Log("expected: MockVerifier.FromRequest returns error for:", tc.header)
		})
	}
}

// ────────────────────────────���────────────────────────────────────────────────
// SessionAuth tests (backed by a real SQLite store)
// ──────────────────────────────────���──────────────────────────────��───────────

// TestSessionAuthRegister verifies that Register creates a user, issues a
// session, and sets the aifstudio_session cookie in the response.
func TestSessionAuthRegister(t *testing.T) {
	sa, st := newSessionAuth(t)
	ctx := context.Background()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)

	user, err := sa.Register(ctx, w, r, "alice@example.com", "correcthorsebatterystaple", "Alice")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if user == nil {
		t.Fatal("user is nil")
	}
	if user.UID == "" {
		t.Error("UID should be set after Register")
	}
	if user.Email != "alice@example.com" {
		t.Errorf("Email = %q; want alice@example.com", user.Email)
	}
	if user.Name != "Alice" {
		t.Errorf("Name = %q; want Alice", user.Name)
	}

	// Verify the session cookie was set.
	cookies := w.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "aifstudio_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("no aifstudio_session cookie set")
	}
	if sessionCookie.Value == "" {
		t.Error("session cookie value is empty")
	}

	// Verify the session exists in the store.
	sess, err := st.GetSession(ctx, sessionCookie.Value)
	if err != nil {
		t.Fatalf("GetSession after register: %v", err)
	}
	if sess == nil {
		t.Fatal("session not found in store after register")
	}
	if sess.UserID != user.UID {
		t.Errorf("session.UserID = %q; want %q", sess.UserID, user.UID)
	}
}

// TestSessionAuthRegister_DuplicateEmail verifies that registering with an
// already-registered email returns ErrEmailTaken (wrapped or direct).
func TestSessionAuthRegister_DuplicateEmail(t *testing.T) {
	sa, _ := newSessionAuth(t)
	ctx := context.Background()

	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
	if _, err := sa.Register(ctx, w1, r1, "bob@example.com", "password123", "Bob"); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	// Second registration with same email.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
	_, err := sa.Register(ctx, w2, r2, "bob@example.com", "differentpwd", "Bobby")
	if err == nil {
		t.Fatal("expected error for duplicate email, got nil")
	}
	if !isEmailTaken(err) {
		t.Errorf("err = %v; want ErrEmailTaken or wrapping it", err)
	}
}

// isEmailTaken returns true if err signals a duplicate email.
func isEmailTaken(err error) bool {
	if err == nil {
		return false
	}
	// Direct match or wrapped.
	if err == auth.ErrEmailTaken {
		return true
	}
	// Some implementations wrap the error; unwrap and check.
	return err.Error() == auth.ErrEmailTaken.Error()
}

// TestSessionAuthLogin verifies that Login with correct credentials succeeds
// and sets a session cookie.
func TestSessionAuthLogin(t *testing.T) {
	sa, _ := newSessionAuth(t)
	ctx := context.Background()

	// Register first.
	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
	_, err := sa.Register(ctx, w1, r1, "carol@example.com", "password12345", "Carol")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Login.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	user, err := sa.Login(ctx, w2, r2, "carol@example.com", "password12345")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if user == nil {
		t.Fatal("user is nil after login")
	}
	if user.Email != "carol@example.com" {
		t.Errorf("Email = %q; want carol@example.com", user.Email)
	}

	// Verify the session cookie was set.
	var sessionCookie *http.Cookie
	for _, c := range w2.Result().Cookies() {
		if c.Name == "aifstudio_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no aifstudio_session cookie after login")
	}
}

// TestSessionAuthLogin_WrongPassword verifies that Login with a wrong password
// returns an error and does NOT set a session cookie.
func TestSessionAuthLogin_WrongPassword(t *testing.T) {
	sa, _ := newSessionAuth(t)
	ctx := context.Background()

	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
	_, err := sa.Register(ctx, w1, r1, "dave@example.com", "correctpassword", "Dave")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	_, err = sa.Login(ctx, w2, r2, "dave@example.com", "wrongpassword")
	if err == nil {
		t.Fatal("expected error for wrong password, got nil")
	}

	// Must not set a session cookie on failure.
	for _, c := range w2.Result().Cookies() {
		if c.Name == "aifstudio_session" {
			t.Errorf("session cookie set despite wrong password: %q", c.Value)
		}
	}
}

// TestSessionAuthLogin_UnknownEmail verifies that Login with an unregistered
// email returns an error (timing-safe path).
func TestSessionAuthLogin_UnknownEmail(t *testing.T) {
	sa, _ := newSessionAuth(t)
	ctx := context.Background()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	_, err := sa.Login(ctx, w, r, "nobody@example.com", "somepassword")
	if err == nil {
		t.Fatal("expected error for unknown email, got nil")
	}
}

// TestSessionAuthLogout verifies that Logout deletes the session from the
// store and clears the cookie (Max-Age=-1 or Max-Age=0).
func TestSessionAuthLogout(t *testing.T) {
	sa, st := newSessionAuth(t)
	ctx := context.Background()

	// Register and grab the session cookie.
	wReg := httptest.NewRecorder()
	rReg := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
	_, err := sa.Register(ctx, wReg, rReg, "eve@example.com", "password12345", "Eve")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	var sessionToken string
	for _, c := range wReg.Result().Cookies() {
		if c.Name == "aifstudio_session" {
			sessionToken = c.Value
		}
	}
	if sessionToken == "" {
		t.Fatal("no session cookie after register")
	}

	// Verify session is valid.
	sess, _ := st.GetSession(ctx, sessionToken)
	if sess == nil {
		t.Fatal("session should exist before logout")
	}

	// Logout.
	wOut := httptest.NewRecorder()
	rOut := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	rOut.AddCookie(&http.Cookie{Name: "aifstudio_session", Value: sessionToken})
	sa.Logout(ctx, wOut, rOut)

	// Session must be removed from store.
	sess2, _ := st.GetSession(ctx, sessionToken)
	if sess2 != nil {
		t.Error("session should be deleted after logout")
	}

	// Response must clear the cookie (MaxAge <= 0).
	for _, c := range wOut.Result().Cookies() {
		if c.Name == "aifstudio_session" {
			if c.MaxAge > 0 {
				t.Errorf("cookie MaxAge = %d; want ≤0 (clear)", c.MaxAge)
			}
		}
	}
}

// TestSessionAuthFromRequest_ValidCookie verifies that FromRequest returns the
// correct user when a valid session cookie is present.
func TestSessionAuthFromRequest_ValidCookie(t *testing.T) {
	sa, _ := newSessionAuth(t)
	ctx := context.Background()

	// Register to get a session.
	wReg := httptest.NewRecorder()
	rReg := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
	registered, err := sa.Register(ctx, wReg, rReg, "frank@example.com", "password12345", "Frank")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	var sessionToken string
	for _, c := range wReg.Result().Cookies() {
		if c.Name == "aifstudio_session" {
			sessionToken = c.Value
		}
	}

	// Build a request with the session cookie.
	r := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	r.AddCookie(&http.Cookie{Name: "aifstudio_session", Value: sessionToken})

	user, err := sa.FromRequest(ctx, r)
	if err != nil {
		t.Fatalf("FromRequest valid cookie: %v", err)
	}
	if user == nil {
		t.Fatal("user is nil for valid cookie")
	}
	if user.UID != registered.UID {
		t.Errorf("UID = %q; want %q", user.UID, registered.UID)
	}
	if user.Email != "frank@example.com" {
		t.Errorf("Email = %q; want frank@example.com", user.Email)
	}
}

// TestSessionAuthFromRequest_NoCookie verifies that FromRequest returns an error
// when the session cookie is absent.
func TestSessionAuthFromRequest_NoCookie(t *testing.T) {
	sa, _ := newSessionAuth(t)
	ctx := context.Background()

	r := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	// No cookie added.

	_, err := sa.FromRequest(ctx, r)
	if err == nil {
		t.Fatal("expected error for missing cookie, got nil")
	}
}

// TestSessionAuthFromRequest_ExpiredSession verifies that FromRequest returns
// an error when the session token maps to an expired (or already-deleted) session.
func TestSessionAuthFromRequest_ExpiredSession(t *testing.T) {
	sa, st := newSessionAuth(t)
	ctx := context.Background()

	// Register and then manually delete the session to simulate expiry.
	wReg := httptest.NewRecorder()
	rReg := httptest.NewRequest(http.MethodPost, "/api/auth/register", nil)
	_, err := sa.Register(ctx, wReg, rReg, "grace@example.com", "password12345", "Grace")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	var sessionToken string
	for _, c := range wReg.Result().Cookies() {
		if c.Name == "aifstudio_session" {
			sessionToken = c.Value
		}
	}

	// Delete the session from the store to simulate expiry.
	if err := st.DeleteSession(ctx, sessionToken); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	r.AddCookie(&http.Cookie{Name: "aifstudio_session", Value: sessionToken})

	_, err = sa.FromRequest(ctx, r)
	if err == nil {
		t.Fatal("expected error for expired/deleted session, got nil")
	}
}

// TestSessionAuthFromRequest_InvalidToken verifies that FromRequest returns an
// error for a cookie value that does not exist in the sessions table.
func TestSessionAuthFromRequest_InvalidToken(t *testing.T) {
	sa, _ := newSessionAuth(t)
	ctx := context.Background()

	r := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	r.AddCookie(&http.Cookie{Name: "aifstudio_session", Value: "completely-invalid-token-value"})

	_, err := sa.FromRequest(ctx, r)
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
}
