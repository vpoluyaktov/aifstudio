// Package store defines the persistence interface used by all handlers.
// The Store interface covers Firestore metadata operations and GCS blob
// operations in a single surface to simplify dependency injection.
package store

import (
	"context"
	"io"
	"time"
)

// Source identifies where a story artifact comes from.
type Source struct {
	Type        string // "ifdb" | "url" | "build"
	IFDBId      string
	Format      string
	ArtifactURL string
	BuildID     string
}

// Run mirrors the Firestore runs/{id} document (§4.2, as amended by §A.7.1).
type Run struct {
	ID             string
	SourceType     string
	IFDBId         string
	Title          string // denormalised from IFDB at create time
	Format         string
	ArtifactURL    string
	BuildID        string
	UserID         string // sc_user cookie value at create time
	Status         string
	CreatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
	LastActiveAt   *time.Time
	ExitCode       *int
	TranscriptPath string
	ErrorCode      string
	ErrorMessage   string

	// Session persistence fields (§A.7.1)
	Interpreter    string     // "dfrotz" or "glulxe"; set once on first spawn
	StoryPath      string     // GCS path of cached story file
	SavePath       string     // GCS path of latest save; "" until first save
	TurnCount      int        // user inputs accepted since run started
	LastSaveAt     *time.Time // last successful save upload
	ReconnectCount int        // incremented on every WS resume

	// CandidateURLs is the ordered list of artifact download URLs to try.
	// Index 0 mirrors ArtifactURL (the preferred link chosen at create time).
	// Only populated for sourceType=="ifdb". Absent on runs created before this
	// field was added; resolve() falls back to []string{ArtifactURL} in that case.
	CandidateURLs []string

	// ProjectID is set for build-sourced runs created from a StoryCloud project
	// (e.g. via handleCommunityPlay). Used for cascade cleanup on project delete.
	// Older runs created before this field was added will have an empty string.
	ProjectID string
}

// Project mirrors the Firestore projects/{id} document (§4.1, as amended by ARCHITECTURE_AI_CREATE.md).
type Project struct {
	ID            string
	OwnerUID      string
	Name          string
	// Source is a temporary migration helper only. Source text has moved to GCS at
	// projects/{ID}/source.i7. This field is populated by projectFromDoc for the
	// lazy Firestore→GCS migration path in GetProjectSource; it is never written
	// to Firestore by PutProjectSource and never serialised in API responses.
	Source      string `json:"-"`
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	LatestBuildID string
	Published   bool
	PublishedAt *time.Time
}

// AITurn mirrors a Firestore projects/{projectId}/ai_turns/{turnId} document (§4.2).
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

// Build mirrors the Firestore builds/{id} document (§4.4).
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

// CachedGame mirrors the Firestore ifdb_cache/{tuid} document (§4.5).
type CachedGame struct {
	TUID      string
	Payload   []byte // JSON-encoded normalized Game
	FetchedAt time.Time
	ExpiresAt time.Time
}

// SignedURL holds a time-limited GCS URL.
type SignedURL struct {
	URL       string
	ExpiresAt time.Time
}

// Store is the full persistence surface used by handlers (§5.1).
// FirestoreStore (firestore.go + gcs.go) is the production implementation.
type Store interface {
	// --- Runs ---
	CreateRun(ctx context.Context, r *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	UpdateRun(ctx context.Context, r *Run) error
	// DeleteRun removes the GCS session prefix (sessions/<id>/) then the Firestore doc.
	// Intended for single-run deletion; the Firestore doc is only removed when GCS
	// cleanup succeeds, allowing safe retries. For project cascade use
	// DeleteRunsForProject instead.
	DeleteRun(ctx context.Context, id string) error
	// DeleteAbandonedPendingRuns sweeps runs with status=="pending" AND createdAt < before.
	// Uses BulkWriter. Returns the number of docs deleted.
	DeleteAbandonedPendingRuns(ctx context.Context, before time.Time) (int, error)
	// ListRunsByUser returns runs owned by userID ordered by lastActiveAt DESC.
	// limit is clamped to [1, 50]. Returns empty slice (not error) on no matches.
	ListRunsByUser(ctx context.Context, userID string, limit int) ([]*Run, error)
	// ListRunsByProject returns runs with projectId == projectID. Used for cascade
	// cleanup on project delete. limit is clamped to [1, 500].
	ListRunsByProject(ctx context.Context, projectID string, limit int) ([]*Run, error)

	// --- Projects ---
	CreateProject(ctx context.Context, p *Project) error
	GetProject(ctx context.Context, id string) (*Project, error)
	// Deprecated: use PutProjectSource instead. Retained as a back-compat shim.
	UpdateProjectSource(ctx context.Context, id, source string, updatedAt time.Time) error
	// UpdateProjectMeta updates the name and description of a project.
	UpdateProjectMeta(ctx context.Context, id, name, description string, updatedAt time.Time) error
	UpdateProjectLatestBuild(ctx context.Context, id, buildID string) error
	ListProjectsByOwner(ctx context.Context, ownerUID string, limit int) ([]*Project, error)

	// GetProjectSource returns the current Inform 7 source for a project from GCS.
	// Key is "projects/{projectID}/source.i7".
	//
	// Lazy migration (§4.4 of ARCHITECTURE_AI_CREATE.md): if the GCS object is absent
	// but the Firestore projects/{projectID} doc carries a legacy "source" field, this
	// method uploads that field's value to GCS, clears the field on the doc, and returns
	// the string. Returns "" (not an error) for projects with no source in either store.
	GetProjectSource(ctx context.Context, projectID string) (string, error)

	// PutProjectSource writes source to GCS at projects/{projectID}/source.i7 and
	// updates Firestore projects/{projectID}.updatedAt, also issuing firestore.Delete
	// on the legacy "source" field so the migration converges even on writes that
	// bypassed GetProjectSource. Overwrites the GCS object. Returns on upload failure
	// without touching Firestore — callers retry on transient errors.
	PutProjectSource(ctx context.Context, projectID, source string, updatedAt time.Time) error

	// DeleteProjectSource removes the GCS object for a project. Idempotent:
	// deleting a missing object is not an error.
	DeleteProjectSource(ctx context.Context, projectID string) error

	// GetProjectSourceSize returns the byte size of the project's GCS source object.
	// exists is false (and err is nil) when the object does not exist.
	GetProjectSourceSize(ctx context.Context, projectID string) (size int64, exists bool, err error)

	// SignedProjectSourceURL returns a V4 signed read URL for the project's source
	// GCS object (projects/{projectID}/source.i7) valid for ttl.
	// Returns empty SignedURL (URL == "") if the object does not exist.
	SignedProjectSourceURL(ctx context.Context, projectID string, ttl time.Duration) (SignedURL, error)

	// UpdateProjectAI writes the updated project metadata (description, updatedAt)
	// AND appends an ai_turns/{turnId} entry in a single Firestore BulkWriter flush.
	// The source itself is written to GCS by the caller via PutProjectSource BEFORE
	// this call — UpdateProjectAI does NOT touch source bytes, only Firestore metadata.
	// If turn is nil, only the project doc is updated.
	// Returns the server-side UpdatedAt (= the updatedAt passed on p).
	UpdateProjectAI(ctx context.Context, p *Project, turn *AITurn) (time.Time, error)

	// SetProjectPublished toggles the published flag and stamps publishedAt when
	// transitioning false→true. Leaves publishedAt unchanged on true→false so the
	// catalog can re-surface a republished project by its most recent publish.
	SetProjectPublished(ctx context.Context, projectID string, published bool, now time.Time) error

	// ListPublishedProjects returns the most recently published projects across
	// all owners, newest first. limit is clamped to [1, 50]. Returns empty slice
	// (not error) on no matches. Requires the projects_published composite index.
	ListPublishedProjects(ctx context.Context, limit int) ([]*Project, error)

	// --- AI conversation (collection: projects/{projectId}/ai_turns) ---

	// CreateAITurn writes a turn document. Used to record a failed turn.
	CreateAITurn(ctx context.Context, t *AITurn) error

	// ListAIConversation returns all turns for a project ordered chronologically
	// (oldest first). limit is clamped to [1, 200].
	ListAIConversation(ctx context.Context, projectID string, limit int) ([]*AITurn, error)

	// DeleteAIConversation removes every ai_turns document for a project.
	// Uses BulkWriter. Returns the count deleted.
	DeleteAIConversation(ctx context.Context, projectID string) (int, error)

	// --- Builds ---
	CreateBuild(ctx context.Context, b *Build) error
	GetBuild(ctx context.Context, id string) (*Build, error)
	UpdateBuild(ctx context.Context, b *Build) error
	ListBuildsByProject(ctx context.Context, projectID string, limit int) ([]*Build, error)
	// DeleteBuildsForProject removes all builds for a project. Uses BulkWriter.
	DeleteBuildsForProject(ctx context.Context, projectID string) (int, error)
	// DeleteRunsForProject removes all run Firestore documents for a project.
	// Uses BulkWriter. GCS blobs (sessions/, transcripts/) must be deleted by
	// the caller before this call. Returns the count deleted.
	DeleteRunsForProject(ctx context.Context, projectID string) (int, error)
	// DeleteProject removes the Firestore project document. All GCS blobs and
	// subcollections (builds, runs, ai_turns) must be deleted by the caller first.
	DeleteProject(ctx context.Context, id string) error

	// --- IFDB cache ---
	// GetCachedGame returns nil,nil if absent or expired.
	GetCachedGame(ctx context.Context, tuid string) (*CachedGame, error)
	PutCachedGame(ctx context.Context, g *CachedGame) error
	// ListFreshCachedGames returns entries with ExpiresAt > now for warm-up.
	ListFreshCachedGames(ctx context.Context, now time.Time) ([]*CachedGame, error)

	// GetAITurnAfterSource reads the after.i7 GCS blob for a specific AI turn.
	// Returns (content, true, nil) on success.
	// Returns ("", false, nil) when the blob does not exist.
	// Returns ("", false, err) on other errors.
	GetAITurnAfterSource(ctx context.Context, projectID, turnID string) (string, bool, error)

	// --- Blob storage (GCS) ---
	UploadBlob(ctx context.Context, path, contentType string, r io.Reader) error
	DownloadBlob(ctx context.Context, path string, w io.Writer) error
	SignedReadURL(ctx context.Context, path string, ttl time.Duration) (SignedURL, error)
	// DeleteBlobPrefix removes every GCS object under the given prefix.
	// Concurrency is capped at 10 in-flight deletes. Returns the count deleted.
	DeleteBlobPrefix(ctx context.Context, prefix string) (int, error)

	// --- Lifecycle ---
	Close() error
}
