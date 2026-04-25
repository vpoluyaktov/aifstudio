package config

import (
	"os"
	"testing"
	"time"
)

func clearEnv() {
	for _, k := range []string{
		"PORT", "APP_VERSION", "ENVIRONMENT", "GCP_PROJECT_ID",
		"FIRESTORE_DATABASE_NAME", "GCS_BUCKET",
		"FIREBASE_WEB_API_KEY", "FIREBASE_AUTH_DOMAIN",
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
	if cfg.ProjectID != "" {
		t.Errorf("ProjectID: want empty, got %s", cfg.ProjectID)
	}
	if cfg.FirestoreDatabaseName != "storycloud" {
		t.Errorf("FirestoreDatabaseName: want storycloud, got %s", cfg.FirestoreDatabaseName)
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
	os.Setenv("GCP_PROJECT_ID", "my-project")
	os.Setenv("FIRESTORE_DATABASE_NAME", "my-db")
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
	if cfg.ProjectID != "my-project" {
		t.Errorf("ProjectID: want my-project, got %s", cfg.ProjectID)
	}
	if cfg.FirestoreDatabaseName != "my-db" {
		t.Errorf("FirestoreDatabaseName: want my-db, got %s", cfg.FirestoreDatabaseName)
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
