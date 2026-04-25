// auth_middleware_test.go — Firebase auth middleware tests (§22.14 QA tasks).
//
// Test coverage:
//  1. Allow-list table: allowlisted paths bypass auth; protected paths return
//     401 (API) or 303 (page routes) when no valid token is present.
//  2. 401 JSON shape: API 401 responses carry {"error":"…","code":"auth_required"}.
//  3. Ownership 403: authenticated user B cannot call /start, /command, /suspend
//     on a run owned by user A; returns 403.
//  4. /api/config shape: returns {"firebase":{"apiKey":…,"authDomain":…,"projectId":…},…};
//     no auth required; returns 503 when FIREBASE_WEB_API_KEY is not set.
//  5. No sc_user cookie: no response sets a Set-Cookie: sc_user= header.
//  6. ?token= suspend: POST /api/runs/{id}/suspend?token=<valid> returns 204;
//     ?token=<invalid> returns 401.
//  7. Firebase UID as userId: run.UserID equals the UID from the verified token.
//
// All tests use auth.MockVerifier so no live Firebase project is needed.
// Tests fail fast if the allow-list or middleware is not yet wired in SetupRoutes.
package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"storycloud/internal/auth"
	"storycloud/internal/runner"
	"storycloud/internal/server"
	"storycloud/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// newTSWithMockAuth creates a test HTTP server backed by a MockVerifier. The
// server behaves like production: no token → 401/303; valid token → handler.
// Unlike newTS(), this uses a real (non-local) verifier that actually rejects
// missing or unregistered tokens.
func newTSWithMockAuth(t *testing.T, ms store.Store, mv *auth.MockVerifier) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	srv := server.New(cfg, ms, nil, nil, nil, mv)
	return httptest.NewServer(srv.SetupRoutes())
}

// newTSWithMockAuthAndRunner creates a test HTTP server with a real
// runner.Manager (needed to reach ownership checks in /start and /command,
// which validate runner != nil before the ownership guard).
func newTSWithMockAuthAndRunner(t *testing.T, ms store.Store, mv *auth.MockVerifier) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	runMgr := runner.NewManager(ms, runner.DefaultConfig())
	srv := server.New(cfg, ms, nil, runMgr, nil, mv)
	ts := httptest.NewServer(srv.SetupRoutes())
	t.Cleanup(ts.Close)
	return ts
}

// newTSWithFirebaseConfig creates a test server with MockVerifier and Firebase
// config fields set so /api/config returns 200 instead of 503.
func newTSWithFirebaseConfig(t *testing.T, ms store.Store, mv *auth.MockVerifier) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	cfg.FirebaseWebAPIKey = "test-web-api-key-for-unit-tests"
	cfg.ProjectID = "test-project-id"
	cfg.FirebaseAuthDomain = "test-project-id.firebaseapp.com"
	srv := server.New(cfg, ms, nil, nil, nil, mv)
	return httptest.NewServer(srv.SetupRoutes())
}

// userA / userB are fixed test users for ownership tests.
var (
	userA = &auth.User{UID: "firebase-uid-user-a-test", Email: "a@example.com"}
	userB = &auth.User{UID: "firebase-uid-user-b-test", Email: "b@example.com"}
)

const (
	tokenA = "mock-token-for-user-a"
	tokenB = "mock-token-for-user-b"

	// A well-formed run ID used across tests.
	authTestRunID = "r-01HXZX5K2V0EQB9M7YPQ399999"
)

// newTwoUserVerifier returns a MockVerifier with userA and userB registered.
func newTwoUserVerifier() *auth.MockVerifier {
	mv := auth.NewMockVerifier()
	mv.SetUser(tokenA, userA)
	mv.SetUser(tokenB, userB)
	return mv
}

// bearerHeader returns a populated Authorization header value.
func bearerHeader(token string) string { return "Bearer " + token }

// ─────────────────────────────────────────────────────────────────────────────
// 1. Allow-list table test (§22.5.3)
// ─────────────────────────────────────────────────────────────────────────────

// TestFirebaseAuthAllowList verifies the allow-list defined in §22.5.3:
//   - Allowlisted paths pass through the auth gate regardless of whether a
//     valid token is present (caller may receive any status from the handler,
//     but NOT 401 from the middleware).
//   - Protected API paths (/api/…) without a valid token → 401 JSON.
//   - Protected page paths without a valid token → 303 redirect to /login.
//
// Uses a MockVerifier with NO registered tokens so every auth attempt fails —
// only the allow-list exemption can prevent a 401/303 response.
func TestFirebaseAuthAllowList(t *testing.T) {
	mv := auth.NewMockVerifier() // no tokens → every FromRequest/VerifyToken fails
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	type row struct {
		method        string
		path          string
		wantExact     int // 0 = "any non-401" (for allowlisted paths)
		wantNotStatus int // checked when wantExact == 0
	}

	tests := []row{
		// ── Allowlisted paths — bypass auth gate ─────────────────────────────
		// Status comes from the handler, NOT the middleware.
		{"GET", "/health", 200, 0},
		{"GET", "/login", 200, 0},
		{"GET", "/register", 200, 0},
		// /api/config is allowlisted but returns 503 (no Firebase env vars in testConfig).
		{"GET", "/api/config", 503, 0},
		// /static/* — allowlisted; file exists → 200.
		{"GET", "/static/app.css", 200, 0},
		{"GET", "/static/app.js", 200, 0},
		{"GET", "/static/auth.js", 200, 0},
		// /favicon.ico — allowlisted, but no handler registered → 404 (not 401).
		{"GET", "/favicon.ico", 0, http.StatusUnauthorized},

		// ── Protected API routes → 401 ────────────────────────────────────────
		{"POST", "/api/runs", 401, 0},
		{"GET", "/api/runs/by-user", 401, 0},
		{"GET", "/api/runs/" + authTestRunID, 401, 0},
		{"GET", "/api/runs/" + authTestRunID + "/start", 401, 0},
		{"POST", "/api/runs/" + authTestRunID + "/command", 401, 0},
		{"POST", "/api/runs/" + authTestRunID + "/suspend", 401, 0},
		{"DELETE", "/api/runs/" + authTestRunID, 401, 0},
		{"POST", "/api/runs/" + authTestRunID + "/restart", 401, 0},
		{"GET", "/api/runs/bad-id", 401, 0}, // bad ID but auth blocks first
		// Build test endpoint — auth required before glulxe or store touch.
		{"POST", "/api/builds/" + btBuildID + "/test", 401, 0},
		{"GET", "/api/ifdb/search?q=zork", 401, 0},
		{"GET", "/api/ifdb/games/0dbnusxunq7fw5ro", 401, 0},
		{"GET", "/api/projects/p-01HXZX5K3Q0RTB9M7YPZL00001", 401, 0},
		{"POST", "/api/projects", 401, 0},

		// ── Page routes — bypass auth gate entirely ───────────────────────────
		// Browsers never send Authorization headers on navigation; auth.js
		// onAuthStateChanged handles the client-side guard. Server must serve
		// the page so the JS can run and redirect if needed.
		{"GET", "/", 200, 0},
		{"GET", "/history", 200, 0},
		{"GET", "/play/" + authTestRunID, 200, 0},
		{"GET", "/create", 200, 0},
		{"GET", "/games/0dbnusxunq7fw5ro", 200, 0},
		{"GET", "/projects/p-01HXZX5K3Q0RTB9M7YPZL00001", 200, 0},
	}

	client := &http.Client{
		// Do NOT follow redirects — we want to see the 303 directly.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for _, tt := range tests {
		name := tt.method + " " + tt.path
		t.Run(name, func(t *testing.T) {
			var body io.Reader
			if tt.method == "POST" || tt.method == "PUT" {
				body = bytes.NewReader([]byte("{}"))
			}
			req, _ := http.NewRequest(tt.method, ts.URL+tt.path, body)
			if tt.method == "POST" || tt.method == "PUT" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if tt.wantExact != 0 {
				if resp.StatusCode != tt.wantExact {
					b, _ := io.ReadAll(resp.Body)
					t.Errorf("status = %d; want %d\nbody: %s",
						resp.StatusCode, tt.wantExact, truncate(string(b), 200))
				}
			} else {
				// Allowlisted path: must NOT be 401 (auth bypass confirmed).
				if resp.StatusCode == tt.wantNotStatus {
					b, _ := io.ReadAll(resp.Body)
					t.Errorf("allowlisted path returned %d (auth gate not bypassed)\nbody: %s",
						resp.StatusCode, truncate(string(b), 200))
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. 401 JSON shape (§22.4.2 — API paths must return JSON, not HTML)
// ─────────────────────────────────────────────────────────────────────────────

// TestAPI401JSONShape verifies that unauthenticated requests to /api/* paths
// return a JSON body with "error" and "code" fields set to "auth_required",
// and Content-Type: application/json — NOT an HTML redirect page.
func TestAPI401JSONShape(t *testing.T) {
	mv := auth.NewMockVerifier()
	ts := newTSWithMockAuth(t, newMockStore(), mv)
	defer ts.Close()

	apiPaths := []struct {
		method string
		path   string
	}{
		{"POST", "/api/runs"},
		{"GET", "/api/runs/by-user"},
		{"GET", "/api/runs/" + authTestRunID},
		{"GET", "/api/ifdb/search?q=test"},
		{"GET", "/api/projects/p-01HXZX5K3Q0RTB9M7YPZL00001"},
	}

	for _, tt := range apiPaths {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			var body io.Reader
			if tt.method == "POST" {
				body = bytes.NewReader([]byte("{}"))
			}
			req, _ := http.NewRequest(tt.method, ts.URL+tt.path, body)
			if tt.method == "POST" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d; want 401", resp.StatusCode)
			}

			// Must be JSON, not HTML.
			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q; want application/json for 401", ct)
			}

			var errBody struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			data, _ := io.ReadAll(resp.Body)
			if err := json.Unmarshal(data, &errBody); err != nil {
				t.Fatalf("decode 401 body: %v\nbody: %s", err, data)
			}
			if errBody.Error == "" {
				t.Errorf("401 body missing 'error' field\nbody: %s", data)
			}
			if errBody.Code != "auth_required" {
				t.Errorf("code = %q; want auth_required\nbody: %s", errBody.Code, data)
			}
		})
	}
}

// TestPageRoutesServedWithoutAuth verifies that unauthenticated requests to page
// routes (non-/api/) are served as HTML (200) rather than being redirected
// server-side to /login. Page-level auth is handled entirely client-side by
// auth.js / onAuthStateChanged — the server must return the HTML so the script
// can run. (§22.4.2, §22.5.1 — no server-side redirect on page routes.)
func TestPageRoutesServedWithoutAuth(t *testing.T) {
	mv := auth.NewMockVerifier() // no tokens → any API auth attempt fails
	ts := newTSWithMockAuth(t, newMockStore(), mv)
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	pagePaths := []string{
		"/",
		"/history",
		"/create",
		"/play/" + authTestRunID,
		"/games/0dbnusxunq7fw5ro",
	}

	for _, path := range pagePaths {
		t.Run("GET "+path, func(t *testing.T) {
			resp, err := client.Get(ts.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer resp.Body.Close()

			// Page routes bypass the server-side auth gate → handler runs → 200 HTML.
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d; want 200 (pages bypass server-side auth)\nbody: %s",
					resp.StatusCode, truncate(string(b), 200))
			}
			ct := resp.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q; want text/html", ct)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Ownership 403 tests (§22.4.3)
// ─────────────────────────────────────────────────────────────────────────────

// TestOwnershipForbiddenOnSuspend verifies that user B cannot call
// POST /api/runs/{id}/suspend on a run owned by user A → 403.
// This endpoint does not require a runner (nil runner is fine for this check).
func TestOwnershipForbiddenOnSuspend(t *testing.T) {
	mv := newTwoUserVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	// Create a run owned by user A.
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:         authTestRunID,
		UserID:     userA.UID,
		Status:     "running",
		SourceType: "ifdb",
		IFDBId:     "0dbnusxunq7fw5ro",
		CreatedAt:  time.Now().UTC(),
	})

	// User B tries to suspend user A's run.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runs/"+authTestRunID+"/suspend",
		bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(tokenB))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST suspend: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403 (user B accessing user A's run)\nbody: %s",
			resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestOwnershipForbiddenOnStart verifies that user B cannot call
// GET /api/runs/{id}/start on a run owned by user A → 403.
// Requires a real runner manager (handleRunStart checks runner != nil before ownership).
func TestOwnershipForbiddenOnStart(t *testing.T) {
	mv := newTwoUserVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuthAndRunner(t, ms, mv)

	// Create a run owned by user A.
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:         authTestRunID,
		UserID:     userA.UID,
		Status:     "pending",
		SourceType: "url",
		ArtifactURL: "https://example.com/story.z5",
		CreatedAt:  time.Now().UTC(),
	})

	// User B tries to start user A's run.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/runs/"+authTestRunID+"/start", nil)
	req.Header.Set("Authorization", bearerHeader(tokenB))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET start: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403 (user B starting user A's run)\nbody: %s",
			resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestOwnershipForbiddenOnCommand verifies that user B cannot call
// POST /api/runs/{id}/command on a run owned by user A → 403.
// Requires a real runner manager (handleRunCommand checks runner != nil before ownership).
func TestOwnershipForbiddenOnCommand(t *testing.T) {
	mv := newTwoUserVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuthAndRunner(t, ms, mv)

	// Create a run owned by user A with a status that passes the early status check.
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:         authTestRunID,
		UserID:     userA.UID,
		Status:     "running",
		SourceType: "url",
		ArtifactURL: "https://example.com/story.z5",
		CreatedAt:  time.Now().UTC(),
	})

	// User B tries to send a command to user A's run.
	body, _ := json.Marshal(map[string]string{"input": "look"})
	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/runs/"+authTestRunID+"/command", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(tokenB))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST command: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403 (user B commanding user A's run)\nbody: %s",
			resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestOwnershipAllowedForOwner verifies that user A can access their own run
// on /suspend (sanity check that ownership check allows the actual owner).
func TestOwnershipAllowedForOwner(t *testing.T) {
	mv := newTwoUserVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	// Run already suspended → handler returns 204 immediately (no runner needed).
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:         authTestRunID,
		UserID:     userA.UID,
		Status:     "suspended",
		SourceType: "ifdb",
		IFDBId:     "0dbnusxunq7fw5ro",
		CreatedAt:  time.Now().UTC(),
	})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runs/"+authTestRunID+"/suspend",
		bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(tokenA))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST suspend: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 204 (owner suspending own run)\nbody: %s",
			resp.StatusCode, b)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. /api/config shape tests (§22.6.1)
// ─────────────────────────────────────────────────────────────────────────────

// TestConfigEndpointShape verifies the 200 response body when Firebase is
// configured: must include firebase.{apiKey, authDomain, projectId}, plus
// environment and version top-level fields.
func TestConfigEndpointShape(t *testing.T) {
	mv := auth.NewMockVerifier()
	ms := newMockStore()
	ts := newTSWithFirebaseConfig(t, ms, mv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}

	// Verify Cache-Control is set (§22.6.1 — "public, max-age=300").
	cc := resp.Header.Get("Cache-Control")
	if !strings.Contains(cc, "max-age=300") {
		t.Errorf("Cache-Control = %q; want max-age=300 present", cc)
	}

	var body struct {
		Firebase struct {
			APIKey     string `json:"apiKey"`
			AuthDomain string `json:"authDomain"`
			ProjectID  string `json:"projectId"`
		} `json:"firebase"`
		Environment string `json:"environment"`
		Version     string `json:"version"`
	}
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode /api/config: %v\nbody: %s", err, data)
	}

	if body.Firebase.APIKey == "" {
		t.Errorf("firebase.apiKey is empty\nbody: %s", data)
	}
	if body.Firebase.AuthDomain == "" {
		t.Errorf("firebase.authDomain is empty\nbody: %s", data)
	}
	if body.Firebase.ProjectID == "" {
		t.Errorf("firebase.projectId is empty\nbody: %s", data)
	}
	if body.Environment == "" {
		t.Errorf("environment is empty\nbody: %s", data)
	}
	if body.Version == "" {
		t.Errorf("version is empty\nbody: %s", data)
	}
}

// TestConfigEndpointAuthDisabledWhenNoAPIKey verifies that when FIREBASE_WEB_API_KEY
// (or GCP_PROJECT_ID) is not set, /api/config returns 503 with auth_disabled error.
func TestConfigEndpointAuthDisabledWhenNoAPIKey(t *testing.T) {
	mv := auth.NewMockVerifier()
	ms := newMockStore()
	// testConfig() has no Firebase fields set → handler returns 503.
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 503 when FirebaseWebAPIKey not set\nbody: %s",
			resp.StatusCode, b)
	}

	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode 503 body: %v\nbody: %s", err, data)
	}
	if body.Error != "auth_disabled" {
		t.Errorf("error = %q; want auth_disabled\nbody: %s", body.Error, data)
	}
	if body.Code != "auth_disabled" {
		t.Errorf("code = %q; want auth_disabled\nbody: %s", body.Code, data)
	}
}

// TestConfigEndpointNoAuthRequired verifies that /api/config is accessible
// without an Authorization header (MockVerifier has no tokens registered, so
// any authenticated request would fail). This confirms the allow-list is wired.
func TestConfigEndpointNoAuthRequired(t *testing.T) {
	mv := auth.NewMockVerifier() // no tokens → auth always fails
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	// Request with no Authorization header to a server that would reject all tokens.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/config", nil)
	// Deliberately do NOT set Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/config (no auth): %v", err)
	}
	defer resp.Body.Close()

	// Must NOT be 401 (auth_required). Any other status from the handler is fine.
	if resp.StatusCode == http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET /api/config returned 401 — allow-list not working\nbody: %s", b)
	}
}

// TestConfigEndpointAuthDomainDefaulted verifies that when FIREBASE_AUTH_DOMAIN
// is not set but the other Firebase fields are, authDomain defaults to
// {projectId}.firebaseapp.com (§22.6.1).
func TestConfigEndpointAuthDomainDefaulted(t *testing.T) {
	mv := auth.NewMockVerifier()
	ms := newMockStore()

	cfg := testConfig()
	cfg.FirebaseWebAPIKey = "AIzaSyTest12345"
	cfg.ProjectID = "my-test-project"
	// FirebaseAuthDomain intentionally left empty → must be defaulted server-side.
	srv := server.New(cfg, ms, nil, nil, nil, mv)
	ts := httptest.NewServer(srv.SetupRoutes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatalf("GET /api/config: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, b)
	}

	var body struct {
		Firebase struct {
			AuthDomain string `json:"authDomain"`
			ProjectID  string `json:"projectId"`
		} `json:"firebase"`
	}
	data, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, data)
	}

	want := "my-test-project.firebaseapp.com"
	if body.Firebase.AuthDomain != want {
		t.Errorf("authDomain = %q; want %q (defaulted from projectId)", body.Firebase.AuthDomain, want)
	}
	if body.Firebase.ProjectID != "my-test-project" {
		t.Errorf("projectId = %q; want my-test-project", body.Firebase.ProjectID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. No sc_user cookie tests (§22.4.1)
// ─────────────────────────────────────────────────────────────────────────────

// TestNoScUserCookieSet verifies that no response sets a "sc_user" cookie.
// The ensureUserCookie middleware and all sc_user references have been removed
// (§22.4.1). Checking both authenticated and unauthenticated responses.
func TestNoScUserCookieSet(t *testing.T) {
	mv := newTwoUserVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	testCases := []struct {
		desc   string
		method string
		path   string
		auth   string // token to use; "" = unauthenticated
	}{
		{"health unauthenticated", "GET", "/health", ""},
		{"login page", "GET", "/login", ""},
		{"api/config unauthenticated", "GET", "/api/config", ""},
		{"index page unauthenticated (→303)", "GET", "/", ""},
		{"api/runs unauthenticated (→401)", "POST", "/api/runs", ""},
		{"api/runs/by-user authenticated", "GET", "/api/runs/by-user", tokenA},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			var body io.Reader
			if tc.method == "POST" {
				body = bytes.NewReader([]byte("{}"))
			}
			req, _ := http.NewRequest(tc.method, ts.URL+tc.path, body)
			if tc.method == "POST" {
				req.Header.Set("Content-Type", "application/json")
			}
			if tc.auth != "" {
				req.Header.Set("Authorization", bearerHeader(tc.auth))
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body) //nolint:errcheck

			for _, cookie := range resp.Cookies() {
				if cookie.Name == "sc_user" {
					t.Errorf("response set sc_user cookie: %q — cookie identity has been removed (§22.4.1)", cookie.Value)
				}
			}

			// Also check raw Set-Cookie header to catch any non-parsed values.
			for _, h := range resp.Header.Values("Set-Cookie") {
				if strings.Contains(h, "sc_user") {
					t.Errorf("Set-Cookie header contains sc_user: %q", h)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. ?token= suspend tests (§22.5.4)
// ─────────────────────────────────────────────────────────────────────────────

// TestSuspendWithValidQueryToken verifies that POST /api/runs/{id}/suspend?token=<valid>
// succeeds (204) without an Authorization header. This is the sendBeacon path.
func TestSuspendWithValidQueryToken(t *testing.T) {
	mv := auth.NewMockVerifier()
	mv.SetUser("suspend-token-valid", userA)

	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	// Run already suspended → handler returns 204 immediately (idempotent).
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:         authTestRunID,
		UserID:     userA.UID,
		Status:     "suspended",
		SourceType: "ifdb",
		IFDBId:     "0dbnusxunq7fw5ro",
		CreatedAt:  time.Now().UTC(),
	})

	// sendBeacon-style: no Authorization header, token in query string.
	path := "/api/runs/" + authTestRunID + "/suspend?token=suspend-token-valid"
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, nil)
	// Deliberately NO Authorization header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST suspend?token=: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 204 (valid ?token= suspend)\nbody: %s",
			resp.StatusCode, b)
	}
}

// TestSuspendWithInvalidQueryToken verifies that POST /api/runs/{id}/suspend?token=<invalid>
// returns 401 — the middleware rejects unrecognised tokens even on the suspend path.
func TestSuspendWithInvalidQueryToken(t *testing.T) {
	mv := auth.NewMockVerifier()
	mv.SetUser("suspend-token-valid", userA) // only this token is valid

	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:         authTestRunID,
		UserID:     userA.UID,
		Status:     "suspended",
		SourceType: "ifdb",
		IFDBId:     "0dbnusxunq7fw5ro",
		CreatedAt:  time.Now().UTC(),
	})

	// Token is not registered in MockVerifier → should 401.
	path := "/api/runs/" + authTestRunID + "/suspend?token=invalid-or-expired-token"
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, nil)
	// No Authorization header.

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST suspend?token=invalid: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 401 (invalid ?token= suspend)\nbody: %s",
			resp.StatusCode, b)
	}
	assertErrorShape(t, resp.Body)
}

// TestSuspendMissingToken verifies that POST /api/runs/{id}/suspend with no
// Authorization header AND no ?token= query param returns 401.
func TestSuspendMissingToken(t *testing.T) {
	mv := auth.NewMockVerifier()
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	path := "/api/runs/" + authTestRunID + "/suspend"
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, nil)
	// No Authorization header, no ?token=

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST suspend (no token): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 401 (no token at all)\nbody: %s",
			resp.StatusCode, b)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Firebase UID as userId (§22.4.3)
// ─────────────────────────────────────────────────────────────────────────────

// TestCreateRunSetsFirebaseUID verifies that after handleCreateRun, the stored
// run's UserID equals auth.UserFromContext(r.Context()).UID — the Firebase UID
// from the verified token, NOT a cookie value or any other source.
func TestCreateRunSetsFirebaseUID(t *testing.T) {
	const testToken = "token-for-uid-test"
	testUser := &auth.User{
		UID:   "AaBbCcDdEeFfGgHhIiJjKkLlMm12", // 28-char Firebase UID shape
		Email: "uid-test@example.com",
	}

	mv := auth.NewMockVerifier()
	mv.SetUser(testToken, testUser)

	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"sourceType":  "url",
		"artifactUrl": "https://example.com/story.z5",
	})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader(testToken))

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

	// Retrieve from store and verify UserID is the Firebase UID.
	run, err := ms.GetRun(context.Background(), runResp.ID)
	if err != nil || run == nil {
		t.Fatalf("GetRun(%q): %v", runResp.ID, err)
	}
	if run.UserID != testUser.UID {
		t.Errorf("run.UserID = %q; want %q (Firebase UID from token, not cookie)",
			run.UserID, testUser.UID)
	}
}

// TestFirebaseUIDIsOpaqueString verifies that code does not reject a Firebase-
// shaped UID as invalid. Firebase UIDs are 28-char [A-Za-z0-9] — they look
// nothing like the old u-<ULID> format and must never be validated against that.
func TestFirebaseUIDIsOpaqueString(t *testing.T) {
	// Register a run with a Firebase-shaped UID and verify the server treats it
	// as a valid owner, not an invalid format.
	const firebaseUID = "aB3kQ9rLmXNPz0vT1uWxYzAbCdEf" // 28 chars, mixed case

	mv := auth.NewMockVerifier()
	mv.SetUser("firebase-shaped-token", &auth.User{UID: firebaseUID, Email: "fb@example.com"})

	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	// Create a run owned by the Firebase UID.
	runID := "r-01HXZX5K2V0EQB9M7YPQ388888"
	_ = ms.CreateRun(context.Background(), &store.Run{
		ID:         runID,
		UserID:     firebaseUID,
		Status:     "suspended",
		SourceType: "ifdb",
		IFDBId:     "0dbnusxunq7fw5ro",
		CreatedAt:  time.Now().UTC(),
	})

	// Owner with Firebase UID can suspend their run (not rejected as invalid format).
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/runs/"+runID+"/suspend",
		bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", bearerHeader("firebase-shaped-token"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST suspend: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("Firebase UID owner suspend = %d; want 204 (UID must not be regex-validated)\nbody: %s",
			resp.StatusCode, b)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// isAllowlisted pure-function coverage via middleware behaviour
// ─────────────────────────────────────────────────────────────────────────────

// TestAllowListEdgeCases covers boundary inputs for isAllowlisted (§22.5.3):
// exact path matches, static prefix, and paths that are similar but not listed.
func TestAllowListEdgeCases(t *testing.T) {
	mv := auth.NewMockVerifier() // no tokens → any auth failure returns 401

	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	type row struct {
		path        string
		expectAllow bool // true = not 401, false = expect 401 or 303
	}

	tests := []row{
		// Non-API paths — always bypass auth gate (page/asset routes).
		{"/health", true},
		{"/login", true},
		{"/register", true},
		{"/favicon.ico", true},
		{"/", true},
		{"/history", true},
		{"/create", true},

		// Static assets — bypass auth (not /api/).
		{"/static/app.css", true},
		{"/static/auth.js", true},
		{"/static/", true},      // bare /static/ prefix
		{"/static/x/y/z", true}, // deep path under /static/

		// Non-/api/ paths with names similar to protected paths — still bypass auth.
		{"/loginextra", true}, // not /api/ → passes through
		{"/healthz", true},    // not /api/ → passes through
		{"/static", true},     // not /api/ → passes through

		// /api/config — the one API path that bypasses auth.
		{"/api/config", true},

		// All other /api/ paths → auth required (MockVerifier has no tokens → 401).
		{"/api/config/extra", false}, // extra segment after /api/config → blocked
		{"/api/runs", false},
		{"/api/ifdb/search", false},
		{"/api/projects", false},
	}

	// One server for all sub-tests.
	ms := newMockStore()
	ts := newTSWithMockAuth(t, ms, mv)
	defer ts.Close()

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := client.Get(ts.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body) //nolint:errcheck

			is401or303 := resp.StatusCode == http.StatusUnauthorized ||
				resp.StatusCode == http.StatusSeeOther

			if tt.expectAllow && is401or303 {
				t.Errorf("GET %s: expected allow-listed (not 401/303), got %d",
					tt.path, resp.StatusCode)
			}
			if !tt.expectAllow && !is401or303 {
				t.Errorf("GET %s: expected blocked (401/303), got %d",
					tt.path, resp.StatusCode)
			}
		})
	}
}
