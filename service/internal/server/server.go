// Package server wires together all HTTP handlers, middleware, and templates.
package server

import (
	"context"
	"html/template"
	"io/fs"
	"net/http"

	"aifstudio/internal/auth"
	"aifstudio/internal/config"
	"aifstudio/internal/ifdb"
	openaiClient "aifstudio/internal/openai"
	"aifstudio/internal/runner"
	"aifstudio/internal/store"
	"aifstudio/internal/templates"
)

// BuilderService is the interface satisfied by *build.Manager.
// Defined here so tests can inject a mock without importing the build package.
type BuilderService interface {
	StartBuild(ctx context.Context, b *store.Build, source string) error
	ActiveBuildID(projectID string) string
}

// TemplateSet holds one parsed template per UI page.
type TemplateSet struct {
	Index         *template.Template
	GameDetail    *template.Template
	Play          *template.Template
	Create        *template.Template
	ProjectDetail *template.Template
	History       *template.Template
	Login         *template.Template
	Register      *template.Template
	AIWorkspace   *template.Template
	Community     *template.Template
	Projects      *template.Template
}

// Server holds all handler dependencies wired by main.go.
type Server struct {
	cfg        *config.Config
	store      store.Store
	ifdbClient *ifdb.Client
	runner     *runner.Manager
	builder    BuilderService
	auth       auth.VerifierIface
	tmpl       *TemplateSet
	openai     *openaiClient.Client
	aiLimiter  *aiUserLimiter
}

// New creates a Server with all dependencies and parses all HTML templates.
// buildMgr satisfies BuilderService; pass nil to disable build features.
// a must implement auth.VerifierIface — pass *auth.Verifier for production
// or *auth.MockVerifier for tests that need controlled token verification.
func New(cfg *config.Config, st store.Store, ifdbClient *ifdb.Client, runMgr *runner.Manager, buildMgr BuilderService, a auth.VerifierIface) *Server {
	oai := openaiClient.NewClient(openaiClient.ClientOptions{
		APIKey:  cfg.OpenAIAPIKey,
		BaseURL: cfg.OpenAIBaseURL,
		Model:   cfg.OpenAIModel,
		Timeout: cfg.OpenAITimeout,
	})
	return &Server{
		cfg:        cfg,
		store:      st,
		ifdbClient: ifdbClient,
		runner:     runMgr,
		builder:    buildMgr,
		auth:       a,
		tmpl:       parseTemplates(),
		openai:     oai,
		aiLimiter:  newAIUserLimiter(cfg.AIRateLimitPerUserQPS, cfg.AIRateLimitPerUserBurst),
	}
}

// parseTemplates parses all page templates (each joined with layout.html).
func parseTemplates() *TemplateSet {
	must := func(name string) *template.Template {
		return template.Must(template.ParseFS(templates.HTMLFS, "layout.html", name))
	}
	return &TemplateSet{
		Index:         must("index.html"),
		GameDetail:    must("game_detail.html"),
		Play:          must("play.html"),
		Create:        must("create.html"),
		ProjectDetail: must("project_detail.html"),
		History:       must("history.html"),
		Login:         must("login.html"),
		Register:      must("register.html"),
		AIWorkspace:   must("ai_workspace.html"),
		Community:     must("community.html"),
		Projects:      must("projects.html"),
	}
}

// SetupRoutes registers all HTTP routes per §6.1 and §22.5.2 of ARCHITECTURE.md
// and wraps the mux in the middleware chain.
//
// IMPORTANT: The root UI route MUST be "GET /{$}" (exact match), not "GET /".
// Using "GET /" would swallow 405 Method Not Allowed responses from API routes.
func (s *Server) SetupRoutes() http.Handler {
	mux := http.NewServeMux()

	// Health — unauthenticated, no store/IFDB touch.
	mux.HandleFunc("GET /health", s.handleHealth)

	// IFDB proxy — behind auth (§22.4.3).
	mux.HandleFunc("GET /api/ifdb/search", s.handleIFDBSearch)
	mux.HandleFunc("GET /api/ifdb/games/{id}", s.handleIFDBGame)

	// Runs — behind auth (§22.4.3).
	mux.HandleFunc("POST /api/runs", s.handleCreateRun)
	// GET /api/runs/by-user must be registered before GET /api/runs/{id} so the
	// literal "by-user" segment takes priority over the {id} wildcard.
	mux.HandleFunc("GET /api/runs/by-user", s.handleRunsByUser)
	mux.HandleFunc("GET /api/runs/{id}", s.handleGetRun)
	mux.HandleFunc("GET /api/runs/{id}/start", s.handleRunStart)
	mux.HandleFunc("GET /api/runs/{id}/transcript", s.handleGetRunTranscript)
	mux.HandleFunc("POST /api/runs/{id}/command", s.handleRunCommand)
	mux.HandleFunc("POST /api/runs/{id}/suspend", s.handleRunSuspend)
	mux.HandleFunc("DELETE /api/runs/{id}", s.handleDeleteRun)
	mux.HandleFunc("POST /api/runs/{id}/restart", s.handleRestartRun)

	// Builds — direct access by build ID (not nested under projects).
	// POST /api/builds/{buildId}/test runs the game headlessly and streams SSE.
	// GET  /api/builds/{buildId}/artifact and /log serve blobs to the owner.
	mux.HandleFunc("POST /api/builds/{buildId}/test", s.handleBuildTest)
	mux.HandleFunc("GET /api/builds/{buildId}/artifact", s.handleGetBuildArtifact)
	mux.HandleFunc("GET /api/builds/{buildId}/log", s.handleGetBuildLog)

	// Projects — behind auth (global middleware covers these; no per-handler wrapper).
	// GET /api/projects must be registered before GET /api/projects/{id} so the
	// exact literal "projects" segment takes priority over the {id} wildcard.
	mux.HandleFunc("GET /api/projects", s.handleListProjects)
	mux.HandleFunc("POST /api/projects", s.handleCreateProject)
	mux.HandleFunc("GET /api/projects/{id}", s.handleGetProject)
	mux.HandleFunc("PATCH /api/projects/{id}", s.handlePatchProject)
	mux.HandleFunc("DELETE /api/projects/{id}", s.handleDeleteProject)
	// PATCH is the canonical method (§10.0.3); PUT returns 204 No Content (§10.0.4).
	// GET /api/projects/{id}/source must be registered before PATCH/PUT so the
	// literal "source" segment takes priority.
	mux.HandleFunc("GET /api/projects/{id}/source", s.handleGetProjectSource)
	mux.HandleFunc("PATCH /api/projects/{id}/source", s.handlePatchProjectSource)
	mux.HandleFunc("PUT /api/projects/{id}/source", s.handlePutProjectSource)
	mux.HandleFunc("POST /api/projects/{id}/builds", s.handleCreateBuild)
	mux.HandleFunc("GET /api/projects/{id}/builds/{buildId}", s.handleGetBuild)

	// History — GET /api/projects/{id}/history and {turnId}/source endpoints.
	mux.HandleFunc("GET /api/projects/{id}/history", s.handleListHistory)
	mux.HandleFunc("GET /api/projects/{id}/history/{turnId}/source", s.handleGetTurnSource)

	// AI generation (SSE) — §10.1 / §10.2 of ARCHITECTURE_AI_CREATE.md.
	mux.HandleFunc("POST /api/projects/{id}/ai/generate", s.handleAIGenerate)
	mux.HandleFunc("POST /api/projects/{id}/ai/chat", s.handleAIChat)

	// Publish toggle — §10.3.
	mux.HandleFunc("PATCH /api/projects/{id}/publish", s.handleTogglePublish)

	// Community catalog — §10.4 / §10.5.
	mux.HandleFunc("GET /api/community", s.handleCommunityList)
	mux.HandleFunc("POST /api/community/{id}/play", s.handleCommunityPlay)

	// Auth API — allow-listed (register/login bypass auth gate; logout/me require session).
	mux.HandleFunc("POST /api/auth/register", s.handleAuthRegister)
	mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("GET /api/auth/me", s.handleAuthMe)

	// Auth pages — unauthenticated (allow-listed in sessionAuthRequired).
	mux.HandleFunc("GET /login", s.handlePageLogin)
	mux.HandleFunc("GET /register", s.handlePageRegister)

	// UI pages — GET /{$} is exact match (see §6.1).
	mux.HandleFunc("GET /{$}", s.handlePageIndex)
	mux.HandleFunc("GET /games/{ifdbId}", s.handlePageGameDetail)
	mux.HandleFunc("GET /play/{runId}", s.handlePagePlay)
	mux.HandleFunc("GET /history", s.handlePageHistory)
	mux.HandleFunc("GET /create", s.handlePageCreate)
	mux.HandleFunc("GET /projects", s.handlePageProjects)
	mux.HandleFunc("GET /projects/{id}/ai", s.handlePageAIWorkspace)
	mux.HandleFunc("GET /projects/{id}", s.handlePageProjectDetail)
	mux.HandleFunc("GET /community", s.handlePageCommunity)

	// Static assets.
	// templates.StaticFS embeds files under a "static/" prefix (from the
	// //go:embed static/* directive). fs.Sub strips that prefix so that
	// FileServer sees "app.js" / "app.css" directly, matching the path left
	// after http.StripPrefix removes the "/static/" URL prefix.
	staticRoot, err := fs.Sub(templates.StaticFS, "static")
	if err != nil {
		panic("static assets sub-FS: " + err.Error())
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		http.FileServer(http.FS(staticRoot))))

	// Middleware chain (outermost → innermost):
	// recover → requestID → logging → cors → maxBody → sessionAuthRequired → mux
	var h http.Handler = mux
	h = s.sessionAuthRequired(h)
	h = maxBodyMiddleware(h)
	h = corsMiddleware(h)
	h = loggingMiddleware(h)
	h = requestIDMiddleware(h)
	h = recoverMiddleware(h)
	return h
}
