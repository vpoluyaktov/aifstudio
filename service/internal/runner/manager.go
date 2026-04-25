package runner

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"storycloud/internal/store"
)

// Manager holds active run sessions and performs periodic cleanup.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session

	st  store.Store
	cfg Config
}

// NewManager creates a new Manager and starts the background cleanup loop.
func NewManager(st store.Store, cfg Config) *Manager {
	m := &Manager{
		sessions: make(map[string]*Session),
		st:       st,
		cfg:      cfg,
	}
	go m.cleanupLoop()
	return m
}

// CreateSession registers a new session for run r, replacing any existing session
// for the same run ID.
func (m *Manager) CreateSession(r *store.Run) *Session {
	s := newSession(r, m.st, m.cfg)
	m.mu.Lock()
	m.sessions[r.ID] = s
	m.mu.Unlock()
	return s
}

// GetSession returns the session for runID, or nil if none is registered.
func (m *Manager) GetSession(runID string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[runID]
}

// RemoveSession removes the session for runID from the map.
func (m *Manager) RemoveSession(runID string) {
	m.mu.Lock()
	delete(m.sessions, runID)
	m.mu.Unlock()
}

// Drain saves all active sessions and kills their interpreters.
// Intended for SIGTERM handling; ctx should carry the drain deadline.
func (m *Manager) Drain(ctx context.Context) {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	if len(sessions) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *Session) {
			defer wg.Done()
			s.SaveAndClose(ctx)
		}(s)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("drain complete", "sessions", len(sessions))
	case <-ctx.Done():
		slog.Warn("drain timed out", "sessions", len(sessions))
	}
}

// cleanupLoop sweeps finished sessions, idles out stale sessions, and deletes
// abandoned pending runs.
func (m *Manager) cleanupLoop() {
	sweepTicker := time.NewTicker(m.cfg.AbandonedSweepInterval)
	idleTicker := time.NewTicker(time.Minute)
	defer sweepTicker.Stop()
	defer idleTicker.Stop()

	for {
		select {
		case <-sweepTicker.C:
			m.sweepFinished()
			if m.st != nil {
				before := time.Now().Add(-m.cfg.AbandonedPendingTTL)
				if n, err := m.st.DeleteAbandonedPendingRuns(context.Background(), before); err != nil {
					slog.Warn("abandoned sweep failed", "err", err)
				} else if n > 0 {
					slog.Info("abandoned pending runs swept", "count", n)
				}
			}
		case <-idleTicker.C:
			m.sweepIdle()
		}
	}
}

// sweepFinished removes sessions whose interpreter has exited.
func (m *Manager) sweepFinished() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		select {
		case <-s.done:
			if m.sessions[id] == s {
				delete(m.sessions, id)
			}
		default:
		}
	}
}

// sweepIdle suspends and removes sessions that have been idle longer than IdleTimeout.
func (m *Manager) sweepIdle() {
	m.mu.Lock()
	var idle []*Session
	for _, s := range m.sessions {
		if s.IsAlive() && s.IdleFor() > m.cfg.IdleTimeout {
			idle = append(idle, s)
		}
	}
	m.mu.Unlock()

	for _, s := range idle {
		slog.Info("idle session suspending", "runId", s.ID(), "idle", s.IdleFor().Round(time.Second))
		if err := s.SuspendAndStop(context.Background()); err != nil && err != ErrBusy {
			slog.Warn("idle suspend failed", "runId", s.ID(), "err", err)
		}
		m.mu.Lock()
		if m.sessions[s.ID()] == s {
			delete(m.sessions, s.ID())
		}
		m.mu.Unlock()
	}
}
