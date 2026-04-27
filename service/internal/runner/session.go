package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"aifstudio/internal/store"
)

// SessionStatus mirrors the run status values from §4.2.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSuspended = "suspended"
	StatusFinished  = "finished"
	StatusFailed    = "failed"
)

// ErrBusy is returned by Command (and Suspend) when a command is already in progress.
var ErrBusy = fmt.Errorf("command already in progress")

// Session manages a single game play session: download → extract → spawn → POST+wait I/O.
// The interpreter process stays alive between HTTP requests; callers issue commands via
// Command() and collect output synchronously.
type Session struct {
	mu sync.Mutex

	run *store.Run
	st  store.Store
	cfg Config

	workDir string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	started bool
	done    chan struct{} // closed when the interpreter process exits

	// commandLock serialises per-session commands. TryLock returns false if busy.
	commandLock sync.Mutex

	// lastActivityAt is updated on every Command() call for idle detection.
	lastActivityAt time.Time

	// unsavable is set permanently when the interpreter rejects in-band saves.
	unsavable bool

	// Mode controls output suppression during save/restore sequences.
	modeMu sync.RWMutex
	mode   InterpreterMode

	// Quiescence tracking: last time the interpreter produced output.
	lastOutputMu sync.Mutex
	lastOutputAt time.Time

	// Save coalescing: at most one save in-flight plus one pending.
	turnCount int
	saveLock  saveLock

	// outCh receives output chunks from the background stdout-reader goroutine.
	// It is non-nil after Start() returns.
	outCh chan string

	// lastOutput stores the most recent output returned to the client so that
	// GET /start can re-send it when reattaching to an already-running session.
	lastOutput string

	// transcript accumulates all interpreter output for upload on session end.
	transcript *transcriptBuffer
}

func newSession(run *store.Run, st store.Store, cfg Config) *Session {
	now := time.Now()
	return &Session{
		run:            run,
		st:             st,
		cfg:            cfg,
		done:           make(chan struct{}),
		lastOutputAt:   now, // prevent immediate quiescence on first save trigger
		lastActivityAt: now,
		transcript:     newTranscriptBuffer(run.ID),
	}
}

// ID returns the run ID for this session.
func (s *Session) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run.ID
}

// Status returns the current run status.
func (s *Session) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run.Status
}

// Done returns a channel that is closed when the interpreter process exits.
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// IsAlive returns true if the interpreter process is still running.
func (s *Session) IsAlive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// IdleFor returns how long since the last Command() call.
func (s *Session) IdleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastActivityAt)
}

// Start downloads the story file, spawns the interpreter, collects startup output,
// and triggers an initial auto-save. Returns the interpreter's startup text.
// Must be called exactly once per session before any Command() calls.
func (s *Session) Start(ctx context.Context) (string, error) {
	storyPath, err := s.resolve(ctx)
	if err != nil {
		if IsUpstreamHTTPError(err) {
			slog.Warn("artifact fetch returned upstream HTTP error", "err", err, "run_id", s.run.ID)
		} else {
			slog.Error("session resolve failed", "err", err, "run_id", s.run.ID)
		}
		s.updateStatus(ctx, StatusFailed, codeFromError(err), err.Error())
		return "", err
	}

	// Detect and spawn interpreter.
	interpreterName := s.run.Interpreter
	var cmd *exec.Cmd
	if interpreterName != "" {
		cmd, err = interpreterCommandByName(interpreterName, storyPath)
	} else {
		var name string
		name, cmd, err = SelectInterpreter(storyPath)
		if err == nil {
			s.mu.Lock()
			s.run.Interpreter = name
			s.mu.Unlock()
		}
	}
	if err != nil {
		slog.Error("interpreter selection failed", "err", err, "run_id", s.run.ID)
		s.updateStatus(ctx, StatusFailed, "unsupported_format", err.Error())
		return "", fmt.Errorf("unsupported_format: %w", err)
	}

	stdin, err2 := cmd.StdinPipe()
	if err2 != nil {
		slog.Error("interpreter stdin pipe failed", "err", err2, "run_id", s.run.ID)
		s.updateStatus(ctx, StatusFailed, "spawn_failed", err2.Error())
		return "", fmt.Errorf("stdin pipe: %w", err2)
	}
	stdout, err3 := cmd.StdoutPipe()
	if err3 != nil {
		stdin.Close() //nolint:errcheck
		slog.Error("interpreter stdout pipe failed", "err", err3, "run_id", s.run.ID)
		s.updateStatus(ctx, StatusFailed, "spawn_failed", err3.Error())
		return "", fmt.Errorf("stdout pipe: %w", err3)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into the stdout pipe

	if startErr := cmd.Start(); startErr != nil {
		stdin.Close()  //nolint:errcheck
		stdout.Close() //nolint:errcheck
		slog.Error("interpreter spawn failed", "err", startErr, "run_id", s.run.ID)
		s.updateStatus(ctx, StatusFailed, "spawn_failed", startErr.Error())
		return "", fmt.Errorf("start: %w", startErr)
	}

	s.mu.Lock()
	s.cmd = cmd
	s.stdin = stdin
	s.started = true
	s.turnCount = s.run.TurnCount
	s.mu.Unlock()

	// Update Firestore: running.
	now := time.Now()
	s.mu.Lock()
	s.run.StartedAt = &now
	s.run.Status = StatusRunning
	s.run.ReconnectCount++
	s.mu.Unlock()
	if s.st != nil {
		s.st.UpdateRun(context.Background(), s.run) //nolint:errcheck
	}

	// Start the background stdout reader before any I/O.
	// Reset quiescence timer so the 200ms window starts from process spawn,
	// not from session creation (download+extract can take hundreds of ms).
	s.outCh = make(chan string, 512)
	s.touchOutput()
	go s.pumpOutputToChannel(stdout)

	// restored is set to true when a save file is successfully loaded.
	// It suppresses the verbose-mode command below (save state already implies it).
	restored := false

	// Restore save state if available.
	if s.run.SavePath == "<unsavable>" {
		s.unsavable = true
	} else if s.run.SavePath != "" {
		localSavePath := s.workDir + "/game.sav"
		if dlErr := s.downloadSaveFile(ctx, s.run.SavePath, localSavePath); dlErr != nil {
			slog.Warn("save download failed — starting fresh", "runId", s.run.ID, "err", dlErr)
		} else {
			restored = true
			s.setMode(ModeRestoring)
			if _, fmtErr := fmt.Fprintf(stdin, "restore\n%s\n", localSavePath); fmtErr != nil {
				slog.Warn("restore command failed", "runId", s.run.ID, "err", fmtErr)
			}
			restoreDur := time.Duration(s.cfg.RestoreTimeoutMs) * time.Millisecond
			select {
			case <-time.After(restoreDur):
			case <-ctx.Done():
				s.setMode(ModeNormal)
				return "", ctx.Err()
			}
			s.setMode(ModeNormal)

			// Discard stale pre-restore output (startup banner + initial room)
			// that accumulated in outCh before the restore completed.
			for len(s.outCh) > 0 {
				<-s.outCh
			}
			// Reset quiescence timer and send `look` so collectUntilQuiescence
			// below captures the player's actual current position, not the banner.
			s.touchOutput()
			if _, fmtErr := fmt.Fprintf(stdin, "look\n"); fmtErr != nil {
				slog.Warn("look after restore failed", "runId", s.run.ID, "err", fmtErr)
			}
		}
	}

	// Collect interpreter startup output (or look response after restore).
	// 60 s gives even very talkative TADS games (which emit one [More] per line
	// of their startup narrative) plenty of room — with the 50 ms per-[More]
	// quiescence window the loop completes in < 5 s for 100 prompts.
	quiesceDur := time.Duration(s.cfg.SaveQuiescenceMs) * time.Millisecond
	output := s.collectUntilQuiescence(ctx, quiesceDur, 60*time.Second)

	// For fresh dfrotz sessions, enable verbose mode so movement commands
	// always print room descriptions. Restored sessions already have verbose
	// set in their saved game state, so skip it there. glulxe and frob have
	// their own verbosity mechanisms and must not receive this command.
	if !restored && s.run.Interpreter == "dfrotz" {
		if _, fmtErr := fmt.Fprintf(stdin, "verbose\n"); fmtErr != nil {
			slog.Warn("verbose command failed", "runId", s.run.ID, "err", fmtErr)
		} else {
			s.touchOutput()
			_ = s.collectUntilQuiescence(ctx, quiesceDur, 3*time.Second) // discard confirmation
		}
	}

	// Async auto-save after startup.
	s.saveLock.triggerAsync(func() {
		if saveErr := s.performAutoSave(context.Background()); saveErr != nil {
			slog.Debug("startup auto-save failed", "runId", s.run.ID, "err", saveErr)
		}
	})

	s.mu.Lock()
	s.lastOutput = output
	s.mu.Unlock()

	return output, nil
}

// LastOutput returns the most recent output produced by Start or Command.
func (s *Session) LastOutput() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOutput
}

// Command sends text to the interpreter and collects the response until quiescence.
// Returns ErrBusy (HTTP 409) if another command is already in progress.
func (s *Session) Command(ctx context.Context, text string) (string, error) {
	if !s.commandLock.TryLock() {
		return "", ErrBusy
	}
	defer s.commandLock.Unlock()

	s.mu.Lock()
	stdin := s.stdin
	s.lastActivityAt = time.Now()
	s.mu.Unlock()

	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if _, err := io.WriteString(stdin, text); err != nil {
		return "", fmt.Errorf("write command: %w", err)
	}
	// Reset the quiescence timer so collectUntilQuiescence waits for the
	// interpreter's response rather than firing immediately on a stale timestamp.
	s.touchOutput()

	quiesceDur := time.Duration(s.cfg.SaveQuiescenceMs) * time.Millisecond
	output := s.collectUntilQuiescence(ctx, quiesceDur, 30*time.Second)

	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		preview := output
		if len(preview) > 200 {
			preview = preview[:200]
		}
		slog.Debug("dfrotz output collected", "runId", s.run.ID, "output_len", len(output), "output_preview", preview)
	}

	s.mu.Lock()
	s.lastOutput = output
	s.mu.Unlock()

	// Async auto-save after each command.
	s.saveLock.triggerAsync(func() {
		if saveErr := s.performAutoSave(context.Background()); saveErr != nil {
			slog.Debug("auto-save failed", "runId", s.run.ID, "err", saveErr)
		}
	})

	return output, nil
}

// Suspend saves the current game state (StatusSuspended). commandLock is held for
// the duration so no Command() can race with the save I/O.
// Returns ErrBusy if a command is already in progress.
func (s *Session) Suspend(ctx context.Context) error {
	if !s.commandLock.TryLock() {
		return ErrBusy
	}
	defer s.commandLock.Unlock()
	return s.performSave(ctx, StatusSuspended)
}

// SuspendAndStop saves the game then kills the interpreter. commandLock is held
// for the entire operation. Used by Manager idle sweep and SIGTERM drain.
func (s *Session) SuspendAndStop(ctx context.Context) error {
	if !s.commandLock.TryLock() {
		// A command is in progress; auto-save already covers the last completed
		// command — just kill.
		s.kill()
		return ErrBusy
	}
	defer s.commandLock.Unlock()
	err := s.performSave(ctx, StatusSuspended)
	s.kill()
	return err
}

// Stop kills the interpreter without saving. Used by handleDeleteRun.
func (s *Session) Stop() {
	s.kill()
}

// collectUntilQuiescence reads from outCh until no output has arrived for dur,
// until maxWait elapses, or until ctx is cancelled.
// It transparently auto-dismisses [MORE] pagination prompts emitted by frob/TADS:
// the prompt is stripped from the output and a newline is sent to continue.
// After each [MORE] dismissal a shorter quiescence window (moreQuiesceDur) is
// used so that high-[MORE]-count startup sequences complete well within maxWait.
func (s *Session) collectUntilQuiescence(ctx context.Context, dur, maxWait time.Duration) string {
	const moreQuiesceDur = 50 * time.Millisecond
	var sb strings.Builder
	deadline := time.Now().Add(maxWait)
	moreDismissals := 0
	recentDismissal := false
	for {
		select {
		case chunk, ok := <-s.outCh:
			if !ok {
				return sb.String()
			}
			sb.WriteString(chunk)
		case <-ctx.Done():
			return sb.String()
		default:
			if time.Now().After(deadline) {
				return sb.String()
			}
			checkDur := dur
			if recentDismissal {
				checkDur = moreQuiesceDur
			}
			if s.quiesced(checkDur) {
				collected := sb.String()
				if moreDismissals < 50 && isMorePrompt(collected) {
					moreDismissals++
					sb.Reset()
					sb.WriteString(stripMoreSuffix(collected))
					s.touchOutput()
					s.mu.Lock()
					stdin := s.stdin
					s.mu.Unlock()
					if stdin != nil {
						io.WriteString(stdin, "\n") //nolint:errcheck
						recentDismissal = true
					}
					continue
				}
				return collected
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// pumpOutputToChannel reads stdout and forwards chunks to outCh.
// Closes outCh and done when the interpreter exits.
func (s *Session) pumpOutputToChannel(stdout io.Reader) {
	buf := make([]byte, 16*1024)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			text := sanitizeUTF8(buf[:n])
			s.touchOutput()
			if s.getMode() == ModeNormal {
				s.transcript.write(text)
				select {
				case s.outCh <- text:
				default:
					// channel full — drop to avoid blocking
				}
			}
		}
		if err != nil {
			break
		}
	}

	// Reap interpreter process.
	if s.cmd != nil {
		s.cmd.Wait() //nolint:errcheck
	}

	// Upload transcript.
	if s.st != nil {
		if transcriptPath, flushErr := s.transcript.flush(context.Background(), s.st); flushErr == nil && transcriptPath != "" {
			s.mu.Lock()
			if s.run.TranscriptPath == "" {
				s.run.TranscriptPath = transcriptPath
			}
			s.mu.Unlock()
		}
	}

	// Mark run finished if still running (natural game-over, not an explicit suspend).
	s.mu.Lock()
	if s.run.Status == StatusRunning {
		now := time.Now()
		s.run.FinishedAt = &now
		s.run.Status = StatusFinished
		if s.st != nil {
			s.st.UpdateRun(context.Background(), s.run) //nolint:errcheck
		}
	}
	s.mu.Unlock()

	close(s.outCh)
	close(s.done)

	if s.workDir != "" {
		os.RemoveAll(s.workDir) //nolint:errcheck
	}
}

// resolve performs resolve → download → extract and returns the story file path.
func (s *Session) resolve(ctx context.Context) (string, error) {
	run := s.run

	workDir := filepath.Join(os.TempDir(), "runs", run.ID+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := os.MkdirAll(workDir, 0700); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	s.mu.Lock()
	s.workDir = workDir
	s.mu.Unlock()

	// Build-sourced runs: fetch the artifact directly from local blob storage
	// (store.DownloadBlob) rather than via an external HTTP URL.
	if run.SourceType == "build" {
		return s.resolveBuildArtifact(ctx, run, workDir)
	}

	// IFDB/URL runs: serve from cached blob storage on repeat plays.
	if run.StoryPath != "" && s.st != nil {
		if localPath, err := s.resolveFromCache(ctx, run, workDir); err == nil {
			return localPath, nil
		}
		// Blob gone — clear and fall through to re-download.
		slog.Warn("story cache miss, re-downloading", "run_id", run.ID, "path", run.StoryPath)
		s.mu.Lock()
		run.StoryPath = ""
		s.mu.Unlock()
	}

	var artifactURL string
	switch run.SourceType {
	case "ifdb":
		artifactURL = run.ArtifactURL
		if artifactURL == "" {
			return "", fmt.Errorf("internal: ifdb run has no artifact URL")
		}
	case "url":
		artifactURL = run.ArtifactURL
	default:
		return "", fmt.Errorf("unknown sourceType: %s", run.SourceType)
	}

	localPath, ext, _, err := downloadArtifact(artifactURL, workDir, s.cfg.DownloadSizeLimitBytes)
	if err != nil && IsUpstreamHTTPError(err) && len(run.CandidateURLs) > 1 {
		// Primary URL returned an upstream HTTP error — try remaining candidates in order.
		for _, candidate := range run.CandidateURLs {
			if candidate == artifactURL {
				continue // already tried
			}
			slog.Info("trying fallback artifact URL", "run_id", run.ID, "url", candidate)
			var ferr error
			localPath, ext, _, ferr = downloadArtifact(candidate, workDir, s.cfg.DownloadSizeLimitBytes)
			if ferr == nil {
				err = nil // success — use this file
				break
			}
			slog.Warn("fallback artifact URL failed", "run_id", run.ID, "url", candidate, "err", ferr)
			err = ferr
			if !IsUpstreamHTTPError(ferr) {
				break // non-upstream error, stop trying
			}
		}
	}
	if err != nil {
		return "", err
	}

	finalPath, err := finalizeArtifact(localPath, ext, workDir)
	if err != nil {
		return "", err
	}

	// Cache the binary so restarts and future plays skip the upstream download.
	if s.st != nil {
		s.cacheStory(ctx, run, finalPath)
	}

	return finalPath, nil
}

// finalizeArtifact resolves the downloaded file to its playable path: returns
// the path as-is for recognised binary extensions, or extracts a ZIP archive.
func finalizeArtifact(localPath, ext, workDir string) (string, error) {
	lowerExt := strings.ToLower(ext)
	if lowerExt == ".zblorb" || lowerExt == ".gblorb" || IsBlorb(localPath) {
		return localPath, nil
	}
	ifBinaries := map[string]bool{
		".z3": true, ".z4": true, ".z5": true, ".z6": true,
		".z7": true, ".z8": true, ".ulx": true,
		".gam": true, ".t3": true,
	}
	if ifBinaries[lowerExt] {
		return localPath, nil
	}
	if lowerExt == ".zip" {
		return extractZip(localPath, workDir)
	}
	return "", fmt.Errorf("unsupported_format: unknown extension %s", ext)
}

// resolveFromCache downloads the cached story binary from blob storage to workDir.
func (s *Session) resolveFromCache(ctx context.Context, run *store.Run, workDir string) (string, error) {
	ext := filepath.Ext(run.StoryPath)
	if ext == "" {
		ext = ".bin"
	}
	localPath := filepath.Join(workDir, "story"+ext)
	f, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("story cache: create local file: %w", err)
	}
	dlErr := s.st.DownloadBlob(ctx, run.StoryPath, f)
	f.Close()
	if dlErr != nil {
		return "", dlErr
	}
	slog.Info("story served from cache", "run_id", run.ID, "blob", run.StoryPath)
	return localPath, nil
}

// cacheStory uploads the resolved story binary to blob storage and persists the
// path on the run record so subsequent starts skip the upstream download.
func (s *Session) cacheStory(ctx context.Context, run *store.Run, localPath string) {
	ext := filepath.Ext(localPath)
	if ext == "" {
		ext = ".bin"
	}
	blobPath := "sessions/" + run.ID + "/story" + ext
	f, err := os.Open(localPath)
	if err != nil {
		slog.Warn("story cache: open failed", "run_id", run.ID, "err", err)
		return
	}
	defer f.Close()
	if err := s.st.UploadBlob(ctx, blobPath, "application/octet-stream", f); err != nil {
		slog.Warn("story cache: upload failed", "run_id", run.ID, "err", err)
		return
	}
	s.mu.Lock()
	run.StoryPath = blobPath
	s.mu.Unlock()
	if err := s.st.UpdateRun(ctx, run); err != nil {
		slog.Warn("story cache: UpdateRun failed", "run_id", run.ID, "err", err)
		return
	}
	slog.Info("story cached", "run_id", run.ID, "blob", blobPath)
}

// downloadSaveFile downloads the GCS save file to localPath.
func (s *Session) downloadSaveFile(ctx context.Context, gcsPath, localPath string) error {
	if s.st == nil {
		return fmt.Errorf("no store available")
	}
	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create save file: %w", err)
	}
	defer f.Close()
	return s.st.DownloadBlob(ctx, gcsPath, f)
}

// resolveBuildArtifact downloads the compiled game artifact for a build-sourced
// run directly from local blob storage (DownloadBlob) and returns the local file
// path. This avoids any HTTP round-trip for locally stored builds.
func (s *Session) resolveBuildArtifact(ctx context.Context, run *store.Run, workDir string) (string, error) {
	if s.st == nil {
		return "", fmt.Errorf("build run requires store: no store configured")
	}
	if run.BuildID == "" {
		return "", fmt.Errorf("build run has no BuildID")
	}
	b, err := s.st.GetBuild(ctx, run.BuildID)
	if err != nil {
		return "", fmt.Errorf("load build %s: %w", run.BuildID, err)
	}
	if b == nil {
		return "", fmt.Errorf("build %s not found", run.BuildID)
	}
	if b.ArtifactPath == "" {
		return "", fmt.Errorf("build %s has no artifact", run.BuildID)
	}
	ext := ".ulx"
	if b.ArtifactFormat != "" {
		ext = "." + b.ArtifactFormat
	}
	localPath := filepath.Join(workDir, "artifact"+ext)
	f, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("create artifact file: %w", err)
	}
	dlErr := s.st.DownloadBlob(ctx, b.ArtifactPath, f)
	f.Close()
	if dlErr != nil {
		return "", fmt.Errorf("download build artifact: %w", dlErr)
	}
	return localPath, nil
}

// kill closes stdin, sends SIGTERM, and waits for the interpreter to exit via done.
// Falls back to SIGKILL after 5 s.
func (s *Session) kill() {
	s.mu.Lock()
	cmd := s.cmd
	started := s.started
	stdin := s.stdin
	s.mu.Unlock()

	if !started || cmd == nil || cmd.Process == nil {
		return
	}

	if stdin != nil {
		stdin.Close() //nolint:errcheck
	}

	cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		cmd.Process.Signal(syscall.SIGKILL) //nolint:errcheck
		<-s.done
	}
}

func (s *Session) updateStatus(ctx context.Context, status, errCode, errMsg string) {
	s.mu.Lock()
	s.run.Status = status
	s.run.ErrorCode = errCode
	s.run.ErrorMessage = errMsg
	s.mu.Unlock()
	if s.st != nil {
		s.st.UpdateRun(ctx, s.run) //nolint:errcheck
	}
}

func codeFromError(err error) string {
	if err == nil {
		return "internal"
	}
	switch {
	case IsUpstreamHTTPError(err):
		return "upstream_unavailable"
	case IsDownloadTooLarge(err):
		return "download_too_large"
	case IsArchiveTooManyFiles(err):
		return "archive_too_many_files"
	case IsArchiveEmpty(err):
		return "archive_empty"
	case IsArchiveInvalidPath(err):
		return "archive_invalid_path"
	case IsUnsupportedFormat(err):
		return "unsupported_format"
	case strings.Contains(err.Error(), "archive_too_large"):
		return "archive_too_large"
	case strings.Contains(err.Error(), "spawn_failed"):
		return "spawn_failed"
	default:
		return "internal"
	}
}
