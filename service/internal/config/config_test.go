package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func clearEnv() {
	for _, k := range []string{
		"PORT", "APP_VERSION", "ENVIRONMENT",
		"DB_PATH", "STORAGE_PATH", "SESSION_MAX_AGE",
		"IFDB_BASE_URL", "IFDB_USER_AGENT", "IFDB_CACHE_TTL",
		"IFDB_RATE_LIMIT_QPS", "IFDB_RATE_LIMIT_BURST",
		"IFDB_RATE_LIMIT_PER_IP_QPS", "IFDB_RATE_LIMIT_PER_IP_BURST",
		"RUN_SESSION_MAX", "RUN_IDLE_TIMEOUT",
		"DOWNLOAD_SIZE_LIMIT_BYTES", "EXTRACT_SIZE_LIMIT_BYTES",
		"EXTRACT_FILE_LIMIT", "BUILD_TIMEOUT",
	} {
		os.Unsetenv(k)
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("Port: want 8080, got %s", cfg.Port)
	}
	if cfg.Version != "dev" {
		t.Errorf("Version: want dev, got %s", cfg.Version)
	}
	if cfg.Environment != "local" {
		t.Errorf("Environment: want local, got %s", cfg.Environment)
	}
	if cfg.DBPath != "/app/data/db/aifstudio.db" {
		t.Errorf("DBPath: want /app/data/db/aifstudio.db, got %s", cfg.DBPath)
	}
	if cfg.StoragePath != "/app/data/storage" {
		t.Errorf("StoragePath: want /app/data/storage, got %s", cfg.StoragePath)
	}
	if cfg.IFDBCacheTTL != 10*time.Minute {
		t.Errorf("IFDBCacheTTL: want 10m, got %v", cfg.IFDBCacheTTL)
	}
	if cfg.IFDBRateLimitQPS != 5.0 {
		t.Errorf("IFDBRateLimitQPS: want 5.0, got %v", cfg.IFDBRateLimitQPS)
	}
	if cfg.DownloadSizeLimitBytes != 52428800 {
		t.Errorf("DownloadSizeLimitBytes: want 52428800, got %d", cfg.DownloadSizeLimitBytes)
	}
}

func TestLoadFromEnv(t *testing.T) {
	clearEnv()
	os.Setenv("PORT", "9090")
	os.Setenv("APP_VERSION", "1.2.3")
	os.Setenv("ENVIRONMENT", "staging")
	os.Setenv("DB_PATH", "/data/custom.db")
	os.Setenv("STORAGE_PATH", "/data/storage")
	os.Setenv("IFDB_CACHE_TTL", "5m")
	defer clearEnv()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Port != "9090" {
		t.Errorf("Port: want 9090, got %s", cfg.Port)
	}
	if cfg.Version != "1.2.3" {
		t.Errorf("Version: want 1.2.3, got %s", cfg.Version)
	}
	if cfg.Environment != "staging" {
		t.Errorf("Environment: want staging, got %s", cfg.Environment)
	}
	if cfg.DBPath != "/data/custom.db" {
		t.Errorf("DBPath: want /data/custom.db, got %s", cfg.DBPath)
	}
	if cfg.StoragePath != "/data/storage" {
		t.Errorf("StoragePath: want /data/storage, got %s", cfg.StoragePath)
	}
	if cfg.IFDBCacheTTL != 5*time.Minute {
		t.Errorf("IFDBCacheTTL: want 5m, got %v", cfg.IFDBCacheTTL)
	}
}

func TestLoadBadDuration(t *testing.T) {
	clearEnv()
	os.Setenv("IFDB_CACHE_TTL", "notaduration")
	defer clearEnv()

	_, err := Load()
	if err == nil {
		t.Error("expected error for invalid duration, got nil")
	}
}

// TestEnvExampleDrift guards against .env.example and config.go falling out of
// sync. Every KEY= variable declared in .env.example must appear as a string
// literal in config.go (e.g. getEnvOrDefault("KEY", …) or os.Getenv("KEY")).
//
// When you add a new env var:
//  1. Add it to .env.example (with a comment and a default value)
//  2. Add the corresponding getEnvOrDefault / parseDuration / etc. call in config.go
//  3. This test will pass only when both files are in sync.
func TestEnvExampleDrift(t *testing.T) {
	// Working directory during go test is the package directory
	// (service/internal/config/), so ../../../ reaches the repo root.
	envExampleBytes, err := os.ReadFile("../../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	configGoBytes, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	configGoStr := string(configGoBytes)

	for _, line := range strings.Split(string(envExampleBytes), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}

		// Each key must appear as a quoted string literal in config.go.
		needle := fmt.Sprintf("%q", key) // e.g. `"OPENAI_API_KEY"`
		if !strings.Contains(configGoStr, needle) {
			t.Errorf(".env.example key %s is not referenced in config.go "+
				"(expected to find %s) — drift detected", key, needle)
		}
	}
}
