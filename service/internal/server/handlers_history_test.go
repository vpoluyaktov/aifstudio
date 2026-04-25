// handlers_history_test.go — tests for version-history HTTP handlers.
//
// Covered endpoints:
//   - GET  /api/projects/{id}/history                  (handleListHistory)
//   - GET  /api/projects/{id}/history/{turnId}/source  (handleGetTurnSource)
//   - PUT  /api/projects/{id}/source                   (handlePutProjectSource)
//
// Auth/ownership tests use MockVerifier (newAIVerifier + newTSWithMockAuth)
// identical to the AI-handler tests, since all three endpoints require auth.
// Success-path tests for PUT (204) with local-dev auth also live in
// TestUpdateProjectSource in routes_test.go; the tests here add coverage for
// the 401/404/403/413 error paths.
//
// Test priority order (per QA standards):
//  1. Routing smoke: wrong method → 405; new routes are registered.
//  2. Auth check: 401 when no token.
//  3. Not-found: 404 when project absent.
//  4. Ownership: 403 when authenticated but not owner.
//  5. Validation: bad turn-ID format (400), oversized source (413).
//  6. Success paths: 200 / 204 + response shape + edge cases.
package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"storycloud/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test constants — distinct from aiTestProjID/btProjectID to make fixture
// origins unambiguous even though each test has its own isolated mockStore.
// ─────────────────────────────────────────────────────────────────────────────

const (
	histProjID  = "p-01HXZX5K3Q0RTB9M7YPZN00002"
	histTurnID1 = "t-00000000000000000000000001"
	histTurnID2 = "t-00000000000000000000000002"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helper: seed a history project owned by aiUserOwner
// ─────────────────────────────────────────────────────────────────────────────

func setupHistoryProject(ms *mockStore) {
	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:        histProjID,
		OwnerUID:  aiUserOwner.UID,
		Name:      "History Test Game",
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Routing smoke
// ─────────────────────────────────────────────────────────────────────────────

// TestHistoryRoutingSmoke checks that the three new routes are registered and
// that wrong-method requests receive 405. Requests carry a valid owner token so
// auth does not interfere with the status assertions.
func TestHistoryRoutingSmoke(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	setupHistoryProject(ms)
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	tests := []struct {
		method string
		path   string
		want   int
	}{
		// GET /api/projects/{id}/history — registered; empty turns → 200
		{"GET", "/api/projects/" + histProjID + "/history", http.StatusOK},
		// Wrong methods on history list
		{"POST", "/api/projects/" + histProjID + "/history", http.StatusMethodNotAllowed},
		{"DELETE", "/api/projects/" + histProjID + "/history", http.StatusMethodNotAllowed},
		{"PUT", "/api/projects/" + histProjID + "/history", http.StatusMethodNotAllowed},

		// GET /api/projects/{id}/history/{turnId}/source — turn absent → 404 (registered)
		{"GET", "/api/projects/" + histProjID + "/history/" + histTurnID1 + "/source", http.StatusNotFound},
		// Wrong methods on history/{turnId}/source
		{"POST", "/api/projects/" + histProjID + "/history/" + histTurnID1 + "/source", http.StatusMethodNotAllowed},
		{"DELETE", "/api/projects/" + histProjID + "/history/" + histTurnID1 + "/source", http.StatusMethodNotAllowed},

		// PUT /api/projects/{id}/source — project exists → 204 (registered)
		{"PUT", "/api/projects/" + histProjID + "/source", http.StatusNoContent},
		// Wrong methods on PUT source (PATCH is the other registered method, not tested here)
		{"GET", "/api/projects/" + histProjID + "/source", http.StatusMethodNotAllowed},
		{"DELETE", "/api/projects/" + histProjID + "/source", http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			var body io.Reader
			if tt.method == "POST" || tt.method == "PUT" || tt.method == "PATCH" {
				body = bytes.NewReader([]byte("{}"))
			}
			req, _ := http.NewRequest(tt.method, ts.URL+tt.path, body)
			req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
			if tt.method == "POST" || tt.method == "PUT" || tt.method == "PATCH" {
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
// GET /api/projects/{id}/history
// ─────────────────────────────────────────────────────────────────────────────

// TestListHistoryUnauthorized: 401 when no auth token is provided.
func TestListHistoryUnauthorized(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+histProjID+"/history", nil)
	// Deliberately no Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 401\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestListHistoryNotFound: 404 when the project does not exist.
func TestListHistoryNotFound(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+histProjID+"/history", nil)
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 404\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestListHistoryForbidden: 403 when authenticated user is not the project owner.
func TestListHistoryForbidden(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	// Project owned by aiUserOwner; request from aiUserNonOwner.
	setupHistoryProject(ms)

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+histProjID+"/history", nil)
	req.Header.Set("Authorization", bearerHeader(aiTokenNonOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestListHistoryEmpty: owner with no turns → 200 with bare [] (not null, not
// wrapped object).
func TestListHistoryEmpty(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupHistoryProject(ms)

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+histProjID+"/history", nil)
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}
	assertContentType(t, resp, "application/json")

	// Must decode as a bare array, not wrapped object.
	var items []json.RawMessage
	mustDecode(t, resp.Body, &items)
	if len(items) != 0 {
		t.Errorf("len(items) = %d; want 0 for project with no turns", len(items))
	}
}

// historyItemShape is the expected JSON shape for a single history item.
type historyItemShape struct {
	ID          string `json:"id"`
	CreatedAt   string `json:"createdAt"`
	Kind        string `json:"kind"`
	UserMessage string `json:"userMessage"`
	HasSource   bool   `json:"hasSource"`
}

// TestListHistoryShape: owner with two turns in store → 200 with ordered items,
// correct field values, and hasSource reflecting SourceAfter blob presence.
func TestListHistoryShape(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupHistoryProject(ms)

	// Turn 1 (older): has SourceAfter blob → hasSource must be true.
	t1At := time.Now().UTC().Add(-2 * time.Minute)
	_ = ms.CreateAITurn(context.Background(), &store.AITurn{
		ID:               histTurnID1,
		ProjectID:        histProjID,
		OwnerUID:         aiUserOwner.UID,
		Kind:             "generate",
		UserMessage:      "Create a spooky dungeon.",
		SourceAfter:      "\"Dungeon\" by author.\n\nDark Room is a room.",
		ModelRequestedAt: t1At,
	})

	// Turn 2 (newer): no SourceAfter blob → hasSource must be false.
	t2At := time.Now().UTC().Add(-1 * time.Minute)
	_ = ms.CreateAITurn(context.Background(), &store.AITurn{
		ID:               histTurnID2,
		ProjectID:        histProjID,
		OwnerUID:         aiUserOwner.UID,
		Kind:             "chat",
		UserMessage:      "Add a treasure chest.",
		SourceAfter:      "", // no blob written
		ModelRequestedAt: t2At,
	})

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+histProjID+"/history", nil)
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}
	assertContentType(t, resp, "application/json")

	var items []historyItemShape
	mustDecode(t, resp.Body, &items)

	if len(items) != 2 {
		t.Fatalf("len(items) = %d; want 2", len(items))
	}

	// Verify oldest-first order: turn1 (older) must come before turn2.
	if items[0].ID != histTurnID1 {
		t.Errorf("items[0].id = %q; want %q (oldest first)", items[0].ID, histTurnID1)
	}
	if items[1].ID != histTurnID2 {
		t.Errorf("items[1].id = %q; want %q", items[1].ID, histTurnID2)
	}

	// Verify field values on turn 1.
	if items[0].Kind != "generate" {
		t.Errorf("items[0].kind = %q; want generate", items[0].Kind)
	}
	if items[0].UserMessage != "Create a spooky dungeon." {
		t.Errorf("items[0].userMessage = %q; want 'Create a spooky dungeon.'", items[0].UserMessage)
	}
	if !items[0].HasSource {
		t.Error("items[0].hasSource = false; want true (SourceAfter blob was written)")
	}
	if items[0].CreatedAt == "" {
		t.Error("items[0].createdAt is empty")
	}

	// Verify field values on turn 2.
	if items[1].Kind != "chat" {
		t.Errorf("items[1].kind = %q; want chat", items[1].Kind)
	}
	if items[1].UserMessage != "Add a treasure chest." {
		t.Errorf("items[1].userMessage = %q; want 'Add a treasure chest.'", items[1].UserMessage)
	}
	if items[1].HasSource {
		t.Error("items[1].hasSource = true; want false (no SourceAfter blob)")
	}
}

// TestListHistoryUserMessageTruncation: userMessage longer than 120 runes is
// truncated to exactly 120 runes; a message at the boundary is left intact.
func TestListHistoryUserMessageTruncation(t *testing.T) {
	tests := []struct {
		name          string
		message       string
		wantLen       int
		wantTruncated bool
	}{
		{
			name:          "exactly 120 runes — not truncated",
			message:       strings.Repeat("a", 120),
			wantLen:       120,
			wantTruncated: false,
		},
		{
			name:          "121 runes — truncated to 120",
			message:       strings.Repeat("a", 121),
			wantLen:       120,
			wantTruncated: true,
		},
		{
			name:    "short message — untouched",
			message: "Short.",
			wantLen: len("Short."),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mv := newAIVerifier()
			ms := newMockStore()
			ts := newTSWithMockAuth(t, ms, mv)
			defer ts.Close()

			setupHistoryProject(ms)

			_ = ms.CreateAITurn(context.Background(), &store.AITurn{
				ID:               histTurnID1,
				ProjectID:        histProjID,
				OwnerUID:         aiUserOwner.UID,
				Kind:             "generate",
				UserMessage:      tt.message,
				ModelRequestedAt: time.Now().UTC(),
			})

			req, _ := http.NewRequest(http.MethodGet,
				ts.URL+"/api/projects/"+histProjID+"/history", nil)
			req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET history: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
			}

			var items []historyItemShape
			mustDecode(t, resp.Body, &items)
			if len(items) != 1 {
				t.Fatalf("len(items) = %d; want 1", len(items))
			}

			gotLen := len([]rune(items[0].UserMessage))
			if gotLen != tt.wantLen {
				t.Errorf("userMessage rune count = %d; want %d\nmessage: %q",
					gotLen, tt.wantLen, items[0].UserMessage)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/projects/{id}/history/{turnId}/source
// ─────────────────────────────────────────────────────────────────────────────

// TestGetTurnSourceUnauthorized: 401 when no auth token is provided.
func TestGetTurnSourceUnauthorized(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+histProjID+"/history/"+histTurnID1+"/source", nil)
	// Deliberately no Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET turn source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 401\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestGetTurnSourceProjectNotFound: 404 when the project does not exist.
func TestGetTurnSourceProjectNotFound(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+histProjID+"/history/"+histTurnID1+"/source", nil)
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET turn source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 404\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestGetTurnSourceForbidden: 403 when authenticated user is not the owner.
func TestGetTurnSourceForbidden(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupHistoryProject(ms)

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+histProjID+"/history/"+histTurnID1+"/source", nil)
	req.Header.Set("Authorization", bearerHeader(aiTokenNonOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET turn source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestGetTurnSourceInvalidTurnID: 400 when the turnId path segment does not
// match ^t-[0-9A-Z]{26}$.
func TestGetTurnSourceInvalidTurnID(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupHistoryProject(ms)

	badIDs := []struct {
		name   string
		turnID string
	}{
		{"no t- prefix", "00000000000000000000000001"},
		{"wrong prefix letter", "x-00000000000000000000000001"},
		{"too short", "t-SHORT"},
		{"lowercase ulid", "t-01hxzx5k3q0rtb9m7ypzn00002"},
	}

	for _, tt := range badIDs {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet,
				ts.URL+"/api/projects/"+histProjID+"/history/"+tt.turnID+"/source", nil)
			req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("turnId=%q → status = %d; want 400\nbody: %s",
					tt.turnID, resp.StatusCode, b)
			}
		})
	}
}

// TestGetTurnSourceBlobNotFound: 404 when the project and turn exist but the
// after.i7 GCS blob is absent (turn was created without SourceAfter).
func TestGetTurnSourceBlobNotFound(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupHistoryProject(ms)

	// Turn exists in Firestore but has no SourceAfter → no blob written.
	_ = ms.CreateAITurn(context.Background(), &store.AITurn{
		ID:               histTurnID1,
		ProjectID:        histProjID,
		OwnerUID:         aiUserOwner.UID,
		Kind:             "generate",
		UserMessage:      "Some message.",
		SourceAfter:      "", // no blob
		ModelRequestedAt: time.Now().UTC(),
	})

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+histProjID+"/history/"+histTurnID1+"/source", nil)
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET turn source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 404 (blob absent)\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestGetTurnSourceSuccess: owner + valid turn with after.i7 blob → 200 with
// {"source": "..."} containing the stored source text.
func TestGetTurnSourceSuccess(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupHistoryProject(ms)

	const wantSource = "\"Dungeon\" by the author.\n\nDark Room is a room.\n"

	// CreateAITurn with SourceAfter set → blob written to mock blob map.
	_ = ms.CreateAITurn(context.Background(), &store.AITurn{
		ID:               histTurnID1,
		ProjectID:        histProjID,
		OwnerUID:         aiUserOwner.UID,
		Kind:             "generate",
		UserMessage:      "Create a dungeon.",
		SourceAfter:      wantSource,
		ModelRequestedAt: time.Now().UTC(),
	})

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+histProjID+"/history/"+histTurnID1+"/source", nil)
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET turn source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}
	assertContentType(t, resp, "application/json")

	var body struct {
		Source string `json:"source"`
	}
	mustDecode(t, resp.Body, &body)

	if body.Source != wantSource {
		t.Errorf("source = %q; want %q", body.Source, wantSource)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// PUT /api/projects/{id}/source  (auth + error paths; success already covered
// by TestUpdateProjectSource in routes_test.go using local-dev auth)
// ─────────────────────────────────────────────────────────────────────────────

// TestPutProjectSourceUnauthorized: 401 when no auth token is provided.
func TestPutProjectSourceUnauthorized(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"source": "some source"})
	req, _ := http.NewRequest(http.MethodPut,
		ts.URL+"/api/projects/"+histProjID+"/source", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 401\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestPutProjectSourceNotFound: 404 when the project does not exist.
func TestPutProjectSourceNotFound(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"source": "some source"})
	req, _ := http.NewRequest(http.MethodPut,
		ts.URL+"/api/projects/"+histProjID+"/source", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 404\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestPutProjectSourceForbidden: 403 when authenticated user is not the owner.
func TestPutProjectSourceForbidden(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupHistoryProject(ms)

	body, _ := json.Marshal(map[string]string{"source": "some source"})
	req, _ := http.NewRequest(http.MethodPut,
		ts.URL+"/api/projects/"+histProjID+"/source", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenNonOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403\nbody: %s", resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestPutProjectSourceTooLarge: source exceeding 500,000 bytes → 413 (or 400).
func TestPutProjectSourceTooLarge(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupHistoryProject(ms)

	oversized := strings.Repeat("a", 500001)
	body, _ := json.Marshal(map[string]string{"source": oversized})
	req, _ := http.NewRequest(http.MethodPut,
		ts.URL+"/api/projects/"+histProjID+"/source", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT source (oversized): %v", err)
	}
	defer resp.Body.Close()

	// Handler returns 413; accept 400 for implementations that choose that code.
	if resp.StatusCode != http.StatusRequestEntityTooLarge && resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 413 (or 400) for oversized source\nbody: %s",
			resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestPutProjectSourceSuccess: owner sends valid source → 204 No Content,
// source is stored in GCS (mock), and project.updatedAt is updated.
func TestPutProjectSourceSuccess(t *testing.T) {
	mv := newAIVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	setupHistoryProject(ms)
	originalUpdatedAt := ms.projects[histProjID].UpdatedAt

	const newSource = "\"Restored Game\" by the author.\n\nThe Cellar is a room.\n"

	body, _ := json.Marshal(map[string]string{"source": newSource})
	req, _ := http.NewRequest(http.MethodPut,
		ts.URL+"/api/projects/"+histProjID+"/source", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(aiTokenOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT source: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 204\nbody: %s", resp.StatusCode, b)
	}

	// 204 must have no body.
	bodyBytes, _ := io.ReadAll(resp.Body)
	if len(bodyBytes) != 0 {
		t.Errorf("204 response body should be empty; got %d bytes: %s",
			len(bodyBytes), bodyBytes)
	}

	// Verify source was persisted in mock GCS.
	ms.mu.RLock()
	storedSource := ms.projectSources[histProjID]
	updatedAt := ms.projects[histProjID].UpdatedAt
	ms.mu.RUnlock()

	if storedSource != newSource {
		t.Errorf("stored source = %q; want %q", storedSource, newSource)
	}

	// project.updatedAt must have advanced.
	if !updatedAt.After(originalUpdatedAt) {
		t.Errorf("project.updatedAt = %v; want > %v (should be updated)", updatedAt, originalUpdatedAt)
	}
}
