package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"storycloud/internal/auth"
)

var turnIDRE = regexp.MustCompile(`^t-[0-9A-Z]{26}$`)

// historyItem is the per-turn JSON shape returned by GET /api/projects/{id}/history.
type historyItem struct {
	ID          string `json:"id"`
	CreatedAt   string `json:"createdAt"`
	Kind        string `json:"kind"`
	UserMessage string `json:"userMessage"`
	HasSource   bool   `json:"hasSource"`
}

// turnSourceResponse is the JSON shape returned by GET /api/projects/{id}/history/{turnId}/source.
type turnSourceResponse struct {
	Source string `json:"source"`
}

// GET /api/projects/{id}/history
// Returns all AI turns for the project ordered oldest first.
// userMessage is truncated to 120 runes. hasSource is true when the GCS
// after.i7 blob for the turn exists (inferred from SourceAfter populated by
// ListAIConversation's readAITurnBlobs pass).
// Auth required; must be project owner.
func (s *Server) handleListHistory(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(w, http.StatusOK, []historyItem{})
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

	turns, err := s.store.ListAIConversation(r.Context(), id, 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to list AI turns")
		return
	}

	items := make([]historyItem, 0, len(turns))
	for _, t := range turns {
		msg := t.UserMessage
		if runes := []rune(msg); len(runes) > 120 {
			msg = string(runes[:120])
		}
		items = append(items, historyItem{
			ID:          t.ID,
			CreatedAt:   t.ModelRequestedAt.UTC().Format(time.RFC3339Nano),
			Kind:        t.Kind,
			UserMessage: msg,
			HasSource:   t.SourceAfter != "",
		})
	}
	writeJSON(w, http.StatusOK, items)
}

// GET /api/projects/{id}/history/{turnId}/source
// Returns the after.i7 source snapshot for the given AI turn.
// 404 if the blob does not exist.
// Auth required; must be project owner.
func (s *Server) handleGetTurnSource(w http.ResponseWriter, r *http.Request) {
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

	turnID := r.PathValue("turnId")
	if !turnIDRE.MatchString(turnID) {
		writeError(w, http.StatusBadRequest, "invalid_turn_id", "turnId must match ^t-[0-9A-Z]{26}$")
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

	source, exists, err := s.store.GetAITurnAfterSource(r.Context(), id, turnID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to read turn source")
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "not_found", "turn source not found")
		return
	}

	writeJSON(w, http.StatusOK, turnSourceResponse{Source: source})
}

// PUT /api/projects/{id}/source
// Writes source text directly to GCS at projects/{projectID}/source.i7.
// No AI turn is created. Updates project.updatedAt in Firestore.
// Returns 204 No Content on success.
// Auth required; must be project owner. Max source size: 500 KB.
func (s *Server) handlePutProjectSource(w http.ResponseWriter, r *http.Request) {
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

	if err := s.store.PutProjectSource(r.Context(), id, req.Source, time.Now().UTC()); err != nil {
		writeError(w, http.StatusBadGateway, "gcs_upload_failed", "failed to write source to storage")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
