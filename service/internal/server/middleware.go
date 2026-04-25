package server

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"storycloud/internal/auth"
)

// suspendPathRE matches POST /api/runs/{id}/suspend so we can accept
// the Firebase token via ?token= for sendBeacon callers (§22.5.4).
var suspendPathRE = regexp.MustCompile(`^/api/runs/r-[0-9A-Z]{26}/suspend$`)

// recoverMiddleware catches panics, logs them, and writes 500.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered", "err", rec, "stack", string(debug.Stack()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type requestIDKey struct{}

// requestIDMiddleware adds X-Request-ID to each request and response.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = generateID()
		}
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// loggingMiddleware logs each request with method, path, status, and duration.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		reqID, _ := r.Context().Value(requestIDKey{}).(string)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", reqID,
		)
	})
}

// corsMiddleware enforces same-origin only (Vary: Origin, no wildcard).
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Vary", "Origin")
		// No Access-Control-Allow-Origin: * — same-origin only.
		next.ServeHTTP(w, r)
	})
}

// maxBodyMiddleware limits request body size. WebSocket routes are excluded.
func maxBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip body limit for WebSocket upgrades.
		if r.Header.Get("Upgrade") == "websocket" {
			next.ServeHTTP(w, r)
			return
		}
		// PUT /api/projects/{id}/source gets 1 MiB; others 64 KiB.
		limit := int64(64 * 1024)
		if r.Method == http.MethodPut {
			limit = 1024 * 1024
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// isAllowlisted reports whether the request should bypass Firebase auth.
//
// Only /api/* routes (except /api/config) require server-side JWT verification.
// Page routes are served without a server-side token check — auth.js /
// onAuthStateChanged handles the client-side redirect to /login for
// unauthenticated users. Browsers never send Authorization headers on normal
// page navigations, so gating page routes server-side causes a login redirect
// loop even for signed-in users.
func isAllowlisted(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	return r.URL.Path == "/api/config"
}

// firebaseAuthRequired verifies the Authorization: Bearer <Firebase ID token>
// on every request except allow-listed paths (§22.5.1). On success, stores
// *auth.User in the context via auth.WithUser. On failure:
//   - /api/* paths: 401 JSON {"error":"auth_required","code":"auth_required"}
//   - page routes: 303 redirect to /login?next=<originalURI>
//
// The POST /api/runs/{id}/suspend endpoint additionally accepts the token via
// the ?token= query param (sendBeacon cannot set custom headers — §22.5.4).
func (s *Server) firebaseAuthRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAllowlisted(r) {
			next.ServeHTTP(w, r)
			return
		}

		var (
			user *auth.User
			err  error
		)

		// sendBeacon suspend: accept ?token= when Authorization header is absent.
		if r.Method == http.MethodPost && suspendPathRE.MatchString(r.URL.Path) {
			if token := r.URL.Query().Get("token"); token != "" {
				user, err = s.auth.VerifyToken(r.Context(), token)
			}
		}

		// Standard path: Authorization: Bearer <token>.
		if user == nil {
			user, err = s.auth.FromRequest(r.Context(), r)
		}

		if err != nil || user == nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
			} else {
				http.Redirect(w, r,
					"/login?next="+url.QueryEscape(r.URL.RequestURI()),
					http.StatusSeeOther)
			}
			return
		}

		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}

// Flush implements http.Flusher so that SSE handlers can push frames through the
// logging wrapper without the type assertion w.(http.Flusher) failing (§14.4 of
// ARCHITECTURE_AI_CREATE.md).
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// generateID produces a pseudo-unique request ID (8 hex chars from time nanoseconds).
func generateID() string {
	return time.Now().Format("20060102150405.999999999")
}
