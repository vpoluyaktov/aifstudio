package store_test

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"storycloud/internal/auth"
	"storycloud/internal/store"
)

// MockStore is an in-memory test double for store.Store.
// Signed URL stubs return the literal path prefixed with "mock://".
// sources mirrors the GCS source bucket (projects/{id}/source.i7) in memory.
// aiTurnBlobs mirrors the GCS ai-turns/{projectId}/{turnId}/{before,after}.i7
// objects; SourceBefore/SourceAfter are stored here and populated on read,
// exactly as the production FirestoreStore does via writeAITurnBlobs /
// readAITurnBlobs.
type MockStore struct {
	runs        map[string]*store.Run
	projects    map[string]*store.Project
	builds      map[string]*store.Build
	cache       map[string]*store.CachedGame
	sources     map[string]string          // projectID → source text (mirrors GCS source bucket)
	aiTurns     map[string][]*store.AITurn // projectID → ordered turns (without source fields)
	aiTurnBlobs map[string]struct {        // turnID → GCS source snapshots
		before, after string
	}
}

func NewMockStore() *MockStore {
	return &MockStore{
		runs:        make(map[string]*store.Run),
		projects:    make(map[string]*store.Project),
		builds:      make(map[string]*store.Build),
		cache:       make(map[string]*store.CachedGame),
		sources:     make(map[string]string),
		aiTurns:     make(map[string][]*store.AITurn),
		aiTurnBlobs: make(map[string]struct{ before, after string }),
	}
}

func (m *MockStore) CreateRun(_ context.Context, r *store.Run) error {
	m.runs[r.ID] = r
	return nil
}

func (m *MockStore) GetRun(_ context.Context, id string) (*store.Run, error) {
	r := m.runs[id]
	return r, nil
}

func (m *MockStore) UpdateRun(_ context.Context, r *store.Run) error {
	m.runs[r.ID] = r
	return nil
}

func (m *MockStore) DeleteRun(_ context.Context, id string) error {
	delete(m.runs, id)
	return nil
}

func (m *MockStore) DeleteAbandonedPendingRuns(_ context.Context, before time.Time) (int, error) {
	count := 0
	for id, r := range m.runs {
		if r.Status == "pending" && r.CreatedAt.Before(before) {
			delete(m.runs, id)
			count++
		}
	}
	return count, nil
}

func (m *MockStore) CreateProject(_ context.Context, p *store.Project) error {
	m.projects[p.ID] = p
	return nil
}

func (m *MockStore) GetProject(_ context.Context, id string) (*store.Project, error) {
	return m.projects[id], nil
}

// Deprecated: use PutProjectSource instead. Retained for backward compatibility.
func (m *MockStore) UpdateProjectSource(_ context.Context, id, source string, updatedAt time.Time) error {
	if p := m.projects[id]; p != nil {
		p.Source = source
		p.UpdatedAt = updatedAt
	}
	m.sources[id] = source
	return nil
}

func (m *MockStore) UpdateProjectMeta(_ context.Context, id, name, description string, updatedAt time.Time) error {
	if p := m.projects[id]; p != nil {
		p.Name = name
		p.Description = description
		p.UpdatedAt = updatedAt
	}
	return nil
}

func (m *MockStore) GetProjectSource(_ context.Context, projectID string) (string, error) {
	if src, ok := m.sources[projectID]; ok {
		return src, nil
	}
	// Fall back to project Source field for projects created before this change.
	if p, ok := m.projects[projectID]; ok {
		return p.Source, nil
	}
	return "", nil
}

func (m *MockStore) PutProjectSource(_ context.Context, projectID, source string, updatedAt time.Time) error {
	m.sources[projectID] = source
	if p := m.projects[projectID]; p != nil {
		p.UpdatedAt = updatedAt
	}
	return nil
}

func (m *MockStore) DeleteProjectSource(_ context.Context, projectID string) error {
	delete(m.sources, projectID)
	return nil
}

func (m *MockStore) GetProjectSourceSize(_ context.Context, projectID string) (int64, bool, error) {
	src, ok := m.sources[projectID]
	if !ok {
		// Fall back to legacy Source field.
		if p, pok := m.projects[projectID]; pok && p.Source != "" {
			return int64(len(p.Source)), true, nil
		}
		return 0, false, nil
	}
	return int64(len(src)), true, nil
}

func (m *MockStore) SignedProjectSourceURL(_ context.Context, projectID string, ttl time.Duration) (store.SignedURL, error) {
	_, exists, _ := m.GetProjectSourceSize(context.Background(), projectID)
	if !exists {
		return store.SignedURL{}, nil
	}
	return store.SignedURL{
		URL:       "mock://source/" + projectID,
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}

func (m *MockStore) UpdateProjectLatestBuild(_ context.Context, id, buildID string) error {
	if p := m.projects[id]; p != nil {
		p.LatestBuildID = buildID
	}
	return nil
}

func (m *MockStore) ListProjectsByOwner(_ context.Context, ownerUID string, limit int) ([]*store.Project, error) {
	var out []*store.Project
	for _, p := range m.projects {
		if p.OwnerUID == ownerUID {
			out = append(out, p)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *MockStore) CreateBuild(_ context.Context, b *store.Build) error {
	m.builds[b.ID] = b
	return nil
}

func (m *MockStore) GetBuild(_ context.Context, id string) (*store.Build, error) {
	return m.builds[id], nil
}

func (m *MockStore) UpdateBuild(_ context.Context, b *store.Build) error {
	m.builds[b.ID] = b
	return nil
}

func (m *MockStore) ListBuildsByProject(_ context.Context, projectID string, limit int) ([]*store.Build, error) {
	var out []*store.Build
	for _, b := range m.builds {
		if b.ProjectID == projectID {
			out = append(out, b)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *MockStore) DeleteBuildsForProject(_ context.Context, projectID string) (int, error) {
	count := 0
	for id, b := range m.builds {
		if b.ProjectID == projectID {
			delete(m.builds, id)
			count++
		}
	}
	return count, nil
}

func (m *MockStore) DeleteRunsForProject(_ context.Context, projectID string) (int, error) {
	count := 0
	for id, r := range m.runs {
		if r.ProjectID == projectID {
			delete(m.runs, id)
			count++
		}
	}
	return count, nil
}

func (m *MockStore) DeleteProject(_ context.Context, id string) error {
	delete(m.projects, id)
	return nil
}

func (m *MockStore) GetCachedGame(_ context.Context, tuid string) (*store.CachedGame, error) {
	g := m.cache[tuid]
	if g == nil {
		return nil, nil
	}
	if !g.ExpiresAt.After(time.Now()) {
		return nil, nil
	}
	return g, nil
}

func (m *MockStore) PutCachedGame(_ context.Context, g *store.CachedGame) error {
	m.cache[g.TUID] = g
	return nil
}

func (m *MockStore) ListFreshCachedGames(_ context.Context, now time.Time) ([]*store.CachedGame, error) {
	var out []*store.CachedGame
	for _, g := range m.cache {
		if g.ExpiresAt.After(now) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (m *MockStore) UploadBlob(_ context.Context, _ string, _ string, _ io.Reader) error {
	return nil
}

func (m *MockStore) DownloadBlob(_ context.Context, _ string, _ io.Writer) error {
	return nil
}

func (m *MockStore) SignedReadURL(_ context.Context, path string, _ time.Duration) (store.SignedURL, error) {
	return store.SignedURL{URL: "mock://" + path, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (m *MockStore) DeleteBlobPrefix(_ context.Context, _ string) (int, error) {
	return 0, nil
}

func (m *MockStore) ListRunsByUser(_ context.Context, userID string, limit int) ([]*store.Run, error) {
	out := make([]*store.Run, 0)
	for _, r := range m.runs {
		if r.UserID == userID {
			out = append(out, r)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *MockStore) ListRunsByProject(_ context.Context, projectID string, limit int) ([]*store.Run, error) {
	out := make([]*store.Run, 0)
	for _, r := range m.runs {
		if r.ProjectID == projectID {
			out = append(out, r)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *MockStore) UpdateProjectAI(_ context.Context, p *store.Project, turn *store.AITurn) (time.Time, error) {
	if proj := m.projects[p.ID]; proj != nil {
		proj.Description = p.Description
		proj.UpdatedAt = p.UpdatedAt
	}
	if turn != nil {
		// Mirror production: store source snapshots in the blob layer (GCS simulation).
		m.aiTurnBlobs[turn.ID] = struct{ before, after string }{turn.SourceBefore, turn.SourceAfter}
		clone := *turn
		clone.SourceBefore = ""
		clone.SourceAfter = ""
		m.aiTurns[p.ID] = append(m.aiTurns[p.ID], &clone)
	}
	return p.UpdatedAt, nil
}

func (m *MockStore) SetProjectPublished(_ context.Context, projectID string, published bool, now time.Time) error {
	if p := m.projects[projectID]; p != nil {
		p.Published = published
		if published {
			p.PublishedAt = &now
		}
	}
	return nil
}

func (m *MockStore) ListPublishedProjects(_ context.Context, limit int) ([]*store.Project, error) {
	var out []*store.Project
	for _, p := range m.projects {
		if p.Published {
			out = append(out, p)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *MockStore) CreateAITurn(_ context.Context, t *store.AITurn) error {
	// Mirror production: store SourceBefore/SourceAfter in the blob layer (GCS
	// simulation); they are NOT stored in the turn doc, matching aiTurnToDoc.
	m.aiTurnBlobs[t.ID] = struct{ before, after string }{t.SourceBefore, t.SourceAfter}
	clone := *t
	clone.SourceBefore = ""
	clone.SourceAfter = ""
	m.aiTurns[t.ProjectID] = append(m.aiTurns[t.ProjectID], &clone)
	return nil
}

func (m *MockStore) ListAIConversation(_ context.Context, projectID string, limit int) ([]*store.AITurn, error) {
	turns := m.aiTurns[projectID]
	if len(turns) > limit {
		turns = turns[:limit]
	}
	// Populate SourceBefore/SourceAfter from blob layer (mirrors production
	// readAITurnBlobs). Missing blobs leave the fields empty, matching GCS
	// behaviour for blobs that were never written.
	result := make([]*store.AITurn, len(turns))
	for i, t := range turns {
		clone := *t
		if blobs, ok := m.aiTurnBlobs[t.ID]; ok {
			clone.SourceBefore = blobs.before
			clone.SourceAfter = blobs.after
		}
		result[i] = &clone
	}
	return result, nil
}

func (m *MockStore) GetAITurnAfterSource(_ context.Context, _ string, turnID string) (string, bool, error) {
	blobs, ok := m.aiTurnBlobs[turnID]
	if !ok || blobs.after == "" {
		return "", false, nil
	}
	return blobs.after, true, nil
}

func (m *MockStore) DeleteAIConversation(_ context.Context, projectID string) (int, error) {
	count := len(m.aiTurns[projectID])
	delete(m.aiTurns, projectID)
	return count, nil
}

func (m *MockStore) Close() error { return nil }

// Auth stubs — satisfy store.Store after auth methods were added to the interface.

func (m *MockStore) CreateUser(_ context.Context, u *auth.User, _ string) error {
	if u.UID == "" {
		u.UID = "u-mock"
	}
	return nil
}

func (m *MockStore) GetUserByEmail(_ context.Context, _ string) (*auth.User, string, error) {
	return nil, "", nil
}

func (m *MockStore) GetUserByID(_ context.Context, _ string) (*auth.User, error) {
	return nil, nil
}

func (m *MockStore) CreateSession(_ context.Context, _ *auth.Session) error { return nil }

func (m *MockStore) GetSession(_ context.Context, _ string) (*auth.Session, error) {
	return nil, nil
}

func (m *MockStore) DeleteSession(_ context.Context, _ string) error { return nil }

func (m *MockStore) DeleteExpiredSessions(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

// compile-time assertion
var _ store.Store = (*MockStore)(nil)

// ---- Tests ----

func TestMockStoreRuns(t *testing.T) {
	ms := NewMockStore()
	ctx := context.Background()

	r := &store.Run{
		ID:         "r-01TESTRUN0000000000001",
		SourceType: "ifdb",
		IFDBId:     "abc123",
		Status:     "pending",
		CreatedAt:  time.Now(),
	}

	if err := ms.CreateRun(ctx, r); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := ms.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got == nil {
		t.Fatal("GetRun: expected non-nil")
	}
	if got.ID != r.ID {
		t.Errorf("ID: want %s, got %s", r.ID, got.ID)
	}

	got.Status = "running"
	if err := ms.UpdateRun(ctx, got); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got2, _ := ms.GetRun(ctx, r.ID)
	if got2.Status != "running" {
		t.Errorf("Status after update: want running, got %s", got2.Status)
	}
}

func TestMockStoreDeleteAbandonedPendingRuns(t *testing.T) {
	ms := NewMockStore()
	ctx := context.Background()

	past := time.Now().Add(-2 * time.Hour)
	future := time.Now().Add(time.Hour)

	// Old pending run — should be swept
	old := &store.Run{ID: "r-OLDPENDING000000000001", Status: "pending", CreatedAt: past}
	// Recent pending run — should NOT be swept
	recent := &store.Run{ID: "r-NEWPENDING000000000001", Status: "pending", CreatedAt: future}
	// Old but running run — should NOT be swept (wrong status)
	running := &store.Run{ID: "r-RUNNING000000000000001", Status: "running", CreatedAt: past}

	ms.CreateRun(ctx, old)     //nolint:errcheck
	ms.CreateRun(ctx, recent)  //nolint:errcheck
	ms.CreateRun(ctx, running) //nolint:errcheck

	count, err := ms.DeleteAbandonedPendingRuns(ctx, time.Now())
	if err != nil {
		t.Fatalf("DeleteAbandonedPendingRuns: %v", err)
	}
	if count != 1 {
		t.Errorf("deleted count: want 1, got %d", count)
	}

	if r, _ := ms.GetRun(ctx, old.ID); r != nil {
		t.Error("old pending run should be deleted")
	}
	if r, _ := ms.GetRun(ctx, recent.ID); r == nil {
		t.Error("recent pending run should not be deleted")
	}
	if r, _ := ms.GetRun(ctx, running.ID); r == nil {
		t.Error("running run should not be deleted")
	}
}

func TestMockStoreDeleteRun(t *testing.T) {
	ms := NewMockStore()
	ctx := context.Background()

	r := &store.Run{ID: "r-DELETETEST000000000001", Status: "suspended"}
	ms.CreateRun(ctx, r) //nolint:errcheck

	if err := ms.DeleteRun(ctx, r.ID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if got, _ := ms.GetRun(ctx, r.ID); got != nil {
		t.Error("run should be deleted")
	}
}

func TestMockStoreProjects(t *testing.T) {
	ms := NewMockStore()
	ctx := context.Background()
	now := time.Now()

	p := &store.Project{
		ID:        "p-01TESTPROJ0000000001",
		OwnerUID:  "user-1",
		Name:      "Test Project",
		Source:    "initial",
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := ms.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	got, _ := ms.GetProject(ctx, p.ID)
	if got == nil || got.Name != "Test Project" {
		t.Fatalf("GetProject: got %v", got)
	}

	updatedAt := now.Add(time.Minute)
	ms.UpdateProjectSource(ctx, p.ID, "new source", updatedAt) //nolint:errcheck

	got2, _ := ms.GetProject(ctx, p.ID)
	if got2.Source != "new source" {
		t.Errorf("Source after update: want 'new source', got %s", got2.Source)
	}
}

func TestMockStoreCachedGame(t *testing.T) {
	ms := NewMockStore()
	ctx := context.Background()

	// fresh entry
	g := &store.CachedGame{
		TUID:      "abc123",
		Payload:   []byte(`{"id":"abc123"}`),
		FetchedAt: time.Now(),
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	ms.PutCachedGame(ctx, g) //nolint:errcheck

	got, _ := ms.GetCachedGame(ctx, "abc123")
	if got == nil {
		t.Fatal("GetCachedGame: expected non-nil for fresh entry")
	}

	// expired entry
	expired := &store.CachedGame{
		TUID:      "expired",
		Payload:   []byte(`{}`),
		FetchedAt: time.Now().Add(-20 * time.Minute),
		ExpiresAt: time.Now().Add(-10 * time.Minute),
	}
	ms.PutCachedGame(ctx, expired) //nolint:errcheck

	got2, _ := ms.GetCachedGame(ctx, "expired")
	if got2 != nil {
		t.Error("GetCachedGame: expected nil for expired entry")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ListRunsByUser — filters by UserID and respects limit (§A.7.1)
// ─────────────────────────────────────────────────────────────────────────────

func TestListRunsByUserFiltersCorrectly(t *testing.T) {
	ms := NewMockStore()
	ctx := context.Background()
	now := time.Now()

	const userA = "u-USERA0000000000000000001"
	const userB = "u-USERB0000000000000000001"

	runs := []*store.Run{
		{ID: "r-A1TESTRUN00000000000001", UserID: userA, Status: "suspended", CreatedAt: now},
		{ID: "r-A2TESTRUN00000000000001", UserID: userA, Status: "finished", CreatedAt: now},
		{ID: "r-B1TESTRUN00000000000001", UserID: userB, Status: "suspended", CreatedAt: now},
	}
	for _, r := range runs {
		if err := ms.CreateRun(ctx, r); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}

	results, err := ms.ListRunsByUser(ctx, userA, 50)
	if err != nil {
		t.Fatalf("ListRunsByUser: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("len(results) = %d; want 2 (only userA runs)", len(results))
	}
	for _, r := range results {
		if r.UserID != userA {
			t.Errorf("got run with UserID = %q; want %q", r.UserID, userA)
		}
	}
}

func TestListRunsByUserReturnsEmptySlice(t *testing.T) {
	ms := NewMockStore()
	ctx := context.Background()

	// Store has a run for a different user; querying for a third user returns empty.
	_ = ms.CreateRun(ctx, &store.Run{
		ID:     "r-SOMEONEELSERUN000000001",
		UserID: "u-SOMEONE0000000000000001",
		Status: "suspended",
	})

	results, err := ms.ListRunsByUser(ctx, "u-UNKNOWN0000000000000001", 50)
	if err != nil {
		t.Fatalf("ListRunsByUser: %v", err)
	}
	if results == nil {
		t.Error("ListRunsByUser: returned nil; want empty slice")
	}
	if len(results) != 0 {
		t.Errorf("len(results) = %d; want 0", len(results))
	}
}

func TestListRunsByUserLimit(t *testing.T) {
	ms := NewMockStore()
	ctx := context.Background()
	now := time.Now()

	const testUser = "u-LIMITUSER000000000000001"

	// Insert 5 runs for the user.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("r-LIMITRUN%017d", i)
		if err := ms.CreateRun(ctx, &store.Run{
			ID:        id,
			UserID:    testUser,
			Status:    "suspended",
			CreatedAt: now,
		}); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}

	// Limit to 3 → at most 3 results.
	results, err := ms.ListRunsByUser(ctx, testUser, 3)
	if err != nil {
		t.Fatalf("ListRunsByUser with limit=3: %v", err)
	}
	if len(results) > 3 {
		t.Errorf("len(results) = %d; want ≤ 3 (limit applied)", len(results))
	}

	// Limit to 10 → all 5 results (limit is a cap, not a requirement).
	all, err := ms.ListRunsByUser(ctx, testUser, 10)
	if err != nil {
		t.Fatalf("ListRunsByUser with limit=10: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("len(all) = %d; want 5 when limit > actual count", len(all))
	}
}

func TestListRunsByUserSingleRun(t *testing.T) {
	// Edge case: single run for user → returns exactly one item.
	ms := NewMockStore()
	ctx := context.Background()

	const user = "u-SINGLEUSER00000000000001"
	runID := "r-SINGLERUN000000000000001"

	_ = ms.CreateRun(ctx, &store.Run{
		ID:     runID,
		UserID: user,
		Status: "pending",
	})

	results, err := ms.ListRunsByUser(ctx, user, 50)
	if err != nil {
		t.Fatalf("ListRunsByUser: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d; want 1", len(results))
	}
	if results[0].ID != runID {
		t.Errorf("ID = %q; want %q", results[0].ID, runID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// SQLiteStore integration tests
// ─────────────────────────────────────────────────────────────────────────────

// newTestSQLiteStore opens a temporary SQLite database backed by a temp
// storage directory. The test is registered for cleanup automatically.
func newTestSQLiteStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	dir := t.TempDir()
	blob := store.NewLocalBlobStore(filepath.Join(dir, "storage"))
	dbPath := filepath.Join(dir, "test.db")
	s, err := store.NewSQLiteStore(context.Background(), dbPath, blob)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// createTestUser inserts a minimal user row and returns it.
func createTestUser(t *testing.T, s *store.SQLiteStore, id, email string) *auth.User {
	t.Helper()
	ctx := context.Background()
	u := &auth.User{UID: id, Email: email, Name: "Test User"}
	if err := s.CreateUser(ctx, u, "$2a$12$dummyhashfortest000000000000000000000000000000000000000000"); err != nil {
		t.Fatalf("CreateUser(%s): %v", id, err)
	}
	return u
}

// ── Users ────────────────────────────────────────────────────────────────────

func TestSQLiteCreateUser(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := &auth.User{Email: "alice@example.com", Name: "Alice"}
	err := s.CreateUser(ctx, u, "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.UID == "" {
		t.Error("UID should be set after CreateUser")
	}
}

func TestSQLiteGetUserByEmail(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	_ = createTestUser(t, s, "u-001", "bob@example.com")

	got, hash, err := s.GetUserByEmail(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil user")
	}
	if got.Email != "bob@example.com" {
		t.Errorf("Email = %q; want bob@example.com", got.Email)
	}
	if hash == "" {
		t.Error("hash should not be empty")
	}
}

func TestSQLiteGetUserByEmail_NotFound(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	got, hash, err := s.GetUserByEmail(ctx, "nobody@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing email, got %+v", got)
	}
	if hash != "" {
		t.Errorf("expected empty hash, got %q", hash)
	}
}

func TestSQLiteGetUserByID(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-getbyid01", "getbyid@example.com")

	got, err := s.GetUserByID(ctx, u.UID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil user")
	}
	if got.UID != u.UID {
		t.Errorf("UID = %q; want %q", got.UID, u.UID)
	}
}

func TestSQLiteGetUserByID_NotFound(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	got, err := s.GetUserByID(ctx, "u-doesnotexist")
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing UID, got %+v", got)
	}
}

func TestSQLiteCreateUser_DuplicateEmail(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	_ = createTestUser(t, s, "u-dup01", "dup@example.com")

	u2 := &auth.User{Email: "dup@example.com", Name: "Dup"}
	err := s.CreateUser(ctx, u2, "hash2")
	if err == nil {
		t.Fatal("expected error for duplicate email, got nil")
	}
	if !strings.Contains(err.Error(), "email") && err != auth.ErrEmailTaken {
		// Either ErrEmailTaken or an error mentioning email is acceptable.
		t.Errorf("error = %v; want ErrEmailTaken", err)
	}
}

// ── Sessions ─────────────────────────────────────────────────────────────────

func TestSQLiteCreateAndGetSession(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-sess01", "sess@example.com")
	now := time.Now().UTC().Truncate(time.Second)

	sess := &auth.Session{
		ID:        "test-session-token-43chars-xxxxxxxxxxx",
		UserID:    u.UID,
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil session")
	}
	if got.UserID != u.UID {
		t.Errorf("UserID = %q; want %q", got.UserID, u.UID)
	}
}

func TestSQLiteGetSession_NotFound(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	got, err := s.GetSession(ctx, "nonexistent-token")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing session, got %+v", got)
	}
}

func TestSQLiteGetSession_Expired(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-expsess", "expsess@example.com")
	past := time.Now().Add(-2 * time.Hour).UTC()

	sess := &auth.Session{
		ID:        "expired-session-token-43chars-xxxxxxxxxx",
		UserID:    u.UID,
		CreatedAt: past,
		ExpiresAt: past.Add(time.Hour), // already expired
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// GetSession must return nil for expired sessions.
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got != nil {
		t.Error("expected nil for expired session, got non-nil")
	}
}

func TestSQLiteDeleteSession(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-delsess", "delsess@example.com")
	now := time.Now().UTC()
	sess := &auth.Session{
		ID:        "deleteme-session-token-43chars-xxxxxxxx",
		UserID:    u.UID,
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
	}
	_ = s.CreateSession(ctx, sess)

	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	got, _ := s.GetSession(ctx, sess.ID)
	if got != nil {
		t.Error("session should be deleted")
	}
}

func TestSQLiteDeleteExpiredSessions(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-expclean", "expclean@example.com")
	now := time.Now().UTC()

	// Insert one expired session and one valid session.
	expired := &auth.Session{
		ID:        "expired-session-43chars-yyyyyyyyyyyyyyy",
		UserID:    u.UID,
		CreatedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	}
	valid := &auth.Session{
		ID:        "valid-session-43chars-zzzzzzzzzzzzzzzzz",
		UserID:    u.UID,
		CreatedAt: now,
		ExpiresAt: now.Add(24 * time.Hour),
	}
	_ = s.CreateSession(ctx, expired)
	_ = s.CreateSession(ctx, valid)

	n, err := s.DeleteExpiredSessions(ctx, now)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted count = %d; want 1", n)
	}
}

// ── Runs ─────────────────────────────────────────────────────────────────────

func TestSQLiteCreateGetUpdateDeleteRun(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-runtest", "runtest@example.com")
	now := time.Now().UTC().Truncate(time.Second)

	r := &store.Run{
		ID:         "r-01TESTRUNSQLITEXX0000001",
		SourceType: "ifdb",
		IFDBId:     "abc123def456",
		Title:      "Zork I",
		Format:     "z5",
		UserID:     u.UID,
		Status:     "pending",
		CreatedAt:  now,
	}

	// Create
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Get
	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil run")
	}
	if got.Title != "Zork I" {
		t.Errorf("Title = %q; want Zork I", got.Title)
	}
	if got.Status != "pending" {
		t.Errorf("Status = %q; want pending", got.Status)
	}

	// Update
	got.Status = "running"
	if err := s.UpdateRun(ctx, got); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	got2, _ := s.GetRun(ctx, r.ID)
	if got2.Status != "running" {
		t.Errorf("Status after update = %q; want running", got2.Status)
	}

	// Delete
	if err := s.DeleteRun(ctx, r.ID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	got3, _ := s.GetRun(ctx, r.ID)
	if got3 != nil {
		t.Error("run should be nil after delete")
	}
}

func TestSQLiteGetRun_NotFound(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	got, err := s.GetRun(ctx, "r-DOESNOTEXISTXXXXXXXX001")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got != nil {
		t.Error("expected nil for missing run")
	}
}

func TestSQLiteListRunsByUser_OrderedByLastActive(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-listruns", "listruns@example.com")
	now := time.Now().UTC().Truncate(time.Second)

	older := now.Add(-time.Hour)
	newer := now

	// Insert in reverse order (older first, newer second) so the ORDER BY is tested.
	r1 := &store.Run{
		ID:           "r-01LISTRUNS0OLDER000001",
		SourceType:   "ifdb",
		UserID:       u.UID,
		Status:       "suspended",
		CreatedAt:    older,
		LastActiveAt: &older,
	}
	r2 := &store.Run{
		ID:           "r-01LISTRUNS0NEWER000001",
		SourceType:   "ifdb",
		UserID:       u.UID,
		Status:       "suspended",
		CreatedAt:    newer,
		LastActiveAt: &newer,
	}
	_ = s.CreateRun(ctx, r1)
	_ = s.CreateRun(ctx, r2)

	results, err := s.ListRunsByUser(ctx, u.UID, 50)
	if err != nil {
		t.Fatalf("ListRunsByUser: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len = %d; want 2", len(results))
	}
	// Newer run should come first (ORDER BY last_active_at DESC).
	if results[0].ID != r2.ID {
		t.Errorf("first result = %q; want %q (newest first)", results[0].ID, r2.ID)
	}
}

func TestSQLiteListRunsByUser_LimitClamp(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-limitclamp", "limitclamp@example.com")
	now := time.Now().UTC()

	// Insert 5 runs.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("r-LIMITCLAMPSQLITE%08d", i)
		_ = s.CreateRun(ctx, &store.Run{
			ID:         id,
			SourceType: "ifdb",
			UserID:     u.UID,
			Status:     "suspended",
			CreatedAt:  now.Add(time.Duration(i) * time.Second),
		})
	}

	// limit=3 should return at most 3.
	results, err := s.ListRunsByUser(ctx, u.UID, 3)
	if err != nil {
		t.Fatalf("ListRunsByUser limit=3: %v", err)
	}
	if len(results) > 3 {
		t.Errorf("limit=3: got %d results; want ≤3", len(results))
	}

	// limit=200 should be clamped to 50.
	all, err := s.ListRunsByUser(ctx, u.UID, 200)
	if err != nil {
		t.Fatalf("ListRunsByUser limit=200: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("limit=200 (clamped to 50): got %d; want 5", len(all))
	}

	// limit=0 should be normalized to 1 (not panic or return all).
	one, err := s.ListRunsByUser(ctx, u.UID, 0)
	if err != nil {
		t.Fatalf("ListRunsByUser limit=0: %v", err)
	}
	if len(one) > 1 {
		t.Errorf("limit=0 (normalized to 1): got %d; want ≤1", len(one))
	}
}

func TestSQLiteListRunsByUser_EmptyResult(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	results, err := s.ListRunsByUser(ctx, "u-nobody", 50)
	if err != nil {
		t.Fatalf("ListRunsByUser: %v", err)
	}
	if results == nil {
		t.Error("expected empty slice, got nil")
	}
	if len(results) != 0 {
		t.Errorf("len = %d; want 0", len(results))
	}
}

func TestSQLiteDeleteAbandonedPendingRuns(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-abandoned", "abandoned@example.com")
	now := time.Now().UTC()

	old := &store.Run{
		ID:         "r-OLDPENDINGSQLITE000001",
		SourceType: "ifdb",
		UserID:     u.UID,
		Status:     "pending",
		CreatedAt:  now.Add(-2 * time.Hour),
	}
	recent := &store.Run{
		ID:         "r-NEWPENDINGSQLITE000001",
		SourceType: "ifdb",
		UserID:     u.UID,
		Status:     "pending",
		CreatedAt:  now.Add(time.Hour),
	}
	running := &store.Run{
		ID:         "r-RUNNINGSQLITE00000001",
		SourceType: "ifdb",
		UserID:     u.UID,
		Status:     "running",
		CreatedAt:  now.Add(-2 * time.Hour),
	}
	_ = s.CreateRun(ctx, old)
	_ = s.CreateRun(ctx, recent)
	_ = s.CreateRun(ctx, running)

	n, err := s.DeleteAbandonedPendingRuns(ctx, now)
	if err != nil {
		t.Fatalf("DeleteAbandonedPendingRuns: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d; want 1", n)
	}
	if r, _ := s.GetRun(ctx, old.ID); r != nil {
		t.Error("old pending run should be deleted")
	}
	if r, _ := s.GetRun(ctx, recent.ID); r == nil {
		t.Error("recent pending run should NOT be deleted")
	}
	if r, _ := s.GetRun(ctx, running.ID); r == nil {
		t.Error("running run should NOT be deleted")
	}
}

// ── Projects ─────────────────────────────────────────────────────────────────

func TestSQLiteCreateGetProject(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-projtest", "projtest@example.com")
	now := time.Now().UTC().Truncate(time.Second)

	p := &store.Project{
		ID:          "p-01SQLITETESTPROJECT001",
		OwnerUID:    u.UID,
		Name:        "My Project",
		Description: "A great project",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	got, err := s.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil project")
	}
	if got.Name != "My Project" {
		t.Errorf("Name = %q; want My Project", got.Name)
	}
	if got.Description != "A great project" {
		t.Errorf("Description = %q; want A great project", got.Description)
	}
}

func TestSQLiteGetProject_NotFound(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	got, err := s.GetProject(ctx, "p-DOESNOTEXISTXXXXXXX001")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got != nil {
		t.Error("expected nil for missing project")
	}
}

func TestSQLiteUpdateProjectMeta(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-projmeta", "projmeta@example.com")
	now := time.Now().UTC().Truncate(time.Second)

	p := &store.Project{
		ID:        "p-01UPDATEMETA000000001",
		OwnerUID:  u.UID,
		Name:      "Old Name",
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.CreateProject(ctx, p)

	if err := s.UpdateProjectMeta(ctx, p.ID, "New Name", "new desc", now.Add(time.Minute)); err != nil {
		t.Fatalf("UpdateProjectMeta: %v", err)
	}

	got, _ := s.GetProject(ctx, p.ID)
	if got.Name != "New Name" {
		t.Errorf("Name = %q; want New Name", got.Name)
	}
	if got.Description != "new desc" {
		t.Errorf("Description = %q; want new desc", got.Description)
	}
}

func TestSQLiteListProjectsByOwner(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u1 := createTestUser(t, s, "u-owner01", "owner01@example.com")
	u2 := createTestUser(t, s, "u-owner02", "owner02@example.com")
	now := time.Now().UTC()

	for i := 0; i < 3; i++ {
		_ = s.CreateProject(ctx, &store.Project{
			ID:        fmt.Sprintf("p-OWNER01PROJ%013d", i),
			OwnerUID:  u1.UID,
			Name:      fmt.Sprintf("Proj %d", i),
			CreatedAt: now,
			UpdatedAt: now.Add(time.Duration(i) * time.Second),
		})
	}
	_ = s.CreateProject(ctx, &store.Project{
		ID:        "p-OWNER02PROJ000000001",
		OwnerUID:  u2.UID,
		Name:      "Other project",
		CreatedAt: now,
		UpdatedAt: now,
	})

	results, err := s.ListProjectsByOwner(ctx, u1.UID, 50)
	if err != nil {
		t.Fatalf("ListProjectsByOwner: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("len = %d; want 3", len(results))
	}
	for _, p := range results {
		if p.OwnerUID != u1.UID {
			t.Errorf("unexpected owner %q", p.OwnerUID)
		}
	}
}

// ── Project sources ───────────────────────────────────────────────────────────

func TestSQLitePutAndGetProjectSource(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-srcstest", "srcstest@example.com")
	now := time.Now().UTC()

	p := &store.Project{
		ID:        "p-01SOURCETESTXXX00001",
		OwnerUID:  u.UID,
		Name:      "Source Test",
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.CreateProject(ctx, p)

	const src = `"The Blue Door" by Alice Author.\n\nThe Hallway is a room.\n`
	if err := s.PutProjectSource(ctx, p.ID, src, now.Add(time.Minute)); err != nil {
		t.Fatalf("PutProjectSource: %v", err)
	}

	got, err := s.GetProjectSource(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProjectSource: %v", err)
	}
	if got != src {
		t.Errorf("source mismatch: got %q; want %q", got, src)
	}

	// project.updated_at should be bumped.
	pUpd, _ := s.GetProject(ctx, p.ID)
	if !pUpd.UpdatedAt.After(now) {
		t.Error("project.updated_at should be bumped by PutProjectSource")
	}
}

func TestSQLiteGetProjectSource_Absent(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-nosrc", "nosrc@example.com")
	now := time.Now().UTC()
	_ = s.CreateProject(ctx, &store.Project{
		ID:        "p-01NOSOURCE0000000001",
		OwnerUID:  u.UID,
		Name:      "No Source",
		CreatedAt: now,
		UpdatedAt: now,
	})

	got, err := s.GetProjectSource(ctx, "p-01NOSOURCE0000000001")
	if err != nil {
		t.Fatalf("GetProjectSource: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestSQLiteDeleteProjectSource(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-delsrc", "delsrc@example.com")
	now := time.Now().UTC()
	_ = s.CreateProject(ctx, &store.Project{
		ID:        "p-01DELSOURCE000000001",
		OwnerUID:  u.UID,
		Name:      "Del Source",
		CreatedAt: now,
		UpdatedAt: now,
	})
	_ = s.PutProjectSource(ctx, "p-01DELSOURCE000000001", "hello", now)

	if err := s.DeleteProjectSource(ctx, "p-01DELSOURCE000000001"); err != nil {
		t.Fatalf("DeleteProjectSource: %v", err)
	}

	got, _ := s.GetProjectSource(ctx, "p-01DELSOURCE000000001")
	if got != "" {
		t.Errorf("source should be empty after delete, got %q", got)
	}

	// Idempotent — second delete should not error.
	if err := s.DeleteProjectSource(ctx, "p-01DELSOURCE000000001"); err != nil {
		t.Errorf("second DeleteProjectSource: %v", err)
	}
}

// ── AI conversation ───────────────────────────────────────────────────────────

func TestSQLiteAIConversation(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-aitest", "aitest@example.com")
	now := time.Now().UTC().Truncate(time.Second)

	p := &store.Project{
		ID:        "p-01AICONVTEST000000001",
		OwnerUID:  u.UID,
		Name:      "AI Test Project",
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.CreateProject(ctx, p)

	// Insert 3 turns.
	for i := 0; i < 3; i++ {
		turn := &store.AITurn{
			ID:               fmt.Sprintf("t-AICONVTEST%013d", i),
			ProjectID:        p.ID,
			OwnerUID:         u.UID,
			Kind:             "chat",
			UserMessage:      fmt.Sprintf("message %d", i),
			AssistantReply:   fmt.Sprintf("reply %d", i),
			ModelRequestedAt: now.Add(time.Duration(i) * time.Minute),
			ModelFinishedAt:  now.Add(time.Duration(i)*time.Minute + 30*time.Second),
		}
		if err := s.CreateAITurn(ctx, turn); err != nil {
			t.Fatalf("CreateAITurn %d: %v", i, err)
		}
	}

	turns, err := s.ListAIConversation(ctx, p.ID, 200)
	if err != nil {
		t.Fatalf("ListAIConversation: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("len = %d; want 3", len(turns))
	}
	// Should be chronological (ASC by model_requested_at).
	if turns[0].UserMessage != "message 0" {
		t.Errorf("turns[0].UserMessage = %q; want message 0", turns[0].UserMessage)
	}

	// DeleteAIConversation.
	n, err := s.DeleteAIConversation(ctx, p.ID)
	if err != nil {
		t.Fatalf("DeleteAIConversation: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted = %d; want 3", n)
	}

	// After delete, list returns empty.
	after, _ := s.ListAIConversation(ctx, p.ID, 200)
	if len(after) != 0 {
		t.Errorf("after delete: len = %d; want 0", len(after))
	}
}

func TestSQLiteListAIConversation_Empty(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	turns, err := s.ListAIConversation(ctx, "p-NOPROJECTXXXXXXXXXXX", 200)
	if err != nil {
		t.Fatalf("ListAIConversation: %v", err)
	}
	if turns == nil {
		t.Error("expected empty slice, got nil")
	}
	if len(turns) != 0 {
		t.Errorf("len = %d; want 0", len(turns))
	}
}

// ── Builds ────────────────────────────────────────────────────────────────────

func TestSQLiteCreateGetUpdateBuild(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-buildtest", "buildtest@example.com")
	now := time.Now().UTC().Truncate(time.Second)

	p := &store.Project{
		ID:        "p-01BUILDTESTPROJECT001",
		OwnerUID:  u.UID,
		Name:      "Build Test",
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.CreateProject(ctx, p)

	b := &store.Build{
		ID:        "b-01SQLITEBUILDTEST0001",
		ProjectID: p.ID,
		OwnerUID:  u.UID,
		Status:    "pending",
		CreatedAt: now,
	}
	if err := s.CreateBuild(ctx, b); err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}

	got, err := s.GetBuild(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil build")
	}
	if got.Status != "pending" {
		t.Errorf("Status = %q; want pending", got.Status)
	}

	// Update to succeeded.
	got.Status = "succeeded"
	got.ArtifactFormat = "ulx"
	if err := s.UpdateBuild(ctx, got); err != nil {
		t.Fatalf("UpdateBuild: %v", err)
	}

	got2, _ := s.GetBuild(ctx, b.ID)
	if got2.Status != "succeeded" {
		t.Errorf("Status after update = %q; want succeeded", got2.Status)
	}
	if got2.ArtifactFormat != "ulx" {
		t.Errorf("ArtifactFormat = %q; want ulx", got2.ArtifactFormat)
	}
}

func TestSQLiteListBuildsByProject(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-listbuilds", "listbuilds@example.com")
	now := time.Now().UTC()

	p := &store.Project{
		ID:        "p-01LISTBUILDSPROJECT01",
		OwnerUID:  u.UID,
		Name:      "List Builds",
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.CreateProject(ctx, p)

	for i := 0; i < 3; i++ {
		_ = s.CreateBuild(ctx, &store.Build{
			ID:        fmt.Sprintf("b-LISTBUILDSSQLITE%08d", i),
			ProjectID: p.ID,
			OwnerUID:  u.UID,
			Status:    "pending",
			CreatedAt: now.Add(time.Duration(i) * time.Second),
		})
	}

	results, err := s.ListBuildsByProject(ctx, p.ID, 50)
	if err != nil {
		t.Fatalf("ListBuildsByProject: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("len = %d; want 3", len(results))
	}
}

func TestSQLiteDeleteBuildsForProject(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	u := createTestUser(t, s, "u-delbuilds", "delbuilds@example.com")
	now := time.Now().UTC()
	p := &store.Project{
		ID:        "p-01DELETEBUILDSTEST001",
		OwnerUID:  u.UID,
		Name:      "Del Builds",
		CreatedAt: now,
		UpdatedAt: now,
	}
	_ = s.CreateProject(ctx, p)

	for i := 0; i < 2; i++ {
		_ = s.CreateBuild(ctx, &store.Build{
			ID:        fmt.Sprintf("b-DELETEBUILDSTEST%08d", i),
			ProjectID: p.ID,
			OwnerUID:  u.UID,
			Status:    "succeeded",
			CreatedAt: now,
		})
	}

	n, err := s.DeleteBuildsForProject(ctx, p.ID)
	if err != nil {
		t.Fatalf("DeleteBuildsForProject: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d; want 2", n)
	}
}

// ── IFDB cache ────────────────────────────────────────────────────────────────

func TestSQLiteGetAndPutCachedGame(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Put a fresh entry.
	g := &store.CachedGame{
		TUID:      "0dbnusxunq7fw5ro",
		Payload:   []byte(`{"id":"0dbnusxunq7fw5ro","title":"Zork I"}`),
		FetchedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := s.PutCachedGame(ctx, g); err != nil {
		t.Fatalf("PutCachedGame: %v", err)
	}

	got, err := s.GetCachedGame(ctx, "0dbnusxunq7fw5ro")
	if err != nil {
		t.Fatalf("GetCachedGame: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil CachedGame")
	}
	if string(got.Payload) != string(g.Payload) {
		t.Errorf("Payload mismatch")
	}
}

func TestSQLiteGetCachedGame_Expired(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Put an already-expired entry.
	g := &store.CachedGame{
		TUID:      "expiredgame0000001",
		Payload:   []byte(`{"id":"expired"}`),
		FetchedAt: now.Add(-20 * time.Minute),
		ExpiresAt: now.Add(-10 * time.Minute), // already expired
	}
	_ = s.PutCachedGame(ctx, g)

	// GetCachedGame must return nil for expired entries.
	got, err := s.GetCachedGame(ctx, "expiredgame0000001")
	if err != nil {
		t.Fatalf("GetCachedGame expired: %v", err)
	}
	if got != nil {
		t.Error("expected nil for expired CachedGame, got non-nil")
	}
}

func TestSQLiteGetCachedGame_NotFound(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()

	got, err := s.GetCachedGame(ctx, "nonexistentgame000001")
	if err != nil {
		t.Fatalf("GetCachedGame not found: %v", err)
	}
	if got != nil {
		t.Error("expected nil for missing cache entry")
	}
}

func TestSQLitePutCachedGame_Overwrite(t *testing.T) {
	s := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	g1 := &store.CachedGame{
		TUID:      "overwrite0000001",
		Payload:   []byte(`{"v":1}`),
		FetchedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
	}
	_ = s.PutCachedGame(ctx, g1)

	g2 := &store.CachedGame{
		TUID:      "overwrite0000001",
		Payload:   []byte(`{"v":2}`),
		FetchedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := s.PutCachedGame(ctx, g2); err != nil {
		t.Fatalf("PutCachedGame overwrite: %v", err)
	}

	got, _ := s.GetCachedGame(ctx, "overwrite0000001")
	if string(got.Payload) != `{"v":2}` {
		t.Errorf("payload after overwrite = %q; want {\"v\":2}", got.Payload)
	}
}
