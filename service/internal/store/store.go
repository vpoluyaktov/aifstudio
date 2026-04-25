// Package store defines the persistence interface used by all handlers.
// SQLiteStore (sqlite.go + local_blob.go) is the production implementation.
// The interface covers SQLite metadata, local filesystem blobs, and the auth
// surface (users, sessions) in a single surface to simplify dependency injection.
package store

import (
	"context"
	"io"
	"time"

	"storycloud/internal/auth"
)

// Source identifies where a story artifact comes from.
type Source struct {
	Type        string // "ifdb" | "url" | "build"
	IFDBId      string
	Format      string
	ArtifactURL string
	BuildID     string
}

// Run mirrors the runs SQLite table.
type Run struct {
	ID             string
	SourceType     string
	IFDBId         string
	Title          string // denormalised from IFDB at create time
	Format         string
	ArtifactURL    string
	BuildID        string
	UserID         string
	Status         string
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	LastActiveAt   *time.Time
	ExitCode       *int
	TranscriptPath string
	ErrorCode      string
	ErrorMessage   string

	// Session persistence fields
	Interpreter    string     // "dfrotz" | "glulxe" | "frob"; set once on first spawn
	StoryPath      string     // local storage path of cached story file
	SavePath       string     // local storage path of latest save; "" until first save
	TurnCount      int
	LastSaveAt     *time.Time
	ReconnectCount int

	// CandidateURLs is the ordered list of artifact download URLs to try.
	CandidateURLs []string

	// ProjectID is set for build-sourced runs; used for cascade cleanup.
	ProjectID string
}

// Project mirrors the projects SQLite table.
type Project struct {
	ID            string
	OwnerUID      string
	Name          string
	// Source is populated by GetProjectSource only; never serialized in API responses.
	Source      string `json:"-"`
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LatestBuildID string
	Published   bool
	PublishedAt *time.Time
}

// AITurn mirrors the ai_turns SQLite table.
type AITurn struct {
	ID               string
	ProjectID        string
	OwnerUID         string
	Kind             string // "generate" | "chat"
	UserMessage      string
	AssistantReply   string
	SourceBefore     string
	SourceAfter      string
	ModelRequestedAt time.Time
	ModelFinishedAt  time.Time
	PromptTokens     int
	CompletionTokens int
	Model            string
	Error            string
}

// Build mirrors the builds SQLite table.
type Build struct {
	ID             string
	ProjectID      string
	OwnerUID       string
	Status         string
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	ArtifactFormat string
	ArtifactPath   string
	LogPath        string
	ErrorMessage   string
}

// CachedGame mirrors the ifdb_cache SQLite table.
type CachedGame struct {
	TUID      string
	Payload   []byte // JSON-encoded normalized Game
	FetchedAt time.Time
	ExpiresAt time.Time
}

// SignedURL holds a time-limited URL. Retained for backward compatibility with
// handler code that has not yet been migrated to same-origin paths.
// Deprecated: new code should use same-origin /api/* routes to stream blobs.
type SignedURL struct {
	URL       string
	ExpiresAt time.Time
}

// Store is the full persistence surface used by handlers and SessionAuth.
// SQLiteStore (sqlite.go + local_blob.go) is the production implementation.
type Store interface {
	// --- Auth: users ---

	// CreateUser inserts a new user. The bcrypt hash is provided by the caller
	// (auth.SessionAuth). The store generates and sets u.UID if it is empty.
	// Returns auth.ErrEmailTaken when the email is already registered.
	CreateUser(ctx context.Context, u *auth.User, passwordHash string) error

	// GetUserByEmail returns the user and bcrypt password hash. Returns
	// (nil, "", nil) when the email is not found — never returns sql.ErrNoRows.
	GetUserByEmail(ctx context.Context, email string) (*auth.User, string, error)

	// GetUserByID returns the user (no password hash). Returns (nil, nil) when
	// not found.
	GetUserByID(ctx context.Context, uid string) (*auth.User, error)

	// --- Auth: sessions ---

	CreateSession(ctx context.Context, s *auth.Session) error

	// GetSession returns the session row or (nil, nil) when not found OR expired.
	GetSession(ctx context.Context, sessionID string) (*auth.Session, error)

	DeleteSession(ctx context.Context, sessionID string) error

	// DeleteExpiredSessions removes sessions with expires_at <= now.
	// Returns the count deleted. Called from a background goroutine periodically.
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error)

	// --- Runs ---

	CreateRun(ctx context.Context, r *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	UpdateRun(ctx context.Context, r *Run) error
	// DeleteRun removes the local storage prefix for the run then the SQLite row.
	DeleteRun(ctx context.Context, id string) error
	// DeleteAbandonedPendingRuns sweeps runs with status=="pending" AND createdAt < before.
	// Returns the number of rows deleted.
	DeleteAbandonedPendingRuns(ctx context.Context, before time.Time) (int, error)
	// ListRunsByUser returns runs owned by userID ordered by lastActiveAt DESC.
	// limit is clamped to [1, 50]. Returns empty slice (not error) on no matches.
	ListRunsByUser(ctx context.Context, userID string, limit int) ([]*Run, error)
	// ListRunsByProject returns runs with projectId == projectID. limit ∈ [1, 500].
	ListRunsByProject(ctx context.Context, projectID string, limit int) ([]*Run, error)
	// DeleteRunsForProject removes all run rows for a project. Returns count deleted.
	DeleteRunsForProject(ctx context.Context, projectID string) (int, error)

	// --- Projects ---

	CreateProject(ctx context.Context, p *Project) error
	GetProject(ctx context.Context, id string) (*Project, error)
	// UpdateProjectSource is a back-compat shim; prefer PutProjectSource.
	UpdateProjectSource(ctx context.Context, id, source string, updatedAt time.Time) error
	UpdateProjectMeta(ctx context.Context, id, name, description string, updatedAt time.Time) error
	UpdateProjectLatestBuild(ctx context.Context, id, buildID string) error
	ListProjectsByOwner(ctx context.Context, ownerUID string, limit int) ([]*Project, error)

	// GetProjectSource returns the Inform 7 source for a project from the
	// project_sources table. Returns "" (not an error) if absent.
	GetProjectSource(ctx context.Context, projectID string) (string, error)

	// PutProjectSource writes source to project_sources and updates
	// projects.updated_at atomically.
	PutProjectSource(ctx context.Context, projectID, source string, updatedAt time.Time) error

	// DeleteProjectSource removes the project_sources row. Idempotent.
	DeleteProjectSource(ctx context.Context, projectID string) error

	// GetProjectSourceSize returns the byte length of the stored source.
	// exists is false (and err is nil) when no source row exists.
	GetProjectSourceSize(ctx context.Context, projectID string) (size int64, exists bool, err error)

	// UpdateProjectAI writes updated project metadata and appends an ai_turns row
	// in a single transaction. Source is written to local storage by the caller
	// before this call. If turn is nil, only the project is updated.
	UpdateProjectAI(ctx context.Context, p *Project, turn *AITurn) (time.Time, error)

	// SetProjectPublished toggles the published flag and stamps publishedAt on false→true.
	SetProjectPublished(ctx context.Context, projectID string, published bool, now time.Time) error

	// ListPublishedProjects returns published projects ordered by publishedAt DESC.
	// limit ∈ [1, 50].
	ListPublishedProjects(ctx context.Context, limit int) ([]*Project, error)

	// DeleteProject removes the project row. All blobs and dependent rows must be
	// deleted by the caller first.
	DeleteProject(ctx context.Context, id string) error

	// --- AI conversation ---

	// CreateAITurn writes a turn row. Used to record failed turns.
	CreateAITurn(ctx context.Context, t *AITurn) error

	// ListAIConversation returns all turns for a project ordered chronologically.
	// limit ∈ [1, 200].
	ListAIConversation(ctx context.Context, projectID string, limit int) ([]*AITurn, error)

	// DeleteAIConversation removes every ai_turns row for a project.
	// Returns the count deleted.
	DeleteAIConversation(ctx context.Context, projectID string) (int, error)

	// GetAITurnAfterSource reads the after.i7 blob for a specific AI turn.
	// Returns (content, true, nil) on success; ("", false, nil) when absent.
	GetAITurnAfterSource(ctx context.Context, projectID, turnID string) (string, bool, error)

	// --- Builds ---

	CreateBuild(ctx context.Context, b *Build) error
	GetBuild(ctx context.Context, id string) (*Build, error)
	UpdateBuild(ctx context.Context, b *Build) error
	ListBuildsByProject(ctx context.Context, projectID string, limit int) ([]*Build, error)
	// DeleteBuildsForProject removes all build rows for a project. Returns count deleted.
	DeleteBuildsForProject(ctx context.Context, projectID string) (int, error)

	// --- IFDB cache ---

	// GetCachedGame returns nil, nil if absent or expired.
	GetCachedGame(ctx context.Context, tuid string) (*CachedGame, error)
	PutCachedGame(ctx context.Context, g *CachedGame) error
	// ListFreshCachedGames returns entries with ExpiresAt > now for warm-up.
	ListFreshCachedGames(ctx context.Context, now time.Time) ([]*CachedGame, error)

	// --- Blob storage (local filesystem under STORAGE_PATH) ---

	UploadBlob(ctx context.Context, path, contentType string, r io.Reader) error
	DownloadBlob(ctx context.Context, path string, w io.Writer) error

	// SignedReadURL is a deprecated no-op retained for handler backward
	// compatibility. Returns an empty SignedURL.
	// Deprecated: use same-origin /api/* routes to stream blobs.
	SignedReadURL(ctx context.Context, path string, ttl time.Duration) (SignedURL, error)

	// DeleteBlobPrefix removes every file under the given path prefix.
	// Returns the count deleted.
	DeleteBlobPrefix(ctx context.Context, prefix string) (int, error)

	// --- Lifecycle ---

	Close() error
}
