package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// InterpreterMode controls output suppression during save/restore sequences.
// The pumpOutputToChannel goroutine checks this mode and suppresses output
// while save/restore prompts are being exchanged with the interpreter.
type InterpreterMode int

const (
	// ModeNormal forwards all interpreter output to outCh.
	ModeNormal InterpreterMode = iota
	// ModeSaving suppresses interpreter output during the in-band save dialog.
	ModeSaving
	// ModeRestoring suppresses interpreter output during the in-band restore dialog.
	ModeRestoring
)

// saveLock serialises save operations within a single session.
// At most one save is in flight at a time; a pending flag coalesces additional
// requests so the latest state is always captured (§A.5.5.1).
type saveLock struct {
	mu        sync.Mutex
	inFlight  bool
	pending   bool
}

// triggerAsync registers that a save is needed.  If one is already in flight
// the pending flag is set so another save runs immediately after.
// runSave is called in a new goroutine when a save should start.
func (sl *saveLock) triggerAsync(runSave func()) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if sl.inFlight {
		sl.pending = true
		return
	}
	sl.inFlight = true
	go func() {
		runSave()
		sl.mu.Lock()
		sl.inFlight = false
		more := sl.pending
		sl.pending = false
		sl.mu.Unlock()
		if more {
			sl.mu.Lock()
			sl.inFlight = true
			sl.mu.Unlock()
			go runSave()
		}
	}()
}

// performAutoSave is the entry point for background auto-saves triggered via
// triggerAsync. It acquires commandLock before entering the save dialog so that
// save I/O (setMode + stdin writes) cannot race with a concurrent Command() call.
//
// If commandLock is held by an active Command(), TryLock returns false and the
// save is deferred — triggerAsync's pending flag ensures another save fires once
// the current command finishes.
//
// Suspend() and SuspendAndStop() do NOT use this path; they hold commandLock
// themselves before calling performSave directly.
func (s *Session) performAutoSave(ctx context.Context) error {
	if !s.commandLock.TryLock() {
		slog.Debug("auto-save deferred — command in progress", "runId", s.run.ID)
		return nil
	}
	defer s.commandLock.Unlock()
	return s.performSave(ctx, StatusRunning)
}

// performSave executes one complete save cycle for the session:
//  1. Wait for interpreter quiescence (no output for quiescenceDur).
//  2. Switch to ModeSaving, write save commands, wait for save to complete.
//  3. Verify the local save file.
//  4. Upload to GCS.
//  5. Update Firestore.
//
// runStatus is the status value to write to Firestore after save ("running" for
// auto-saves, "suspended" for disconnect/SIGTERM saves).
// Returns an error on failure; the session continues regardless (next trigger retries).
// Callers must hold commandLock for the duration (auto-saves via performAutoSave;
// suspends via Suspend/SuspendAndStop).
func (s *Session) performSave(ctx context.Context, runStatus string) error {
	s.mu.Lock()
	if !s.started || s.cmd == nil || s.workDir == "" || s.stdin == nil {
		s.mu.Unlock()
		return fmt.Errorf("interpreter not started")
	}
	stdin := s.stdin
	workDir := s.workDir
	run := s.run
	s.mu.Unlock()

	if s.unsavable {
		return nil // unsavable games never save
	}

	quiesceDur := time.Duration(s.cfg.SaveQuiescenceMs) * time.Millisecond
	saveDur := time.Duration(s.cfg.SaveTimeoutMs) * time.Millisecond
	quiesceDeadline := 2 * time.Second

	// 1. Wait for quiescence.
	if !s.waitForQuiescence(ctx, quiesceDur, quiesceDeadline) {
		return fmt.Errorf("quiescence timeout — interpreter busy")
	}

	// 2. Enter SAVING mode and issue in-band save commands.
	s.setMode(ModeSaving)
	savePath := workDir + "/game.sav"
	if _, err := fmt.Fprintf(stdin, "save\n%s\ny\n", savePath); err != nil {
		s.setMode(ModeNormal)
		return fmt.Errorf("write save command: %w", err)
	}

	// Wait for save to complete (timeout-based FSM exit).
	select {
	case <-time.After(saveDur):
	case <-ctx.Done():
		s.setMode(ModeNormal)
		return ctx.Err()
	}
	s.setMode(ModeNormal)

	// 3. Verify save file.
	info, err := os.Stat(savePath)
	if err != nil || info.Size() == 0 {
		// Check if this is the very first save attempt for the run.
		s.mu.Lock()
		neverSaved := run.SavePath == ""
		s.mu.Unlock()
		if neverSaved {
			// Interpreter does not support saves — mark permanently unsavable.
			s.unsavable = true
			s.mu.Lock()
			run.SavePath = "<unsavable>"
			s.mu.Unlock()
			if updateErr := s.st.UpdateRun(context.Background(), run); updateErr != nil {
				slog.Warn("failed to persist unsavable sentinel", "runId", run.ID, "err", updateErr)
			}
			return nil // not a transient error; future saves skipped via s.unsavable
		}
		return fmt.Errorf("save file missing or empty after save command")
	}

	// 4. Upload to GCS.
	gcsPath := "sessions/" + run.ID + "/game.sav"
	f, err := os.Open(savePath)
	if err != nil {
		return fmt.Errorf("open save file: %w", err)
	}
	if uploadErr := s.st.UploadBlob(ctx, gcsPath, "application/octet-stream", f); uploadErr != nil {
		f.Close()
		return fmt.Errorf("upload save: %w", uploadErr)
	}
	f.Close()

	// 5. Update Firestore.
	now := time.Now().UTC()
	s.mu.Lock()
	s.turnCount++
	run.SavePath = gcsPath
	run.LastSaveAt = &now
	run.LastActiveAt = &now
	run.TurnCount = s.turnCount
	run.Status = runStatus
	s.mu.Unlock()

	if updateErr := s.st.UpdateRun(context.Background(), run); updateErr != nil {
		slog.Warn("failed to update run after save", "runId", run.ID, "err", updateErr)
	}

	return nil
}

// waitForQuiescence polls until no interpreter output has arrived for dur,
// or until timeout elapses. Returns true if quiescence was achieved.
func (s *Session) waitForQuiescence(ctx context.Context, dur, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.quiesced(dur) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(20 * time.Millisecond):
		}
	}
	return false
}

// quiesced returns true if no output has arrived from the interpreter for at least dur.
func (s *Session) quiesced(dur time.Duration) bool {
	s.lastOutputMu.Lock()
	last := s.lastOutputAt
	s.lastOutputMu.Unlock()
	return time.Since(last) >= dur
}

// touchOutput records that the interpreter produced output (for quiescence detection).
func (s *Session) touchOutput() {
	s.lastOutputMu.Lock()
	s.lastOutputAt = time.Now()
	s.lastOutputMu.Unlock()
}

// getMode returns the current InterpreterMode under the read lock.
func (s *Session) getMode() InterpreterMode {
	s.modeMu.RLock()
	defer s.modeMu.RUnlock()
	return s.mode
}

// setMode updates the current InterpreterMode under the write lock.
func (s *Session) setMode(m InterpreterMode) {
	s.modeMu.Lock()
	s.mode = m
	s.modeMu.Unlock()
}

// SaveAndClose performs a synchronous save then kills the interpreter.
// Used by Manager.Drain during SIGTERM.
func (s *Session) SaveAndClose(ctx context.Context) {
	if err := s.SuspendAndStop(ctx); err != nil && err != ErrBusy {
		slog.Warn("SaveAndClose: save failed", "runId", s.run.ID, "err", err)
	}
}
