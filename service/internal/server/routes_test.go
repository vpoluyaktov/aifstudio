// Package server_test contains comprehensive integration-style tests for all
// HTTP endpoints defined in ARCHITECTURE.md §3 and §6.1. These complement the
// unit tests in server_test.go (package server).
//
// Test priority order per QA standards:
//  1. Routing smoke tests (correct status + 405 on method mismatch)
//  2. Minimal decode checks (response decodes into expected type)
//  3. Detailed assertion tests (field values, error shapes)
//
// NOTE: This file targets the FINAL server implementation from ARCHITECTURE.md.
// Some tests will fail until the backend completes all routes.
package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"storycloud/internal/auth"
	"storycloud/internal/config"
	"storycloud/internal/ifdb"
	"storycloud/internal/server"
	"storycloud/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// MockStore for server handler tests
// ─────────────────────────────────────────────────────────────────────────────

type mockStore struct {
	mu       sync.RWMutex
	runs     map[string]*store.Run
	projects map[string]*store.Project
	builds   map[string]*store.Build
	cache    map[string]*store.CachedGame
	blobs    map[string][]byte

	// Call tracking for delete operations.
	deletedPrefixes []string
	deletedRunIDs   []string

	// Error injection for specific methods.
	GetRunErr           error
	CreateRunErr        error
	GetProjErr          error
	CreateBuildErr      error
	DownloadBlobErr     error
	GetProjectSourceErr error

	// AI / publish support.
	aiTurns              map[string][]*store.AITurn // keyed by projectID
	publishedProjectsList []*store.Project           // returned by ListPublishedProjects
	sourceSizes          map[string]int64            // keyed by projectID; present → exists with that size

	// projectSources simulates GCS source objects keyed by project ID.
	// If a key is absent, GetProjectSource returns ("", nil).
	projectSources map[string]string

	// lastSavedProject captures the project snapshot passed to UpdateProjectAI,
	// allowing tests to assert on description and other metadata after a generate turn.
	lastSavedProject *store.Project
}

func newMockStore() *mockStore {
	return &mockStore{
		runs:           make(map[string]*store.Run),
		projects:       make(map[string]*store.Project),
		builds:         make(map[string]*store.Build),
		cache:          make(map[string]*store.CachedGame),
		blobs:          make(map[string][]byte),
		aiTurns:        make(map[string][]*store.AITurn),
		sourceSizes:    make(map[string]int64),
		projectSources: make(map[string]string),
	}
}

func (m *mockStore) CreateRun(_ context.Context, r *store.Run) error {
	if m.CreateRunErr != nil {
		return m.CreateRunErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *r
	m.runs[r.ID] = &cp
	return nil
}
func (m *mockStore) GetRun(_ context.Context, id string) (*store.Run, error) {
	if m.GetRunErr != nil {
		return nil, m.GetRunErr
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.runs[id], nil
}
func (m *mockStore) UpdateRun(_ context.Context, r *store.Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[r.ID] = r
	return nil
}
func (m *mockStore) DeleteRun(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runs, id)
	m.deletedRunIDs = append(m.deletedRunIDs, id)
	return nil
}
func (m *mockStore) DeleteAbandonedPendingRuns(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}
func (m *mockStore) ListRunsByUser(_ context.Context, userID string, limit int) ([]*store.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*store.Run
	for _, r := range m.runs {
		if r.UserID == userID {
			out = append(out, r)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
func (m *mockStore) ListRunsByProject(_ context.Context, projectID string, limit int) ([]*store.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*store.Run
	for _, r := range m.runs {
		if r.ProjectID == projectID {
			out = append(out, r)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
func (m *mockStore) CreateProject(_ context.Context, p *store.Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.projects[p.ID] = &cp
	return nil
}
func (m *mockStore) GetProject(_ context.Context, id string) (*store.Project, error) {
	if m.GetProjErr != nil {
		return nil, m.GetProjErr
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.projects[id], nil
}
func (m *mockStore) UpdateProjectSource(_ context.Context, id, source string, updatedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.projects[id]; p != nil {
		p.Source = source
		p.UpdatedAt = updatedAt
	}
	return nil
}
func (m *mockStore) UpdateProjectMeta(_ context.Context, id, name, description string, updatedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.projects[id]; p != nil {
		p.Name = name
		p.Description = description
		p.UpdatedAt = updatedAt
	}
	return nil
}
func (m *mockStore) UpdateProjectLatestBuild(_ context.Context, id, buildID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.projects[id]; p != nil {
		p.LatestBuildID = buildID
	}
	return nil
}
func (m *mockStore) ListProjectsByOwner(_ context.Context, ownerUID string, limit int) ([]*store.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*store.Project
	for _, p := range m.projects {
		if p.OwnerUID == ownerUID {
			out = append(out, p)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
func (m *mockStore) CreateBuild(_ context.Context, b *store.Build) error {
	if m.CreateBuildErr != nil {
		return m.CreateBuildErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *b
	m.builds[b.ID] = &cp
	return nil
}
func (m *mockStore) GetBuild(_ context.Context, id string) (*store.Build, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.builds[id], nil
}
func (m *mockStore) UpdateBuild(_ context.Context, b *store.Build) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.builds[b.ID] = b
	return nil
}
func (m *mockStore) ListBuildsByProject(_ context.Context, projectID string, limit int) ([]*store.Build, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*store.Build
	for _, b := range m.builds {
		if b.ProjectID == projectID {
			out = append(out, b)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
func (m *mockStore) DeleteBuildsForProject(_ context.Context, projectID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for id, b := range m.builds {
		if b.ProjectID == projectID {
			delete(m.builds, id)
			count++
		}
	}
	return count, nil
}
func (m *mockStore) DeleteRunsForProject(_ context.Context, projectID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for id, r := range m.runs {
		if r.ProjectID == projectID {
			delete(m.runs, id)
			count++
		}
	}
	return count, nil
}
func (m *mockStore) DeleteProject(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.projects, id)
	return nil
}
func (m *mockStore) GetCachedGame(_ context.Context, tuid string) (*store.CachedGame, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g := m.cache[tuid]
	if g == nil || !g.ExpiresAt.After(time.Now()) {
		return nil, nil
	}
	return g, nil
}
func (m *mockStore) PutCachedGame(_ context.Context, g *store.CachedGame) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache[g.TUID] = g
	return nil
}
func (m *mockStore) ListFreshCachedGames(_ context.Context, now time.Time) ([]*store.CachedGame, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*store.CachedGame
	for _, g := range m.cache {
		if g.ExpiresAt.After(now) {
			out = append(out, g)
		}
	}
	return out, nil
}
func (m *mockStore) UploadBlob(_ context.Context, path, _ string, r io.Reader) error {
	data, _ := io.ReadAll(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blobs[path] = data
	return nil
}
func (m *mockStore) DownloadBlob(_ context.Context, _ string, _ io.Writer) error {
	if m.DownloadBlobErr != nil {
		return m.DownloadBlobErr
	}
	return nil
}
func (m *mockStore) SignedReadURL(_ context.Context, path string, ttl time.Duration) (store.SignedURL, error) {
	return store.SignedURL{URL: "mock://" + path, ExpiresAt: time.Now().Add(ttl)}, nil
}
func (m *mockStore) DeleteBlobPrefix(_ context.Context, prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedPrefixes = append(m.deletedPrefixes, prefix)
	return 0, nil
}
func (m *mockStore) GetProjectSource(_ context.Context, id string) (string, error) {
	if m.GetProjectSourceErr != nil {
		return "", m.GetProjectSourceErr
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.projectSources[id], nil
}
func (m *mockStore) PutProjectSource(_ context.Context, projectID, source string, updatedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.projectSources[projectID] = source
	if p := m.projects[projectID]; p != nil {
		p.UpdatedAt = updatedAt
	}
	return nil
}
func (m *mockStore) DeleteProjectSource(_ context.Context, _ string) error                    { return nil }
func (m *mockStore) GetProjectSourceSize(_ context.Context, id string) (int64, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	size, ok := m.sourceSizes[id]
	return size, ok, nil
}
func (m *mockStore) UpdateProjectAI(_ context.Context, p *store.Project, _ *store.AITurn) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.lastSavedProject = &cp
	return p.UpdatedAt, nil
}
func (m *mockStore) SetProjectPublished(_ context.Context, id string, published bool, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.projects[id]; p != nil {
		p.Published = published
		if published {
			p.PublishedAt = &now
		}
	}
	return nil
}
func (m *mockStore) ListPublishedProjects(_ context.Context, limit int) ([]*store.Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.publishedProjectsList) == 0 {
		return []*store.Project{}, nil
	}
	out := m.publishedProjectsList
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (m *mockStore) CreateAITurn(_ context.Context, t *store.AITurn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Mirror production: write source snapshots to the blob map (GCS simulation).
	if t.SourceBefore != "" {
		m.blobs["ai-turns/"+t.ProjectID+"/"+t.ID+"/before.i7"] = []byte(t.SourceBefore)
	}
	if t.SourceAfter != "" {
		m.blobs["ai-turns/"+t.ProjectID+"/"+t.ID+"/after.i7"] = []byte(t.SourceAfter)
	}
	cp := *t
	cp.SourceBefore = ""
	cp.SourceAfter = ""
	m.aiTurns[t.ProjectID] = append(m.aiTurns[t.ProjectID], &cp)
	return nil
}
func (m *mockStore) ListAIConversation(_ context.Context, projectID string, limit int) ([]*store.AITurn, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	turns := m.aiTurns[projectID]
	if len(turns) == 0 {
		return []*store.AITurn{}, nil
	}
	if limit > 0 && len(turns) > limit {
		turns = turns[:limit]
	}
	// Populate SourceBefore/SourceAfter from blob map (mirrors production readAITurnBlobs).
	result := make([]*store.AITurn, len(turns))
	for i, t := range turns {
		clone := *t
		beforeKey := "ai-turns/" + t.ProjectID + "/" + t.ID + "/before.i7"
		afterKey := "ai-turns/" + t.ProjectID + "/" + t.ID + "/after.i7"
		if data, ok := m.blobs[beforeKey]; ok {
			clone.SourceBefore = string(data)
		}
		if data, ok := m.blobs[afterKey]; ok {
			clone.SourceAfter = string(data)
		}
		result[i] = &clone
	}
	return result, nil
}
func (m *mockStore) DeleteAIConversation(_ context.Context, projectID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.aiTurns[projectID])
	delete(m.aiTurns, projectID)
	return n, nil
}
func (m *mockStore) GetAITurnAfterSource(_ context.Context, projectID, turnID string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	key := "ai-turns/" + projectID + "/" + turnID + "/after.i7"
	if data, ok := m.blobs[key]; ok {
		return string(data), true, nil
	}
	return "", false, nil
}
func (m *mockStore) Close() error { return nil }

// Auth stubs — satisfy store.Store interface after auth methods were added.
func (m *mockStore) CreateUser(_ context.Context, _ *auth.User, _ string) error { return nil }
func (m *mockStore) GetUserByEmail(_ context.Context, _ string) (*auth.User, string, error) {
	return nil, "", nil
}
func (m *mockStore) GetUserByID(_ context.Context, _ string) (*auth.User, error) { return nil, nil }
func (m *mockStore) CreateSession(_ context.Context, _ *auth.Session) error       { return nil }
func (m *mockStore) GetSession(_ context.Context, _ string) (*auth.Session, error) {
	return nil, nil
}
func (m *mockStore) DeleteSession(_ context.Context, _ string) error              { return nil }
func (m *mockStore) DeleteExpiredSessions(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

var _ store.Store = (*mockStore)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// Test server factory
// ─────────────────────────────────────────────────────────────────────────────

func testConfig() *config.Config {
	cfg, _ := config.Load()
	cfg.Version = "1.0.test"
	cfg.Environment = "test"
	return cfg
}

// newTS creates an httptest.Server using a local-mode auth verifier
// (always returns "local-dev" user, no Firebase calls).
func newTS(t *testing.T, ms store.Store) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	// NewLocalDevVerifier always returns "local-dev" user — no database calls.
	verifier := auth.NewLocalDevVerifier()
	// nil for ifdb.Client, runner.Manager, build.Manager — only inject what each test needs.
	srv := server.New(cfg, ms, nil, nil, nil, verifier)
	return httptest.NewServer(srv.SetupRoutes())
}

// ─────────────────────────────────────────────────────────────────────────────
// Routing smoke tests — ARCHITECTURE.md §6.1
// Priority 1: every registered route returns the correct status code;
//             wrong-method requests return 405 (not 404 or HTML).
// ─────────────────────────────────────────────────────────────────────────────

func TestRoutingSmoke(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	tests := []struct {
		method     string
		path       string
		wantStatus int
	}{
		// ── Health ──────────────────────────────────────────────
		{"GET", "/health", http.StatusOK},
		{"POST", "/health", http.StatusMethodNotAllowed},

		// ── IFDB (nil client → 503 is OK; 400 on bad param is what we test) ──
		{"GET", "/api/ifdb/search", http.StatusBadRequest},           // missing q → 400
		{"POST", "/api/ifdb/search", http.StatusMethodNotAllowed},    // POST on GET-only
		{"DELETE", "/api/ifdb/search", http.StatusMethodNotAllowed},  // wrong method
		{"GET", "/api/ifdb/games/BAD!", http.StatusBadRequest},       // bad ID regex → 400
		{"POST", "/api/ifdb/games/abc", http.StatusMethodNotAllowed}, // wrong method

		// ── Runs ────────────────────────────────────────────────
		{"GET", "/api/runs", http.StatusMethodNotAllowed},               // GET on POST-only route
		{"GET", "/api/runs/r-01HXZX5K2V0EQB9M7YPQ300001", http.StatusNotFound}, // valid ID, not in store → 404
		{"GET", "/api/runs/bad-id", http.StatusBadRequest},             // invalid ID → 400
		{"PUT", "/api/runs/r-01HXZX5K2V0EQB9M7YPQ300001", http.StatusMethodNotAllowed}, // wrong method

		// ── Projects ────────────────────────────────────────────
		{"GET", "/api/projects/p-01HXZX5K3Q0RTB9M7YPZL00001", http.StatusNotFound},    // not found → 404
		{"DELETE", "/api/projects/p-01HXZX5K3Q0RTB9M7YPZL00001", http.StatusNotFound}, // not found → 404

		// ── UI pages ────────────────────────────────────────────
		{"GET", "/", http.StatusOK},
		{"POST", "/", http.StatusMethodNotAllowed},
		{"GET", "/games/0dbnusxunq7fw5ro", http.StatusOK},
		{"GET", "/play/r-01HXZX5K2V0EQB9M7YPQ300001", http.StatusOK},
		{"GET", "/create", http.StatusOK},
		{"POST", "/create", http.StatusMethodNotAllowed},
		{"GET", "/projects/p-01HXZX5K3Q0RTB9M7YPZL00001", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			var body io.Reader
			if tt.method == "POST" || tt.method == "PUT" {
				body = bytes.NewReader([]byte("{}"))
			}
			req, _ := http.NewRequest(tt.method, ts.URL+tt.path, body)
			if tt.method == "POST" || tt.method == "PUT" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request error: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d; want %d\nbody snippet: %s",
					resp.StatusCode, tt.wantStatus, truncate(string(b), 300))
			}
		})
	}
}

// ARCHITECTURE.md §6.1 CRITICAL: GET /{$} (exact match) must NOT swallow 405
// responses from API routes. This is the key test for the root pattern decision.
func TestRootPatternExactMatchDoesNotSwallow405(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	// GET /api/runs is a POST-only route. Sending GET must get 405, not 200 HTML.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/runs", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/runs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("GET /api/runs = 200 (likely index HTML swallowed the 405)\n"+
			"body snippet: %s\n"+
			"FIX: Use 'GET /{$}' not 'GET /' for the index route (ARCHITECTURE.md §6.1)",
			truncate(string(body), 200))
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/runs = %d; want 405", resp.StatusCode)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /health — ARCHITECTURE.md §3.1
// ─────────────────────────────────────────────────────────────────────────────

func TestHealthOK(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	assertContentType(t, resp, "application/json")

	var body struct {
		Status      string `json:"status"`
		Version     string `json:"version"`
		Environment string `json:"environment"`
	}
	mustDecode(t, resp.Body, &body)

	if body.Status != "ok" {
		t.Errorf("status = %q; want ok", body.Status)
	}
	if body.Version == "" {
		t.Error("version is empty")
	}
	if body.Environment == "" {
		t.Error("environment is empty")
	}
}

func TestHealthWithNilStore(t *testing.T) {
	// ARCHITECTURE.md §3.1: health must not touch store, GCS, or IFDB.
	cfg := testConfig()
	verifier := auth.NewLocalDevVerifier()
	srv := server.New(cfg, nil, nil, nil, nil, verifier)
	ts := httptest.NewServer(srv.SetupRoutes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health with nil store = %d; want 200", resp.StatusCode)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/ifdb/search — ARCHITECTURE.md §3.2 (validation only, no IFDB client)
// ─────────────────────────────────────────────────────────────────────────────

func TestIFDBSearchValidation(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	tests := []struct {
		name   string
		query  string
		want   int
	}{
		{"missing q param", "", http.StatusBadRequest},
		{"empty q after trim", "   ", http.StatusBadRequest},
		{"q too long (201 chars)", strings.Repeat("a", 201), http.StatusBadRequest},
		{"q exactly 200 chars (max)", strings.Repeat("a", 200), -1}, // not 400; may be 503 with nil client
		{"q 1 char (min)", "z", -1},                                   // not 400
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqURL := ts.URL + "/api/ifdb/search"
			if tt.query != "" {
				reqURL += "?q=" + url.QueryEscape(tt.query)
			}
			resp, err := http.Get(reqURL)
			if err != nil {
				t.Fatalf("GET %s: %v", reqURL, err)
			}
			defer resp.Body.Close()

			if tt.want == http.StatusBadRequest {
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("status = %d; want 400", resp.StatusCode)
				}
				assertErrorShape(t, resp.Body)
			} else {
				// Not a validation failure — just ensure it's NOT 400
				if resp.StatusCode == http.StatusBadRequest {
					body, _ := io.ReadAll(resp.Body)
					t.Errorf("valid query returned 400: %s", body)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/ifdb/games/{id} — ARCHITECTURE.md §3.3 (validation only)
// ─────────────────────────────────────────────────────────────────────────────

func TestIFDBGameIDValidation(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	tests := []struct {
		name string
		id   string
		want int
	}{
		{"too short (< 10)", "abc", http.StatusBadRequest},
		{"too long (> 32)", strings.Repeat("a", 33), http.StatusBadRequest},
		{"uppercase letter", "ABCDEFGHIJ", http.StatusBadRequest},
		{"uppercase letter (regex rejects)", "ABCDEFGHIJ1", http.StatusBadRequest},
		{"valid 10 chars", "abcdefghij", -1}, // may be 503 with nil client, but NOT 400
		{"valid TUID", "0dbnusxunq7fw5ro", -1},
		{"valid 32 chars", strings.Repeat("a", 32), -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/api/ifdb/games/" + url.PathEscape(tt.id))
			if err != nil {
				t.Fatalf("GET /api/ifdb/games/%s: %v", tt.id, err)
			}
			defer resp.Body.Close()

			if tt.want == http.StatusBadRequest {
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("id=%q → status = %d; want 400", tt.id, resp.StatusCode)
				}
			} else {
				if resp.StatusCode == http.StatusBadRequest {
					body, _ := io.ReadAll(resp.Body)
					t.Errorf("valid id=%q returned 400: %s", tt.id, body)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/runs — ARCHITECTURE.md §3.4 (validation)
// ─────────────────────────────────────────────────────────────────────────────

func TestCreateRunValidation(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	tests := []struct {
		name   string
		body   map[string]interface{}
		want   int
	}{
		{"missing sourceType", map[string]interface{}{}, 400},
		{"invalid sourceType", map[string]interface{}{"sourceType": "ftp"}, 400},
		{"ifdb with bad ifdbId regex", map[string]interface{}{
			"sourceType": "ifdb", "ifdbId": "BAD!",
		}, 400},
		{"url with http:// not https://", map[string]interface{}{
			"sourceType": "url", "artifactUrl": "http://example.com/story.z5",
		}, 400},
		{"url missing artifactUrl", map[string]interface{}{
			"sourceType": "url",
		}, 400},
		{"build missing buildId", map[string]interface{}{
			"sourceType": "build",
		}, 400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := json.Marshal(tt.body)
			resp, err := http.Post(ts.URL+"/api/runs", "application/json", bytes.NewReader(b))
			if err != nil {
				t.Fatalf("POST /api/runs: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.want {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d; want %d\nbody: %s", resp.StatusCode, tt.want, body)
			}
			if resp.StatusCode == 400 {
				assertErrorShape(t, resp.Body)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/runs/{id} — ARCHITECTURE.md §3.5 (ID validation + not found)
// ─────────────────────────────────────────────────────────────────────────────

func TestGetRunIDValidation(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	tests := []struct {
		name string
		id   string
		want int
	}{
		{"no r- prefix", "01HXZX5K2V0EQB9M7YPQ300001", 400},
		{"wrong prefix", "x-01HXZX5K2V0EQB9M7YPQ300001", 400},
		{"too short", "r-SHORT", 400},
		{"lowercase ulid", "r-01hxzx5k2v0eqb9m7ypq3", 400},
		{"valid ID not in store", "r-01HXZX5K2V0EQB9M7YPQ300001", 404},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + "/api/runs/" + tt.id)
			if err != nil {
				t.Fatalf("GET /api/runs/%s: %v", tt.id, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.want {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("id=%q → status = %d; want %d\nbody: %s", tt.id, resp.StatusCode, tt.want, body)
			}
		})
	}
}

func TestGetRunFoundShape(t *testing.T) {
	ms := newMockStore()
	runID := "r-01HXZX5K2V0EQB9M7YPQ300001"
	now := time.Now().UTC()

	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:         runID,
		SourceType: "ifdb",
		IFDBId:     "0dbnusxunq7fw5ro",
		Format:     "z5",
		Status:     "pending",
		CreatedAt:  now,
	})

	ts := newTS(t, ms)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/runs/" + runID)
	if err != nil {
		t.Fatalf("GET /api/runs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, body)
	}

	assertContentType(t, resp, "application/json")

	var body struct {
		ID         string `json:"id"`
		SourceType string `json:"sourceType"`
		IFDBId     string `json:"ifdbId"`
		Format     string `json:"format"`
		Status     string `json:"status"`
		CreatedAt  string `json:"createdAt"`
		StartURL   string `json:"startURL"`
	}
	mustDecode(t, resp.Body, &body)

	if body.ID != runID {
		t.Errorf("id = %q; want %q", body.ID, runID)
	}
	if body.SourceType != "ifdb" {
		t.Errorf("sourceType = %q; want ifdb", body.SourceType)
	}
	if body.Status != "pending" {
		t.Errorf("status = %q; want pending", body.Status)
	}
	if body.StartURL == "" {
		t.Error("startURL is empty")
	}
	if !strings.Contains(body.StartURL, runID) {
		t.Errorf("startURL %q should contain run ID", body.StartURL)
	}
	if body.CreatedAt == "" {
		t.Error("createdAt is empty")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/projects — ARCHITECTURE.md §3.7
// ─────────────────────────────────────────────────────────────────────────────

func TestCreateProjectValidation(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	tests := []struct {
		name string
		body map[string]interface{}
		want int
	}{
		{"empty name", map[string]interface{}{"name": "", "source": ""}, 400},
		{"whitespace-only name", map[string]interface{}{"name": "   ", "source": ""}, 400},
		{"name too long (81 chars)", map[string]interface{}{
			"name": strings.Repeat("a", 81), "source": "",
		}, 400},
		{"source too large (500001 chars)", map[string]interface{}{
			"name": "X", "source": strings.Repeat("a", 500001),
		}, 413}, // or 400, either is acceptable
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _ := json.Marshal(tt.body)
			resp, err := http.Post(ts.URL+"/api/projects", "application/json", bytes.NewReader(b))
			if err != nil {
				t.Fatalf("POST /api/projects: %v", err)
			}
			defer resp.Body.Close()

			// Allow either 413 or 400 for the source-too-large case.
			gotWant := resp.StatusCode == tt.want ||
				(tt.want == 413 && resp.StatusCode == 400)
			if !gotWant {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d; want %d\nbody: %s", resp.StatusCode, tt.want, body)
			}
		})
	}
}

func TestCreateProjectSuccess(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	b, _ := json.Marshal(map[string]interface{}{
		"name":   "The Blue Door",
		"source": "\"The Blue Door\" by Alex.\n\nThe Hallway is a room.\n",
	})
	resp, err := http.Post(ts.URL+"/api/projects", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 201\nbody: %s", resp.StatusCode, body)
	}
	assertContentType(t, resp, "application/json")

	var body struct {
		ID            string `json:"id"`
		OwnerUID      string `json:"ownerUid"`
		Name          string `json:"name"`
		CreatedAt     string `json:"createdAt"`
		UpdatedAt     string `json:"updatedAt"`
		Source        string `json:"source"`
		LatestBuildID string `json:"latestBuildId"`
	}
	mustDecode(t, resp.Body, &body)

	if !strings.HasPrefix(body.ID, "p-") {
		t.Errorf("id = %q; want p- prefix", body.ID)
	}
	if body.Name != "The Blue Door" {
		t.Errorf("name = %q; want 'The Blue Door'", body.Name)
	}
	if body.OwnerUID == "" {
		t.Error("ownerUid is empty")
	}
	if body.CreatedAt == "" {
		t.Error("createdAt is empty")
	}
	if body.UpdatedAt == "" {
		t.Error("updatedAt is empty")
	}
	// latestBuildId is empty for a new project
	if body.LatestBuildID != "" {
		t.Errorf("latestBuildId = %q; want empty for new project", body.LatestBuildID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/projects/{id} — ARCHITECTURE.md §3.8
// ─────────────────────────────────────────────────────────────────────────────

func TestGetProjectNotFound(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/projects/p-01HXZX5K3Q0RTB9M7YPZL00001", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404", resp.StatusCode)
	}
	assertErrorShape(t, resp.Body)
}

func TestGetProjectForbiddenForOtherOwner(t *testing.T) {
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	// Create a project owned by "other-user", not "local-dev".
	projID := "p-01HXZX5K3Q0RTB9M7YPZL00001"
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:       projID,
		OwnerUID: "other-user-uid",
		Name:     "Other's Project",
	})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/projects/"+projID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("other owner → status = %d; want 403", resp.StatusCode)
	}
	assertErrorShape(t, resp.Body)
}

func TestGetProjectOwnerSuccess(t *testing.T) {
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	projID := "p-01HXZX5K3Q0RTB9M7YPZL00001"
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:       projID,
		OwnerUID: "local-dev", // matches local-mode auth user
		Name:     "The Blue Door",
		Source:   "source code",
	})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/projects/"+projID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/projects: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, body)
	}

	var body struct {
		ID       string `json:"id"`
		OwnerUID string `json:"ownerUid"`
		Name     string `json:"name"`
	}
	mustDecode(t, resp.Body, &body)

	if body.ID != projID {
		t.Errorf("id = %q; want %q", body.ID, projID)
	}
	if body.Name != "The Blue Door" {
		t.Errorf("name = %q; want 'The Blue Door'", body.Name)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// PUT /api/projects/{id}/source — ARCHITECTURE.md §3.9
// ─────────────────────────────────────────────────────────────────────────────

func TestUpdateProjectSource(t *testing.T) {
	newSource := "\"The Blue Door\" by Alex.\n\nThe Hallway is a room.\n"

	t.Run("PATCH returns 200 with body", func(t *testing.T) {
		ms := newMockStore()
		ts := newTS(t, ms)
		defer ts.Close()

		projID := "p-01HXZX5K3Q0RTB9M7YPZL00001"
		_ = ms.CreateProject(context.Background(), &store.Project{
			ID:       projID,
			OwnerUID: "local-dev",
			Name:     "The Blue Door",
		})

		body, _ := json.Marshal(map[string]string{"source": newSource})
		req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/projects/"+projID+"/source", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PATCH /api/projects/source: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
		}

		var result struct {
			ID          string `json:"id"`
			UpdatedAt   string `json:"updatedAt"`
			SourceBytes int    `json:"sourceBytes"`
		}
		mustDecode(t, resp.Body, &result)

		if result.ID != projID {
			t.Errorf("id = %q; want %q", result.ID, projID)
		}
		if result.UpdatedAt == "" {
			t.Error("updatedAt is empty")
		}
		if result.SourceBytes != len(newSource) {
			t.Errorf("sourceBytes = %d; want %d", result.SourceBytes, len(newSource))
		}
	})

	t.Run("PUT returns 204 no content", func(t *testing.T) {
		ms := newMockStore()
		ts := newTS(t, ms)
		defer ts.Close()

		projID := "p-01HXZX5K3Q0RTB9M7YPZL00001"
		_ = ms.CreateProject(context.Background(), &store.Project{
			ID:       projID,
			OwnerUID: "local-dev",
			Name:     "The Blue Door",
		})

		body, _ := json.Marshal(map[string]string{"source": newSource})
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/projects/"+projID+"/source", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT /api/projects/source: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNoContent {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d; want 204\nbody: %s", resp.StatusCode, b)
		}

		// Verify source was persisted.
		got, _ := ms.GetProjectSource(context.Background(), projID)
		if got != newSource {
			t.Errorf("stored source = %q; want %q", got, newSource)
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/projects/{id}/builds — ARCHITECTURE.md §3.10
// ─────────────────────────────────────────────────────────────────────────────

func TestCreateBuild202(t *testing.T) {
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	projID := "p-01HXZX5K3Q0RTB9M7YPZL00001"
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:       projID,
		OwnerUID: "local-dev",
		Name:     "The Blue Door",
		Source:   "source",
	})

	b, _ := json.Marshal(map[string]interface{}{})
	resp, err := http.Post(ts.URL+"/api/projects/"+projID+"/builds", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/projects/builds: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 202\nbody: %s", resp.StatusCode, body)
	}

	var body struct {
		ID            string `json:"id"`
		ProjectID     string `json:"projectId"`
		Status        string `json:"status"`
		CreatedAt     string `json:"createdAt"`
		QueuePosition int    `json:"queuePosition"`
	}
	mustDecode(t, resp.Body, &body)

	if !strings.HasPrefix(body.ID, "b-") {
		t.Errorf("id = %q; want b- prefix", body.ID)
	}
	if body.ProjectID != projID {
		t.Errorf("projectId = %q; want %q", body.ProjectID, projID)
	}
	if body.Status != "pending" {
		t.Errorf("status = %q; want pending", body.Status)
	}
}

// mockBuilder implements server.BuilderService for conflict-detection tests.
type mockBuilder struct {
	mu     sync.Mutex
	active map[string]string // projectID → buildID
}

func newMockBuilder() *mockBuilder {
	return &mockBuilder{active: make(map[string]string)}
}

func (b *mockBuilder) StartBuild(_ context.Context, bld *store.Build, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if existingID, ok := b.active[bld.ProjectID]; ok {
		return fmt.Errorf("conflict:409:build %s already active for project %s", existingID, bld.ProjectID)
	}
	b.active[bld.ProjectID] = bld.ID
	return nil
}

func (b *mockBuilder) ActiveBuildID(projectID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.active[projectID]
}

var _ server.BuilderService = (*mockBuilder)(nil)

// newTSWithBuilder creates a test server with a custom BuilderService.
func newTSWithBuilder(t *testing.T, ms store.Store, builder server.BuilderService) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	verifier := auth.NewLocalDevVerifier()
	srv := server.New(cfg, ms, nil, nil, builder, verifier)
	return httptest.NewServer(srv.SetupRoutes())
}

func TestCreateBuildConflict(t *testing.T) {
	ms := newMockStore()
	ts := newTSWithBuilder(t, ms, newMockBuilder())
	defer ts.Close()

	projID := "p-01HXZX5K3Q0RTB9M7YPZL00001"
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:       projID,
		OwnerUID: "local-dev",
		Name:     "Conflict Project",
		Source:   "source",
	})

	body, _ := json.Marshal(map[string]interface{}{})

	// First build
	r1, _ := http.Post(ts.URL+"/api/projects/"+projID+"/builds", "application/json", bytes.NewReader(body))
	r1.Body.Close()
	if r1.StatusCode != http.StatusAccepted {
		t.Skipf("first build returned %d, cannot test conflict", r1.StatusCode)
	}

	// Second build — same project, same state → 409
	body, _ = json.Marshal(map[string]interface{}{})
	r2, err := http.Post(ts.URL+"/api/projects/"+projID+"/builds", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("second POST /builds: %v", err)
	}
	defer r2.Body.Close()

	if r2.StatusCode != http.StatusConflict {
		t.Errorf("second build → status = %d; want 409", r2.StatusCode)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/projects/{id}/builds/{buildId} — ARCHITECTURE.md §3.11
// ─────────────────────────────────────────────────────────────────────────────

func TestGetBuildNotFound(t *testing.T) {
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	projID := "p-01HXZX5K3Q0RTB9M7YPZL00001"
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:       projID,
		OwnerUID: "local-dev",
	})

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+projID+"/builds/b-00000000000000000000000001", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/projects/.../builds/...: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404", resp.StatusCode)
	}
}

func TestGetBuildShape(t *testing.T) {
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	projID := "p-01HXZX5K3Q0RTB9M7YPZL00001"
	buildID := "b-00000000000000000000000001"
	now := time.Now().UTC()

	_ = ms.CreateProject(context.Background(), &store.Project{
		ID: projID, OwnerUID: "local-dev",
	})
	_ = ms.CreateBuild(context.Background(), &store.Build{
		ID:        buildID,
		ProjectID: projID,
		OwnerUID:  "local-dev",
		Status:    "succeeded",
		CreatedAt: now,
	})

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/projects/"+projID+"/builds/"+buildID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET build: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, body)
	}

	var body struct {
		ID        string `json:"id"`
		ProjectID string `json:"projectId"`
		Status    string `json:"status"`
		CreatedAt string `json:"createdAt"`
	}
	mustDecode(t, resp.Body, &body)

	if body.ID != buildID {
		t.Errorf("id = %q; want %q", body.ID, buildID)
	}
	if body.ProjectID != projID {
		t.Errorf("projectId = %q; want %q", body.ProjectID, projID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// UI pages — ARCHITECTURE.md §3.12
// ─────────────────────────────────────────────────────────────────────────────

func TestUIPageResponses(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	tests := []struct {
		name string
		path string
	}{
		{"index /{$}", "/"},
		{"game detail", "/games/0dbnusxunq7fw5ro"},
		{"play page", "/play/r-01HXZX5K2V0EQB9M7YPQ300001"},
		{"create page", "/create"},
		{"project detail page", "/projects/p-01HXZX5K3Q0RTB9M7YPZL00001"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Get(ts.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("GET %s = %d; want 200\nbody: %s", tt.path, resp.StatusCode, body)
			}
			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q; want text/html", ct)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Error response shape — ARCHITECTURE.md §3 preamble
// ─────────────────────────────────────────────────────────────────────────────

func TestAllErrorResponsesHaveErrorAndCode(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	errCases := []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"search missing q", "GET", "/api/ifdb/search", nil},
		{"invalid run ID", "GET", "/api/runs/bad-id", nil},
		{"run not found", "GET", "/api/runs/r-01HXZX5K2V0EQB9M7YPQ300001", nil},
		{"create run missing sourceType", "POST", "/api/runs", map[string]interface{}{}},
		{"project not found", "GET", "/api/projects/p-01HXZX5K3Q0RTB9M7YPZL00001", nil},
	}

	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			var reqBody io.Reader
			if tc.body != nil {
				b, _ := json.Marshal(tc.body)
				reqBody = bytes.NewReader(b)
			}
			req, _ := http.NewRequest(tc.method, ts.URL+tc.path, reqBody)
			if tc.body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode < 400 {
				t.Skipf("status %d < 400; not an error response", resp.StatusCode)
			}

			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q; want application/json for error", ct)
			}

			var errBody struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			body, _ := io.ReadAll(resp.Body)
			if err := json.Unmarshal(body, &errBody); err != nil {
				t.Fatalf("decode error response: %v\nbody: %s", err, body)
			}
			if errBody.Error == "" {
				t.Errorf("error response missing 'error' field\nbody: %s", body)
			}
			if errBody.Code == "" {
				t.Errorf("error response missing 'code' field\nbody: %s", body)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DELETE /api/projects/{id} cascade — artifact cleanup
// ─────────────────────────────────────────────────────────────────────────────

// TestDeleteProjectCascade verifies that all GCS prefixes and Firestore docs
// are cleaned up when a project is deleted, including:
//   - builds/{id}/ for every build, regardless of whether ArtifactPath is set
//   - sessions/{id}/ and transcripts/{id} for every run
//   - ai-turns/{projectId}/ (handled inside DeleteAIConversation)
//   - projects/{projectId}/source.i7 (handled inside DeleteProjectSource)
func TestDeleteProjectCascade(t *testing.T) {
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	const ownerID = "local-dev"
	projID := "p-01HXZX5K3Q0RTB9M7YPZL00001"
	succeededBuildID := "b-00000000000000000000000001"
	failedBuildID := "b-00000000000000000000000002"
	runID := "r-01HXZX5K2V0EQB9M7YPQ300001"
	now := time.Now().UTC()

	_ = ms.CreateProject(context.Background(), &store.Project{
		ID:       projID,
		OwnerUID: ownerID,
		Name:     "Cascade Test",
	})
	// Succeeded build: has both ArtifactPath and LogPath.
	_ = ms.CreateBuild(context.Background(), &store.Build{
		ID:           succeededBuildID,
		ProjectID:    projID,
		OwnerUID:     ownerID,
		Status:       "succeeded",
		ArtifactPath: "builds/" + succeededBuildID + "/artifact.ulx",
		LogPath:      "builds/" + succeededBuildID + "/build.log",
		CreatedAt:    now,
	})
	// Failed build: ArtifactPath is empty but LogPath is set — this is the key
	// regression case; the old code skipped GCS cleanup when ArtifactPath was "".
	_ = ms.CreateBuild(context.Background(), &store.Build{
		ID:           failedBuildID,
		ProjectID:    projID,
		OwnerUID:     ownerID,
		Status:       "failed",
		ArtifactPath: "", // empty — build failed before producing an artifact
		LogPath:      "builds/" + failedBuildID + "/build.log",
		CreatedAt:    now,
	})
	// A run linked to the project.
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:        runID,
		ProjectID: projID,
		UserID:    ownerID,
		Status:    "finished",
		CreatedAt: now,
	})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/projects/"+projID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /api/projects/%s: %v", projID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 204\nbody: %s", resp.StatusCode, body)
	}

	// ── Firestore docs must be gone ───────────────────────────────────────────

	if p, _ := ms.GetProject(context.Background(), projID); p != nil {
		t.Error("project Firestore doc should be deleted")
	}
	if b, _ := ms.GetBuild(context.Background(), succeededBuildID); b != nil {
		t.Error("succeeded build Firestore doc should be deleted")
	}
	if b, _ := ms.GetBuild(context.Background(), failedBuildID); b != nil {
		t.Error("failed build Firestore doc should be deleted")
	}
	if r, _ := ms.GetRun(context.Background(), runID); r != nil {
		t.Error("run Firestore doc should be deleted")
	}

	// ── GCS DeleteBlobPrefix calls ────────────────────────────────────────────

	ms.mu.RLock()
	prefixes := make([]string, len(ms.deletedPrefixes))
	copy(prefixes, ms.deletedPrefixes)
	ms.mu.RUnlock()

	wantPrefixes := []string{
		"builds/" + succeededBuildID + "/",
		// Key regression: failed build must be cleaned even with empty ArtifactPath.
		"builds/" + failedBuildID + "/",
		"sessions/" + runID + "/",
		// Legacy transcript prefix (no trailing slash) covers transcripts/{id}.txt.
		"transcripts/" + runID,
	}
	for _, want := range wantPrefixes {
		found := false
		for _, got := range prefixes {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DeleteBlobPrefix not called with %q\ncalled with: %v", want, prefixes)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func assertContentType(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, want) {
		t.Errorf("Content-Type = %q; want prefix %q", ct, want)
	}
}

func assertErrorShape(t *testing.T, body io.Reader) {
	t.Helper()
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	data, _ := io.ReadAll(body)
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("decode error response: %v\nbody: %s", err, data)
	}
	if e.Error == "" {
		t.Errorf("error response missing 'error' field\nbody: %s", data)
	}
	if e.Code == "" {
		t.Errorf("error response missing 'code' field\nbody: %s", data)
	}
}

func mustDecode(t *testing.T, r io.Reader, v interface{}) {
	t.Helper()
	data, _ := io.ReadAll(r)
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, data)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// ─────────────────────────────────────────────────────────────────────────────
// newTSWithIFDB — test server factory with an injected IFDB client
// ─────────────────────────────────────────────────────────────────────────────

func newTSWithIFDB(t *testing.T, ms store.Store, ifdbClient *ifdb.Client) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	verifier := auth.NewLocalDevVerifier()
	srv := server.New(cfg, ms, ifdbClient, nil, nil, verifier)
	return httptest.NewServer(srv.SetupRoutes())
}

// newTestIFDBClient creates an ifdb.Client pointing at the given baseURL.
func newTestIFDBClient(baseURL string) *ifdb.Client {
	return ifdb.NewClient(ifdb.ClientOptions{
		BaseURL:     baseURL,
		UserAgent:   "StoryCloud/test",
		CacheTTL:    10 * time.Minute,
		GlobalQPS:   100,
		GlobalBurst: 1000,
		PerIPQPS:    100,
		PerIPBurst:  1000,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Routing smoke — session persistence routes (ARCHITECTURE.md §A.7.1)
// ─────────────────────────────────────────────────────────────────────────────

func TestSessionPersistenceRoutingSmoke(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	tests := []struct {
		method     string
		path       string
		wantStatus int
	}{
		// GET /api/runs/by-user — always 200, returns [] for a new random user
		{"GET", "/api/runs/by-user", http.StatusOK},
		// Wrong method on by-user: POST → 405 (literal path registered for GET only).
		// DELETE /api/runs/by-user falls through to DELETE /api/runs/{id} with
		// id="by-user" which fails regex validation → 400, not 405.
		{"POST", "/api/runs/by-user", http.StatusMethodNotAllowed},
		{"DELETE", "/api/runs/by-user", http.StatusBadRequest},

		// DELETE /api/runs/{id} — 404 when run not in store
		{"DELETE", "/api/runs/r-01HXZX5K2V0EQB9M7YPQ300001", http.StatusNotFound},

		// POST /api/runs/{id}/restart — 404 when run not in store
		{"POST", "/api/runs/r-01HXZX5K2V0EQB9M7YPQ300001/restart", http.StatusNotFound},
		// Wrong method on restart
		{"GET", "/api/runs/r-01HXZX5K2V0EQB9M7YPQ300001/restart", http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			var body io.Reader
			if tt.method == "POST" || tt.method == "PUT" {
				body = bytes.NewReader([]byte("{}"))
			}
			req, _ := http.NewRequest(tt.method, ts.URL+tt.path, body)
			if tt.method == "POST" || tt.method == "PUT" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request error: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d; want %d\nbody: %s",
					resp.StatusCode, tt.wantStatus, truncate(string(b), 300))
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// GET /api/runs/by-user — ARCHITECTURE.md §A.7.1
// ─────────────────────────────────────────────────────────────────────────────

func TestListRunsByUserEmpty(t *testing.T) {
	// Empty store → 200 with bare [] (not null, not wrapped object).
	ts := newTS(t, newMockStore())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/runs/by-user", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/runs/by-user: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, body)
	}
	assertContentType(t, resp, "application/json")

	var items []json.RawMessage
	mustDecode(t, resp.Body, &items)
	if len(items) != 0 {
		t.Errorf("len(items) = %d; want 0 for empty store", len(items))
	}
}

func TestListRunsByUserFiltersToOwner(t *testing.T) {
	// Pre-populate store with runs for two different users.
	// Local-mode auth returns UID "local-dev"; only that user's runs should appear.
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	// Local-mode auth.Verifier always identifies the caller as "local-dev".
	const myUser = "local-dev"
	const otherUser = "other-user-uid"
	now := time.Now().UTC()

	myRunID := "r-01HXZX5K2V0EQB9M7YPQ300001"
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:        myRunID,
		UserID:    myUser,
		Title:     "Zork I",
		Status:    "suspended",
		Format:    "z5",
		TurnCount: 7,
		CreatedAt: now,
	})
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:        "r-01HXZX5K2V0EQB9M7YPQ300002",
		UserID:    otherUser,
		Title:     "Other Game",
		Status:    "suspended",
		CreatedAt: now,
	})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/runs/by-user", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/runs/by-user: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, body)
	}
	assertContentType(t, resp, "application/json")

	var items []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Status    string `json:"status"`
		Format    string `json:"format"`
		TurnCount int    `json:"turnCount"`
	}
	mustDecode(t, resp.Body, &items)

	if len(items) != 1 {
		t.Fatalf("len(items) = %d; want 1 (only my run)", len(items))
	}
	if items[0].ID != myRunID {
		t.Errorf("id = %q; want %q", items[0].ID, myRunID)
	}
	if items[0].Title != "Zork I" {
		t.Errorf("title = %q; want Zork I", items[0].Title)
	}
	if items[0].Format != "z5" {
		t.Errorf("format = %q; want z5", items[0].Format)
	}
	if items[0].TurnCount != 7 {
		t.Errorf("turnCount = %d; want 7", items[0].TurnCount)
	}
}

func TestListRunsByUserNoCookieReturnsEmpty(t *testing.T) {
	// Runs owned by a different user should not appear for "local-dev".
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	// Pre-populate a run for a different user — should NOT appear for local-dev.
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:     "r-01HXZX5K2V0EQB9M7YPQ300001",
		UserID: "some-other-firebase-uid",
		Status: "suspended",
	})

	resp, err := http.Get(ts.URL + "/api/runs/by-user")
	if err != nil {
		t.Fatalf("GET /api/runs/by-user: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, body)
	}

	var items []json.RawMessage
	mustDecode(t, resp.Body, &items)
	if len(items) != 0 {
		t.Errorf("len(items) = %d; want 0 for new (cookie-less) user", len(items))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// DELETE /api/runs/{id} — ARCHITECTURE.md §A.7.1
// ─────────────────────────────────────────────────────────────────────────────

func TestDeleteRunNotFound(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodDelete,
		ts.URL+"/api/runs/r-01HXZX5K2V0EQB9M7YPQ300001", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404", resp.StatusCode)
	}
	assertErrorShape(t, resp.Body)
}

func TestDeleteRunWrongUser(t *testing.T) {
	// Run is owned by a different Firebase UID; local-mode auth returns "local-dev"
	// which does not match → expect 403.
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	runID := "r-01HXZX5K2V0EQB9M7YPQ300001"
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:     runID,
		UserID: "another-firebase-uid",
		Status: "suspended",
	})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/runs/"+runID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d; want 403", resp.StatusCode)
	}
	assertErrorShape(t, resp.Body)
}

func TestDeleteRunOwnerSuccess(t *testing.T) {
	// Local-mode auth returns UID "local-dev"; create the run owned by that UID.
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	const ownerID = "local-dev" // matches local-mode auth.Verifier fixed UID
	runID := "r-01HXZX5K2V0EQB9M7YPQ300001"

	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:     runID,
		UserID: ownerID,
		Status: "suspended",
	})

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/runs/"+runID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 204\nbody: %s", resp.StatusCode, body)
	}

	// Run must be gone from store.
	got, _ := ms.GetRun(context.Background(), runID)
	if got != nil {
		t.Error("run should be deleted from store")
	}

	ms.mu.RLock()
	prefixes := make([]string, len(ms.deletedPrefixes))
	copy(prefixes, ms.deletedPrefixes)
	runIDs := make([]string, len(ms.deletedRunIDs))
	copy(runIDs, ms.deletedRunIDs)
	ms.mu.RUnlock()

	// Verify DeleteBlobPrefix was called for sessions (covers new-format transcripts)
	// and for the legacy transcript prefix (no trailing slash, covers transcripts/{id}.txt).
	wantPrefixes := []string{"sessions/" + runID + "/", "transcripts/" + runID}
	for _, want := range wantPrefixes {
		found := false
		for _, got := range prefixes {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DeleteBlobPrefix not called with %q; called with %v", want, prefixes)
		}
	}

	// Verify DeleteRun was called.
	deletedRun := false
	for _, id := range runIDs {
		if id == runID {
			deletedRun = true
			break
		}
	}
	if !deletedRun {
		t.Errorf("DeleteRun not called with %q; called with %v", runID, runIDs)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/runs/{id}/restart — ARCHITECTURE.md §A.7.1
// ─────────────────────────────────────────────────────────────────────────────

func TestRestartRunNotFound(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	body := bytes.NewReader([]byte("{}"))
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/runs/r-01HXZX5K2V0EQB9M7YPQ300001/restart", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST restart: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404", resp.StatusCode)
	}
	assertErrorShape(t, resp.Body)
}

func TestRestartRunWrongUser(t *testing.T) {
	// Run owned by a different Firebase UID; local-mode auth is "local-dev" → 403.
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	runID := "r-01HXZX5K2V0EQB9M7YPQ300001"
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:     runID,
		UserID: "another-firebase-uid",
		Status: "suspended",
		Title:  "Zork I",
		Format: "z5",
	})

	body := bytes.NewReader([]byte("{}"))
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runs/"+runID+"/restart", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST restart: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d; want 403", resp.StatusCode)
	}
	assertErrorShape(t, resp.Body)
}

func TestRestartRunSuccess(t *testing.T) {
	// Local-mode auth returns UID "local-dev"; create the run owned by that UID.
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	const ownerID = "local-dev" // matches local-mode auth.Verifier fixed UID
	origRunID := "r-01HXZX5K2V0EQB9M7YPQ300001"

	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:         origRunID,
		UserID:     ownerID,
		Title:      "Zork I",
		Format:     "z5",
		SourceType: "ifdb",
		IFDBId:     "0dbnusxunq7fw5ro",
		Status:     "finished",
	})

	body := bytes.NewReader([]byte("{}"))
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runs/"+origRunID+"/restart", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST restart: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 201\nbody: %s", resp.StatusCode, b)
	}
	assertContentType(t, resp, "application/json")

	var runResp struct {
		ID         string `json:"id"`
		SourceType string `json:"sourceType"`
		IFDBId     string `json:"ifdbId"`
		Title      string `json:"title"`
		Format     string `json:"format"`
		Status     string `json:"status"`
		CreatedAt  string `json:"createdAt"`
		StartURL   string `json:"startURL"`
	}
	mustDecode(t, resp.Body, &runResp)

	// New run must have a different ID with r- prefix.
	if !strings.HasPrefix(runResp.ID, "r-") {
		t.Errorf("id = %q; want r- prefix", runResp.ID)
	}
	if runResp.ID == origRunID {
		t.Error("restarted run should have a new ID, not the original")
	}

	// Must copy story metadata from original.
	if runResp.SourceType != "ifdb" {
		t.Errorf("sourceType = %q; want ifdb", runResp.SourceType)
	}
	if runResp.IFDBId != "0dbnusxunq7fw5ro" {
		t.Errorf("ifdbId = %q; want 0dbnusxunq7fw5ro", runResp.IFDBId)
	}
	if runResp.Title != "Zork I" {
		t.Errorf("title = %q; want Zork I", runResp.Title)
	}
	if runResp.Format != "z5" {
		t.Errorf("format = %q; want z5", runResp.Format)
	}
	if runResp.Status != "pending" {
		t.Errorf("status = %q; want pending", runResp.Status)
	}
	if runResp.CreatedAt == "" {
		t.Error("createdAt is empty")
	}
	if !strings.Contains(runResp.StartURL, runResp.ID) {
		t.Errorf("startURL %q should contain new run ID %q", runResp.StartURL, runResp.ID)
	}

	// Verify new run was persisted to store with correct UserID.
	newRun, err := ms.GetRun(context.Background(), runResp.ID)
	if err != nil || newRun == nil {
		t.Fatalf("new run not found in store: %v", err)
	}
	if newRun.UserID != ownerID {
		t.Errorf("new run UserID = %q; want %q", newRun.UserID, ownerID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// POST /api/runs — UserID and unsupported_format (ARCHITECTURE.md §A.7.1)
// ─────────────────────────────────────────────────────────────────────────────

func TestCreateRunSetsUserIDFromAuth(t *testing.T) {
	// Local-mode auth always returns UID "local-dev"; verify it is persisted on the run.
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	// Local-mode auth.Verifier always identifies the caller as "local-dev".
	const wantUserID = "local-dev"

	body, _ := json.Marshal(map[string]interface{}{
		"sourceType":  "url",
		"artifactUrl": "https://example.com/story.z5",
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/runs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 201\nbody: %s", resp.StatusCode, b)
	}

	var runResp struct {
		ID string `json:"id"`
	}
	mustDecode(t, resp.Body, &runResp)

	// Verify UserID was persisted from the Firebase auth user (local-dev in test).
	run, err := ms.GetRun(context.Background(), runResp.ID)
	if err != nil || run == nil {
		t.Fatalf("GetRun(%q): %v", runResp.ID, err)
	}
	if run.UserID != wantUserID {
		t.Errorf("UserID = %q; want %q", run.UserID, wantUserID)
	}
}

func TestCreateRunUnsupportedFormat(t *testing.T) {
	// IFDB returns a game whose only download link uses an unrecognized format
	// (e.g. ALAN). The handler must respond 422 unsupported_format.
	mockIFDB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// "alan" is not in knownFormats → detectFormat returns "" → UnsupportedFormats populated.
		fmt.Fprint(w, `{
			"identification": {"format": "alan"},
			"bibliographic": {
				"title": "Alan Adventure",
				"author": "Tester",
				"firstpublished": "2000",
				"description": ""
			},
			"ifdb": {
				"tuid": "alangame1234567890",
				"downloads": {
					"links": [
						{
							"url": "https://example.com/game.alan",
							"format": "alan",
							"fileType": "alan",
							"size": 1000,
							"isGame": true
						}
					]
				}
			}
		}`)
	}))
	defer mockIFDB.Close()

	ms := newMockStore()
	ts := newTSWithIFDB(t, ms, newTestIFDBClient(mockIFDB.URL))
	defer ts.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"sourceType": "ifdb",
		"ifdbId":     "alangame1234567890",
	})
	resp, err := http.Post(ts.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/runs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 422\nbody: %s", resp.StatusCode, b)
	}

	var errBody struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, &errBody); err != nil {
		t.Fatalf("decode error body: %v\nbody: %s", err, data)
	}
	if errBody.Code != "unsupported_format" {
		t.Errorf("code = %q; want unsupported_format", errBody.Code)
	}
	if errBody.Error == "" {
		t.Error("error field is empty")
	}
}
