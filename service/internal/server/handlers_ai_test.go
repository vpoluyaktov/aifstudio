// handlers_ai_test.go — tests for AI-related HTTP handlers (§10.1–§10.5 of
// ARCHITECTURE_AI_CREATE.md).
//
// Covered endpoints:
//   - POST /api/projects/{id}/ai/generate  (§10.1)
//   - POST /api/projects/{id}/ai/chat      (§10.2)
//   - PATCH /api/projects/{id}/publish     (§10.3)
//   - GET  /api/community                  (§10.4)
//   - POST /api/community/{id}/play        (§10.5)
//
// All tests use MockVerifier for auth (no live Firebase) and MockStore for
// persistence. OpenAI calls are intercepted via an in-process httptest.Server
// that returns canned SSE payloads.
//
// Test priority order (per QA standards):
//  1. Routing smoke: 401 when unauthenticated.
//  2. Auth checks: 403 when authenticated but not owner.
//  3. Not-found checks: 404 when project absent.
//  4. Validation / pre-condition checks.
//  5. Success path (200/201 + response shape).
package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"storycloud/internal/auth"
	"storycloud/internal/server"
	"storycloud/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test IDs and tokens used throughout this file
// ─────────────────────────────────────────────────────────────────────────────

const (
	aiTestProjID  = "p-01HXZX5K3Q0RTB9M7YPZL00001"
	aiTestBuildID = "b-01HXZX5K4MQS0RTB9M7YPA0001"

	aiTokenOwner    = "ai-mock-token-owner"
	aiTokenNonOwner = "ai-mock-token-non-owner"
)

var (
	aiUserOwner    = &auth.User{UID: "ai-owner-uid-test", Email: "owner@example.com"}
	aiUserNonOwner = &auth.User{UID: "ai-nonowner-uid-test", Email: "nonowner@example.com"}
)

// ─────────────────────────────────────────────────────────────────────────────
// Test server factories
// ─────────────────────────────────────────────────────────────────────────────

// newAIVerifier returns a MockVerifier with two users registered.
func newAIVerifier() *auth.MockVerifier {
	mv := auth.NewMockVerifier()
	mv.SetUser(aiTokenOwner, aiUserOwner)
	mv.SetUser(aiTokenNonOwner, aiUserNonOwner)
	return mv
}

// newTSWithOpenAI creates a test server whose OpenAI client is pointed at
// openAIBaseURL instead of the real API endpoint. apiKey is set so the
// "openai_unavailable" 503 short-circuit in handlers is not triggered.
func newTSWithOpenAI(t *testing.T, ms store.Store, mv *auth.MockVerifier, openAIBaseURL string) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	cfg.OpenAIAPIKey = "test-key-not-real"
	cfg.OpenAIBaseURL = openAIBaseURL
	// Ensure AI config has sensible defaults (config.Load already sets these,
	// but testConfig may not always call Load in all environments).
	if cfg.AIMaxDescriptionChars == 0 {
		cfg.AIMaxDescriptionChars = 2000
	}
	if cfg.AIMaxMessageChars == 0 {
		cfg.AIMaxMessageChars = 16000
	}
	if cfg.AIMaxTurnsPerProject == 0 {
		cfg.AIMaxTurnsPerProject = 200
	}
	if cfg.AIRateLimitPerUserQPS == 0 {
		cfg.AIRateLimitPerUserQPS = 0.2
	}
	if cfg.AIRateLimitPerUserBurst == 0 {
		cfg.AIRateLimitPerUserBurst = 3
	}
	srv := server.New(cfg, ms, nil, nil, nil, mv)
	ts := httptest.NewServer(srv.SetupRoutes())
	t.Cleanup(ts.Close)
	return ts
}

// newFakeOpenAIServer returns an httptest.Server that replies to any request
// with a canned OpenAI SSE stream containing one text chunk and a [DONE] line.
// The chunk contains a minimal valid inform7 fenced block so handlers can
// extract source without error.
func newFakeOpenAIServer(t *testing.T) *httptest.Server {
	t.Helper()
	const cannedSource = "\"Test Game\" by the author.\n\nA room is here."
	// Build the content string and JSON-encode it so that embedded newlines are
	// properly escaped as \n in the JSON value. Raw newlines inside a JSON string
	// are invalid and break the line-by-line SSE scanner in openai/stream.go.
	content := "```inform7\n" + cannedSource + "\n```\n\nI created the game."
	contentJSON, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("newFakeOpenAIServer: marshal content: %v", err)
	}
	body := fmt.Sprintf(
		"data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20}}\n\n"+
			"data: [DONE]\n\n",
		contentJSON,
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		fmt.Fprint(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ─────────────────────────────────────────────────────────────────────────────
// SSE helpers
// ─────────────────────────────────────────────────────────────────────────────

// sseEvents reads SSE events from r until EOF or a "done" event.
// Returns a map of event → last data seen for that event type.
func sseEvents(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	events := make(map[string]string)
	scanner := bufio.NewScanner(r)
	var currentEvent string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if currentEvent != "" {
				events[currentEvent] = data
			}
			if currentEvent == "done" || currentEvent == "error" {
				return events
			}
		}
	}
	return events
}

// ─────────────────────────────────────────────────────────────────────────────
// Helper: build a project with a succeeded build and non-empty source
// ─────────────────────────────────────────────────────────────────────────────

func setupPublishableProject(ms *mockStore) {
	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:            aiTestProjID,
		OwnerUID:      aiUserOwner.UID,
		Name:          "Test Game",
		Description:   "A test game.",
		LatestBuildID: aiTestBuildID,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	_ = ms.CreateBuild(context.Background(), &store.Build{
		ID:        aiTestBuildID,
		ProjectID: aiTestProjID,
		OwnerUID:  aiUserOwner.UID,
		Status:    "succeeded",
		CreatedAt: now,
	})
	// Non-zero source size → satisfies empty_source check.
	ms.mu.Lock()
	ms.sourceSizes[aiTestProjID] = 1024
	ms.mu.Unlock()
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/projects/{id}/ai/generate — §10.1
// ─────────────────────────────────────────────────────────────────────────────

// TestAIGenerateUnauthorized: 401 when no auth token is provided.
func TestAIGenerateUnauthorized(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	fakeOAI := newFakeOpenAIServer(t)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	body, _ := json.Marshal(map[string]string{"description": "A test game."})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/generate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no Authorization header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/generate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 401\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestAIGenerateForbiddenNonOwner: 403 when authenticated user is not the
// project owner.
func TestAIGenerateForbiddenNonOwner(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	fakeOAI := newFakeOpenAIServer(t)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	// Project owned by aiUserOwner, request comes from aiUserNonOwner.
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:       aiTestProjID,
		OwnerUID: aiUserOwner.UID,
		Name:     "Test Game",
	})

	body, _ := json.Marshal(map[string]string{"description": "A test game."})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/generate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenNonOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/generate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestAIGenerateNotFound: 404 when the project does not exist.
func TestAIGenerateNotFound(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	fakeOAI := newFakeOpenAIServer(t)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	body, _ := json.Marshal(map[string]string{"description": "A test game."})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/generate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/generate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 404\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestAIGenerateSSEContentType: owner on a new project → response Content-Type
// must be text/event-stream and the stream must contain a "start" and "done"
// event.
func TestAIGenerateSSEContentType(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	fakeOAI := newFakeOpenAIServer(t)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:          aiTestProjID,
		OwnerUID:    aiUserOwner.UID,
		Name:        "Test Game",
		Description: "A test game.",
		CreatedAt:   now,
		UpdatedAt:   now,
	})

	body, _ := json.Marshal(map[string]string{"description": "A test game."})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/generate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/generate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q; want text/event-stream", ct)
	}

	// Read the SSE stream and verify start + done events are present.
	events := sseEvents(t, resp.Body)

	if _, ok := events["start"]; !ok {
		t.Error("SSE stream missing 'start' event")
	}
	if _, ok := events["done"]; !ok {
		t.Error("SSE stream missing 'done' event")
	}

	// Verify start event contains turnId and kind.
	if startData, ok := events["start"]; ok {
		var startPayload struct {
			TurnID string `json:"turnId"`
			Kind   string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(startData), &startPayload); err != nil {
			t.Errorf("decode start event: %v\ndata: %s", err, startData)
		} else {
			if startPayload.TurnID == "" {
				t.Error("start event missing turnId")
			}
			if startPayload.Kind != "generate" {
				t.Errorf("start.kind = %q; want generate", startPayload.Kind)
			}
		}
	}
}

// TestAIGenerateUnavailableWhenNoAPIKey: 503 when OPENAI_API_KEY is unset
// (handled before any SSE headers are sent, so it's a normal JSON response).
func TestAIGenerateUnavailableWhenNoAPIKey(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()

	cfg := testConfig()
	cfg.OpenAIAPIKey = "" // unset — handler should return 503
	if cfg.AIMaxDescriptionChars == 0 {
		cfg.AIMaxDescriptionChars = 2000
	}
	if cfg.AIMaxTurnsPerProject == 0 {
		cfg.AIMaxTurnsPerProject = 200
	}
	if cfg.AIRateLimitPerUserBurst == 0 {
		cfg.AIRateLimitPerUserBurst = 3
	}
	srv := server.New(cfg, ms, nil, nil, nil, mv)
	ts := httptest.NewServer(srv.SetupRoutes())
	t.Cleanup(ts.Close)

	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:          aiTestProjID,
		OwnerUID:    aiUserOwner.UID,
		Name:        "Test Game",
		Description: "A test game.",
		CreatedAt:   now,
		UpdatedAt:   now,
	})

	body, _ := json.Marshal(map[string]string{"description": "A test game."})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/generate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/generate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 503 (no API key)\nbody: %s", resp.StatusCode, b)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/projects/{id}/ai/chat — §10.2
// ─────────────────────────────────────────────────────────────────────────────

// TestAIChatUnauthorized: 401 when no auth token.
func TestAIChatUnauthorized(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	fakeOAI := newFakeOpenAIServer(t)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	body, _ := json.Marshal(map[string]string{"message": "Add a room."})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/chat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 401\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestAIChatForbiddenNonOwner: 403 when authenticated but not the owner.
func TestAIChatForbiddenNonOwner(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	fakeOAI := newFakeOpenAIServer(t)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:       aiTestProjID,
		OwnerUID: aiUserOwner.UID,
		Name:     "Test Game",
	})

	body, _ := json.Marshal(map[string]string{"message": "Add a room."})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenNonOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/chat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestAIChatNotFound: 404 when the project does not exist.
func TestAIChatNotFound(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	fakeOAI := newFakeOpenAIServer(t)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	body, _ := json.Marshal(map[string]string{"message": "Add a room."})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/chat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 404\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestAIChatMessageTooLarge: 413 when message exceeds AIMaxMessageChars.
func TestAIChatMessageTooLarge(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	fakeOAI := newFakeOpenAIServer(t)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:       aiTestProjID,
		OwnerUID: aiUserOwner.UID,
		Name:     "Test Game",
	})

	// Default AIMaxMessageChars = 16000; send 16001 chars.
	oversizedMsg := strings.Repeat("x", 16001)
	body, _ := json.Marshal(map[string]string{"message": oversizedMsg})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/chat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 413\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestAIChatSSEContentType: owner on a project with existing turns → SSE
// response with text/event-stream content type.
func TestAIChatSSEContentType(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	fakeOAI := newFakeOpenAIServer(t)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:        aiTestProjID,
		OwnerUID:  aiUserOwner.UID,
		Name:      "Test Game",
		CreatedAt: now,
		UpdatedAt: now,
	})
	// Pre-populate an existing turn via CreateAITurn so SourceAfter is properly
	// stored in the blob layer (mirrors production GCS-backed behaviour).
	_ = ms.CreateAITurn(context.Background(), &store.AITurn{
		ID:               "t-00000000000000000000000001",
		ProjectID:        aiTestProjID,
		Kind:             "generate",
		UserMessage:      "Make a game.",
		SourceAfter:      "\"Test Game\" by the author.\n\nA room is here.",
		ModelRequestedAt: now,
	})

	body, _ := json.Marshal(map[string]string{"message": "Add a treasure room."})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/chat: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q; want text/event-stream", ct)
	}

	events := sseEvents(t, resp.Body)
	if _, ok := events["start"]; !ok {
		t.Error("SSE stream missing 'start' event")
	}
	if startData, ok := events["start"]; ok {
		var p struct{ Kind string `json:"kind"` }
		if err := json.Unmarshal([]byte(startData), &p); err == nil {
			if p.Kind != "chat" {
				t.Errorf("start.kind = %q; want chat", p.Kind)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// PATCH /api/projects/{id}/publish — §10.3
// ─────────────────────────────────────────────────────────────────────────────

// TestTogglePublishUnauthorized: 401 when no auth token.
func TestTogglePublishUnauthorized(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	body, _ := json.Marshal(map[string]bool{"published": true})
	req, _ := http.NewRequest(http.MethodPatch,
		ts.URL+"/api/projects/"+aiTestProjID+"/publish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH publish: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 401\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestTogglePublishForbiddenNonOwner: 403 when not the project owner.
func TestTogglePublishForbiddenNonOwner(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:       aiTestProjID,
		OwnerUID: aiUserOwner.UID,
		Name:     "Test Game",
	})

	body, _ := json.Marshal(map[string]bool{"published": true})
	req, _ := http.NewRequest(http.MethodPatch,
		ts.URL+"/api/projects/"+aiTestProjID+"/publish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenNonOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH publish: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestTogglePublishNotFound: 404 when project does not exist.
func TestTogglePublishNotFound(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	body, _ := json.Marshal(map[string]bool{"published": true})
	req, _ := http.NewRequest(http.MethodPatch,
		ts.URL+"/api/projects/"+aiTestProjID+"/publish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH publish: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 404\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestTogglePublishBuildRequired: 422 when publish=true but the project has
// no latestBuildId (pre-condition check §17.4).
func TestTogglePublishBuildRequired(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	// Project has no build.
	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:            aiTestProjID,
		OwnerUID:      aiUserOwner.UID,
		Name:          "Test Game",
		LatestBuildID: "", // no build
		CreatedAt:     now,
		UpdatedAt:     now,
	})

	body, _ := json.Marshal(map[string]bool{"published": true})
	req, _ := http.NewRequest(http.MethodPatch,
		ts.URL+"/api/projects/"+aiTestProjID+"/publish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH publish: %v", err)
	}
	defer resp.Body.Close()

	// Handler returns 422 when LatestBuildID is empty (§10.3).
	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 422 (build_required)\nbody: %s", resp.StatusCode, b)
	}
	var errBody struct {
		Code string `json:"code"`
	}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &errBody)
	if errBody.Code != "build_required" {
		t.Errorf("code = %q; want build_required\nbody: %s", errBody.Code, data)
	}
}

// TestTogglePublishEmptySource: 409 when publish=true and the project source
// is empty (pre-condition check §17.4).
func TestTogglePublishEmptySource(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:            aiTestProjID,
		OwnerUID:      aiUserOwner.UID,
		Name:          "Test Game",
		LatestBuildID: aiTestBuildID,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	_ = ms.CreateBuild(context.Background(), &store.Build{
		ID:        aiTestBuildID,
		ProjectID: aiTestProjID,
		OwnerUID:  aiUserOwner.UID,
		Status:    "succeeded",
		CreatedAt: now,
	})
	// sourceSizes map has no entry for this project → GetProjectSourceSize returns (0, false, nil).

	body, _ := json.Marshal(map[string]bool{"published": true})
	req, _ := http.NewRequest(http.MethodPatch,
		ts.URL+"/api/projects/"+aiTestProjID+"/publish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH publish: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 409 (empty_source)\nbody: %s", resp.StatusCode, b)
	}
	var errBody struct {
		Code string `json:"code"`
	}
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &errBody)
	if errBody.Code != "empty_source" {
		t.Errorf("code = %q; want empty_source\nbody: %s", errBody.Code, data)
	}
}

// TestTogglePublishToggleOn: owner with a publishable project → 200 with
// published=true and publishedAt set.
func TestTogglePublishToggleOn(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupPublishableProject(ms)

	body, _ := json.Marshal(map[string]bool{"published": true})
	req, _ := http.NewRequest(http.MethodPatch,
		ts.URL+"/api/projects/"+aiTestProjID+"/publish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH publish: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}
	assertContentType(t, resp, "application/json")

	var result struct {
		ID          string  `json:"id"`
		Published   bool    `json:"published"`
		PublishedAt *string `json:"publishedAt,omitempty"`
	}
	mustDecode(t, resp.Body, &result)

	if result.ID != aiTestProjID {
		t.Errorf("id = %q; want %q", result.ID, aiTestProjID)
	}
	if !result.Published {
		t.Error("published = false; want true")
	}
	if result.PublishedAt == nil || *result.PublishedAt == "" {
		t.Error("publishedAt should be set when publishing")
	}
}

// TestTogglePublishToggleOff: owner unpublishes a project → 200 with
// published=false.
func TestTogglePublishToggleOff(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupPublishableProject(ms)

	body, _ := json.Marshal(map[string]bool{"published": false})
	req, _ := http.NewRequest(http.MethodPatch,
		ts.URL+"/api/projects/"+aiTestProjID+"/publish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH publish: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}
	assertContentType(t, resp, "application/json")

	var result struct {
		ID        string `json:"id"`
		Published bool   `json:"published"`
	}
	mustDecode(t, resp.Body, &result)

	if result.ID != aiTestProjID {
		t.Errorf("id = %q; want %q", result.ID, aiTestProjID)
	}
	if result.Published {
		t.Error("published = true; want false after unpublish")
	}
}

// TestTogglePublishMissingField: 400 when the body omits the published field.
func TestTogglePublishMissingField(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupPublishableProject(ms)

	// Send empty JSON — no "published" key.
	body := bytes.NewReader([]byte("{}"))
	req, _ := http.NewRequest(http.MethodPatch,
		ts.URL+"/api/projects/"+aiTestProjID+"/publish", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH publish: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 400 (missing published field)\nbody: %s", resp.StatusCode, b)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/community — §10.4
// ─────────────────────────────────────────────────────────────────────────────

// TestCommunityListUnauthorized: 401 when no auth token.
func TestCommunityListUnauthorized(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/community", nil)
	// Deliberately no Authorization header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/community: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 401\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestCommunityListEmpty: authenticated user, no published projects → 200
// with {"games":[]}.
func TestCommunityListEmpty(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/community", nil)
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/community: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}
	assertContentType(t, resp, "application/json")

	var body struct {
		Games []json.RawMessage `json:"games"`
	}
	mustDecode(t, resp.Body, &body)
	if len(body.Games) != 0 {
		t.Errorf("games len = %d; want 0 for empty catalog", len(body.Games))
	}
}

// TestCommunityListOnlyPublished: store has a mix of published and unpublished
// projects; only published ones appear in the response.
func TestCommunityListOnlyPublished(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	now := time.Now().UTC()

	publishedProjID := "p-01HXZX5K3Q0RTB9M7YPZL00002"
	unpublishedProjID := "p-01HXZX5K3Q0RTB9M7YPZL00003"

	// Insert into store (for other lookups if needed).
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:          publishedProjID,
		OwnerUID:    aiUserOwner.UID,
		Name:        "Published Game",
		Description: "A published game.",
		Published:   true,
		PublishedAt: &now,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:        unpublishedProjID,
		OwnerUID:  aiUserNonOwner.UID,
		Name:      "Draft Game",
		Published: false,
		CreatedAt: now,
		UpdatedAt: now,
	})

	// ListPublishedProjects is backed by m.publishedProjectsList — set only
	// the published project here to simulate what Firestore's filtered query
	// would return (store.ListPublishedProjects already filters by Published==true).
	ms.mu.Lock()
	ms.publishedProjectsList = []*store.Project{
		{
			ID:            publishedProjID,
			OwnerUID:      aiUserOwner.UID,
			Name:          "Published Game",
			Description:   "A published game.",
			Published:     true,
			PublishedAt:   &now,
			LatestBuildID: aiTestBuildID,
		},
	}
	ms.mu.Unlock()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/community", nil)
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/community: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}
	assertContentType(t, resp, "application/json")

	var body struct {
		Games []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			OwnerUID    string `json:"ownerUid"`
			PublishedAt string `json:"publishedAt"`
		} `json:"games"`
	}
	mustDecode(t, resp.Body, &body)

	if len(body.Games) != 1 {
		t.Fatalf("games len = %d; want 1 (only published game)", len(body.Games))
	}
	if body.Games[0].ID != publishedProjID {
		t.Errorf("games[0].id = %q; want %q", body.Games[0].ID, publishedProjID)
	}
	if body.Games[0].Name != "Published Game" {
		t.Errorf("games[0].name = %q; want 'Published Game'", body.Games[0].Name)
	}
	if body.Games[0].PublishedAt == "" {
		t.Error("games[0].publishedAt is empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/community/{id}/play — §10.5
// ─────────────────────────────────────────────────────────────────────────────

// TestCommunityPlayUnauthorized: 401 when no auth token.
func TestCommunityPlayUnauthorized(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/community/"+aiTestProjID+"/play", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST community/play: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 401\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestCommunityPlayNotFound: 404 when the project does not exist.
func TestCommunityPlayNotFound(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/community/"+aiTestProjID+"/play", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenNonOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST community/play: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 404\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestCommunityPlayNotPublished: 403 when the project exists but is not
// published.
func TestCommunityPlayNotPublished(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:        aiTestProjID,
		OwnerUID:  aiUserOwner.UID,
		Name:      "Draft Game",
		Published: false, // not published
		CreatedAt: now,
		UpdatedAt: now,
	})

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/community/"+aiTestProjID+"/play", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenNonOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST community/play: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403 (not_published)\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestCommunityPlayNonOwnerLaunchesRun: a non-owner can launch a run from a
// published game → 201 with a runId.
func TestCommunityPlayNonOwnerLaunchesRun(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	now := time.Now().UTC()
	// Build for the published project.
	_ = ms.CreateBuild(context.Background(), &store.Build{
		ID:             aiTestBuildID,
		ProjectID:      aiTestProjID,
		OwnerUID:       aiUserOwner.UID,
		Status:         "succeeded",
		ArtifactFormat: "ulx",
		ArtifactPath:   "builds/" + aiTestBuildID + "/story.ulx",
		CreatedAt:      now,
	})
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:            aiTestProjID,
		OwnerUID:      aiUserOwner.UID,
		Name:          "Published Game",
		Published:     true,
		PublishedAt:   &now,
		LatestBuildID: aiTestBuildID,
		CreatedAt:     now,
		UpdatedAt:     now,
	})

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/community/"+aiTestProjID+"/play", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	// Request comes from aiUserNonOwner — they are NOT the project owner.
	req.Header.Set("Authorization", bearerHeader(aiTokenNonOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST community/play: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 201\nbody: %s", resp.StatusCode, b)
	}
	assertContentType(t, resp, "application/json")

	var result struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		StartURL string `json:"startURL"`
	}
	mustDecode(t, resp.Body, &result)

	if !strings.HasPrefix(result.ID, "r-") {
		t.Errorf("id = %q; want r- prefix", result.ID)
	}
	if result.Status != "pending" {
		t.Errorf("status = %q; want pending", result.Status)
	}
	if !strings.Contains(result.StartURL, result.ID) {
		t.Errorf("startURL %q does not contain run id %q", result.StartURL, result.ID)
	}

	// Verify the run was persisted in the store and belongs to the non-owner.
	run, err := ms.GetRun(context.Background(), result.ID)
	if err != nil || run == nil {
		t.Fatalf("run %q not found in store: %v", result.ID, err)
	}
	if run.UserID != aiUserNonOwner.UID {
		t.Errorf("run.UserID = %q; want %q (non-owner launched the run)", run.UserID, aiUserNonOwner.UID)
	}
	if run.BuildID != aiTestBuildID {
		t.Errorf("run.BuildID = %q; want %q", run.BuildID, aiTestBuildID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Routing smoke — new AI + community routes (§14.1 of ARCHITECTURE_AI_CREATE.md)
// ─────────────────────────────────────────────────────────────────────────────

// TestAIAndCommunityRoutingSmoke verifies all new routes are registered and
// return expected status codes for correct + wrong methods.
func TestAIAndCommunityRoutingSmoke(t *testing.T) {
	// Use local-dev auth (always succeeds) so auth doesn't interfere with
	// method-mismatch tests.
	cfg := testConfig()
	if cfg.AIMaxTurnsPerProject == 0 {
		cfg.AIMaxTurnsPerProject = 200
	}
	if cfg.AIRateLimitPerUserBurst == 0 {
		cfg.AIRateLimitPerUserBurst = 3
	}
	verifier, _ := auth.NewVerifier(context.Background(), "")
	srv := server.New(cfg, newMockStore(), nil, nil, nil, verifier)
	ts := httptest.NewServer(srv.SetupRoutes())
	defer ts.Close()

	tests := []struct {
		method string
		path   string
		want   int
	}{
		// AI generate
		{"POST", "/api/projects/" + aiTestProjID + "/ai/generate", http.StatusNotFound}, // project not in store → 404
		{"GET", "/api/projects/" + aiTestProjID + "/ai/generate", http.StatusMethodNotAllowed},
		// AI chat
		{"POST", "/api/projects/" + aiTestProjID + "/ai/chat", http.StatusBadRequest}, // empty body → 400
		{"GET", "/api/projects/" + aiTestProjID + "/ai/chat", http.StatusMethodNotAllowed},
		// Publish
		{"PATCH", "/api/projects/" + aiTestProjID + "/publish", http.StatusBadRequest}, // missing field → 400
		{"PUT", "/api/projects/" + aiTestProjID + "/publish", http.StatusMethodNotAllowed},
		// Community list
		{"GET", "/api/community", http.StatusOK},
		{"POST", "/api/community", http.StatusMethodNotAllowed},
		// Community play
		{"POST", "/api/community/" + aiTestProjID + "/play", http.StatusNotFound}, // project not in store → 404
		{"GET", "/api/community/" + aiTestProjID + "/play", http.StatusMethodNotAllowed},
		// AI workspace page (no auth needed — serves HTML)
		{"GET", "/projects/" + aiTestProjID + "/ai", http.StatusOK},
		{"POST", "/projects/" + aiTestProjID + "/ai", http.StatusMethodNotAllowed},
		// Community page
		{"GET", "/community", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			var body io.Reader
			if tt.method == "POST" || tt.method == "PATCH" || tt.method == "PUT" {
				body = bytes.NewReader([]byte("{}"))
			}
			req, _ := http.NewRequest(tt.method, ts.URL+tt.path, body)
			if tt.method == "POST" || tt.method == "PATCH" || tt.method == "PUT" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body) //nolint:errcheck

			if resp.StatusCode != tt.want {
				t.Errorf("status = %d; want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ExtractDescriptionBlock unit tests
// ─────────────────────────────────────────────────────────────────────────────

// TestExtractDescriptionBlockPresent: block is found, description extracted,
// and the raw reply has the block removed.
func TestExtractDescriptionBlockPresent(t *testing.T) {
	raw := "<DESCRIPTION>\nNavigate the exclusion zone.\n</DESCRIPTION>\n\n```inform7\ncode here\n```\n\nI built the game."
	wantDesc := "Navigate the exclusion zone."
	wantStripped := "\n\n```inform7\ncode here\n```\n\nI built the game."

	gotDesc, gotStripped := server.ExtractDescriptionBlock(raw)
	if gotDesc != wantDesc {
		t.Errorf("description = %q; want %q", gotDesc, wantDesc)
	}
	if gotStripped != wantStripped {
		t.Errorf("stripped = %q; want %q", gotStripped, wantStripped)
	}
}

// TestExtractDescriptionBlockCaseInsensitive: tag matching is case-insensitive.
func TestExtractDescriptionBlockCaseInsensitive(t *testing.T) {
	raw := "<description>A short game.</description>\n```inform7\n```"
	gotDesc, _ := server.ExtractDescriptionBlock(raw)
	if gotDesc != "A short game." {
		t.Errorf("description = %q; want 'A short game.'", gotDesc)
	}
}

// TestExtractDescriptionBlockAbsent: when no block is present, description is
// empty and the raw string is returned unchanged.
func TestExtractDescriptionBlockAbsent(t *testing.T) {
	raw := "Some reply without a block.\n```inform7\ncode\n```"
	gotDesc, gotStripped := server.ExtractDescriptionBlock(raw)
	if gotDesc != "" {
		t.Errorf("description = %q; want empty string", gotDesc)
	}
	if gotStripped != raw {
		t.Errorf("stripped changed when no block present: got %q, want %q", gotStripped, raw)
	}
}

// TestExtractDescriptionBlockMultiSentence: multi-sentence content is trimmed correctly.
func TestExtractDescriptionBlockMultiSentence(t *testing.T) {
	raw := "<DESCRIPTION>\n  You are a detective in 1920s Paris.\n  Solve the murder before dawn.\n</DESCRIPTION>\n```inform7\n```"
	wantDesc := "You are a detective in 1920s Paris.\n  Solve the murder before dawn."
	gotDesc, _ := server.ExtractDescriptionBlock(raw)
	if gotDesc != wantDesc {
		t.Errorf("description = %q; want %q", gotDesc, wantDesc)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// handleAIGenerate — description extraction integration test
// ─────────────────────────────────────────────────────────────────────────────

// newFakeOpenAIServerWithDescription returns a fake OpenAI SSE server whose
// response includes a <DESCRIPTION> block before the inform7 fence, mirroring
// what the real model will produce after rule 36 is in the system prompt.
func newFakeOpenAIServerWithDescription(t *testing.T, aiDesc string) *httptest.Server {
	t.Helper()
	const cannedSource = "\"Test Game\" by the author.\n\nA room is here."
	content := "<DESCRIPTION>\n" + aiDesc + "\n</DESCRIPTION>\n\n```inform7\n" + cannedSource + "\n```\n\nI created the game."
	contentJSON, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("newFakeOpenAIServerWithDescription: marshal content: %v", err)
	}
	body := fmt.Sprintf(
		"data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20}}\n\n"+
			"data: [DONE]\n\n",
		contentJSON,
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		fmt.Fprint(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAIGenerateDescriptionExtracted: when the AI response includes a
// <DESCRIPTION> block, the handler strips it and saves the extracted text as
// the project description instead of the raw user prompt.
func TestAIGenerateDescriptionExtracted(t *testing.T) {
	const aiDesc = "Navigate the Chernobyl exclusion zone in 1986. Avoid lethal radiation."

	mv := newAIVerifier()
	ms := newMockStore()
	fakeOAI := newFakeOpenAIServerWithDescription(t, aiDesc)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:          aiTestProjID,
		OwnerUID:    aiUserOwner.UID,
		Name:        "Test Game",
		Description: "raw user prompt text",
		CreatedAt:   now,
		UpdatedAt:   now,
	})

	reqBody, _ := json.Marshal(map[string]string{"description": "raw user prompt text"})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/generate", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/generate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}

	// Drain the SSE stream and ensure we got a "done" event.
	events := sseEvents(t, resp.Body)
	if _, ok := events["done"]; !ok {
		t.Fatal("SSE stream missing 'done' event")
	}

	// Verify the description passed to UpdateProjectAI is the extracted one.
	ms.mu.RLock()
	lastProj := ms.lastSavedProject
	ms.mu.RUnlock()

	if lastProj == nil {
		t.Fatal("UpdateProjectAI was not called (lastSavedProject is nil)")
	}
	if lastProj.Description != aiDesc {
		t.Errorf("project.Description = %q; want AI-extracted %q", lastProj.Description, aiDesc)
	}
}

// TestAIGenerateDescriptionFallback: when the AI response has no <DESCRIPTION>
// block, the project description falls back to the user-supplied prompt text.
func TestAIGenerateDescriptionFallback(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	// Use the standard fake server which does NOT include a <DESCRIPTION> block.
	fakeOAI := newFakeOpenAIServer(t)
	ts := newTSWithOpenAI(t, ms, mv, fakeOAI.URL)

	const userPrompt = "raw user prompt text"
	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:          aiTestProjID,
		OwnerUID:    aiUserOwner.UID,
		Name:        "Test Game",
		Description: userPrompt,
		CreatedAt:   now,
		UpdatedAt:   now,
	})

	reqBody, _ := json.Marshal(map[string]string{"description": userPrompt})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/projects/"+aiTestProjID+"/ai/generate", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ai/generate: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}

	events := sseEvents(t, resp.Body)
	if _, ok := events["done"]; !ok {
		t.Fatal("SSE stream missing 'done' event")
	}

	ms.mu.RLock()
	lastProj := ms.lastSavedProject
	ms.mu.RUnlock()

	if lastProj == nil {
		t.Fatal("UpdateProjectAI was not called (lastSavedProject is nil)")
	}
	if lastProj.Description != userPrompt {
		t.Errorf("project.Description = %q; want fallback %q", lastProj.Description, userPrompt)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────


