package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aifstudio/internal/auth"
	"aifstudio/internal/build"
	"aifstudio/internal/config"
	"aifstudio/internal/ifdb"
	"aifstudio/internal/runner"
	"aifstudio/internal/server"
	"aifstudio/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Structured JSON logging.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx := context.Background()

	// --- Store (SQLite + local filesystem) ---
	blob := store.NewLocalBlobStore(cfg.StoragePath)
	st, err := store.NewSQLiteStore(ctx, cfg.DBPath, blob)
	if err != nil {
		log.Fatalf("Failed to initialize SQLite store: %v", err)
	}
	defer st.Close() //nolint:errcheck
	slog.Info("sqlite connected", "path", cfg.DBPath, "storage", cfg.StoragePath)

	// --- IFDB client ---
	ifdbClient := ifdb.NewClient(ifdb.ClientOptions{
		BaseURL:     cfg.IFDBBaseURL,
		UserAgent:   cfg.IFDBUserAgent,
		CacheTTL:    cfg.IFDBCacheTTL,
		GlobalQPS:   cfg.IFDBRateLimitQPS,
		GlobalBurst: cfg.IFDBRateLimitBurst,
		PerIPQPS:    cfg.IFDBRateLimitPerIPQPS,
		PerIPBurst:  cfg.IFDBRateLimitPerIPBurst,
	})

	// Warm up IFDB in-memory cache from SQLite on cold start.
	if games, err := st.ListFreshCachedGames(ctx, time.Now()); err == nil {
		for _, g := range games {
			ifdbClient.SeedCache(g.TUID, g.Payload, g.ExpiresAt)
		}
		slog.Info("IFDB cache warmed", "entries", len(games))
	}

	// --- Runner ---
	runMgr := runner.NewManager(st, runner.Config{
		DownloadSizeLimitBytes: cfg.DownloadSizeLimitBytes,
		MaxExtractFiles:        cfg.ExtractFileLimit,
		SessionMax:             cfg.RunSessionMax,
		IdleTimeout:            cfg.RunIdleTimeout,
		DrainTimeout:           cfg.ShutdownDrainTimeout,
		AbandonedPendingTTL:    cfg.AbandonedPendingTTL,
		AbandonedSweepInterval: cfg.AbandonedSweepInterval,
		SaveQuiescenceMs:       cfg.SaveQuiescenceMs,
		SaveTimeoutMs:          cfg.SaveTimeoutMs,
		RestoreTimeoutMs:       cfg.RestoreTimeoutMs,
	})

	// --- Build manager ---
	buildMgr := build.NewManager(st, cfg.BuildTimeout)

	// --- Session auth (replaces Firebase Auth) ---
	sessionAuth := auth.NewSessionAuth(st, cfg.SessionMaxAge)

	// --- Expired session sweep (background goroutine) ---
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			n, err := st.DeleteExpiredSessions(context.Background(), time.Now())
			if err != nil {
				slog.Warn("expired session sweep failed", "err", err)
			} else if n > 0 {
				slog.Info("expired sessions swept", "count", n)
			}
		}
	}()

	// --- HTTP server ---
	srv := server.New(cfg, st, ifdbClient, runMgr, buildMgr, sessionAuth)
	handler := srv.SetupRoutes()

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%s", cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	slog.Info("starting aifstudio", "version", cfg.Version, "env", cfg.Environment, "port", cfg.Port)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down server")
	drainCtx, drainCancel := context.WithTimeout(context.Background(), cfg.ShutdownDrainTimeout)
	defer drainCancel()
	runMgr.Drain(drainCtx)

	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
	slog.Info("server stopped")
}
