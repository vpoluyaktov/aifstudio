package server

import (
	"encoding/json"
	"log"
	"net/http"
)

// pageData is the minimum data injected into every page template.
// All pages share Version and Environment; page-specific fields are added
// in the concrete page data types below.
type pageData struct {
	Version     string
	Environment string
	CacheBuster string
}

// ── Health ──────────────────────────────────────────────────────────────────

type healthResponse struct {
	Status      string `json:"status"`
	Version     string `json:"version"`
	Environment string `json:"environment"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp := healthResponse{
		Status:      "ok",
		Version:     s.cfg.Version,
		Environment: s.cfg.Environment,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("handleHealth encode error: %v", err)
	}
}

// ── UI page handlers ─────────────────────────────────────────────────────────
// Each handler renders the corresponding embedded template. Dynamic data is
// loaded client-side via fetch() calls to the JSON API.

// handlePageIndex renders the IFDB search page (GET /{$}).
func (s *Server) handlePageIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pageData{
		Version:     s.cfg.Version,
		Environment: s.cfg.Environment,
		CacheBuster: s.cacheBuster,
	}
	if err := s.tmpl.Index.Execute(w, data); err != nil {
		log.Printf("handlePageIndex template error: %v", err)
	}
}

// gameDetailData extends pageData with the IFDB game ID embedded in the page.
type gameDetailData struct {
	pageData
	IfdbId string
}

// handlePageGameDetail renders the game detail page (GET /games/{ifdbId}).
func (s *Server) handlePageGameDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := gameDetailData{
		pageData: pageData{
			Version:     s.cfg.Version,
			Environment: s.cfg.Environment,
			CacheBuster: s.cacheBuster,
		},
		IfdbId: r.PathValue("ifdbId"),
	}
	if err := s.tmpl.GameDetail.Execute(w, data); err != nil {
		log.Printf("handlePageGameDetail template error: %v", err)
	}
}

// playData extends pageData with the run session ID.
type playData struct {
	pageData
	RunId string
}

// handlePagePlay renders the WebSocket terminal UI (GET /play/{runId}).
func (s *Server) handlePagePlay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := playData{
		pageData: pageData{
			Version:     s.cfg.Version,
			Environment: s.cfg.Environment,
			CacheBuster: s.cacheBuster,
		},
		RunId: r.PathValue("runId"),
	}
	if err := s.tmpl.Play.Execute(w, data); err != nil {
		log.Printf("handlePagePlay template error: %v", err)
	}
}

// handlePageCreate renders the new-project creation form (GET /create).
func (s *Server) handlePageCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pageData{
		Version:     s.cfg.Version,
		Environment: s.cfg.Environment,
		CacheBuster: s.cacheBuster,
	}
	if err := s.tmpl.Create.Execute(w, data); err != nil {
		log.Printf("handlePageCreate template error: %v", err)
	}
}

// projectDetailData extends pageData with the project ID.
type projectDetailData struct {
	pageData
	ProjectId string
}

// handlePageHistory renders the user's game history page (GET /history).
func (s *Server) handlePageHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pageData{
		Version:     s.cfg.Version,
		Environment: s.cfg.Environment,
		CacheBuster: s.cacheBuster,
	}
	if err := s.tmpl.History.Execute(w, data); err != nil {
		log.Printf("handlePageHistory template error: %v", err)
	}
}

// handlePageProjectDetail renders the project editor and builds list (GET /projects/{id}).
func (s *Server) handlePageProjectDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := projectDetailData{
		pageData: pageData{
			Version:     s.cfg.Version,
			Environment: s.cfg.Environment,
			CacheBuster: s.cacheBuster,
		},
		ProjectId: r.PathValue("id"),
	}
	if err := s.tmpl.ProjectDetail.Execute(w, data); err != nil {
		log.Printf("handlePageProjectDetail template error: %v", err)
	}
}

// handlePageLogin renders the Firebase email/password sign-in form (GET /login).
// Unauthenticated — allow-listed in firebaseAuthRequired.
func (s *Server) handlePageLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pageData{
		Version:     s.cfg.Version,
		Environment: s.cfg.Environment,
		CacheBuster: s.cacheBuster,
	}
	if err := s.tmpl.Login.Execute(w, data); err != nil {
		log.Printf("handlePageLogin template error: %v", err)
	}
}

// handlePageProjects renders the My Projects list page (GET /projects).
func (s *Server) handlePageProjects(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pageData{
		Version:     s.cfg.Version,
		Environment: s.cfg.Environment,
		CacheBuster: s.cacheBuster,
	}
	if err := s.tmpl.Projects.Execute(w, data); err != nil {
		log.Printf("handlePageProjects template error: %v", err)
	}
}

// handlePageRegister renders the Firebase account creation form (GET /register).
// Unauthenticated — allow-listed in firebaseAuthRequired.
func (s *Server) handlePageRegister(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pageData{
		Version:     s.cfg.Version,
		Environment: s.cfg.Environment,
		CacheBuster: s.cacheBuster,
	}
	if err := s.tmpl.Register.Execute(w, data); err != nil {
		log.Printf("handlePageRegister template error: %v", err)
	}
}
