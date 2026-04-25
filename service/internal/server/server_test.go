package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"storycloud/internal/auth"
	"storycloud/internal/config"
	"storycloud/internal/store"
)

// mockStore is a minimal in-memory store for server tests.
type mockStore struct{}

func (m *mockStore) CreateRun(_ context.Context, _ *store.Run) error                         { return nil }
func (m *mockStore) GetRun(_ context.Context, _ string) (*store.Run, error)                  { return nil, nil }
func (m *mockStore) UpdateRun(_ context.Context, _ *store.Run) error                         { return nil }
func (m *mockStore) DeleteRun(_ context.Context, _ string) error                             { return nil }
func (m *mockStore) DeleteAbandonedPendingRuns(_ context.Context, _ time.Time) (int, error)  { return 0, nil }
func (m *mockStore) ListRunsByUser(_ context.Context, _ string, _ int) ([]*store.Run, error)    { return nil, nil }
func (m *mockStore) ListRunsByProject(_ context.Context, _ string, _ int) ([]*store.Run, error) { return nil, nil }
func (m *mockStore) CreateProject(_ context.Context, _ *store.Project) error                 { return nil }
func (m *mockStore) GetProject(_ context.Context, _ string) (*store.Project, error)          { return nil, nil }
func (m *mockStore) UpdateProjectSource(_ context.Context, _, _ string, _ time.Time) error             { return nil }
func (m *mockStore) UpdateProjectMeta(_ context.Context, _, _, _ string, _ time.Time) error           { return nil }
func (m *mockStore) UpdateProjectLatestBuild(_ context.Context, _, _ string) error                    { return nil }
func (m *mockStore) ListProjectsByOwner(_ context.Context, _ string, _ int) ([]*store.Project, error) {
	return nil, nil
}
func (m *mockStore) CreateBuild(_ context.Context, _ *store.Build) error                    { return nil }
func (m *mockStore) GetBuild(_ context.Context, _ string) (*store.Build, error)             { return nil, nil }
func (m *mockStore) UpdateBuild(_ context.Context, _ *store.Build) error                    { return nil }
func (m *mockStore) ListBuildsByProject(_ context.Context, _ string, _ int) ([]*store.Build, error) {
	return nil, nil
}
func (m *mockStore) DeleteBuildsForProject(_ context.Context, _ string) (int, error) { return 0, nil }
func (m *mockStore) DeleteRunsForProject(_ context.Context, _ string) (int, error)  { return 0, nil }
func (m *mockStore) DeleteProject(_ context.Context, _ string) error                { return nil }
func (m *mockStore) GetCachedGame(_ context.Context, _ string) (*store.CachedGame, error)  { return nil, nil }
func (m *mockStore) PutCachedGame(_ context.Context, _ *store.CachedGame) error            { return nil }
func (m *mockStore) ListFreshCachedGames(_ context.Context, _ time.Time) ([]*store.CachedGame, error) {
	return nil, nil
}
func (m *mockStore) UploadBlob(_ context.Context, _ string, _ string, _ io.Reader) error   { return nil }
func (m *mockStore) DownloadBlob(_ context.Context, _ string, _ io.Writer) error           { return nil }
func (m *mockStore) SignedReadURL(_ context.Context, path string, _ time.Duration) (store.SignedURL, error) {
	return store.SignedURL{URL: "mock://" + path, ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (m *mockStore) DeleteBlobPrefix(_ context.Context, _ string) (int, error)             { return 0, nil }
func (m *mockStore) GetProjectSource(_ context.Context, _ string) (string, error)               { return "", nil }
func (m *mockStore) PutProjectSource(_ context.Context, _, _ string, _ time.Time) error        { return nil }
func (m *mockStore) DeleteProjectSource(_ context.Context, _ string) error                     { return nil }
func (m *mockStore) GetProjectSourceSize(_ context.Context, _ string) (int64, bool, error)     { return 0, false, nil }
func (m *mockStore) SignedProjectSourceURL(_ context.Context, _ string, _ time.Duration) (store.SignedURL, error) {
	return store.SignedURL{}, nil
}
func (m *mockStore) UpdateProjectAI(_ context.Context, p *store.Project, _ *store.AITurn) (time.Time, error) {
	return p.UpdatedAt, nil
}
func (m *mockStore) SetProjectPublished(_ context.Context, _ string, _ bool, _ time.Time) error { return nil }
func (m *mockStore) ListPublishedProjects(_ context.Context, _ int) ([]*store.Project, error)   { return nil, nil }
func (m *mockStore) CreateAITurn(_ context.Context, _ *store.AITurn) error                      { return nil }
func (m *mockStore) ListAIConversation(_ context.Context, _ string, _ int) ([]*store.AITurn, error) {
	return nil, nil
}
func (m *mockStore) DeleteAIConversation(_ context.Context, _ string) (int, error) { return 0, nil }
func (m *mockStore) GetAITurnAfterSource(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, nil
}
func (m *mockStore) Close() error { return nil }

// Auth stubs — satisfy store.Store after the auth methods were added to the interface.
func (m *mockStore) CreateUser(_ context.Context, _ *auth.User, _ string) error {
	return nil
}
func (m *mockStore) GetUserByEmail(_ context.Context, _ string) (*auth.User, string, error) {
	return nil, "", nil
}
func (m *mockStore) GetUserByID(_ context.Context, _ string) (*auth.User, error) {
	return nil, nil
}
func (m *mockStore) CreateSession(_ context.Context, _ *auth.Session) error { return nil }
func (m *mockStore) GetSession(_ context.Context, _ string) (*auth.Session, error) {
	return nil, nil
}
func (m *mockStore) DeleteSession(_ context.Context, _ string) error { return nil }
func (m *mockStore) DeleteExpiredSessions(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

var _ store.Store = (*mockStore)(nil)

func newTestServer() *Server {
	cfg := &config.Config{
		Port:                "8080",
		Version:             "1.0.0",
		Environment:         "test",
		RunSessionMax:       30 * time.Minute,
		RunIdleTimeout:      10 * time.Minute,
		HistoryDefaultLimit: 20,
	}
	return New(cfg, &mockStore{}, nil, nil, nil, auth.NewLocalDevVerifier())
}

// ── Health ───────────────────────────────────────────────────────────────────

func TestHandleHealth(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json; charset=utf-8" {
		t.Errorf("unexpected Content-Type: %s", ct)
	}

	var resp healthResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status: want ok, got %s", resp.Status)
	}
	if resp.Version != "1.0.0" {
		t.Errorf("version: want 1.0.0, got %s", resp.Version)
	}
	if resp.Environment != "test" {
		t.Errorf("environment: want test, got %s", resp.Environment)
	}
}

// ── Template parse ───────────────────────────────────────────────────────────

func TestNewServerParsesAllTemplates(t *testing.T) {
	srv := newTestServer()
	if srv.tmpl == nil {
		t.Fatal("expected tmpl to be non-nil after New()")
	}
	if srv.tmpl.Index == nil {
		t.Error("Index template is nil")
	}
	if srv.tmpl.GameDetail == nil {
		t.Error("GameDetail template is nil")
	}
	if srv.tmpl.Play == nil {
		t.Error("Play template is nil")
	}
	if srv.tmpl.Create == nil {
		t.Error("Create template is nil")
	}
	if srv.tmpl.ProjectDetail == nil {
		t.Error("ProjectDetail template is nil")
	}
}

// ── UI page handlers ─────────────────────────────────────────────────────────

func TestHandlePageIndex(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("unexpected Content-Type: %s", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "StoryCloud") {
		t.Error("expected body to contain 'StoryCloud'")
	}
	if !strings.Contains(body, "1.0.0") {
		t.Error("expected body to contain version")
	}
	if !strings.Contains(body, "test") {
		t.Error("expected body to contain environment")
	}
	if !strings.Contains(body, "/api/ifdb/search") {
		t.Error("expected body to reference IFDB search API")
	}
}

func TestHandlePageGameDetail(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	req := httptest.NewRequest(http.MethodGet, "/games/0dbnusxunq7fw5ro", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "0dbnusxunq7fw5ro") {
		t.Error("expected body to embed the IFDB id")
	}
	if !strings.Contains(body, "/api/ifdb/games/") {
		t.Error("expected body to reference IFDB games API")
	}
}

func TestHandlePagePlay(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	req := httptest.NewRequest(http.MethodGet, "/play/r-01HXZX5K2V0EQB9M7YPQ3", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "r-01HXZX5K2V0EQB9M7YPQ3") {
		t.Error("expected body to embed the run ID")
	}
	if !strings.Contains(body, "/api/runs/") {
		t.Error("expected body to reference the runs API")
	}
}

func TestHandlePageCreate(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	req := httptest.NewRequest(http.MethodGet, "/create", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/api/projects") {
		t.Error("expected body to reference projects API")
	}
	if !strings.Contains(body, "Inform 7") {
		t.Error("expected body to mention Inform 7")
	}
}

func TestHandlePageProjectDetail(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	req := httptest.NewRequest(http.MethodGet, "/projects/p-01HXZX5K3Q0RTB9M7YPZL", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "p-01HXZX5K3Q0RTB9M7YPZL") {
		t.Error("expected body to embed the project ID")
	}
	if !strings.Contains(body, "/api/projects/") {
		t.Error("expected body to reference projects API")
	}
}

// ── §6.1 critical: method mismatch on API routes must return 405, not 200/404 ──

func TestMethodMismatch405(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/health"},
		{http.MethodGet, "/api/runs"},    // POST-only
		{http.MethodDelete, "/api/runs"}, // POST-only
	}

	for _, tc := range tests {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: expected 405, got %d", tc.method, tc.path, w.Code)
		}
	}
}

// TestIndexExactMatch verifies GET / returns the index page but unknown paths
// return 404 (not the index page), confirming /{$} exact-match is working.
func TestIndexExactMatch(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	// Root must return 200.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET / expected 200, got %d", w.Code)
	}

	// Unknown path must return 404, not the index HTML.
	req2 := httptest.NewRequest(http.MethodGet, "/unknown-path-xyz", nil)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("GET /unknown-path-xyz expected 404, got %d", w2.Code)
	}
}

func TestRequestIDHeader(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Header().Get("X-Request-ID") == "" {
		t.Error("X-Request-ID header should be set")
	}
}

func TestIFDBSearchMissingQuery(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/ifdb/search", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", w.Code)
	}
}

func TestIFDBGameInvalidID(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/ifdb/games/INVALID!!!", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", w.Code)
	}
}

func TestRunIDValidation(t *testing.T) {
	srv := newTestServer()
	handler := srv.SetupRoutes()

	req := httptest.NewRequest(http.MethodGet, "/api/runs/not-a-valid-id", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", w.Code)
	}
}
