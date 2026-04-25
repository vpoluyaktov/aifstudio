package runner

import "time"

// Config holds tunable parameters for the runner package.
// Used by tests via DefaultConfig() and by NewManager callers.
type Config struct {
	DownloadSizeLimitBytes int64
	MaxExtractFiles        int
	SessionMax             time.Duration
	IdleTimeout            time.Duration
	// DrainTimeout is the budget for saving all active sessions during SIGTERM.
	// Cloud Run v2 hardcodes a 10 s grace period; 8 s leaves 2 s for process exit.
	DrainTimeout           time.Duration
	// AbandonedPendingTTL is the age at which orphaned pending runs are swept.
	AbandonedPendingTTL    time.Duration
	// AbandonedSweepInterval is how often the orphan sweep runs.
	AbandonedSweepInterval time.Duration
	// Save/restore timing.
	SaveQuiescenceMs  int
	SaveTimeoutMs     int
	RestoreTimeoutMs  int
}

// DefaultConfig returns production-safe defaults.
func DefaultConfig() Config {
	return Config{
		DownloadSizeLimitBytes: downloadSizeCap,
		MaxExtractFiles:        maxExtractFiles,
		SessionMax:             30 * time.Minute,
		IdleTimeout:            10 * time.Minute,
		DrainTimeout:           8 * time.Second,
		AbandonedPendingTTL:    time.Hour,
		AbandonedSweepInterval: 15 * time.Minute,
		SaveQuiescenceMs:       200,
		SaveTimeoutMs:          1000,
		RestoreTimeoutMs:       2000,
	}
}
