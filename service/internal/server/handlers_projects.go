package server

import (
	"encoding/json"
	"fmt"
	crand "crypto/rand"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"aifstudio/internal/auth"
	"aifstudio/internal/build"
	"aifstudio/internal/store"
)

var (
	projectIDRE = regexp.MustCompile(`^p-[0-9A-Z]{26}$`)
	buildIDRE   = regexp.MustCompile(`^b-[0-9A-Z]{26}$`)
)

// ── Request / Response types ──────────────────────────────────────────────────

type createProjectRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"` // initial source (optional, written to GCS not Firestore)
}

// projectResponse is the JSON shape returned by POST /api/projects and
// GET /api/projects/{id}. The source text is not inlined; callers fetch it
// via the signed URL (§10.0.2 of ARCHITECTURE_AI_CREATE.md).
type projectResponse struct {
	ID                 string `json:"id"`
	OwnerUID           string `json:"ownerUid"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	CreatedAt          string `json:"createdAt"`
	UpdatedAt          string `json:"updatedAt"`
	LatestBuildID      string `json:"latestBuildId"`
	Published          bool   `json:"published"`
	PublishedAt        string `json:"publishedAt,omitempty"`
	SourceBytes        int64  `json:"sourceBytes"`
	SourceURL          string `json:"sourceUrl"`
	SourceURLExpiresAt string `json:"sourceUrlExpiresAt,omitempty"`
}

// listProjectResponse is the per-item JSON shape returned by GET /api/projects.
// SourceBytes and SourceURL are intentionally omitted — fetching GCS object
// sizes for every project in a list would be too expensive.
type listProjectResponse struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Published     bool   `json:"published"`
	LatestBuildID string `json:"latestBuildId"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

type updateSourceRequest struct {
	Source string `json:"source"`
}

type updateSourceResponse struct {
	ID          string `json:"id"`
	UpdatedAt   string `json:"updatedAt"`
	SourceBytes int    `json:"sourceBytes"`
}

type createBuildResponse struct {
	ID            string `json:"id"`
	ProjectID     string `json:"projectId"`
	Status        string `json:"status"`
	CreatedAt     string `json:"createdAt"`
	QueuePosition int    `json:"queuePosition"`
}

type buildResponse struct {
	ID             string `json:"id"`
	ProjectID      string `json:"projectId"`
	Status         string `json:"status"`
	CreatedAt      string `json:"createdAt"`
	StartedAt      string `json:"startedAt,omitempty"`
	FinishedAt     string `json:"finishedAt,omitempty"`
	DurationMs     int64  `json:"durationMs,omitempty"`
	ArtifactFormat string `json:"artifactFormat"`
	ArtifactURL    string `json:"artifactURL"`
	LogURL         string `json:"logURL"`
	ErrorMessage   string `json:"errorMessage"`
	// HasTest is true when the project source contains a "Test me with" definition
	// (case-insensitive). Computed at read time from GCS; absent means false.
	HasTest bool `json:"hasTest"`
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// POST /api/projects
// Creates a Firestore metadata doc (no source field) and writes the initial
// source to GCS at projects/{id}/source.i7. Returns a signed URL so the client
// can immediately read the source (§10.0.1 of ARCHITECTURE_AI_CREATE.md).
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	var req createProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON body")
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 80 {
		writeError(w, http.StatusBadRequest, "invalid_name", "name must be 1-80 characters")
		return
	}
	if len(req.Description) > 2000 {
		writeError(w, http.StatusBadRequest, "invalid_description", "description must be at most 2,000 characters")
		return
	}
	if len(req.Source) > 500000 {
		writeError(w, http.StatusRequestEntityTooLarge, "source_too_large", "source must be at most 500,000 characters")
		return
	}

	id := "p-" + strings.ToUpper(ulid.MustNew(ulid.Timestamp(time.Now()), crand.Reader).String())
	now := time.Now().UTC()

	p := &store.Project{
		ID:          id,
		OwnerUID:    user.UID,
		Name:        req.Name,
		Description: req.Description,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if s.store != nil {
		if err := s.store.CreateProject(r.Context(), p); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "failed to create project")
			return
		}
		// Write initial source to GCS (even if empty — establishes the GCS object
		// so the migration branch never fires on subsequent reads of this project).
		if err := s.store.PutProjectSource(r.Context(), id, req.Source, now); err != nil {
			writeError(w, http.StatusBadGateway, "gcs_upload_failed", "failed to write source to storage")
			return
		}
	}

	resp := projectToResponse(p)
	resp.SourceBytes = int64(len(req.Source))

	if s.store != nil {
		// Local storage: serve source directly via the project source endpoint.
		resp.SourceURL = "/api/projects/" + id + "/source"
	}

	writeJSON(w, http.StatusCreated, resp)
}

// GET /api/projects/{id}
// Returns project metadata plus a signed URL for the GCS source object.
// The inline source text is never returned; clients fetch it via the signed URL
// (§10.0.2 of ARCHITECTURE_AI_CREATE.md).
func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !projectIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^p-[0-9A-Z]{26}$")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}

	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to get project")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	if p.OwnerUID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "not owner")
		return
	}

	resp := projectToResponse(p)

	// Determine sourceBytes and sourceUrl via attrs (no body download).
	size, exists, sizeErr := s.store.GetProjectSourceSize(r.Context(), id)
	if sizeErr != nil {
		// Non-fatal: log and return response with no source info.
		resp.SourceBytes = 0
	} else if exists {
		resp.SourceBytes = size
	} else {
		// GCS object absent: trigger lazy migration if Firestore has a legacy source.
		// GetProjectSource handles the Firestore→GCS copy and returns the source string.
		src, migErr := s.store.GetProjectSource(r.Context(), id)
		if migErr == nil && src != "" {
			resp.SourceBytes = int64(len(src))
			exists = true // object now present after migration
		}
	}

	if exists {
		// Local storage: serve source directly via the project source endpoint.
		resp.SourceURL = "/api/projects/" + id + "/source"
	}

	writeJSON(w, http.StatusOK, resp)
}

// PATCH /api/projects/{id}/source (and PUT alias for back-compat, §10.0.3)
// Writes source to GCS at projects/{id}/source.i7 and updates Firestore updatedAt.
// Never writes the source text to Firestore.
func (s *Server) handlePatchProjectSource(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !projectIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^p-[0-9A-Z]{26}$")
		return
	}

	var req updateSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON body")
		return
	}
	if len(req.Source) > 500000 {
		writeError(w, http.StatusRequestEntityTooLarge, "source_too_large", "source must be at most 500,000 characters")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}

	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to get project")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	if p.OwnerUID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "not owner")
		return
	}

	updatedAt := time.Now().UTC()
	if err := s.store.PutProjectSource(r.Context(), id, req.Source, updatedAt); err != nil {
		writeError(w, http.StatusBadGateway, "gcs_upload_failed", "failed to write source to storage")
		return
	}

	writeJSON(w, http.StatusOK, updateSourceResponse{
		ID:          id,
		UpdatedAt:   updatedAt.Format(time.RFC3339Nano),
		SourceBytes: len(req.Source),
	})
}

// POST /api/projects/{id}/builds
func (s *Server) handleCreateBuild(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !projectIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^p-[0-9A-Z]{26}$")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}

	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to get project")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	if p.OwnerUID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "not owner")
		return
	}

	// Read source from GCS (triggering lazy migration if needed, §10.0.2).
	source, err := s.store.GetProjectSource(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to read project source")
		return
	}

	buildID := "b-" + strings.ToUpper(ulid.MustNew(ulid.Timestamp(time.Now()), crand.Reader).String())
	now := time.Now().UTC()

	b := &store.Build{
		ID:        buildID,
		ProjectID: id,
		OwnerUID:  user.UID,
		Status:    build.StatusPending,
		CreatedAt: now,
	}

	if s.builder != nil {
		// Builder handles in-memory conflict detection and store creation.
		if err := s.builder.StartBuild(r.Context(), b, source); err != nil {
			if strings.Contains(err.Error(), "conflict:409:") {
				activeID := s.builder.ActiveBuildID(id)
				writeJSON(w, http.StatusConflict, map[string]string{
					"error":         "another build is already active",
					"code":          "build_conflict",
					"activeBuildId": activeID,
				})
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", fmt.Sprintf("failed to start build: %v", err))
			return
		}
	} else {
		// No builder (e.g. tests or dev mode): check store for pending/running builds,
		// then create the build record ourselves.
		if s.store != nil {
			existing, err := s.store.ListBuildsByProject(r.Context(), id, 100)
			if err == nil {
				for _, eb := range existing {
					if eb.Status == build.StatusPending || eb.Status == build.StatusRunning {
						writeJSON(w, http.StatusConflict, map[string]string{
							"error":         "another build is already active",
							"code":          "build_conflict",
							"activeBuildId": eb.ID,
						})
						return
					}
				}
			}
			if err := s.store.CreateBuild(r.Context(), b); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "failed to create build")
				return
			}
		}
	}

	writeJSON(w, http.StatusAccepted, createBuildResponse{
		ID:        buildID,
		ProjectID: id,
		Status:    build.StatusPending,
		CreatedAt: now.Format(time.RFC3339Nano),
	})
}

// GET /api/projects/{id}/builds/{buildId}
func (s *Server) handleGetBuild(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	projectID := r.PathValue("id")
	bID := r.PathValue("buildId")

	if !projectIDRE.MatchString(projectID) {
		writeError(w, http.StatusBadRequest, "invalid_project_id", "project id format invalid")
		return
	}
	if !buildIDRE.MatchString(bID) {
		writeError(w, http.StatusBadRequest, "invalid_build_id", "build id format invalid")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusNotFound, "not_found", "build not found")
		return
	}

	b, err := s.store.GetBuild(r.Context(), bID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to get build")
		return
	}
	if b == nil || b.ProjectID != projectID {
		writeError(w, http.StatusNotFound, "not_found", "build not found")
		return
	}
	if b.OwnerUID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "not owner")
		return
	}

	resp := buildToResponse(b)

	// Direct-serve URLs — files are streamed by owner-gated GET handlers.
	if b.ArtifactPath != "" {
		resp.ArtifactURL = "/api/builds/" + bID + "/artifact"
	}
	if b.LogPath != "" {
		resp.LogURL = "/api/builds/" + bID + "/log"
	}

	// Compute HasTest: read the project source from GCS and scan for "Test me with "
	// (case-insensitive). Non-fatal: on error HasTest stays false.
	if s.store != nil && b.ProjectID != "" {
		if src, srcErr := s.store.GetProjectSource(r.Context(), b.ProjectID); srcErr == nil {
			resp.HasTest = strings.Contains(strings.ToLower(src), `test me with "`)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// GET /api/projects
// Returns the authenticated user's own projects ordered by updatedAt DESC.
// Source bytes are not fetched — metadata only. No GCS calls on this path.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	if s.store == nil {
		writeJSON(w, http.StatusOK, []listProjectResponse{})
		return
	}

	projects, err := s.store.ListProjectsByOwner(r.Context(), user.UID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list projects")
		return
	}

	out := make([]listProjectResponse, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectToListResponse(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /api/projects/{id}
// Cascade-deletes a project: GCS build artifacts, Firestore builds, GCS
// run transcripts, Firestore runs, AI turns subcollection, GCS source file,
// and finally the project Firestore document. Returns 204 No Content.
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !projectIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^p-[0-9A-Z]{26}$")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}

	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to get project")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	if p.OwnerUID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "not owner")
		return
	}

	ctx := r.Context()

	// 1. Delete GCS build artifacts then Firestore build docs.
	builds, err := s.store.ListBuildsByProject(ctx, id, 1000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list builds")
		return
	}
	for _, b := range builds {
		// Always delete everything under builds/{buildId}/ — failed builds may
		// have a log blob (builds/{buildId}/build.log) but no artifact.
		s.store.DeleteBlobPrefix(ctx, "builds/"+b.ID+"/") //nolint:errcheck
	}
	if _, err := s.store.DeleteBuildsForProject(ctx, id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to delete builds")
		return
	}

	// 2. Delete GCS run data then Firestore run docs.
	// GCS cleanup is best-effort per run; Firestore deletion is bulk via
	// DeleteRunsForProject so run docs are removed even when GCS cleanup fails.
	runs, err := s.store.ListRunsByProject(ctx, id, 500)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list runs")
		return
	}
	for _, run := range runs {
		// sessions/{id}/ covers save files and new-format transcripts
		// (sessions/{id}/transcript.txt written by the runner).
		s.store.DeleteBlobPrefix(ctx, "sessions/"+run.ID+"/") //nolint:errcheck
		// transcripts/{id} (no trailing slash) covers legacy transcripts/{id}.txt
		// blobs written by older versions of the runner.
		s.store.DeleteBlobPrefix(ctx, "transcripts/"+run.ID) //nolint:errcheck
	}
	if _, err := s.store.DeleteRunsForProject(ctx, id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to delete runs")
		return
	}

	// 3. Delete AI turns subcollection.
	if _, err := s.store.DeleteAIConversation(ctx, id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to delete AI turns")
		return
	}

	// 4. Delete GCS source file.
	if err := s.store.DeleteProjectSource(ctx, id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to delete project source")
		return
	}

	// 5. Delete the project Firestore document.
	if err := s.store.DeleteProject(ctx, id); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to delete project")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handlePatchProject updates the name and description of a project.
//
// Route: PATCH /api/projects/{id}
// Body:  {"name":"...","description":"..."}
// Owner-only. Name is required and capped at 120 characters.
func (s *Server) handlePatchProject(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}
	id := r.PathValue("id")
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil || p == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	if p.OwnerUID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "project does not belong to you")
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name_required", "name must not be empty")
		return
	}
	if len(req.Name) > 120 {
		writeError(w, http.StatusBadRequest, "name_too_long", "name must be 120 characters or fewer")
		return
	}
	now := time.Now().UTC()
	if err := s.store.UpdateProjectMeta(r.Context(), id, req.Name, req.Description, now); err != nil {
		slog.Error("UpdateProjectMeta failed", "err", err, "project_id", id)
		writeError(w, http.StatusInternalServerError, "internal", "failed to update project")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":        req.Name,
		"description": req.Description,
		"updatedAt":   now,
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func projectToResponse(p *store.Project) projectResponse {
	resp := projectResponse{
		ID:            p.ID,
		OwnerUID:      p.OwnerUID,
		Name:          p.Name,
		Description:   p.Description,
		CreatedAt:     p.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:     p.UpdatedAt.UTC().Format(time.RFC3339Nano),
		LatestBuildID: p.LatestBuildID,
		Published:     p.Published,
	}
	if p.PublishedAt != nil {
		resp.PublishedAt = p.PublishedAt.UTC().Format(time.RFC3339Nano)
	}
	return resp
}

func projectToListResponse(p *store.Project) listProjectResponse {
	return listProjectResponse{
		ID:            p.ID,
		Name:          p.Name,
		Description:   p.Description,
		Published:     p.Published,
		LatestBuildID: p.LatestBuildID,
		CreatedAt:     p.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:     p.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// GET /api/projects/{id}/source
// Returns the Inform 7 source text for the project. Owner-only.
// Replaces the former GCS signed-URL pattern: the client requests the source
// directly from the service rather than fetching a pre-signed object URL.
func (s *Server) handleGetProjectSource(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !projectIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^p-[0-9A-Z]{26}$")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}

	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to get project")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "project not found")
		return
	}
	if p.OwnerUID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "not owner")
		return
	}

	src, err := s.store.GetProjectSource(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to read project source")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(src))
}

func buildToResponse(b *store.Build) buildResponse {
	resp := buildResponse{
		ID:             b.ID,
		ProjectID:      b.ProjectID,
		Status:         b.Status,
		CreatedAt:      b.CreatedAt.UTC().Format(time.RFC3339Nano),
		ArtifactFormat: b.ArtifactFormat,
		ErrorMessage:   b.ErrorMessage,
	}
	if b.StartedAt != nil {
		resp.StartedAt = b.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if b.FinishedAt != nil {
		resp.FinishedAt = b.FinishedAt.UTC().Format(time.RFC3339Nano)
		if b.StartedAt != nil {
			resp.DurationMs = b.FinishedAt.Sub(*b.StartedAt).Milliseconds()
		}
	}
	return resp
}
