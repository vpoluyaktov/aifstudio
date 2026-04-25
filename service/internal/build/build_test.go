// Package build_test tests the build manager: single-active-build enforcement,
// directory layout creation, compiler invocation, and GCS upload on success/failure.
package build_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"storycloud/internal/auth"
	"storycloud/internal/build"
	"storycloud/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Minimal mock store for build tests
// ─────────────────────────────────────────────────────────────────────────────

type mockStore struct {
	mu       sync.Mutex
	builds   map[string]*store.Build
	projects map[string]*store.Project
}

func newMockStore() *mockStore {
	return &mockStore{
		builds:   make(map[string]*store.Build),
		projects: make(map[string]*store.Project),
	}
}

func (m *mockStore) CreateBuild(_ context.Context, b *store.Build) error {
	m.mu.Lock(); defer m.mu.Unlock(); m.builds[b.ID] = b; return nil
}
func (m *mockStore) GetBuild(_ context.Context, id string) (*store.Build, error) {
	m.mu.Lock(); defer m.mu.Unlock(); return m.builds[id], nil
}
func (m *mockStore) UpdateBuild(_ context.Context, b *store.Build) error {
	m.mu.Lock(); defer m.mu.Unlock(); m.builds[b.ID] = b; return nil
}
func (m *mockStore) ListBuildsByProject(_ context.Context, pid string, lim int) ([]*store.Build, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*store.Build
	for _, b := range m.builds {
		if b.ProjectID == pid {
			out = append(out, b)
			if len(out) >= lim {
				break
			}
		}
	}
	return out, nil
}
func (m *mockStore) DeleteBuildsForProject(_ context.Context, pid string) (int, error) {
	c := 0
	for id, b := range m.builds {
		if b.ProjectID == pid {
			delete(m.builds, id)
			c++
		}
	}
	return c, nil
}
func (m *mockStore) GetProject(_ context.Context, id string) (*store.Project, error) { return m.projects[id], nil }

// Unused interface methods — return zero values.
func (m *mockStore) CreateRun(_ context.Context, _ *store.Run) error                               { return nil }
func (m *mockStore) GetRun(_ context.Context, _ string) (*store.Run, error)                         { return nil, nil }
func (m *mockStore) UpdateRun(_ context.Context, _ *store.Run) error                               { return nil }
func (m *mockStore) DeleteRun(_ context.Context, _ string) error                                   { return nil }
func (m *mockStore) DeleteAbandonedPendingRuns(_ context.Context, _ time.Time) (int, error)        { return 0, nil }
func (m *mockStore) ListRunsByUser(_ context.Context, _ string, _ int) ([]*store.Run, error)       { return nil, nil }
func (m *mockStore) CreateProject(_ context.Context, _ *store.Project) error            { return nil }
func (m *mockStore) UpdateProjectSource(_ context.Context, _, _ string, _ time.Time) error      { return nil }
func (m *mockStore) UpdateProjectMeta(_ context.Context, _, _, _ string, _ time.Time) error    { return nil }
func (m *mockStore) UpdateProjectLatestBuild(_ context.Context, _, _ string) error             { return nil }
func (m *mockStore) ListProjectsByOwner(_ context.Context, _ string, _ int) ([]*store.Project, error) {
	return nil, nil
}
func (m *mockStore) GetCachedGame(_ context.Context, _ string) (*store.CachedGame, error) { return nil, nil }
func (m *mockStore) PutCachedGame(_ context.Context, _ *store.CachedGame) error           { return nil }
func (m *mockStore) ListFreshCachedGames(_ context.Context, _ time.Time) ([]*store.CachedGame, error) {
	return nil, nil
}
func (m *mockStore) UploadBlob(_ context.Context, _ string, _ string, _ io.Reader) error { return nil }
func (m *mockStore) DownloadBlob(_ context.Context, _ string, _ io.Writer) error         { return nil }
func (m *mockStore) SignedReadURL(_ context.Context, path string, _ time.Duration) (store.SignedURL, error) {
	return store.SignedURL{URL: "mock://" + path}, nil
}
func (m *mockStore) DeleteBlobPrefix(_ context.Context, _ string) (int, error) { return 0, nil }
func (m *mockStore) GetProjectSource(_ context.Context, _ string) (string, error)           { return "", nil }
func (m *mockStore) PutProjectSource(_ context.Context, _, _ string, _ time.Time) error    { return nil }
func (m *mockStore) DeleteProjectSource(_ context.Context, _ string) error                 { return nil }
func (m *mockStore) GetProjectSourceSize(_ context.Context, _ string) (int64, bool, error) { return 0, false, nil }
func (m *mockStore) UpdateProjectAI(_ context.Context, p *store.Project, _ *store.AITurn) (time.Time, error) {
	return p.UpdatedAt, nil
}
func (m *mockStore) SetProjectPublished(_ context.Context, _ string, _ bool, _ time.Time) error { return nil }
func (m *mockStore) ListPublishedProjects(_ context.Context, _ int) ([]*store.Project, error)   { return nil, nil }
func (m *mockStore) CreateAITurn(_ context.Context, _ *store.AITurn) error                      { return nil }
func (m *mockStore) ListAIConversation(_ context.Context, _ string, _ int) ([]*store.AITurn, error) {
	return nil, nil
}
func (m *mockStore) DeleteAIConversation(_ context.Context, _ string) (int, error)             { return 0, nil }
func (m *mockStore) GetAITurnAfterSource(_ context.Context, _, _ string) (string, bool, error) { return "", false, nil }
func (m *mockStore) ListRunsByProject(_ context.Context, _ string, _ int) ([]*store.Run, error) { return nil, nil }
func (m *mockStore) DeleteRunsForProject(_ context.Context, _ string) (int, error)              { return 0, nil }
func (m *mockStore) DeleteProject(_ context.Context, _ string) error                            { return nil }
func (m *mockStore) Close() error { return nil }

// Auth stubs — satisfy store.Store after auth methods were added to the interface.
func (m *mockStore) CreateUser(_ context.Context, _ *auth.User, _ string) error          { return nil }
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

func newTestManager(t *testing.T) *build.Manager {
	t.Helper()
	return build.NewManager(newMockStore(), 2*time.Minute)
}

// ─────────────────────────────────────────────────────────────────────────────
// Single-active-build enforcement — ARCHITECTURE.md §9.6
// ─────────────────────────────────────────────────────────────────────────────

func TestConcurrentBuildRejected409(t *testing.T) {
	if _, err := exec.LookPath("inform7"); err != nil {
		t.Skip("inform7 not found on PATH — skipping build integration test")
	}
	mgr := newTestManager(t)
	ctx := context.Background()

	projectID := "p-01HXZX5K3Q0RTB9M7YPZL"

	// First build for this project.
	b1 := &store.Build{
		ID:        "b-00000001000000000000001",
		ProjectID: projectID,
		OwnerUID:  "user-abc",
		Status:    build.StatusPending,
		CreatedAt: time.Now(),
	}
	if err := mgr.StartBuild(ctx, b1, "\"The Blue Door\" by Alex.\n\nThe Hallway is a room.\n"); err != nil {
		t.Fatalf("first StartBuild() error = %v", err)
	}

	// Second build for the same project — should conflict.
	b2 := &store.Build{
		ID:        "b-00000002000000000000002",
		ProjectID: projectID,
		OwnerUID:  "user-abc",
		Status:    build.StatusPending,
		CreatedAt: time.Now(),
	}
	err := mgr.StartBuild(ctx, b2, "updated source")
	if err == nil {
		t.Error("second StartBuild for same project should return conflict error (409)")
	}
	if !strings.Contains(err.Error(), "conflict") && !strings.Contains(err.Error(), "active") {
		t.Errorf("error should indicate conflict, got: %v", err)
	}
}

func TestBuildForDifferentProjectAllowed(t *testing.T) {
	if _, err := exec.LookPath("inform7"); err != nil {
		t.Skip("inform7 not found on PATH — skipping build integration test")
	}
	mgr := newTestManager(t)
	ctx := context.Background()

	// Build for project 1.
	b1 := &store.Build{
		ID:        "b-00000001000000000000001",
		ProjectID: "p-PROJECT1",
		OwnerUID:  "user-abc",
		Status:    build.StatusPending,
		CreatedAt: time.Now(),
	}
	if err := mgr.StartBuild(ctx, b1, "source 1"); err != nil {
		t.Fatalf("StartBuild project 1: %v", err)
	}

	// Build for a DIFFERENT project — should be allowed.
	b2 := &store.Build{
		ID:        "b-00000002000000000000002",
		ProjectID: "p-PROJECT2",
		OwnerUID:  "user-abc",
		Status:    build.StatusPending,
		CreatedAt: time.Now(),
	}
	if err := mgr.StartBuild(ctx, b2, "source 2"); err != nil {
		t.Errorf("StartBuild for different project should succeed, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Build directory layout — ARCHITECTURE.md §8.5 + §9.6
// Expected path: /tmp/build/<uuid>/Project.inform/Source/story.ni
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildDirectoryLayout(t *testing.T) {
	buildID := "b-LAYOUT00000000000000001"
	source := "\"The Blue Door\" by Alex.\n\nThe Hallway is a room.\n"

	projectRoot, err := build.ExportedCreateLayout(buildID, source)
	if err != nil {
		t.Fatalf("CreateLayout() error = %v", err)
	}
	defer build.ExportedCleanup(buildID)

	// Verify the Source/story.ni file exists.
	storyNi := filepath.Join(projectRoot, "Source", "story.ni")
	if _, err := os.Stat(storyNi); os.IsNotExist(err) {
		t.Errorf("story.ni not created at: %s", storyNi)
	}

	// Verify the file content.
	content, err := os.ReadFile(storyNi)
	if err != nil {
		t.Fatalf("ReadFile story.ni: %v", err)
	}
	if string(content) != source {
		t.Errorf("story.ni content = %q; want %q", content, source)
	}
}

func TestBuildDirectoryLayoutEmptySource(t *testing.T) {
	// ARCHITECTURE.md §9.6: empty source is valid — Inform 7 will emit a compile error.
	buildID := "b-EMPTY000000000000000001"
	projectRoot, err := build.ExportedCreateLayout(buildID, "")
	if err != nil {
		t.Fatalf("CreateLayout() with empty source error = %v", err)
	}
	defer build.ExportedCleanup(buildID)

	storyNi := filepath.Join(projectRoot, "Source", "story.ni")
	content, _ := os.ReadFile(storyNi)
	if string(content) != "" {
		t.Errorf("story.ni content = %q; want empty string", content)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Build status constants
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildStatusConstants(t *testing.T) {
	if build.StatusPending != "pending" {
		t.Errorf("StatusPending = %q; want pending", build.StatusPending)
	}
	if build.StatusRunning != "running" {
		t.Errorf("StatusRunning = %q; want running", build.StatusRunning)
	}
	if build.StatusSucceeded != "succeeded" {
		t.Errorf("StatusSucceeded = %q; want succeeded", build.StatusSucceeded)
	}
	if build.StatusFailed != "failed" {
		t.Errorf("StatusFailed = %q; want failed", build.StatusFailed)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ActiveBuildID
// ─────────────────────────────────────────────────────────────────────────────

func TestActiveBuildIDTracking(t *testing.T) {
	if _, err := exec.LookPath("inform7"); err != nil {
		t.Skip("inform7 not found on PATH — skipping build integration test")
	}
	mgr := newTestManager(t)
	ctx := context.Background()

	projectID := "p-TRACK0000000000000001"

	if id := mgr.ActiveBuildID(projectID); id != "" {
		t.Errorf("ActiveBuildID before any build = %q; want empty", id)
	}

	b := &store.Build{
		ID:        "b-TRACK0000000000000001",
		ProjectID: projectID,
		OwnerUID:  "user-1",
		Status:    build.StatusPending,
		CreatedAt: time.Now(),
	}
	if err := mgr.StartBuild(ctx, b, "source"); err != nil {
		t.Fatalf("StartBuild: %v", err)
	}

	if id := mgr.ActiveBuildID(projectID); id != b.ID {
		t.Errorf("ActiveBuildID = %q; want %q", id, b.ID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Skipped integration tests
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildSuccessUploadsArtifact(t *testing.T) {
	t.Skip("requires full build pipeline integration — implement when build.Manager is complete")
}

func TestBuildFailureDoesNotUploadArtifact(t *testing.T) {
	t.Skip("requires full build pipeline integration")
}

func TestBuildTimeoutKillsProcess(t *testing.T) {
	t.Skip("requires a long-running fake compiler binary")
}
