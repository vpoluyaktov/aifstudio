// Package store — SQLite metadata store.
// SQLiteStore implements the full Store interface using database/sql backed by
// modernc.org/sqlite (pure Go, no CGO). Blob operations are delegated to the
// embedded LocalBlobStore.
package store

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	_ "modernc.org/sqlite" // registers "sqlite" driver

	"aifstudio/internal/auth"
)

// schema is the canonical DDL applied idempotently on every open.
const schema = `
CREATE TABLE IF NOT EXISTS users (
  id            TEXT PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  display_name  TEXT NOT NULL,
  created_at    TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email ON users(email);

CREATE TABLE IF NOT EXISTS sessions (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  TEXT NOT NULL,
  expires_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user_id    ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS runs (
  id               TEXT PRIMARY KEY,
  source_type      TEXT NOT NULL,
  ifdb_id          TEXT NOT NULL DEFAULT '',
  title            TEXT NOT NULL DEFAULT '',
  format           TEXT NOT NULL DEFAULT '',
  artifact_url     TEXT NOT NULL DEFAULT '',
  build_id         TEXT NOT NULL DEFAULT '',
  user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  status           TEXT NOT NULL,
  created_at       TEXT NOT NULL,
  started_at       TEXT,
  finished_at      TEXT,
  last_active_at   TEXT,
  exit_code        INTEGER,
  transcript_path  TEXT NOT NULL DEFAULT '',
  error_code       TEXT NOT NULL DEFAULT '',
  error_message    TEXT NOT NULL DEFAULT '',
  interpreter      TEXT NOT NULL DEFAULT '',
  story_path       TEXT NOT NULL DEFAULT '',
  save_path        TEXT NOT NULL DEFAULT '',
  turn_count       INTEGER NOT NULL DEFAULT 0,
  last_save_at     TEXT,
  reconnect_count  INTEGER NOT NULL DEFAULT 0,
  candidate_urls   TEXT NOT NULL DEFAULT '[]',
  project_id       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_runs_user_active    ON runs(user_id, last_active_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_status_created ON runs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_project_id     ON runs(project_id);

CREATE TABLE IF NOT EXISTS projects (
  id              TEXT PRIMARY KEY,
  owner_uid       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name            TEXT NOT NULL,
  description     TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  latest_build_id TEXT NOT NULL DEFAULT '',
  published       INTEGER NOT NULL DEFAULT 0,
  published_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_projects_owner_updated ON projects(owner_uid, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_projects_published_at  ON projects(published, published_at DESC);

CREATE TABLE IF NOT EXISTS project_sources (
  project_id  TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  source      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ai_turns (
  id                   TEXT PRIMARY KEY,
  project_id           TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  owner_uid            TEXT NOT NULL,
  kind                 TEXT NOT NULL,
  user_message         TEXT NOT NULL DEFAULT '',
  assistant_reply      TEXT NOT NULL DEFAULT '',
  source_before        TEXT NOT NULL DEFAULT '',
  source_after         TEXT NOT NULL DEFAULT '',
  model_requested_at   TEXT NOT NULL,
  model_finished_at    TEXT NOT NULL,
  prompt_tokens        INTEGER NOT NULL DEFAULT 0,
  completion_tokens    INTEGER NOT NULL DEFAULT 0,
  model                TEXT NOT NULL DEFAULT '',
  error                TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_ai_turns_project_time ON ai_turns(project_id, model_requested_at);

CREATE TABLE IF NOT EXISTS builds (
  id              TEXT PRIMARY KEY,
  project_id      TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  owner_uid       TEXT NOT NULL,
  status          TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  started_at      TEXT,
  finished_at     TEXT,
  artifact_format TEXT NOT NULL DEFAULT '',
  artifact_path   TEXT NOT NULL DEFAULT '',
  log_path        TEXT NOT NULL DEFAULT '',
  error_message   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_builds_project_created ON builds(project_id, created_at DESC);

CREATE TABLE IF NOT EXISTS ifdb_cache (
  tuid       TEXT PRIMARY KEY,
  payload    BLOB NOT NULL,
  fetched_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ifdb_cache_expires_at ON ifdb_cache(expires_at);
`

// SQLiteStore implements the full Store interface: metadata in SQLite and blob
// operations delegated to LocalBlobStore.
type SQLiteStore struct {
	db   *sql.DB
	blob *LocalBlobStore
}

// Compile-time assertion.
var _ Store = (*SQLiteStore)(nil)

// NewSQLiteStore opens (or creates) the SQLite database at dbPath, applies WAL
// mode and schema, and returns a ready-to-use SQLiteStore.
func NewSQLiteStore(ctx context.Context, dbPath string, blob *LocalBlobStore) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	// Limit to one writer connection to avoid WAL contention; reads are fast.
	db.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}

	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return &SQLiteStore{db: db, blob: blob}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Time helpers
// ─────────────────────────────────────────────────────────────────────────────

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

func parseNullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullTimeVal(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func nullIntVal(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

// ─────────────────────────────────────────────────────────────────────────────
// ID generation
// ─────────────────────────────────────────────────────────────────────────────

func newULIDStr() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), crand.Reader).String()
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth: users
// ─────────────────────────────────────────────────────────────────────────────

// CreateUser inserts a new user row. Generates a "u-<ULID>" ID if u.UID is
// empty. Returns auth.ErrEmailTaken on duplicate email.
func (s *SQLiteStore) CreateUser(ctx context.Context, u *auth.User, passwordHash string) error {
	if u.UID == "" {
		u.UID = "u-" + newULIDStr()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, email, password_hash, display_name, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		u.UID, strings.ToLower(u.Email), passwordHash, u.Name, fmtTime(u.CreatedAt),
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return auth.ErrEmailTaken
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// GetUserByEmail returns the user and bcrypt password hash for email.
// Returns (nil, "", nil) when the email is not found — never returns sql.ErrNoRows.
func (s *SQLiteStore) GetUserByEmail(ctx context.Context, email string) (*auth.User, string, error) {
	var u auth.User
	var hash, createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, password_hash, display_name, created_at
		 FROM users WHERE email = ?`,
		strings.ToLower(email),
	).Scan(&u.UID, &u.Email, &hash, &u.Name, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("get user by email: %w", err)
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return nil, "", err
	}
	u.CreatedAt = t
	return &u, hash, nil
}

// GetUserByID returns the user for uid. Returns (nil, nil) when not found.
func (s *SQLiteStore) GetUserByID(ctx context.Context, uid string) (*auth.User, error) {
	var u auth.User
	var createdAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, display_name, created_at FROM users WHERE id = ?`, uid,
	).Scan(&u.UID, &u.Email, &u.Name, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return nil, err
	}
	u.CreatedAt = t
	return &u, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth: sessions
// ─────────────────────────────────────────────────────────────────────────────

func (s *SQLiteStore) CreateSession(ctx context.Context, sess *auth.Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		sess.ID, sess.UserID, fmtTime(sess.CreatedAt), fmtTime(sess.ExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession returns the session row or (nil, nil) when not found OR expired.
func (s *SQLiteStore) GetSession(ctx context.Context, sessionID string) (*auth.Session, error) {
	var sess auth.Session
	var createdAt, expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, created_at, expires_at FROM sessions
		 WHERE id = ? AND expires_at > ?`,
		sessionID, fmtTime(time.Now().UTC()),
	).Scan(&sess.ID, &sess.UserID, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	if sess.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if sess.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *SQLiteStore) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID)
	return err
}

// DeleteExpiredSessions removes sessions with expires_at <= now.
func (s *SQLiteStore) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at <= ?`, fmtTime(now))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Runs
// ─────────────────────────────────────────────────────────────────────────────

const runColumns = `id, source_type, ifdb_id, title, format, artifact_url, build_id,
	user_id, status, created_at, started_at, finished_at, last_active_at, exit_code,
	transcript_path, error_code, error_message, interpreter, story_path, save_path,
	turn_count, last_save_at, reconnect_count, candidate_urls, project_id`

func scanRun(scan func(...any) error) (*Run, error) {
	var r Run
	var createdAt string
	var startedAt, finishedAt, lastActiveAt, lastSaveAt sql.NullString
	var exitCode sql.NullInt64
	var candidateURLs string

	err := scan(
		&r.ID, &r.SourceType, &r.IFDBId, &r.Title, &r.Format,
		&r.ArtifactURL, &r.BuildID, &r.UserID, &r.Status,
		&createdAt, &startedAt, &finishedAt, &lastActiveAt, &exitCode,
		&r.TranscriptPath, &r.ErrorCode, &r.ErrorMessage,
		&r.Interpreter, &r.StoryPath, &r.SavePath,
		&r.TurnCount, &lastSaveAt, &r.ReconnectCount,
		&candidateURLs, &r.ProjectID,
	)
	if err != nil {
		return nil, err
	}

	var parseErr error
	if r.CreatedAt, parseErr = parseTime(createdAt); parseErr != nil {
		return nil, fmt.Errorf("parse created_at: %w", parseErr)
	}
	if r.StartedAt, parseErr = parseNullTime(startedAt); parseErr != nil {
		return nil, fmt.Errorf("parse started_at: %w", parseErr)
	}
	if r.FinishedAt, parseErr = parseNullTime(finishedAt); parseErr != nil {
		return nil, fmt.Errorf("parse finished_at: %w", parseErr)
	}
	if r.LastActiveAt, parseErr = parseNullTime(lastActiveAt); parseErr != nil {
		return nil, fmt.Errorf("parse last_active_at: %w", parseErr)
	}
	if r.LastSaveAt, parseErr = parseNullTime(lastSaveAt); parseErr != nil {
		return nil, fmt.Errorf("parse last_save_at: %w", parseErr)
	}

	if exitCode.Valid {
		v := int(exitCode.Int64)
		r.ExitCode = &v
	}

	if candidateURLs != "" && candidateURLs != "[]" {
		if err := json.Unmarshal([]byte(candidateURLs), &r.CandidateURLs); err != nil {
			r.CandidateURLs = nil
		}
	}

	return &r, nil
}

func (s *SQLiteStore) CreateRun(ctx context.Context, r *Run) error {
	urlsJSON, _ := json.Marshal(r.CandidateURLs)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (`+runColumns+`) VALUES
		 (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.SourceType, r.IFDBId, r.Title, r.Format, r.ArtifactURL, r.BuildID,
		r.UserID, r.Status, fmtTime(r.CreatedAt),
		nullTimeVal(r.StartedAt), nullTimeVal(r.FinishedAt), nullTimeVal(r.LastActiveAt),
		nullIntVal(r.ExitCode),
		r.TranscriptPath, r.ErrorCode, r.ErrorMessage,
		r.Interpreter, r.StoryPath, r.SavePath,
		r.TurnCount, nullTimeVal(r.LastSaveAt), r.ReconnectCount,
		string(urlsJSON), r.ProjectID,
	)
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetRun(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	r, err := scanRun(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *SQLiteStore) UpdateRun(ctx context.Context, r *Run) error {
	urlsJSON, _ := json.Marshal(r.CandidateURLs)
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET
		 source_type=?, ifdb_id=?, title=?, format=?, artifact_url=?, build_id=?,
		 user_id=?, status=?, created_at=?, started_at=?, finished_at=?, last_active_at=?,
		 exit_code=?, transcript_path=?, error_code=?, error_message=?,
		 interpreter=?, story_path=?, save_path=?, turn_count=?, last_save_at=?,
		 reconnect_count=?, candidate_urls=?, project_id=?
		 WHERE id=?`,
		r.SourceType, r.IFDBId, r.Title, r.Format, r.ArtifactURL, r.BuildID,
		r.UserID, r.Status, fmtTime(r.CreatedAt),
		nullTimeVal(r.StartedAt), nullTimeVal(r.FinishedAt), nullTimeVal(r.LastActiveAt),
		nullIntVal(r.ExitCode),
		r.TranscriptPath, r.ErrorCode, r.ErrorMessage,
		r.Interpreter, r.StoryPath, r.SavePath,
		r.TurnCount, nullTimeVal(r.LastSaveAt), r.ReconnectCount,
		string(urlsJSON), r.ProjectID,
		r.ID,
	)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	return nil
}

// DeleteRun removes the local storage prefix for the run then the SQLite row.
func (s *SQLiteStore) DeleteRun(ctx context.Context, id string) error {
	if _, err := s.blob.DeleteBlobPrefix(ctx, "sessions/"+id+"/"); err != nil {
		return fmt.Errorf("delete run blobs: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, id)
	return err
}

// DeleteAbandonedPendingRuns deletes pending runs older than before.
func (s *SQLiteStore) DeleteAbandonedPendingRuns(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM runs WHERE status = 'pending' AND created_at < ?`,
		fmtTime(before))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ListRunsByUser returns runs owned by userID ordered by lastActiveAt DESC.
func (s *SQLiteStore) ListRunsByUser(ctx context.Context, userID string, limit int) ([]*Run, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE user_id = ?
		 ORDER BY COALESCE(last_active_at, created_at) DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return collectRuns(rows)
}

// ListRunsByProject returns runs with project_id == projectID.
func (s *SQLiteStore) ListRunsByProject(ctx context.Context, projectID string, limit int) ([]*Run, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM runs WHERE project_id = ?
		 ORDER BY created_at DESC LIMIT ?`,
		projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return collectRuns(rows)
}

// DeleteRunsForProject removes all run rows for a project (no blob cleanup).
func (s *SQLiteStore) DeleteRunsForProject(ctx context.Context, projectID string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM runs WHERE project_id = ?`, projectID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func collectRuns(rows *sql.Rows) ([]*Run, error) {
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if out == nil {
		out = []*Run{}
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// Projects
// ─────────────────────────────────────────────────────────────────────────────

const projectColumns = `id, owner_uid, name, description, created_at, updated_at,
	latest_build_id, published, published_at`

func scanProject(scan func(...any) error) (*Project, error) {
	var p Project
	var createdAt, updatedAt string
	var published int
	var publishedAt sql.NullString

	err := scan(
		&p.ID, &p.OwnerUID, &p.Name, &p.Description,
		&createdAt, &updatedAt, &p.LatestBuildID,
		&published, &publishedAt,
	)
	if err != nil {
		return nil, err
	}

	var parseErr error
	if p.CreatedAt, parseErr = parseTime(createdAt); parseErr != nil {
		return nil, fmt.Errorf("parse project created_at: %w", parseErr)
	}
	if p.UpdatedAt, parseErr = parseTime(updatedAt); parseErr != nil {
		return nil, fmt.Errorf("parse project updated_at: %w", parseErr)
	}
	p.Published = published != 0
	if p.PublishedAt, parseErr = parseNullTime(publishedAt); parseErr != nil {
		return nil, fmt.Errorf("parse project published_at: %w", parseErr)
	}
	return &p, nil
}

func (s *SQLiteStore) CreateProject(ctx context.Context, p *Project) error {
	pub := 0
	if p.Published {
		pub = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (`+projectColumns+`) VALUES (?,?,?,?,?,?,?,?,?)`,
		p.ID, p.OwnerUID, p.Name, p.Description,
		fmtTime(p.CreatedAt), fmtTime(p.UpdatedAt),
		p.LatestBuildID, pub, nullTimeVal(p.PublishedAt),
	)
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetProject(ctx context.Context, id string) (*Project, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
	p, err := scanProject(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// UpdateProjectSource is a deprecated shim; prefer PutProjectSource.
func (s *SQLiteStore) UpdateProjectSource(ctx context.Context, id, source string, updatedAt time.Time) error {
	return s.PutProjectSource(ctx, id, source, updatedAt)
}

func (s *SQLiteStore) UpdateProjectMeta(ctx context.Context, id, name, description string, updatedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name=?, description=?, updated_at=? WHERE id=?`,
		name, description, fmtTime(updatedAt), id)
	return err
}

func (s *SQLiteStore) UpdateProjectLatestBuild(ctx context.Context, id, buildID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET latest_build_id=? WHERE id=?`, buildID, id)
	return err
}

func (s *SQLiteStore) ListProjectsByOwner(ctx context.Context, ownerUID string, limit int) ([]*Project, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE owner_uid=?
		 ORDER BY updated_at DESC LIMIT ?`,
		ownerUID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return collectProjects(rows)
}

func collectProjects(rows *sql.Rows) ([]*Project, error) {
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if out == nil {
		out = []*Project{}
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// Project sources
// ─────────────────────────────────────────────────────────────────────────────

func (s *SQLiteStore) GetProjectSource(ctx context.Context, projectID string) (string, error) {
	var src string
	err := s.db.QueryRowContext(ctx,
		`SELECT source FROM project_sources WHERE project_id=?`, projectID,
	).Scan(&src)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return src, err
}

func (s *SQLiteStore) PutProjectSource(ctx context.Context, projectID, source string, updatedAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx,
		`INSERT INTO project_sources (project_id, source) VALUES (?, ?)
		 ON CONFLICT(project_id) DO UPDATE SET source=excluded.source`,
		projectID, source)
	if err != nil {
		return fmt.Errorf("upsert project_sources: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE projects SET updated_at=? WHERE id=?`,
		fmtTime(updatedAt), projectID)
	if err != nil {
		return fmt.Errorf("update project updated_at: %w", err)
	}
	return tx.Commit()
}

func (s *SQLiteStore) DeleteProjectSource(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM project_sources WHERE project_id=?`, projectID)
	return err
}

func (s *SQLiteStore) GetProjectSourceSize(ctx context.Context, projectID string) (int64, bool, error) {
	var size int64
	err := s.db.QueryRowContext(ctx,
		`SELECT length(source) FROM project_sources WHERE project_id=?`, projectID,
	).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return size, true, nil
}


// UpdateProjectAI updates project metadata and appends an ai_turns row in a
// single transaction. If turn is nil, only the project is updated.
func (s *SQLiteStore) UpdateProjectAI(ctx context.Context, p *Project, turn *AITurn) (time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx,
		`UPDATE projects SET description=?, updated_at=? WHERE id=?`,
		p.Description, fmtTime(p.UpdatedAt), p.ID)
	if err != nil {
		return time.Time{}, fmt.Errorf("update project: %w", err)
	}

	if turn != nil {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO ai_turns
			 (id, project_id, owner_uid, kind, user_message, assistant_reply,
			  source_before, source_after, model_requested_at, model_finished_at,
			  prompt_tokens, completion_tokens, model, error)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			turn.ID, turn.ProjectID, turn.OwnerUID, turn.Kind,
			turn.UserMessage, turn.AssistantReply,
			turn.SourceBefore, turn.SourceAfter,
			fmtTime(turn.ModelRequestedAt), fmtTime(turn.ModelFinishedAt),
			turn.PromptTokens, turn.CompletionTokens, turn.Model, turn.Error,
		)
		if err != nil {
			return time.Time{}, fmt.Errorf("insert ai_turn: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return p.UpdatedAt, nil
}

func (s *SQLiteStore) SetProjectPublished(ctx context.Context, projectID string, published bool, now time.Time) error {
	if published {
		_, err := s.db.ExecContext(ctx,
			`UPDATE projects SET published=1, published_at=? WHERE id=?`,
			fmtTime(now), projectID)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE projects SET published=0 WHERE id=?`, projectID)
	return err
}

func (s *SQLiteStore) ListPublishedProjects(ctx context.Context, limit int) ([]*Project, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE published=1
		 ORDER BY published_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	return collectProjects(rows)
}

func (s *SQLiteStore) DeleteProject(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// AI conversation
// ─────────────────────────────────────────────────────────────────────────────

func (s *SQLiteStore) CreateAITurn(ctx context.Context, t *AITurn) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ai_turns
		 (id, project_id, owner_uid, kind, user_message, assistant_reply,
		  source_before, source_after, model_requested_at, model_finished_at,
		  prompt_tokens, completion_tokens, model, error)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.ProjectID, t.OwnerUID, t.Kind,
		t.UserMessage, t.AssistantReply,
		t.SourceBefore, t.SourceAfter,
		fmtTime(t.ModelRequestedAt), fmtTime(t.ModelFinishedAt),
		t.PromptTokens, t.CompletionTokens, t.Model, t.Error,
	)
	if err != nil {
		return fmt.Errorf("create ai_turn: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListAIConversation(ctx context.Context, projectID string, limit int) ([]*AITurn, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, owner_uid, kind, user_message, assistant_reply,
		        source_before, source_after, model_requested_at, model_finished_at,
		        prompt_tokens, completion_tokens, model, error
		 FROM ai_turns WHERE project_id=? ORDER BY model_requested_at ASC LIMIT ?`,
		projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []*AITurn
	for rows.Next() {
		var t AITurn
		var reqAt, finAt string
		if err := rows.Scan(
			&t.ID, &t.ProjectID, &t.OwnerUID, &t.Kind,
			&t.UserMessage, &t.AssistantReply,
			&t.SourceBefore, &t.SourceAfter,
			&reqAt, &finAt,
			&t.PromptTokens, &t.CompletionTokens, &t.Model, &t.Error,
		); err != nil {
			return nil, err
		}
		if t.ModelRequestedAt, err = parseTime(reqAt); err != nil {
			return nil, err
		}
		if t.ModelFinishedAt, err = parseTime(finAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	if out == nil {
		out = []*AITurn{}
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteAIConversation(ctx context.Context, projectID string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ai_turns WHERE project_id=?`, projectID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// GetAITurnAfterSource reads the source_after column from the ai_turns table.
// Returns ("", false, nil) when absent or empty.
func (s *SQLiteStore) GetAITurnAfterSource(ctx context.Context, projectID, turnID string) (string, bool, error) {
	var after string
	err := s.db.QueryRowContext(ctx,
		`SELECT source_after FROM ai_turns WHERE id=? AND project_id=?`,
		turnID, projectID,
	).Scan(&after)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if after == "" {
		return "", false, nil
	}
	return after, true, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Builds
// ─────────────────────────────────────────────────────────────────────────────

const buildColumns = `id, project_id, owner_uid, status, created_at, started_at,
	finished_at, artifact_format, artifact_path, log_path, error_message`

func scanBuild(scan func(...any) error) (*Build, error) {
	var b Build
	var createdAt string
	var startedAt, finishedAt sql.NullString

	err := scan(
		&b.ID, &b.ProjectID, &b.OwnerUID, &b.Status,
		&createdAt, &startedAt, &finishedAt,
		&b.ArtifactFormat, &b.ArtifactPath, &b.LogPath, &b.ErrorMessage,
	)
	if err != nil {
		return nil, err
	}

	var parseErr error
	if b.CreatedAt, parseErr = parseTime(createdAt); parseErr != nil {
		return nil, fmt.Errorf("parse build created_at: %w", parseErr)
	}
	if b.StartedAt, parseErr = parseNullTime(startedAt); parseErr != nil {
		return nil, fmt.Errorf("parse build started_at: %w", parseErr)
	}
	if b.FinishedAt, parseErr = parseNullTime(finishedAt); parseErr != nil {
		return nil, fmt.Errorf("parse build finished_at: %w", parseErr)
	}
	return &b, nil
}

func (s *SQLiteStore) CreateBuild(ctx context.Context, b *Build) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO builds (`+buildColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.ProjectID, b.OwnerUID, b.Status,
		fmtTime(b.CreatedAt), nullTimeVal(b.StartedAt), nullTimeVal(b.FinishedAt),
		b.ArtifactFormat, b.ArtifactPath, b.LogPath, b.ErrorMessage,
	)
	if err != nil {
		return fmt.Errorf("create build: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetBuild(ctx context.Context, id string) (*Build, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+buildColumns+` FROM builds WHERE id=?`, id)
	b, err := scanBuild(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

func (s *SQLiteStore) UpdateBuild(ctx context.Context, b *Build) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE builds SET project_id=?, owner_uid=?, status=?, created_at=?,
		 started_at=?, finished_at=?, artifact_format=?, artifact_path=?,
		 log_path=?, error_message=? WHERE id=?`,
		b.ProjectID, b.OwnerUID, b.Status, fmtTime(b.CreatedAt),
		nullTimeVal(b.StartedAt), nullTimeVal(b.FinishedAt),
		b.ArtifactFormat, b.ArtifactPath, b.LogPath, b.ErrorMessage,
		b.ID,
	)
	if err != nil {
		return fmt.Errorf("update build: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListBuildsByProject(ctx context.Context, projectID string, limit int) ([]*Build, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+buildColumns+` FROM builds WHERE project_id=?
		 ORDER BY created_at DESC LIMIT ?`,
		projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []*Build
	for rows.Next() {
		b, err := scanBuild(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if out == nil {
		out = []*Build{}
	}
	return out, rows.Err()
}

func (s *SQLiteStore) DeleteBuildsForProject(ctx context.Context, projectID string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM builds WHERE project_id=?`, projectID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// IFDB cache
// ─────────────────────────────────────────────────────────────────────────────

func (s *SQLiteStore) GetCachedGame(ctx context.Context, tuid string) (*CachedGame, error) {
	var g CachedGame
	var fetchedAt, expiresAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT tuid, payload, fetched_at, expires_at FROM ifdb_cache
		 WHERE tuid=? AND expires_at > ?`,
		tuid, time.Now().UnixMilli(),
	).Scan(&g.TUID, &g.Payload, &fetchedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.FetchedAt = time.UnixMilli(fetchedAt).UTC()
	g.ExpiresAt = time.UnixMilli(expiresAt).UTC()
	return &g, nil
}

func (s *SQLiteStore) PutCachedGame(ctx context.Context, g *CachedGame) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ifdb_cache (tuid, payload, fetched_at, expires_at) VALUES (?,?,?,?)
		 ON CONFLICT(tuid) DO UPDATE SET payload=excluded.payload,
		 fetched_at=excluded.fetched_at, expires_at=excluded.expires_at`,
		g.TUID, g.Payload, g.FetchedAt.UnixMilli(), g.ExpiresAt.UnixMilli(),
	)
	return err
}

func (s *SQLiteStore) ListFreshCachedGames(ctx context.Context, now time.Time) ([]*CachedGame, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tuid, payload, fetched_at, expires_at FROM ifdb_cache WHERE expires_at > ?`,
		now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []*CachedGame
	for rows.Next() {
		var g CachedGame
		var fetchedAt, expiresAt int64
		if err := rows.Scan(&g.TUID, &g.Payload, &fetchedAt, &expiresAt); err != nil {
			return nil, err
		}
		g.FetchedAt = time.UnixMilli(fetchedAt).UTC()
		g.ExpiresAt = time.UnixMilli(expiresAt).UTC()
		out = append(out, &g)
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// Blob storage — delegated to LocalBlobStore
// ─────────────────────────────────────────────────────────────────────────────

func (s *SQLiteStore) UploadBlob(ctx context.Context, path, contentType string, r io.Reader) error {
	return s.blob.UploadBlob(ctx, path, contentType, r)
}

func (s *SQLiteStore) DownloadBlob(ctx context.Context, path string, w io.Writer) error {
	return s.blob.DownloadBlob(ctx, path, w)
}

func (s *SQLiteStore) DeleteBlobPrefix(ctx context.Context, prefix string) (int, error) {
	return s.blob.DeleteBlobPrefix(ctx, prefix)
}

// ─────────────────────────────────────────────────────────────────────────────
// Lifecycle
// ─────────────────────────────────────────────────────────────────────────────

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
