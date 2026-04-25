// Package server — /api/auth/* handlers (register, login, logout, me).
// These handlers require that Server.auth is a *auth.SessionAuth; when the
// server is constructed with a MockVerifier (in tests), the auth endpoints
// return 503 Service Unavailable so tests can ignore them.
package server

import (
	"encoding/json"
	"net/http"
	"net/mail"
	"strings"

	"aifstudio/internal/auth"
)

// sessionAuth type-asserts Server.auth to *auth.SessionAuth. Returns nil when
// the server is in test mode (MockVerifier).
func (s *Server) sessionAuth() *auth.SessionAuth {
	sa, _ := s.auth.(*auth.SessionAuth)
	return sa
}

// ── POST /api/auth/register ───────────────────────────────────────────────────

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
}

type userResponse struct {
	UID         string `json:"uid"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

func (s *Server) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	sa := s.sessionAuth()
	if sa == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "session auth not configured")
		return
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.DisplayName = strings.TrimSpace(req.DisplayName)

	// Validate email.
	if len(req.Email) == 0 || len(req.Email) > 254 {
		writeError(w, http.StatusBadRequest, "invalid_email", "email must be 1–254 characters")
		return
	}
	if _, err := mail.ParseAddress(req.Email); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_email", "invalid email address")
		return
	}

	// Validate password.
	if len(req.Password) < 8 || len(req.Password) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_password", "password must be 8–128 characters")
		return
	}

	// Validate display name.
	if len(req.DisplayName) == 0 || len(req.DisplayName) > 80 {
		writeError(w, http.StatusBadRequest, "invalid_display_name", "displayName must be 1–80 characters")
		return
	}

	user, err := sa.Register(r.Context(), w, r, req.Email, req.Password, req.DisplayName)
	if err != nil {
		if err == auth.ErrEmailTaken {
			writeError(w, http.StatusConflict, "email_taken", "email already registered")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "registration failed")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"user": userResponse{
			UID:         user.UID,
			Email:       user.Email,
			DisplayName: user.Name,
		},
	})
}

// ── POST /api/auth/login ──────────────────────────────────────────────────────

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	sa := s.sessionAuth()
	if sa == nil {
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "session auth not configured")
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}

	req.Email = strings.TrimSpace(strings.ToLower(req.Email))

	user, err := sa.Login(r.Context(), w, r, req.Email, req.Password)
	if err != nil {
		// Timing-safe: same 401 for wrong email or wrong password.
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"user": userResponse{
			UID:         user.UID,
			Email:       user.Email,
			DisplayName: user.Name,
		},
	})
}

// ── POST /api/auth/logout ─────────────────────────────────────────────────────

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	sa := s.sessionAuth()
	if sa == nil {
		// In test mode just return 204.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sa.Logout(r.Context(), w, r)
	w.WriteHeader(http.StatusNoContent)
}

// ── GET /api/auth/me ──────────────────────────────────────────────────────────

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, userResponse{
		UID:         user.UID,
		Email:       user.Email,
		DisplayName: user.Name,
	})
}
