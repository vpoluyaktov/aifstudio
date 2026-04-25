package build

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"aifstudio/internal/store"
)

// logTail returns up to maxBytes of the trailing portion of s, for
// diagnostic logging. Truncates on a UTF-8 rune boundary and prefixes
// an ellipsis marker when the original was longer.
func logTail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	// Advance forward to a valid rune boundary so we never emit a partial
	// UTF-8 sequence at the start of the tail.
	for start < len(s) && (s[start]&0xC0) == 0x80 {
		start++
	}
	return "…[truncated]" + s[start:]
}

// Status constants for builds.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Manager serialises builds per project, enforcing the one-active-build-per-project rule.
type Manager struct {
	mu           sync.Mutex
	activeBuilds map[string]string // projectID → buildID

	st           store.Store
	buildTimeout time.Duration
}

// NewManager creates a new build Manager.
func NewManager(st store.Store, buildTimeout time.Duration) *Manager {
	return &Manager{
		activeBuilds: make(map[string]string),
		st:           st,
		buildTimeout: buildTimeout,
	}
}

// StartBuild enqueues a build for project. Returns 409 conflict error if another
// build is already pending/running for this project.
func (m *Manager) StartBuild(ctx context.Context, b *store.Build, source string) error {
	m.mu.Lock()
	if existingID, active := m.activeBuilds[b.ProjectID]; active {
		m.mu.Unlock()
		return fmt.Errorf("conflict:409:build %s already active for project %s", existingID, b.ProjectID)
	}
	m.activeBuilds[b.ProjectID] = b.ID
	m.mu.Unlock()

	if err := m.st.CreateBuild(ctx, b); err != nil {
		m.clearActive(b.ProjectID)
		return fmt.Errorf("create build: %w", err)
	}

	// Copy b before handing to goroutine so the caller cannot observe
	// mutations made by runBuild (e.g. Status, StartedAt, FinishedAt).
	buildCopy := *b
	go m.runBuild(&buildCopy, source)
	return nil
}

// ActiveBuildID returns the active build ID for a project, or "" if none.
func (m *Manager) ActiveBuildID(projectID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeBuilds[projectID]
}

func (m *Manager) clearActive(projectID string) {
	m.mu.Lock()
	delete(m.activeBuilds, projectID)
	m.mu.Unlock()
}

func (m *Manager) runBuild(b *store.Build, source string) {
	defer m.clearActive(b.ProjectID)

	ctx := context.Background()
	now := time.Now()
	b.Status = StatusRunning
	b.StartedAt = &now
	if err := m.st.UpdateBuild(ctx, b); err != nil {
		slog.Error("failed to update build to running", "buildId", b.ID, "err", err)
	}

	// Create project layout.
	projectRoot, err := createProjectLayout(b.ID, source)
	if err != nil {
		m.failBuild(ctx, b, fmt.Sprintf("failed to create project layout: %v", err), "")
		return
	}
	defer cleanupBuildDir(b.ID)

	// Run compiler.
	result := runCompiler(ctx, projectRoot, m.buildTimeout)
	buildLog := result.Log

	finishedAt := time.Now()
	b.FinishedAt = &finishedAt

	if result.Err != nil {
		// Compiler failed — upload log only.
		logPath, uploadErr := uploadLogOnly(ctx, m.st, b.ID, buildLog)
		if uploadErr != nil {
			slog.Error("failed to upload build log", "buildId", b.ID, "err", uploadErr)
		}
		b.LogPath = logPath
		b.Status = StatusFailed
		b.ErrorMessage = result.Err.Error()
		// Surface the compiler failure in Cloud Logging so ops can triage
		// without downloading the full log from GCS.
		slog.Warn("build failed",
			"build_id", b.ID,
			"project_id", b.ProjectID,
			"duration_ms", result.Duration.Milliseconds(),
			"err", result.Err.Error(),
			"log_tail", logTail(buildLog, 500),
		)
		m.st.UpdateBuild(ctx, b) //nolint:errcheck
		return
	}

	// Check artifact was produced.
	if _, err := os.Stat(artifactPath(projectRoot)); os.IsNotExist(err) {
		m.failBuild(ctx, b, "compiler reported success but no artifact produced", buildLog)
		return
	}

	// Upload artifact + log.
	artifactGCSPath, logGCSPath, uploadErr := uploadArtifacts(ctx, m.st, b.ID, projectRoot, buildLog)
	if uploadErr != nil {
		m.failBuild(ctx, b, fmt.Sprintf("upload failed: %v", uploadErr), buildLog)
		return
	}

	b.Status = StatusSucceeded
	b.ArtifactFormat = "ulx"
	b.ArtifactPath = artifactGCSPath
	b.LogPath = logGCSPath
	m.st.UpdateBuild(ctx, b) //nolint:errcheck

	// Update project's latestBuildId.
	if err := m.st.UpdateProjectLatestBuild(ctx, b.ProjectID, b.ID); err != nil {
		slog.Warn("failed to update project latestBuildId", "projectId", b.ProjectID, "err", err)
	}
}

func (m *Manager) failBuild(ctx context.Context, b *store.Build, errMsg, buildLog string) {
	now := time.Now()
	if b.FinishedAt == nil {
		b.FinishedAt = &now
	}
	b.Status = StatusFailed
	b.ErrorMessage = errMsg
	if buildLog != "" {
		logPath, err := uploadLogOnly(ctx, m.st, b.ID, buildLog)
		if err == nil {
			b.LogPath = logPath
		}
	}
	m.st.UpdateBuild(ctx, b) //nolint:errcheck
}
