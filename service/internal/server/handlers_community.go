package server

import (
	crand "crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"aifstudio/internal/auth"
	"aifstudio/internal/store"
)

// ── handleTogglePublish — PATCH /api/projects/{id}/publish ───────────────────

type publishRequest struct {
	Published *bool `json:"published"` // pointer so we can detect missing field
}

type publishResponse struct {
	ID          string  `json:"id"`
	Published   bool    `json:"published"`
	PublishedAt *string `json:"publishedAt,omitempty"`
}

func (s *Server) handleTogglePublish(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !projectIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid project ID")
		return
	}

	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Published == nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "request must include published field")
		return
	}

	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to load project")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	if p.OwnerUID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "access denied")
		return
	}

	// Publishing requires a successful build and non-empty source.
	if *req.Published {
		if p.LatestBuildID == "" {
			writeError(w, http.StatusUnprocessableEntity, "build_required", "project must have a successful build to publish")
			return
		}
		b, berr := s.store.GetBuild(r.Context(), p.LatestBuildID)
		if berr != nil || b == nil || b.Status != "succeeded" {
			writeError(w, http.StatusUnprocessableEntity, "build_required", "latest build must have status 'succeeded' to publish")
			return
		}
		size, exists, serr := s.store.GetProjectSourceSize(r.Context(), id)
		if serr != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to check source")
			return
		}
		if !exists || size == 0 {
			writeError(w, http.StatusConflict, "empty_source", "project source is empty; save source before publishing")
			return
		}
	}

	now := time.Now().UTC()
	if err := s.store.SetProjectPublished(r.Context(), id, *req.Published, now); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to update published status")
		return
	}

	resp := publishResponse{
		ID:        id,
		Published: *req.Published,
	}
	if *req.Published {
		ts := now.Format(time.RFC3339Nano)
		resp.PublishedAt = &ts
	} else if p.PublishedAt != nil {
		ts := p.PublishedAt.Format(time.RFC3339Nano)
		resp.PublishedAt = &ts
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── handleCommunityList — GET /api/community ──────────────────────────────────

type communityGameEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	OwnerUID      string `json:"ownerUid"`
	PublishedAt   string `json:"publishedAt"`
	LatestBuildID string `json:"latestBuildId"`
}

type communityListResponse struct {
	Games []communityGameEntry `json:"games"`
}

func (s *Server) handleCommunityList(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	limit := 50
	if lq := r.URL.Query().Get("limit"); lq != "" {
		if v, err := strconv.Atoi(lq); err == nil {
			limit = v
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}

	projects, err := s.store.ListPublishedProjects(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list published projects")
		return
	}

	games := make([]communityGameEntry, 0, len(projects))
	for _, p := range projects {
		var publishedAt string
		if p.PublishedAt != nil {
			publishedAt = p.PublishedAt.Format(time.RFC3339Nano)
		}
		games = append(games, communityGameEntry{
			ID:            p.ID,
			Name:          p.Name,
			Description:   p.Description,
			OwnerUID:      p.OwnerUID,
			PublishedAt:   publishedAt,
			LatestBuildID: p.LatestBuildID,
		})
	}
	writeJSON(w, http.StatusOK, communityListResponse{Games: games})
}

// ── handleCommunityPlay — POST /api/community/{id}/play ──────────────────────

func (s *Server) handleCommunityPlay(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !projectIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "invalid project ID")
		return
	}

	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to load project")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	if !p.Published {
		writeError(w, http.StatusForbidden, "not_published", "project is not published")
		return
	}
	if p.LatestBuildID == "" {
		writeError(w, http.StatusUnprocessableEntity, "no_build", "project has no build")
		return
	}

	b, berr := s.store.GetBuild(r.Context(), p.LatestBuildID)
	if berr != nil || b == nil || b.Status != "succeeded" {
		writeError(w, http.StatusUnprocessableEntity, "build_not_ready", "latest build is not ready")
		return
	}

	// Create a run sourced from the build.
	// ArtifactURL is intentionally empty — the runner fetches the artifact
	// directly from local blob storage via store.DownloadBlob.
	runID := "r-" + strings.ToUpper(ulid.MustNew(ulid.Timestamp(time.Now()), crand.Reader).String())
	now := time.Now().UTC()
	run := &store.Run{
		ID:           runID,
		SourceType:   "build",
		BuildID:      p.LatestBuildID,
		ProjectID:    p.ID,
		Title:        p.Name,
		Format:       b.ArtifactFormat,
		UserID:       user.UID,
		Status:       "pending",
		CreatedAt:    now,
		LastActiveAt: &now,
	}
	if err := s.store.CreateRun(r.Context(), run); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to create run")
		return
	}

	slog.Info("community play", "projectID", id, "runID", runID, "userUID", user.UID)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":        runID,
		"status":    "pending",
		"startURL":  "/api/runs/" + runID + "/start",
		"createdAt": now.Format(time.RFC3339Nano),
	})
}

// ── handlePageCommunity — GET /community ──────────────────────────────────────

func (s *Server) handlePageCommunity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pageData{
		Version:     s.cfg.Version,
		Environment: s.cfg.Environment,
		CacheBuster: s.cacheBuster,
	}
	if err := s.tmpl.Community.Execute(w, data); err != nil {
		slog.Error("handlePageCommunity template", "err", err)
	}
}
