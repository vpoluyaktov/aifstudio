// Package auth provides session-cookie-based authentication for AIFStudio.
// The bcrypt password hash is stored in SQLite; session tokens are 32-byte
// random values base64url-encoded to 43 characters.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// ErrEmailTaken is returned by AuthStore.CreateUser when the email is already registered.
var ErrEmailTaken = errors.New("email already registered")

// User represents an authenticated AIFStudio user.
type User struct {
	UID       string    // "u-<ULID>"
	Email     string    // lowercased
	Name      string    // display name
	CreatedAt time.Time
}

// Session represents an active session stored in SQLite.
type Session struct {
	ID        string    // 43-char base64url token (32 random bytes)
	UserID    string    // FK → users.id
	CreatedAt time.Time
	ExpiresAt time.Time
}

type contextKey struct{}

// WithUser stores u in ctx.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, contextKey{}, u)
}

// UserFromContext returns the user stored in ctx, or nil if none.
func UserFromContext(ctx context.Context) *User {
	u, _ := ctx.Value(contextKey{}).(*User)
	return u
}

// AuthStore is the subset of store.Store that SessionAuth requires.
// Defined here (not in store) to avoid a circular import: store imports auth
// for *auth.User/*auth.Session types; auth must not import store.
type AuthStore interface {
	CreateUser(ctx context.Context, u *User, passwordHash string) error
	GetUserByEmail(ctx context.Context, email string) (*User, string, error)
	GetUserByID(ctx context.Context, uid string) (*User, error)
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, sessionID string) (*Session, error)
	DeleteSession(ctx context.Context, sessionID string) error
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error)
}

// SessionAuth implements session-cookie authentication.
// It satisfies VerifierIface so it can be injected into Server the same
// way MockVerifier is injected in tests.
type SessionAuth struct {
	store  AuthStore
	maxAge time.Duration
}

// NewSessionAuth creates a SessionAuth backed by st with sessions lasting maxAge.
func NewSessionAuth(st AuthStore, maxAge time.Duration) *SessionAuth {
	return &SessionAuth{store: st, maxAge: maxAge}
}

// FromRequest reads the "aifstudio_session" cookie and returns the authenticated
// user. Implements VerifierIface.
func (a *SessionAuth) FromRequest(ctx context.Context, r *http.Request) (*User, error) {
	cookie, err := r.Cookie("aifstudio_session")
	if err != nil {
		return nil, fmt.Errorf("no session cookie")
	}
	return a.verifySessionID(ctx, cookie.Value)
}

// VerifyToken is retained for VerifierIface compatibility. Session auth does not
// use bearer tokens; always returns an error.
func (a *SessionAuth) VerifyToken(_ context.Context, _ string) (*User, error) {
	return nil, fmt.Errorf("bearer token auth not supported; use session cookie")
}

func (a *SessionAuth) verifySessionID(ctx context.Context, sessionID string) (*User, error) {
	sess, err := a.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("session not found or expired")
	}
	if time.Now().After(sess.ExpiresAt) {
		go func() { _ = a.store.DeleteSession(context.Background(), sess.ID) }() //nolint:errcheck
		return nil, fmt.Errorf("session expired")
	}
	user, err := a.store.GetUserByID(ctx, sess.UserID)
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	if user == nil {
		return nil, fmt.Errorf("user not found")
	}
	return user, nil
}

// Login verifies credentials and issues a session cookie.
// Timing-safe: bcrypt.CompareHashAndPassword runs even when the user does not
// exist, so no timing oracle leaks whether the email is registered.
func (a *SessionAuth) Login(ctx context.Context, w http.ResponseWriter, r *http.Request, email, password string) (*User, error) {
	user, hash, err := a.store.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("lookup user: %w", err)
	}
	// Compare against a valid-looking hash even when user is missing.
	compareHash := hash
	if user == nil || compareHash == "" {
		// A pre-hashed dummy — same cost (12); prevents short-circuit.
		compareHash = "$2a$12$XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	}
	if bcrypt.CompareHashAndPassword([]byte(compareHash), []byte(password)) != nil || user == nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	sess, err := a.createSession(ctx, user.UID)
	if err != nil {
		return nil, err
	}
	setSessionCookie(w, r, sess.ID, a.maxAge)
	return user, nil
}

// Register creates a new user account and issues a session cookie.
// Returns ErrEmailTaken (unwrappable via errors.Is) if the email is taken.
func (a *SessionAuth) Register(ctx context.Context, w http.ResponseWriter, r *http.Request, email, password, displayName string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	user := &User{Email: email, Name: displayName}
	if err := a.store.CreateUser(ctx, user, string(hash)); err != nil {
		return nil, err // caller checks errors.Is(err, ErrEmailTaken)
	}
	sess, err := a.createSession(ctx, user.UID)
	if err != nil {
		return nil, err
	}
	setSessionCookie(w, r, sess.ID, a.maxAge)
	return user, nil
}

// Logout deletes the current session and clears the cookie. Idempotent: missing
// or unknown session is not an error.
func (a *SessionAuth) Logout(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("aifstudio_session"); err == nil {
		_ = a.store.DeleteSession(ctx, cookie.Value) //nolint:errcheck
	}
	clearSessionCookie(w)
}

func (a *SessionAuth) createSession(ctx context.Context, userID string) (*Session, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate session token: %w", err)
	}
	id := base64.RawURLEncoding.EncodeToString(b) // 43 chars
	now := time.Now().UTC()
	sess := &Session{
		ID:        id,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(a.maxAge),
	}
	if err := a.store.CreateSession(ctx, sess); err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return sess, nil
}

// setSessionCookie writes the aifstudio_session cookie. Secure is set only when
// the request is over HTTPS so local dev (http://localhost) still works.
func setSessionCookie(w http.ResponseWriter, r *http.Request, sessionID string, maxAge time.Duration) {
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     "aifstudio_session",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(maxAge.Seconds()),
		Secure:   secure,
	})
}

// clearSessionCookie removes the aifstudio_session cookie from the client.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "aifstudio_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}
