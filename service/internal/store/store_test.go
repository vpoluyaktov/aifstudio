package store_test

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

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
