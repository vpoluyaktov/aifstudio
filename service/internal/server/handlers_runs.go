package server

import (
	crand "crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"storycloud/internal/auth"
	"storycloud/internal/ifdb"
	"storycloud/internal/runner"
	"storycloud/internal/store"
)

var (
	runIDRE   = regexp.MustCompile(`^r-[0-9A-Z]{26}$`)
	urlScheme = regexp.MustCompile(`^https://`)
)

type createRunRequest struct {
	SourceType  string `json:"sourceType"`
	IFDBId      string `json:"ifdbId"`
	Format      string `json:"format"`
	ArtifactURL string `json:"artifactUrl"`
	BuildID     string `json:"buildId"`
}

type runResponse struct {
	ID            string `json:"id"`
	SourceType    string `json:"sourceType"`
	IFDBId        string `json:"ifdbId,omitempty"`
	Title         string `json:"title,omitempty"`
	Format        string `json:"format,omitempty"`
	Status        string `json:"status"`
	CreatedAt     string `json:"createdAt"`
	StartedAt     string `json:"startedAt,omitempty"`
	LastActiveAt  string `json:"lastActiveAt,omitempty"`
	TurnCount     int    `json:"turnCount,omitempty"`
	StartURL      string `json:"startURL"`
	TranscriptURL string `json:"transcriptURL,omitempty"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON body")
		return
	}

	switch req.SourceType {
	case "ifdb", "url", "build":
	default:
		writeError(w, http.StatusBadRequest, "invalid_source_type", "sourceType must be ifdb, url, or build")
		return
	}

	if req.SourceType == "ifdb" {
		if !ifdbIDRE.MatchString(req.IFDBId) {
			writeError(w, http.StatusBadRequest, "invalid_ifdb_id", "ifdbId must match ^[a-z0-9]{10,32}$")
			return
		}
	}
	if req.SourceType == "url" {
		if req.ArtifactURL == "" || !urlScheme.MatchString(req.ArtifactURL) {
			writeError(w, http.StatusBadRequest, "invalid_artifact_url", "artifactUrl must be an https:// URL")
			return
		}
	}
	if req.SourceType == "build" {
		if req.BuildID == "" {
			writeError(w, http.StatusBadRequest, "missing_build_id", "buildId required when sourceType=build")
			return
		}
	}

	// Resolve artifact URL and game title for IFDB source.
	artifactURL := req.ArtifactURL
	format := req.Format
	title := ""
	var candidateURLs []string
	if req.SourceType == "ifdb" {
		clientIP := ifdb.ExtractClientIP(r.Header.Get("X-Forwarded-For"), r.RemoteAddr)
		game, _, err := s.ifdbClient.GetGame(r.Context(), clientIP, req.IFDBId)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "ifdb_error", "could not fetch IFDB game")
			return
		}
		if len(game.DownloadLinks) == 0 {
			if len(game.UnsupportedFormats) > 0 {
				msg := "Unable to start the game: The game format is not supported yet"
				for _, f := range game.UnsupportedFormats {
					if f != "" && f != "*" {
						msg = "Unable to start the game: The game format (" + f + ") is not supported yet"
						break
					}
				}
				writeError(w, http.StatusUnprocessableEntity, "unsupported_format", msg)
				return
			}
			writeError(w, http.StatusNotFound, "no_download_links", "This game is not available for download due to licensing reasons")
			return
		}
		var link ifdb.DownloadLink
		var found bool
		if format != "" {
			for _, l := range game.DownloadLinks {
				if l.Format == format {
					link = l
					found = true
					break
				}
			}
		} else {
			link, found = ifdb.PreferredFormat(game.DownloadLinks)
		}
		if !found {
			writeError(w, http.StatusBadRequest, "no_compatible_format", "no compatible format found for IFDB game")
			return
		}
		artifactURL = link.URL
		format = link.Format
		title = game.Title
		// Build ordered candidate list: preferred link first, then all others.
		candidateURLs = make([]string, 0, len(game.DownloadLinks))
		candidateURLs = append(candidateURLs, link.URL)
		for _, dl := range game.DownloadLinks {
			if dl.URL != link.URL {
				candidateURLs = append(candidateURLs, dl.URL)
			}
		}
	}

	if req.SourceType == "build" {
		build, err := s.store.GetBuild(r.Context(), req.BuildID)
		if err != nil || build == nil {
			slog.Error("handleRunCreate: GetBuild failed", "err", err, "build_id", req.BuildID)
			writeError(w, http.StatusInternalServerError, "internal", "failed to load build")
			return
		}
		project, err := s.store.GetProject(r.Context(), build.ProjectID)
		if err != nil || project == nil {
			slog.Error("handleRunCreate: GetProject failed", "err", err, "project_id", build.ProjectID)
			writeError(w, http.StatusInternalServerError, "internal", "failed to load project")
			return
		}
		title = project.Name
	}

	id := "r-" + strings.ToUpper(ulid.MustNew(ulid.Timestamp(time.Now()), crand.Reader).String())
	now := time.Now().UTC()

	run := &store.Run{
		ID:            id,
		SourceType:    req.SourceType,
		IFDBId:        req.IFDBId,
		Title:         title,
		Format:        format,
		ArtifactURL:   artifactURL,
		CandidateURLs: candidateURLs,
		BuildID:       req.BuildID,
		UserID:        user.UID,
		Status:        runner.StatusPending,
		CreatedAt:     now,
	}

	if s.store != nil {
		if err := s.store.CreateRun(r.Context(), run); err != nil {
			slog.Error("store.CreateRun failed", "err", err, "run_id", run.ID)
			writeError(w, http.StatusInternalServerError, "internal", "failed to create run")
			return
		}
	}

	writeJSON(w, http.StatusCreated, runToResponse(run))
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !runIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^r-[0-9A-Z]{26}$")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		slog.Error("store.GetRun failed", "err", err, "run_id", id)
		writeError(w, http.StatusInternalServerError, "internal", "failed to get run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	resp := runToResponse(run)
	if run.TranscriptPath != "" && run.Status == runner.StatusFinished {
		su, err := s.store.SignedReadURL(r.Context(), run.TranscriptPath, time.Hour)
		if err == nil {
			resp.TranscriptURL = su.URL
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRunStart initialises (or reattaches to) a live interpreter session and
// returns the startup/current-prompt text.
// GET /api/runs/{id}/start
func (s *Server) handleRunStart(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !runIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^r-[0-9A-Z]{26}$")
		return
	}
	if s.store == nil || s.runner == nil {
		writeError(w, http.StatusInternalServerError, "internal", "service not available")
		return
	}
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		slog.Error("store.GetRun failed", "err", err, "run_id", id, "handler", "handleRunStart")
		writeError(w, http.StatusInternalServerError, "internal", "failed to get run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	// Ownership check — prevent a signed-in user from taking over another's run.
	if run.UserID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "run does not belong to you")
		return
	}

	wasResumed := run.Status == runner.StatusSuspended

	switch run.Status {
	case runner.StatusPending, runner.StatusRunning, runner.StatusSuspended:
		// ok to (re-)start
	default:
		writeError(w, http.StatusGone, "run_ended", "run has already ended")
		return
	}

	// For build-sourced runs the ArtifactURL is not stored — resolve a fresh
	// signed URL from the build's GCS artifact path at play time.
	if run.SourceType == "build" && run.BuildID != "" {
		build, berr := s.store.GetBuild(r.Context(), run.BuildID)
		if berr != nil || build == nil {
			slog.Error("handleRunStart: GetBuild failed", "err", berr, "build_id", run.BuildID)
			writeError(w, http.StatusInternalServerError, "internal", "failed to load build for run")
			return
		}
		su, serr := s.store.SignedReadURL(r.Context(), build.ArtifactPath, 2*time.Hour)
		if serr != nil {
			slog.Error("handleRunStart: SignedReadURL failed", "err", serr, "path", build.ArtifactPath)
			writeError(w, http.StatusInternalServerError, "internal", "failed to sign artifact URL")
			return
		}
		run.ArtifactURL = su.URL
	}

	// Reuse an alive session if one exists; return its last output so the page
	// has context without re-spawning (idempotent on page reload).
	if existing := s.runner.GetSession(id); existing != nil && existing.IsAlive() {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"output":    existing.LastOutput(),
			"turnCount": run.TurnCount,
			"resumed":   wasResumed,
		})
		return
	}

	session := s.runner.CreateSession(run)
	output, startErr := session.Start(r.Context())
	if startErr != nil {
		if runner.IsUpstreamHTTPError(startErr) {
			slog.Warn("upstream artifact fetch failed", "err", startErr, "run_id", id)
			writeError(w, http.StatusBadGateway, "upstream_unavailable", "This game's download link is no longer available. The file may have moved or been removed from the host site.")
			return
		}
		slog.Error("session.Start failed", "err", startErr, "run_id", id)
		writeError(w, http.StatusInternalServerError, "internal", startErr.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"output":    output,
		"turnCount": run.TurnCount,
		"resumed":   wasResumed,
	})
}

type commandRequest struct {
	Input string `json:"input"`
}

// handleRunCommand sends a text command to the live interpreter and returns the response.
// POST /api/runs/{id}/command
func (s *Server) handleRunCommand(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !runIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^r-[0-9A-Z]{26}$")
		return
	}
	if s.store == nil || s.runner == nil {
		writeError(w, http.StatusInternalServerError, "internal", "service not available")
		return
	}
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		slog.Error("store.GetRun failed", "err", err, "run_id", id, "handler", "handleRunCommand")
		writeError(w, http.StatusInternalServerError, "internal", "failed to get run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	// Ownership check — prevent a signed-in user from sending commands to another's run.
	if run.UserID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "run does not belong to you")
		return
	}

	switch run.Status {
	case runner.StatusRunning, runner.StatusSuspended:
		// ok
	default:
		writeError(w, http.StatusGone, "run_ended", "run has already ended")
		return
	}

	var req commandRequest
	if decErr := json.NewDecoder(r.Body).Decode(&req); decErr != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "input field required")
		return
	}
	input := strings.TrimSpace(req.Input)
	if input == "" {
		writeError(w, http.StatusBadRequest, "empty_input", "input must not be blank")
		return
	}

	session := s.runner.GetSession(id)
	if session == nil || !session.IsAlive() {
		writeError(w, http.StatusConflict, "not_started", "game not started; call /start first")
		return
	}

	output, cmdErr := session.Command(r.Context(), input)
	if cmdErr == runner.ErrBusy {
		writeError(w, http.StatusLocked, "busy", "command already in progress")
		return
	}
	if cmdErr != nil {
		writeError(w, http.StatusInternalServerError, "internal", cmdErr.Error())
		return
	}

	slog.Info("command executed", "run_id", id, "input", input, "output_len", len(output))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"output":    output,
		"turnCount": run.TurnCount,
	})
}

// handleRunSuspend saves the current game state without killing the interpreter.
// POST /api/runs/{id}/suspend
// Also accepts ?token=<idToken> for sendBeacon callers (handled in middleware).
func (s *Server) handleRunSuspend(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !runIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^r-[0-9A-Z]{26}$")
		return
	}
	if s.store == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		slog.Error("store.GetRun failed", "err", err, "run_id", id, "handler", "handleRunSuspend")
		writeError(w, http.StatusInternalServerError, "internal", "failed to get run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	// Ownership check.
	if run.UserID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "run does not belong to you")
		return
	}

	switch run.Status {
	case runner.StatusSuspended, runner.StatusFinished, runner.StatusFailed:
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if s.runner != nil {
		if session := s.runner.GetSession(id); session != nil && session.IsAlive() {
			if suspErr := session.Suspend(r.Context()); suspErr != nil && suspErr != runner.ErrBusy {
				writeError(w, http.StatusInternalServerError, "internal", "suspend failed")
				return
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRunsByUser returns runs owned by the authenticated Firebase user.
func (s *Server) handleRunsByUser(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	if s.store == nil {
		writeJSON(w, http.StatusOK, []runResponse{})
		return
	}

	limit := s.cfg.HistoryDefaultLimit
	if lp := r.URL.Query().Get("limit"); lp != "" {
		if v, err := strconv.Atoi(lp); err == nil && v > 0 {
			limit = v
		}
	}

	runs, err := s.store.ListRunsByUser(r.Context(), user.UID, limit)
	if err != nil {
		slog.Error("store.ListRunsByUser failed", "err", err, "user_id", user.UID)
		writeError(w, http.StatusInternalServerError, "internal", "failed to list runs")
		return
	}

	resp := make([]runResponse, 0, len(runs))
	for _, run := range runs {
		resp = append(resp, runToResponse(run))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteRun deletes a run and its associated GCS artifacts.
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !runIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^r-[0-9A-Z]{26}$")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		slog.Error("store.GetRun failed", "err", err, "run_id", id, "handler", "handleDeleteRun")
		writeError(w, http.StatusInternalServerError, "internal", "failed to get run")
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	// Ownership check — no empty-UserID bypass (§22.4.3).
	if run.UserID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "run does not belong to you")
		return
	}

	// Kill active session if any.
	if s.runner != nil {
		if existing := s.runner.GetSession(id); existing != nil {
			existing.Stop()
			s.runner.RemoveSession(id)
		}
	}

	// Delete GCS blobs before removing the Firestore doc.
	// sessions/{id}/ covers save files and new-format transcripts
	// (sessions/{id}/transcript.txt written by the runner).
	// transcripts/{id} (no trailing slash) covers legacy transcripts/{id}.txt
	// blobs written by older versions of the runner.
	if s.store != nil {
		s.store.DeleteBlobPrefix(r.Context(), "sessions/"+id+"/") //nolint:errcheck
		s.store.DeleteBlobPrefix(r.Context(), "transcripts/"+id)  //nolint:errcheck
	}

	if err := s.store.DeleteRun(r.Context(), id); err != nil {
		slog.Error("store.DeleteRun failed", "err", err, "run_id", id)
		writeError(w, http.StatusInternalServerError, "internal", "failed to delete run")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleRestartRun creates a fresh run copying the source game from an existing run.
func (s *Server) handleRestartRun(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	id := r.PathValue("id")
	if !runIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_id", "id must match ^r-[0-9A-Z]{26}$")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	orig, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		slog.Error("store.GetRun failed", "err", err, "run_id", id, "handler", "handleRestartRun")
		writeError(w, http.StatusInternalServerError, "internal", "failed to get run")
		return
	}
	if orig == nil {
		writeError(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	// Ownership check — no empty-UserID bypass (§22.4.3).
	if orig.UserID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "run does not belong to you")
		return
	}

	newID := "r-" + strings.ToUpper(ulid.MustNew(ulid.Timestamp(time.Now()), crand.Reader).String())
	now := time.Now().UTC()

	newRun := &store.Run{
		ID:            newID,
		SourceType:    orig.SourceType,
		IFDBId:        orig.IFDBId,
		Title:         orig.Title,
		Format:        orig.Format,
		ArtifactURL:   orig.ArtifactURL,
		CandidateURLs: orig.CandidateURLs,
		BuildID:       orig.BuildID,
		UserID:        user.UID,
		Status:        runner.StatusPending,
		CreatedAt:     now,
	}

	if err := s.store.CreateRun(r.Context(), newRun); err != nil {
		slog.Error("store.CreateRun failed", "err", err, "run_id", newRun.ID)
		writeError(w, http.StatusInternalServerError, "internal", "failed to create run")
		return
	}

	writeJSON(w, http.StatusCreated, runToResponse(newRun))
}

func runToResponse(r *store.Run) runResponse {
	resp := runResponse{
		ID:         r.ID,
		SourceType: r.SourceType,
		IFDBId:     r.IFDBId,
		Title:      r.Title,
		Format:     r.Format,
		Status:     r.Status,
		CreatedAt:  r.CreatedAt.UTC().Format(time.RFC3339Nano),
		TurnCount:  r.TurnCount,
		StartURL:   "/api/runs/" + r.ID + "/start",
	}
	if r.StartedAt != nil {
		resp.StartedAt = r.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	if r.LastActiveAt != nil {
		resp.LastActiveAt = r.LastActiveAt.UTC().Format(time.RFC3339Nano)
	}
	return resp
}
