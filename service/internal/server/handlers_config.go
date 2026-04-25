package server

import "net/http"

// handleConfig serves the Firebase Web SDK configuration (GET /api/config).
// Unauthenticated — allow-listed in firebaseAuthRequired (§22.4.4, §22.6.1).
//
// Response 200 when Firebase is configured:
//
//	{
//	  "firebase": {"apiKey":"...","authDomain":"...","projectId":"..."},
//	  "environment": "staging",
//	  "version": "1.0.x"
//	}
//
// Response 503 when FIREBASE_WEB_API_KEY or GCP_PROJECT_ID is not set
// (local dev mode — frontend displays a stub "dev@local" sign-in state).
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	apiKey := s.cfg.FirebaseWebAPIKey
	projectID := s.cfg.ProjectID
	authDomain := s.cfg.FirebaseAuthDomain

	if apiKey == "" || projectID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "auth_disabled",
			"code":  "auth_disabled",
		})
		return
	}

	// Default authDomain when not set explicitly.
	if authDomain == "" {
		authDomain = projectID + ".firebaseapp.com"
	}

	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Vary", "Accept-Encoding")

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"firebase": map[string]string{
			"apiKey":     apiKey,
			"authDomain": authDomain,
			"projectId":  projectID,
		},
		"environment": s.cfg.Environment,
		"version":     s.cfg.Version,
	})
}
