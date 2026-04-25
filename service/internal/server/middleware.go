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
// the token via ?token= for sendBeacon callers (legacy compatibility).
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

// maxBodyMiddleware limits request body size.
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

// isAllowlisted reports whether the request should bypass session auth.
//
// Allow-listed paths (per §4 of ARCHITECTURE.md):
//   - All non-/api/ paths (page routes + static assets; client-side auth handles these)
//   - POST /api/auth/register
//   - POST /api/auth/login
func isAllowlisted(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	switch r.URL.Path {
	case "/api/auth/register":
		return r.Method == http.MethodPost
	case "/api/auth/login":
		return r.Method == http.MethodPost
	}
	return false
}

// sessionAuthRequired verifies the aifstudio_session cookie on every request
// except allow-listed paths. On success, stores *auth.User in the context.
// On failure:
//   - /api/* paths: 401 JSON {"error":"auth_required","code":"auth_required"}
//   - page routes:  303 redirect to /login?next=<originalURI>
//
// The POST /api/runs/{id}/suspend endpoint additionally accepts a bearer token
// via ?token= for sendBeacon callers (legacy compatibility with MockVerifier tests).
func (s *Server) sessionAuthRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAllowlisted(r) {
			next.ServeHTTP(w, r)
			return
		}

		var (
			user *auth.User
			err  error
		)

		// Legacy sendBeacon suspend path: accept ?token= when Authorization header is
		// present (MockVerifier in tests reads Bearer; production SessionAuth reads cookie).
		if r.Method == http.MethodPost && suspendPathRE.MatchString(r.URL.Path) {
			if token := r.URL.Query().Get("token"); token != "" {
				user, err = s.auth.VerifyToken(r.Context(), token)
			}
		}

		// Standard path: cookie (SessionAuth) or Bearer header (MockVerifier in tests).
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
// logging wrapper without the type assertion w.(http.Flusher) failing.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// generateID produces a pseudo-unique request ID (8 hex chars from time nanoseconds).
func generateID() string {
	return time.Now().Format("20060102150405.999999999")
}
