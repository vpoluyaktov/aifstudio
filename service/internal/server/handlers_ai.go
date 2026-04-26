package server

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"aifstudio/internal/auth"
	openaiPkg "aifstudio/internal/openai"
	"aifstudio/internal/store"
)

// newTurnID generates a new ai_turns document ID: "t-<ULID>".
func newTurnID() string {
	return "t-" + strings.ToUpper(ulid.MustNew(ulid.Timestamp(time.Now()), crand.Reader).String())
}

// sendSSE writes a single SSE event frame and flushes.
func sendSSE(w http.ResponseWriter, f http.Flusher, event string, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		slog.Error("sendSSE marshal error", "event", event, "err", err)
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	f.Flush()
}

// headPreview returns the first max bytes of s for diagnostic logging,
// truncating on a rune boundary and appending an ellipsis marker when
// the original was longer.
func headPreview(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Walk back to a valid rune boundary so we never log a partial UTF-8 sequence.
	end := max
	for end > 0 && (s[end]&0xC0) == 0x80 {
		end--
	}
	return s[:end] + "…[truncated]"
}

// ── handleAIGenerate — POST /api/projects/{id}/ai/generate ────────────────────

type generateRequest struct {
	Description string `json:"description"`
}

func (s *Server) handleAIGenerate(w http.ResponseWriter, r *http.Request) {
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

	var req generateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
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

	// Check for existing turns — /ai/generate is only valid on a fresh project.
	turns, err := s.store.ListAIConversation(r.Context(), id, 1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to check conversation")
		return
	}
	if len(turns) > 0 {
		writeError(w, http.StatusConflict, "ai_already_generated", "project already has AI turns; use /ai/chat")
		return
	}

	// Resolve description: request body overrides stored description.
	desc := strings.TrimSpace(req.Description)
	if desc == "" {
		desc = p.Description
	}
	if desc == "" {
		writeError(w, http.StatusBadRequest, "invalid_description", "description is required for the first generate turn")
		return
	}
	if len(desc) > s.cfg.AIMaxDescriptionChars {
		writeError(w, http.StatusRequestEntityTooLarge, "description_too_large",
			fmt.Sprintf("description exceeds %d characters", s.cfg.AIMaxDescriptionChars))
		return
	}

	// Rate limit.
	if !s.aiLimiter.Allow(user.UID) {
		writeError(w, http.StatusTooManyRequests, "ai_rate_limited", "too many AI requests; please wait")
		return
	}

	// AI is unavailable if no API key.
	if s.cfg.OpenAIAPIKey == "" {
		writeError(w, http.StatusServiceUnavailable, "openai_unavailable", "AI is not configured")
		return
	}

	// Load current source (should be empty for a fresh project).
	sourceBefore, err := s.store.GetProjectSource(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "source_read_failed", "failed to read project source")
		return
	}

	// Build messages.
	authorName := user.Name
	if authorName == "" {
		authorName = user.Email
	}
	userMsg := BuildGenerateUserMessage(p.Name, desc)
	msgs := []openaiPkg.Message{
		{Role: "system", Content: BuildSystem(p.Name, desc, authorName, sourceBefore)},
		{Role: "user", Content: userMsg},
	}

	// SSE headers must be set before any write.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_flusher", "streaming not supported")
		return
	}

	turnID := newTurnID()
	now := time.Now().UTC()
	turn := &store.AITurn{
		ID:               turnID,
		ProjectID:        id,
		OwnerUID:         user.UID,
		Kind:             "generate",
		UserMessage:      userMsg,
		SourceBefore:     sourceBefore,
		ModelRequestedAt: now,
		Model:            s.cfg.OpenAIModel,
	}

	sendSSE(w, flusher, "start", map[string]interface{}{
		"turnId":       turnID,
		"kind":         "generate",
		"sourceBefore": sourceBefore,
	})

	// Stream from OpenAI.
	stream, err := s.openai.StreamChat(r.Context(), openaiPkg.ChatRequest{
		Model:    s.cfg.OpenAIModel,
		Messages: msgs,
		Stream:   true,
	})
	if err != nil {
		sendSSE(w, flusher, "error", map[string]string{"code": "openai_upstream", "error": err.Error()})
		turn.Error = err.Error()
		turn.SourceAfter = sourceBefore
		if werr := s.store.CreateAITurn(r.Context(), turn); werr != nil {
			slog.Error("handleAIGenerate: write failed turn", "err", werr)
		}
		return
	}

	var buf strings.Builder
	for chunk := range stream {
		if chunk.Err != nil {
			sendSSE(w, flusher, "error", map[string]string{"code": "openai_upstream", "error": chunk.Err.Error()})
			turn.Error = chunk.Err.Error()
			turn.SourceAfter = sourceBefore
			if werr := s.store.CreateAITurn(r.Context(), turn); werr != nil {
				slog.Error("handleAIGenerate: write failed turn", "err", werr)
			}
			return
		}
		if chunk.Delta != "" {
			buf.WriteString(chunk.Delta)
			sendSSE(w, flusher, "delta", map[string]string{"delta": chunk.Delta})
		}
		if chunk.Done {
			turn.PromptTokens = chunk.PromptTokens
			turn.CompletionTokens = chunk.CompletionTokens
		}
	}

	// Extract source + conversational reply.
	rawReply := buf.String()

	// Extract and strip the <DESCRIPTION> block before Inform 7 fence parsing.
	// This prevents the XML tag from leaking into the extracted source code.
	extractedDesc, rawReply := ExtractDescriptionBlock(rawReply)

	newSource, reply, xerr := openaiPkg.ExtractFencedInform7(rawReply)
	if xerr != nil {
		slog.Warn("handleAIGenerate: no_fence",
			"turn_id", turnID,
			"project_id", id,
			"raw_len", len(rawReply),
			"raw_head", headPreview(rawReply, 2000),
		)
		sendSSE(w, flusher, "error", map[string]string{"code": "no_fence", "error": xerr.Error()})
		turn.Error = xerr.Error()
		turn.SourceAfter = sourceBefore
		turn.AssistantReply = rawReply
		if werr := s.store.CreateAITurn(r.Context(), turn); werr != nil {
			slog.Error("handleAIGenerate: write no-fence turn", "err", werr)
		}
		return
	}
	if len(newSource) > 500000 {
		sendSSE(w, flusher, "error", map[string]string{"code": "source_too_large", "error": "generated source exceeds 500,000 characters"})
		return
	}

	finishedAt := time.Now().UTC()
	turn.SourceAfter = newSource
	turn.AssistantReply = reply
	turn.ModelFinishedAt = finishedAt

	// Update project description: use AI-generated catalog description if the
	// model included a <DESCRIPTION> block; fall back to the user-supplied prompt.
	if extractedDesc != "" {
		p.Description = extractedDesc
	} else {
		p.Description = desc
	}
	p.UpdatedAt = finishedAt

	// 1. Write source to GCS.
	if err := s.store.PutProjectSource(r.Context(), p.ID, newSource, finishedAt); err != nil {
		sendSSE(w, flusher, "error", map[string]string{"code": "gcs_upload_failed", "error": "failed to write source"})
		return
	}
	// 2. Write metadata + turn to Firestore.
	if _, err := s.store.UpdateProjectAI(r.Context(), p, turn); err != nil {
		sendSSE(w, flusher, "error", map[string]string{"code": "internal", "error": "failed to save turn"})
		return
	}

	sendSSE(w, flusher, "source", map[string]string{"source": newSource})
	sendSSE(w, flusher, "done", map[string]interface{}{
		"assistantReply":    reply,
		"promptTokens":      turn.PromptTokens,
		"completionTokens":  turn.CompletionTokens,
		"model":             s.cfg.OpenAIModel,
		"updatedAt":         finishedAt.Format(time.RFC3339Nano),
	})
}

// ── handleAIChat — POST /api/projects/{id}/ai/chat ────────────────────────────

type chatRequest struct {
	Message string `json:"message"`
}

func (s *Server) handleAIChat(w http.ResponseWriter, r *http.Request) {
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

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		writeError(w, http.StatusBadRequest, "empty_message", "message is required")
		return
	}
	if len(msg) > s.cfg.AIMaxMessageChars {
		writeError(w, http.StatusRequestEntityTooLarge, "message_too_large",
			fmt.Sprintf("message exceeds %d characters", s.cfg.AIMaxMessageChars))
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

	// Rate limit.
	if !s.aiLimiter.Allow(user.UID) {
		writeError(w, http.StatusTooManyRequests, "ai_rate_limited", "too many AI requests; please wait")
		return
	}

	if s.cfg.OpenAIAPIKey == "" {
		writeError(w, http.StatusServiceUnavailable, "openai_unavailable", "AI is not configured")
		return
	}

	// Load conversation history + check turn cap.
	turns, err := s.store.ListAIConversation(r.Context(), id, s.cfg.AIMaxTurnsPerProject)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "failed to load conversation")
		return
	}
	if len(turns) >= s.cfg.AIMaxTurnsPerProject {
		writeError(w, http.StatusConflict, "conversation_full",
			fmt.Sprintf("conversation limit of %d turns reached; start a new project", s.cfg.AIMaxTurnsPerProject))
		return
	}

	// Load current source.
	sourceBefore, err := s.store.GetProjectSource(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "source_read_failed", "failed to read project source")
		return
	}

	// Build OpenAI messages with truncation policy (§8.3).
	// The system prompt already embeds the current source, so we do NOT
	// re-append source_after to each history turn — doing so bloats every
	// turn by ~8 KB and pushes early attempts out of the context window
	// after only 3–4 turns, preventing the model from learning from its
	// own prior mistakes in the same session.
	chatAuthorName := user.Name
	if chatAuthorName == "" {
		chatAuthorName = user.Email
	}
	sysMsgs := []openaiPkg.Message{
		{Role: "system", Content: BuildSystem(p.Name, p.Description, chatAuthorName, sourceBefore)},
	}
	budget := 80000
	var tail []openaiPkg.Message
	for i := len(turns) - 1; i >= 0; i-- {
		uLen := len(turns[i].UserMessage)
		aLen := len(turns[i].AssistantReply)
		if uLen+aLen > budget {
			break
		}
		budget -= uLen + aLen
		tail = append([]openaiPkg.Message{
			{Role: "user", Content: turns[i].UserMessage},
			{Role: "assistant", Content: turns[i].AssistantReply},
		}, tail...)
	}
	msgs := append(sysMsgs, tail...)
	msgs = append(msgs, openaiPkg.Message{Role: "user", Content: msg})

	// SSE setup.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_flusher", "streaming not supported")
		return
	}

	turnID := newTurnID()
	now := time.Now().UTC()
	turn := &store.AITurn{
		ID:               turnID,
		ProjectID:        id,
		OwnerUID:         user.UID,
		Kind:             "chat",
		UserMessage:      msg,
		SourceBefore:     sourceBefore,
		ModelRequestedAt: now,
		Model:            s.cfg.OpenAIModel,
	}

	sendSSE(w, flusher, "start", map[string]interface{}{
		"turnId":       turnID,
		"kind":         "chat",
		"sourceBefore": sourceBefore,
	})

	stream, err := s.openai.StreamChat(r.Context(), openaiPkg.ChatRequest{
		Model:    s.cfg.OpenAIModel,
		Messages: msgs,
		Stream:   true,
	})
	if err != nil {
		sendSSE(w, flusher, "error", map[string]string{"code": "openai_upstream", "error": err.Error()})
		turn.Error = err.Error()
		turn.SourceAfter = sourceBefore
		if werr := s.store.CreateAITurn(r.Context(), turn); werr != nil {
			slog.Error("handleAIChat: write failed turn", "err", werr)
		}
		return
	}

	var buf strings.Builder
	for chunk := range stream {
		if chunk.Err != nil {
			sendSSE(w, flusher, "error", map[string]string{"code": "openai_upstream", "error": chunk.Err.Error()})
			turn.Error = chunk.Err.Error()
			turn.SourceAfter = sourceBefore
			if werr := s.store.CreateAITurn(r.Context(), turn); werr != nil {
				slog.Error("handleAIChat: write failed turn", "err", werr)
			}
			return
		}
		if chunk.Delta != "" {
			buf.WriteString(chunk.Delta)
			sendSSE(w, flusher, "delta", map[string]string{"delta": chunk.Delta})
		}
		if chunk.Done {
			turn.PromptTokens = chunk.PromptTokens
			turn.CompletionTokens = chunk.CompletionTokens
		}
	}

	rawReply := buf.String()
	newSource, reply, xerr := openaiPkg.ExtractFencedInform7(rawReply)
	if xerr != nil {
		slog.Warn("handleAIChat: no_fence",
			"turn_id", turnID,
			"project_id", id,
			"raw_len", len(rawReply),
			"raw_head", headPreview(rawReply, 2000),
		)
		sendSSE(w, flusher, "error", map[string]string{"code": "no_fence", "error": xerr.Error()})
		turn.Error = xerr.Error()
		turn.SourceAfter = sourceBefore
		turn.AssistantReply = rawReply
		if werr := s.store.CreateAITurn(r.Context(), turn); werr != nil {
			slog.Error("handleAIChat: write no-fence turn", "err", werr)
		}
		return
	}
	if len(newSource) > 500000 {
		sendSSE(w, flusher, "error", map[string]string{"code": "source_too_large", "error": "generated source exceeds 500,000 characters"})
		return
	}

	finishedAt := time.Now().UTC()
	turn.SourceAfter = newSource
	turn.AssistantReply = reply
	turn.ModelFinishedAt = finishedAt
	p.UpdatedAt = finishedAt

	if err := s.store.PutProjectSource(r.Context(), p.ID, newSource, finishedAt); err != nil {
		sendSSE(w, flusher, "error", map[string]string{"code": "gcs_upload_failed", "error": "failed to write source"})
		return
	}
	if _, err := s.store.UpdateProjectAI(r.Context(), p, turn); err != nil {
		sendSSE(w, flusher, "error", map[string]string{"code": "internal", "error": "failed to save turn"})
		return
	}

	sendSSE(w, flusher, "source", map[string]string{"source": newSource})
	sendSSE(w, flusher, "done", map[string]interface{}{
		"assistantReply":   reply,
		"promptTokens":     turn.PromptTokens,
		"completionTokens": turn.CompletionTokens,
		"model":            s.cfg.OpenAIModel,
		"updatedAt":        finishedAt.Format(time.RFC3339Nano),
	})
}

// ── handlePageAIWorkspace — GET /projects/{id}/ai ────────────────────────────

func (s *Server) handlePageAIWorkspace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.AIWorkspace.Execute(w, nil); err != nil {
		slog.Error("handlePageAIWorkspace template", "err", err)
	}
}
