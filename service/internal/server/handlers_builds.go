package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"storycloud/internal/auth"
	"storycloud/internal/build"
)

const buildTestTimeout = 30 * time.Second

// buildTestOutputEvent is sent for each line of game output during a test run.
type buildTestOutputEvent struct {
	Type string `json:"type"`
	Line string `json:"line"`
}

// buildTestResultEvent is the final SSE event sent after the test completes.
type buildTestResultEvent struct {
	Type       string `json:"type"`
	Won        bool   `json:"won"`
	Transcript string `json:"transcript"`
}

// sendBuildTestEvent writes a single unnamed SSE data frame and flushes.
// Unnamed events use only the "data:" prefix; the type is encoded inside
// the JSON payload so the client can distinguish output from result.
func sendBuildTestEvent(w http.ResponseWriter, f http.Flusher, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		slog.Error("sendBuildTestEvent marshal error", "err", err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", payload)
	f.Flush()
}

// handleBuildTest runs the compiled game headlessly via glulxe, sends "test me",
// streams each output line as an SSE output event, then emits a final result event
// indicating win or fail.
//
// Route: POST /api/builds/{buildId}/test
//
// SSE events:
//
//	data: {"type":"output","line":"..."}
//	data: {"type":"result","won":true,"transcript":"..."}
//
// Win detection: presence of a *** banner without a known loss/death phrase.
// Overall timeout: 30 seconds — context cancellation kills glulxe via SIGKILL.
func (s *Server) handleBuildTest(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "auth_required", "authentication required")
		return
	}

	bID := r.PathValue("buildId")
	if !buildIDRE.MatchString(bID) {
		writeError(w, http.StatusBadRequest, "invalid_build_id", "build id format invalid")
		return
	}

	if s.store == nil {
		writeError(w, http.StatusNotFound, "not_found", "build not found")
		return
	}

	b, err := s.store.GetBuild(r.Context(), bID)
	if err != nil {
		slog.Error("store.GetBuild failed", "err", err, "build_id", bID, "handler", "handleBuildTest")
		writeError(w, http.StatusInternalServerError, "internal", "failed to get build")
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "not_found", "build not found")
		return
	}
	if b.OwnerUID != user.UID {
		writeError(w, http.StatusForbidden, "forbidden", "not owner")
		return
	}
	if b.Status != build.StatusSucceeded || b.ArtifactPath == "" {
		writeError(w, http.StatusConflict, "build_not_ready", "build must have status=succeeded to run a test")
		return
	}

	// Create a 30-second timeout context that also cancels when the request is
	// disconnected. The context is wired into exec.CommandContext so glulxe is
	// SIGKILL'd automatically when the deadline fires or the client disconnects.
	ctx, cancel := context.WithTimeout(r.Context(), buildTestTimeout)
	defer cancel()

	// Download the .ulx artifact to a temporary directory before setting SSE
	// headers, so we can still return a proper HTTP error if the download fails.
	tmpDir, err := os.MkdirTemp("", "buildtest-"+bID+"-*")
	if err != nil {
		slog.Error("MkdirTemp failed", "err", err, "build_id", bID)
		writeError(w, http.StatusInternalServerError, "internal", "failed to create workspace")
		return
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck

	artifactLocal := filepath.Join(tmpDir, "game.ulx")
	af, err := os.Create(artifactLocal)
	if err != nil {
		slog.Error("create artifact file failed", "err", err, "build_id", bID)
		writeError(w, http.StatusInternalServerError, "internal", "failed to create artifact file")
		return
	}
	if dlErr := s.store.DownloadBlob(ctx, b.ArtifactPath, af); dlErr != nil {
		af.Close()
		slog.Error("DownloadBlob failed", "err", dlErr, "path", b.ArtifactPath)
		writeError(w, http.StatusInternalServerError, "internal", "failed to download artifact")
		return
	}
	af.Close()

	// Switch to SSE mode. No HTTP error responses are possible after this point.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Extremely unlikely — httptest.ResponseRecorder and real net/http both
		// implement Flusher. Bail without sending anything; client gets EOF.
		slog.Error("ResponseWriter does not implement http.Flusher", "build_id", bID)
		return
	}

	// Spawn glulxe with a context-aware command so it is killed on timeout.
	cmd := exec.CommandContext(ctx, "glulxe", artifactLocal)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.Error("glulxe StdinPipe failed", "err", err, "build_id", bID)
		sendBuildTestEvent(w, flusher, buildTestResultEvent{
			Type: "result", Won: false, Transcript: "failed to spawn interpreter",
		})
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close() //nolint:errcheck
		slog.Error("glulxe StdoutPipe failed", "err", err, "build_id", bID)
		sendBuildTestEvent(w, flusher, buildTestResultEvent{
			Type: "result", Won: false, Transcript: "failed to spawn interpreter",
		})
		return
	}
	// Merge stderr into the stdout pipe so any interpreter errors are captured.
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		stdin.Close() //nolint:errcheck
		slog.Error("glulxe Start failed", "err", err, "build_id", bID)
		sendBuildTestEvent(w, flusher, buildTestResultEvent{
			Type: "result", Won: false,
			Transcript: "failed to start interpreter: " + err.Error(),
		})
		return
	}
	slog.Info("build test started", "build_id", bID, "user_id", user.UID)

	// Goroutine pumps raw stdout/stderr bytes into outCh.
	outCh := make(chan string, 512)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rdErr := stdout.Read(buf)
			if n > 0 {
				outCh <- string(buf[:n])
			}
			if rdErr != nil {
				break
			}
		}
		close(outCh)
	}()

	var transcript strings.Builder

	// collectAndStream reads chunks from outCh until quiescence, the deadline
	// fires, or the request context is done. Each chunk is split on newlines
	// and non-empty lines are sent as SSE output events.
	//
	// Returns true if outCh was closed (process exited naturally or was killed
	// and the goroutine finished), false if we stopped due to quiescence/deadline.
	collectAndStream := func(quiesce, maxWait time.Duration) bool {
		quiesceTimer := time.NewTimer(quiesce)
		deadline := time.NewTimer(maxWait)
		defer quiesceTimer.Stop()
		defer deadline.Stop()

		streamChunk := func(chunk string) {
			transcript.WriteString(chunk)
			for _, line := range strings.Split(chunk, "\n") {
				line = strings.TrimRight(line, "\r")
				if line != "" {
					sendBuildTestEvent(w, flusher, buildTestOutputEvent{
						Type: "output", Line: line,
					})
				}
			}
		}

		for {
			select {
			case chunk, ok := <-outCh:
				if !ok {
					return true // process exited; outCh drained
				}
				// New output arrived — reset the quiescence timer.
				if !quiesceTimer.Stop() {
					select {
					case <-quiesceTimer.C:
					default:
					}
				}
				quiesceTimer.Reset(quiesce)
				streamChunk(chunk)

			case <-quiesceTimer.C:
				return false // no output for quiesce duration
			case <-deadline.C:
				return false // per-phase max wait elapsed
			case <-ctx.Done():
				return false // overall 30-second timeout or client disconnect
			}
		}
	}

	// Phase 1: collect glulxe startup banner until output quiesces for 400 ms,
	// capped at 10 s absolute. After this the game is at its first prompt.
	if done := collectAndStream(400*time.Millisecond, 10*time.Second); done {
		// Process exited during startup (e.g. bad file, unsupported format).
		won := isWinningOutcome(transcript.String())
		sendBuildTestEvent(w, flusher, buildTestResultEvent{
			Type: "result", Won: won, Transcript: transcript.String(),
		})
		cmd.Wait() //nolint:errcheck
		return
	}

	// Phase 2: send the "test me" command to execute the Inform 7 test script.
	if _, wErr := fmt.Fprintf(stdin, "test me\n"); wErr != nil {
		slog.Warn("write 'test me' to glulxe stdin failed", "err", wErr, "build_id", bID)
	}

	// Phase 3: collect test output until it quiesces for 2 s (the test script
	// has finished executing), capped at 25 s so startup + test ≤ 30 s total.
	collectAndStream(2*time.Second, 25*time.Second)

	// Kill glulxe now that we have all the output we need. Closing stdin is
	// a polite hint; Kill() is the guaranteed terminator.
	stdin.Close() //nolint:errcheck
	if cmd.Process != nil {
		cmd.Process.Kill() //nolint:errcheck
	}

	// Drain any chunks still in the channel after the kill (the pump goroutine
	// may buffer a final burst before reading EOF from the killed process).
	for chunk := range outCh {
		transcript.WriteString(chunk)
		for _, line := range strings.Split(chunk, "\n") {
			line = strings.TrimRight(line, "\r")
			if line != "" {
				sendBuildTestEvent(w, flusher, buildTestOutputEvent{
					Type: "output", Line: line,
				})
			}
		}
	}
	cmd.Wait() //nolint:errcheck

	// Emit the final result event.
	full := transcript.String()
	won := isWinningOutcome(full)
	compressed := compressTranscript(full)
	slog.Info("build test finished", "build_id", bID, "won", won,
		"transcript_bytes", len(full), "compressed_bytes", len(compressed))
	sendBuildTestEvent(w, flusher, buildTestResultEvent{
		Type: "result", Won: won, Transcript: compressed,
	})
}

// isWinningOutcome returns true when the transcript ends with a *** banner that
// does not contain a known loss/death phrase.  Inform 7 always emits a
// *** [message] *** line at game-end; the absence of one means the game timed
// out or crashed rather than finishing.
func isWinningOutcome(transcript string) bool {
	lower := strings.ToLower(transcript)
	if !strings.Contains(lower, "***") {
		return false // game never ended cleanly
	}
	lossPatterns := []string{
		"you have died", "you are dead", "you died",
		"you have lost", "you lose", "game over",
	}
	for _, p := range lossPatterns {
		if strings.Contains(lower, p) {
			return false
		}
	}
	return true
}
