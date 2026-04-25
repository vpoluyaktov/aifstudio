package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	Port        string
	Version     string
	Environment string

	// SQLite + local filesystem (replaces Firestore + GCS).
	DBPath        string        // DB_PATH: path to aifstudio.db (default /app/data/db/aifstudio.db)
	StoragePath   string        // STORAGE_PATH: local blob root (default /app/data/storage)
	SessionMaxAge time.Duration // SESSION_MAX_AGE: session cookie lifetime (default 720h = 30 days)

	// SourceSignedURLTTL is retained for handler compatibility — value is hardcoded (no env var).
	SourceSignedURLTTL time.Duration

	IFDBBaseURL             string
	IFDBUserAgent           string
	IFDBCacheTTL            time.Duration
	IFDBRateLimitQPS        float64
	IFDBRateLimitBurst      int
	IFDBRateLimitPerIPQPS   float64
	IFDBRateLimitPerIPBurst int

	RunSessionMax  time.Duration
	RunIdleTimeout time.Duration

	DownloadSizeLimitBytes int64
	ExtractSizeLimitBytes  int64
	ExtractFileLimit       int

	BuildTimeout time.Duration

	// Session persistence (§A.8)
	SaveQuiescenceMs       int           // SAVE_QUIESCENCE_MS: stdout silence before save
	SaveTimeoutMs          int           // SAVE_TIMEOUT_MS: max time to wait for save
	RestoreTimeoutMs       int           // RESTORE_TIMEOUT_MS: max time to wait for restore
	ShutdownDrainTimeout   time.Duration // SHUTDOWN_DRAIN_TIMEOUT: budget for SIGTERM save drain (default 8s; Cloud Run v2 grace period is hardcoded 10s)
	HistoryDefaultLimit    int           // HISTORY_DEFAULT_LIMIT: default limit for by-user query
	AbandonedPendingTTL    time.Duration // ABANDONED_PENDING_TTL: age at which orphaned pending runs are swept
	AbandonedSweepInterval time.Duration // ABANDONED_SWEEP_INTERVAL: cadence of the orphan sweep

	// AI / OpenAI (§15 of ARCHITECTURE_AI_CREATE.md)
	OpenAIAPIKey              string        // OPENAI_API_KEY: empty disables AI endpoints (503)
	OpenAIModel               string        // OPENAI_MODEL: default "gpt-5.2"
	OpenAIBaseURL             string        // OPENAI_BASE_URL: default "https://api.openai.com/v1"
	OpenAITimeout             time.Duration // OPENAI_TIMEOUT: total stream timeout (default 300s)
	AIMaxTurnsPerProject      int           // AI_MAX_TURNS_PER_PROJECT: hard cap per project (default 200)
	AIRateLimitPerUserQPS     float64       // AI_RATE_LIMIT_PER_USER_QPS: token-bucket rate (default 0.2)
	AIRateLimitPerUserBurst   int           // AI_RATE_LIMIT_PER_USER_BURST: token-bucket burst (default 3)
	AIMaxDescriptionChars     int           // AI_MAX_DESCRIPTION_CHARS: description cap (default 2000)
	AIMaxMessageChars         int           // AI_MAX_MESSAGE_CHARS: per-message cap (default 16000; test transcripts need headroom)
}

// Load reads environment variables and returns a Config. Parsing failures are
// returned as an error; callers should exit non-zero on error.
func Load() (*Config, error) {
	port := getEnvOrDefault("PORT", "8080")
	version := getEnvOrDefault("APP_VERSION", "dev")
	env := getEnvOrDefault("ENVIRONMENT", "local")

	dbPath := getEnvOrDefault("DB_PATH", "/app/data/db/aifstudio.db")
	storagePath := getEnvOrDefault("STORAGE_PATH", "/app/data/storage")
	sessionMaxAge, err := parseDuration("SESSION_MAX_AGE", "720h")
	if err != nil {
		return nil, err
	}

	ifdbBaseURL := getEnvOrDefault("IFDB_BASE_URL", "https://ifdb.org")
	ifdbUserAgent := getEnvOrDefault("IFDB_USER_AGENT", "StoryCloud/0.1 (contact: vpoluyaktov@gmail.com)")

	ifdbCacheTTL, err := parseDuration("IFDB_CACHE_TTL", "10m")
	if err != nil {
		return nil, err
	}

	ifdbRateLimitQPS, err := parseFloat("IFDB_RATE_LIMIT_QPS", 5.0)
	if err != nil {
		return nil, err
	}
	ifdbRateLimitBurst, err := parseInt("IFDB_RATE_LIMIT_BURST", 10)
	if err != nil {
		return nil, err
	}
	ifdbRateLimitPerIPQPS, err := parseFloat("IFDB_RATE_LIMIT_PER_IP_QPS", 1.0)
	if err != nil {
		return nil, err
	}
	ifdbRateLimitPerIPBurst, err := parseInt("IFDB_RATE_LIMIT_PER_IP_BURST", 3)
	if err != nil {
		return nil, err
	}

	runSessionMax, err := parseDuration("RUN_SESSION_MAX", "30m")
	if err != nil {
		return nil, err
	}
	runIdleTimeout, err := parseDuration("RUN_IDLE_TIMEOUT", "10m")
	if err != nil {
		return nil, err
	}

	downloadSizeLimit, err := parseInt64("DOWNLOAD_SIZE_LIMIT_BYTES", 52428800)
	if err != nil {
		return nil, err
	}
	extractSizeLimit, err := parseInt64("EXTRACT_SIZE_LIMIT_BYTES", 104857600)
	if err != nil {
		return nil, err
	}
	extractFileLimit, err := parseInt("EXTRACT_FILE_LIMIT", 100)
	if err != nil {
		return nil, err
	}

	buildTimeout, err := parseDuration("BUILD_TIMEOUT", "5m")
	if err != nil {
		return nil, err
	}

	saveQuiescenceMs, err := parseInt("SAVE_QUIESCENCE_MS", 200)
	if err != nil {
		return nil, err
	}
	saveTimeoutMs, err := parseInt("SAVE_TIMEOUT_MS", 1000)
	if err != nil {
		return nil, err
	}
	restoreTimeoutMs, err := parseInt("RESTORE_TIMEOUT_MS", 2000)
	if err != nil {
		return nil, err
	}
	shutdownDrainTimeout, err := parseDuration("SHUTDOWN_DRAIN_TIMEOUT", "8s")
	if err != nil {
		return nil, err
	}
	historyDefaultLimit, err := parseInt("HISTORY_DEFAULT_LIMIT", 20)
	if err != nil {
		return nil, err
	}
	abandonedPendingTTL, err := parseDuration("ABANDONED_PENDING_TTL", "1h")
	if err != nil {
		return nil, err
	}
	abandonedSweepInterval, err := parseDuration("ABANDONED_SWEEP_INTERVAL", "15m")
	if err != nil {
		return nil, err
	}

	openAIAPIKey := os.Getenv("OPENAI_API_KEY")
	openAIModel := getEnvOrDefault("OPENAI_MODEL", "gpt-5.2")
	openAIBaseURL := getEnvOrDefault("OPENAI_BASE_URL", "https://api.openai.com/v1")
	openAITimeout, err := parseDuration("OPENAI_TIMEOUT", "300s")
	if err != nil {
		return nil, err
	}
	aiMaxTurns, err := parseInt("AI_MAX_TURNS_PER_PROJECT", 200)
	if err != nil {
		return nil, err
	}
	aiRateLimitPerUserQPS, err := parseFloat("AI_RATE_LIMIT_PER_USER_QPS", 0.2)
	if err != nil {
		return nil, err
	}
	aiRateLimitPerUserBurst, err := parseInt("AI_RATE_LIMIT_PER_USER_BURST", 3)
	if err != nil {
		return nil, err
	}
	aiMaxDescriptionChars, err := parseInt("AI_MAX_DESCRIPTION_CHARS", 2000)
	if err != nil {
		return nil, err
	}
	aiMaxMessageChars, err := parseInt("AI_MAX_MESSAGE_CHARS", 16000)
	if err != nil {
		return nil, err
	}

	return &Config{
		Port:        port,
		Version:     version,
		Environment: env,

		DBPath:             dbPath,
		StoragePath:        storagePath,
		SessionMaxAge:      sessionMaxAge,
		SourceSignedURLTTL: 15 * time.Minute,

		IFDBBaseURL:             ifdbBaseURL,
		IFDBUserAgent:           ifdbUserAgent,
		IFDBCacheTTL:            ifdbCacheTTL,
		IFDBRateLimitQPS:        ifdbRateLimitQPS,
		IFDBRateLimitBurst:      ifdbRateLimitBurst,
		IFDBRateLimitPerIPQPS:   ifdbRateLimitPerIPQPS,
		IFDBRateLimitPerIPBurst: ifdbRateLimitPerIPBurst,

		RunSessionMax:  runSessionMax,
		RunIdleTimeout: runIdleTimeout,

		DownloadSizeLimitBytes: downloadSizeLimit,
		ExtractSizeLimitBytes:  extractSizeLimit,
		ExtractFileLimit:       extractFileLimit,

		BuildTimeout: buildTimeout,

		SaveQuiescenceMs:       saveQuiescenceMs,
		SaveTimeoutMs:          saveTimeoutMs,
		RestoreTimeoutMs:       restoreTimeoutMs,
		ShutdownDrainTimeout:   shutdownDrainTimeout,
		HistoryDefaultLimit:    historyDefaultLimit,
		AbandonedPendingTTL:    abandonedPendingTTL,
		AbandonedSweepInterval: abandonedSweepInterval,

		OpenAIAPIKey:            openAIAPIKey,
		OpenAIModel:             openAIModel,
		OpenAIBaseURL:           openAIBaseURL,
		OpenAITimeout:           openAITimeout,
		AIMaxTurnsPerProject:    aiMaxTurns,
		AIRateLimitPerUserQPS:   aiRateLimitPerUserQPS,
		AIRateLimitPerUserBurst: aiRateLimitPerUserBurst,
		AIMaxDescriptionChars:   aiMaxDescriptionChars,
		AIMaxMessageChars:       aiMaxMessageChars,
	}, nil
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseDuration(key, def string) (time.Duration, error) {
	s := getEnvOrDefault(key, def)
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", key, s, err)
	}
	return d, nil
}

func parseFloat(key string, def float64) (float64, error) {
	s := os.Getenv(key)
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", key, s, err)
	}
	return v, nil
}

func parseInt(key string, def int) (int, error) {
	s := os.Getenv(key)
	if s == "" {
		return def, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", key, s, err)
	}
	return v, nil
}

func parseInt64(key string, def int64) (int64, error) {
	s := os.Getenv(key)
	if s == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", key, s, err)
	}
	return v, nil
}
