// handlers_builds_test.go — tests for POST /api/builds/{buildId}/test (SSE)
// and the HasTest field on GET /api/projects/{id}/builds/{buildId}.
//
// Test priority order (per QA standards):
//  1. Routing smoke: wrong HTTP method → 405
//  2. Pre-SSE error paths: invalid ID (400), not found (404), forbidden (403),
//     build not ready (409), GCS download failure (500 before SSE mode)
//  3. SSE mode: Content-Type, event shape, won detection, output streaming
//  4. HasTest field on buildResponse: true/false based on source content
//
// Win detection tests use a fake "glulxe" shell script injected into PATH via
// t.Setenv so they run in any environment without a real game file.
package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aifstudio/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test constants
// ─────────────────────────────────────────────────────────────────────────────

const (
	// btBuildID / btProjectID are used in build-test handler tests.
	// Distinct from the AI-handler constants (aiTestBuildID, aiTestProjID) to
	// avoid cross-test store collisions when both sets of tests run in the same
	// package.
	btBuildID   = "b-01HXZX5K4MQS0RTB9M7YPB0001"
	btProjectID = "p-01HXZX5K3Q0RTB9M7YPZM00001"
	btOwnerUID  = "local-dev" // matches the fixed UID returned by local-mode auth.Verifier
)

// ─────────────────────────────────────────────────────────────────────────────
// Fake glulxe helpers
// ─────────────────────────────────────────────────────────────────────────────

// withFakeGlulxe installs a temporary "glulxe" shell script into PATH for the
// duration of t. The script cats output from a data file so arbitrary bytes
// (including newlines) are emitted reliably without shell-quoting issues.
// t.Setenv restores the original PATH when the test ends.
func withFakeGlulxe(t *testing.T, output string) {
	t.Helper()
	dir := t.TempDir()

	// Write the output payload to a side-car file.
	dataFile := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(dataFile, []byte(output), 0o644); err != nil {
		t.Fatalf("withFakeGlulxe: write output file: %v", err)
	}

	// The script cats the payload and exits 0.  Single-quoting the path is safe
	// because t.TempDir() never returns a path with single-quote characters.
	scriptPath := filepath.Join(dir, "glulxe")
	script := fmt.Sprintf("#!/bin/sh\ncat '%s'\n", dataFile)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("withFakeGlulxe: write script: %v", err)
	}

	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// ─────────────────────────────────────────────────────────────────────────────
// SSE parsing helpers (unnamed events: "data: {json}\n\n")
// ─────────────────────────────────────────────────────────────────────────────

// parseBuildTestSSE reads all unnamed SSE data lines from body (lines starting
// with "data: ") and returns them as decoded JSON payloads.
func parseBuildTestSSE(t *testing.T, body io.Reader) []json.RawMessage {
	t.Helper()
	var events []json.RawMessage
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, json.RawMessage(strings.TrimPrefix(line, "data: ")))
		}
	}
	return events
}

// findResultEvent returns the first SSE payload whose "type" field is "result".
func findResultEvent(events []json.RawMessage) (json.RawMessage, bool) {
	for _, ev := range events {
		var m struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(ev, &m); err == nil && m.Type == "result" {
			return ev, true
		}
	}
	return nil, false
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

// setupSucceededBuild seeds ms with a succeeded build owned by btOwnerUID.
func setupSucceededBuild(t *testing.T, ms *mockStore, buildID string) {
	t.Helper()
	_ = ms.CreateBuild(context.Background(), &store.Build{
		ID:           buildID,
		ProjectID:    btProjectID,
		OwnerUID:     btOwnerUID,
		Status:       "succeeded",
		ArtifactPath: "builds/" + buildID + "/game.ulx",
		CreatedAt:    time.Now().UTC(),
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Routing smoke — wrong method on POST-only route → 405
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildTestRouteSmoke(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	wrongMethods := []string{
		http.MethodGet,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
	}
	for _, method := range wrongMethods {
		t.Run(method, func(t *testing.T) {
			req, _ := http.NewRequest(method,
				ts.URL+"/api/builds/"+btBuildID+"/test", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusMethodNotAllowed {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("%s /api/builds/.../test = %d; want 405\nbody: %s",
					method, resp.StatusCode, truncate(string(body), 200))
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Pre-SSE error paths
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildTestInvalidBuildID(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	tests := []struct {
		name    string
		buildID string
	}{
		{"no b- prefix", "01HXZX5K4MQS0RTB9M7YPB0001"},
		{"project prefix", "p-01HXZX5K4MQS0RTB9M7YPB0001"},
		{"too short", "b-SHORT"},
		{"lowercase ulid", "b-01hxzx5k4mqs0rtb9m7ypb0001"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost,
				ts.URL+"/api/builds/"+tt.buildID+"/test", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("id=%q → status = %d; want 400\nbody: %s",
					tt.buildID, resp.StatusCode, truncate(string(body), 200))
			}
		})
	}
}

func TestBuildTestBuildNotFound(t *testing.T) {
	ts := newTS(t, newMockStore())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 404\nbody: %s", resp.StatusCode, body)
	}
	assertErrorShape(t, resp.Body)
}

func TestBuildTestForbidden(t *testing.T) {
	// Build is owned by a different user; local-mode auth returns "local-dev" → 403.
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	_ = ms.CreateBuild(context.Background(), &store.Build{
		ID:           btBuildID,
		ProjectID:    btProjectID,
		OwnerUID:     "some-other-firebase-uid",
		Status:       "succeeded",
		ArtifactPath: "builds/" + btBuildID + "/game.ulx",
		CreatedAt:    time.Now().UTC(),
	})

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d; want 403\nbody: %s", resp.StatusCode, body)
	}
	assertErrorShape(t, resp.Body)
}

func TestBuildTestNotReady(t *testing.T) {
	// Build must be status=succeeded AND have a non-empty ArtifactPath.
	// Any other combination must return 409 build_not_ready.
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	tests := []struct {
		buildID      string
		status       string
		artifactPath string
	}{
		// Pending / running / failed builds are never ready for testing.
		{"b-00000000000000000000000001", "pending", ""},
		{"b-00000000000000000000000002", "running", "builds/b-0/game.ulx"},
		{"b-00000000000000000000000003", "failed", ""},
		// A succeeded build with no artifact (compiler crashed before producing output).
		{"b-00000000000000000000000004", "succeeded", ""},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("status=%s artifact=%q", tt.status, tt.artifactPath), func(t *testing.T) {
			_ = ms.CreateBuild(context.Background(), &store.Build{
				ID:           tt.buildID,
				ProjectID:    btProjectID,
				OwnerUID:     btOwnerUID,
				Status:       tt.status,
				ArtifactPath: tt.artifactPath,
				CreatedAt:    time.Now().UTC(),
			})

			req, _ := http.NewRequest(http.MethodPost,
				ts.URL+"/api/builds/"+tt.buildID+"/test", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusConflict {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status=%q artifact=%q → status = %d; want 409\nbody: %s",
					tt.status, tt.artifactPath, resp.StatusCode, truncate(string(body), 200))
			}
		})
	}
}

func TestBuildTestDownloadFailure(t *testing.T) {
	// DownloadBlob returns an error before SSE mode is entered.
	// Handler must return a plain JSON 500 error (not SSE).
	ms := newMockStore()
	ms.DownloadBlobErr = fmt.Errorf("simulated GCS network error")
	ts := newTS(t, ms)
	defer ts.Close()

	setupSucceededBuild(t, ms, btBuildID)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 500\nbody: %s", resp.StatusCode, body)
	}
	// Must be application/json (not text/event-stream) — error fires before SSE headers.
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json for pre-SSE error", ct)
	}
	assertErrorShape(t, resp.Body)
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. SSE mode — Content-Type, event shape, won detection, output streaming
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildTestSSEContentType(t *testing.T) {
	// Succeeded build + download succeeds → response is text/event-stream.
	withFakeGlulxe(t, "Game starting...\n")
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	setupSucceededBuild(t, ms, btBuildID)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/builds/.../test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200\nbody: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q; want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q; want no-cache", cc)
	}
}

func TestBuildTestResultEventAlwaysEmitted(t *testing.T) {
	// Even when the game produces no winnable output, a final "result" event
	// must always be emitted. Transcript field must be present.
	withFakeGlulxe(t, "fatal: not a valid Glulx file\n")
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	setupSucceededBuild(t, ms, btBuildID)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/builds/.../test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	events := parseBuildTestSSE(t, resp.Body)
	if len(events) == 0 {
		t.Fatal("no SSE events received")
	}

	resultEv, ok := findResultEvent(events)
	if !ok {
		t.Fatalf("no result event in %d SSE event(s); events: %v", len(events), events)
	}

	// Minimal decode check: result must have type, won, transcript.
	var result struct {
		Type       string `json:"type"`
		Won        bool   `json:"won"`
		Transcript string `json:"transcript"`
	}
	if err := json.Unmarshal(resultEv, &result); err != nil {
		t.Fatalf("decode result event: %v\nevent: %s", err, resultEv)
	}
	if result.Type != "result" {
		t.Errorf("type = %q; want result", result.Type)
	}
	// Transcript field must be present in raw JSON (empty string is a valid value).
	if !strings.Contains(string(resultEv), `"transcript"`) {
		t.Errorf("result event missing transcript field: %s", resultEv)
	}
}

func TestBuildTestNoWinString(t *testing.T) {
	// Output that does NOT contain the win string → won must be false.
	withFakeGlulxe(t, "The Hallway\nYou are in a dark room.\n> ")
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	setupSucceededBuild(t, ms, btBuildID)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/builds/.../test: %v", err)
	}
	defer resp.Body.Close()

	events := parseBuildTestSSE(t, resp.Body)
	resultEv, ok := findResultEvent(events)
	if !ok {
		t.Fatalf("no result event among %d events", len(events))
	}

	var result struct {
		Won        bool   `json:"won"`
		Transcript string `json:"transcript"`
	}
	if err := json.Unmarshal(resultEv, &result); err != nil {
		t.Fatalf("decode result event: %v", err)
	}
	if result.Won {
		t.Errorf("won = true; want false (output has no win string)\ntranscript: %q", result.Transcript)
	}
}

func TestBuildTestWinStringDetected(t *testing.T) {
	// Output that contains "*** You have won. ***" → won must be true.
	// Win detection: presence of a *** banner without a known loss/death phrase.
	withFakeGlulxe(t, "The Trophy Room\n*** You have won. ***\n")
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	setupSucceededBuild(t, ms, btBuildID)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/builds/.../test: %v", err)
	}
	defer resp.Body.Close()

	events := parseBuildTestSSE(t, resp.Body)
	resultEv, ok := findResultEvent(events)
	if !ok {
		t.Fatalf("no result event among %d events", len(events))
	}

	var result struct {
		Won        bool   `json:"won"`
		Transcript string `json:"transcript"`
	}
	if err := json.Unmarshal(resultEv, &result); err != nil {
		t.Fatalf("decode result event: %v", err)
	}
	if !result.Won {
		t.Errorf("won = false; want true (output contains win string)\ntranscript: %q", result.Transcript)
	}
	if !strings.Contains(strings.ToLower(result.Transcript), "you have won") {
		t.Errorf("transcript missing win string: %q", result.Transcript)
	}
}

func TestBuildTestCustomWinBannerDetected(t *testing.T) {
	// Custom *** banner with no loss phrase → won must be true.
	withFakeGlulxe(t, "*** The pumps lock into safe mode before the flood can reach them. ***\nIn that game you scored 2 out of a possible 17\n")
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	setupSucceededBuild(t, ms, btBuildID)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/builds/.../test: %v", err)
	}
	defer resp.Body.Close()

	events := parseBuildTestSSE(t, resp.Body)
	resultEv, ok := findResultEvent(events)
	if !ok {
		t.Fatalf("no result event among %d events", len(events))
	}

	var result struct {
		Won        bool   `json:"won"`
		Transcript string `json:"transcript"`
	}
	if err := json.Unmarshal(resultEv, &result); err != nil {
		t.Fatalf("decode result event: %v", err)
	}
	if !result.Won {
		t.Errorf("won = false; want true (custom win banner with no loss phrase)\ntranscript: %q", result.Transcript)
	}
}

func TestBuildTestCustomLossBannerDetected(t *testing.T) {
	// *** banner containing a loss phrase → won must be false.
	withFakeGlulxe(t, "*** You have died ***\nIn that game you scored 0 out of a possible 17\n")
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	setupSucceededBuild(t, ms, btBuildID)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/builds/.../test: %v", err)
	}
	defer resp.Body.Close()

	events := parseBuildTestSSE(t, resp.Body)
	resultEv, ok := findResultEvent(events)
	if !ok {
		t.Fatalf("no result event among %d events", len(events))
	}

	var result struct {
		Won        bool   `json:"won"`
		Transcript string `json:"transcript"`
	}
	if err := json.Unmarshal(resultEv, &result); err != nil {
		t.Fatalf("decode result event: %v", err)
	}
	if result.Won {
		t.Errorf("won = true; want false (banner contains loss phrase 'you have died')\ntranscript: %q", result.Transcript)
	}
}

func TestBuildTestOutputEventsStreamed(t *testing.T) {
	// Multi-line output → at least one output event before the result.
	// The result event must be exactly one and must be last.
	withFakeGlulxe(t, "Line one\nLine two\nLine three\n")
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	setupSucceededBuild(t, ms, btBuildID)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/builds/.../test: %v", err)
	}
	defer resp.Body.Close()

	events := parseBuildTestSSE(t, resp.Body)

	var outputCount, resultCount int
	for _, ev := range events {
		var m struct{ Type string `json:"type"` }
		if err := json.Unmarshal(ev, &m); err == nil {
			switch m.Type {
			case "output":
				outputCount++
			case "result":
				resultCount++
			}
		}
	}

	if outputCount == 0 {
		t.Errorf("expected ≥1 output events, got 0 (total events: %d)", len(events))
	}
	if resultCount != 1 {
		t.Errorf("expected exactly 1 result event, got %d", resultCount)
	}
}

func TestBuildTestOutputEventShape(t *testing.T) {
	// Each output event must have type="output" and a non-empty "line" string.
	withFakeGlulxe(t, "Hello from the game\n")
	ms := newMockStore()
	ts := newTS(t, ms)
	defer ts.Close()

	setupSucceededBuild(t, ms, btBuildID)

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/builds/"+btBuildID+"/test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/builds/.../test: %v", err)
	}
	defer resp.Body.Close()

	events := parseBuildTestSSE(t, resp.Body)
	for i, ev := range events {
		var m struct {
			Type string `json:"type"`
			Line string `json:"line"`
		}
		if err := json.Unmarshal(ev, &m); err != nil {
			t.Fatalf("event[%d] decode error: %v\npayload: %s", i, err, ev)
		}
		if m.Type == "output" && m.Line == "" {
			t.Errorf("event[%d]: output event has empty line field: %s", i, ev)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. HasTest field on GET /api/projects/{id}/builds/{buildId}
// ─────────────────────────────────────────────────────────────────────────────

func TestGetBuildHasTest(t *testing.T) {
	const (
		projID   = "p-01HXZX5K3Q0RTB9M7YPZN00001"
		buildID  = "b-01HXZX5K4MQS0RTB9M7YPC0001"
		ownerUID = "local-dev"
	)

	tests := []struct {
		name        string
		source      string
		wantHasTest bool
	}{
		{
			name:        "source has Test me with double-quote (canonical)",
			source:      `"The Blue Door" by Alex.` + "\n\n" + `Test me with "go north / go south".` + "\n",
			wantHasTest: true,
		},
		{
			name:        "source has Test Me With mixed case",
			source:      `"Game" by Alex.` + "\n\n" + `Test Me With "go north".` + "\n",
			wantHasTest: true,
		},
		{
			name:        "source has TEST ME WITH uppercase",
			source:      `TEST ME WITH "go north".`,
			wantHasTest: true,
		},
		{
			name:        "source without any test statement",
			source:      `"The Blue Door" by Alex.` + "\n\n" + `The Hallway is a room.` + "\n",
			wantHasTest: false,
		},
		{
			name:        "empty source",
			source:      "",
			wantHasTest: false,
		},
		{
			name: "test me with no following double-quote",
			// "Test me with north" — missing the opening double-quote after "with "
			source:      `Test me with north`,
			wantHasTest: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := newMockStore()
			ts := newTS(t, ms)
			defer ts.Close()

			now := time.Now().UTC()
			_ = ms.CreateProject(context.Background(), &store.Project{
				ID: projID, OwnerUID: ownerUID, Name: "HasTest Project", CreatedAt: now, UpdatedAt: now,
			})
			_ = ms.CreateBuild(context.Background(), &store.Build{
				ID:        buildID,
				ProjectID: projID,
				OwnerUID:  ownerUID,
				Status:    "succeeded",
				CreatedAt: now,
			})
			// Seed the GCS source so GetProjectSource returns it.
			ms.projectSources[projID] = tt.source

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
				ID      string `json:"id"`
				HasTest bool   `json:"hasTest"`
			}
			mustDecode(t, resp.Body, &body)

			if body.HasTest != tt.wantHasTest {
				t.Errorf("hasTest = %v; want %v\nsource: %q", body.HasTest, tt.wantHasTest, tt.source)
			}
		})
	}
}

func TestGetBuildHasTestFalseOnSourceFetchError(t *testing.T) {
	// When GetProjectSource fails, HasTest must default to false.
	// The build response itself must still succeed (200).
	const (
		projID   = "p-01HXZX5K3Q0RTB9M7YPZN00001"
		buildID  = "b-01HXZX5K4MQS0RTB9M7YPC0001"
		ownerUID = "local-dev"
	)

	ms := newMockStore()
	ms.GetProjectSourceErr = fmt.Errorf("simulated GCS source fetch error")
	ts := newTS(t, ms)
	defer ts.Close()

	now := time.Now().UTC()
	_ = ms.CreateProject(context.Background(), &store.Project{
		ID: projID, OwnerUID: ownerUID, Name: "Error Project", CreatedAt: now, UpdatedAt: now,
	})
	_ = ms.CreateBuild(context.Background(), &store.Build{
		ID:        buildID,
		ProjectID: projID,
		OwnerUID:  ownerUID,
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
		t.Fatalf("status = %d; want 200 (source error is non-fatal)\nbody: %s", resp.StatusCode, body)
	}

	var body struct {
		HasTest bool `json:"hasTest"`
	}
	mustDecode(t, resp.Body, &body)

	if body.HasTest {
		t.Error("hasTest = true; want false when source fetch fails (non-fatal)")
	}
}
