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

	"storycloud/internal/auth"
	"storycloud/internal/build"
	"storycloud/internal/config"
	"storycloud/internal/ifdb"
	"storycloud/internal/runner"
	"storycloud/internal/server"
	"storycloud/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Structured JSON logging.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	ctx := context.Background()

	// --- Store (Firestore + GCS) ---
	var st store.Store
	if cfg.ProjectID != "" {
		fs, err := store.NewFirestoreStore(ctx, cfg.ProjectID, cfg.FirestoreDatabaseName, cfg.GCSBucket)
		if err != nil {
			log.Fatalf("Failed to initialize Firestore: %v", err)
		}
		defer fs.Close() //nolint:errcheck
		st = fs
		slog.Info("firestore connected", "project", cfg.ProjectID, "db", cfg.FirestoreDatabaseName)
	} else {
		slog.Warn("GCP_PROJECT_ID not set — running without Firestore/GCS")
	}

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

	// Warm up IFDB in-memory cache from Firestore on cold start.
	if st != nil {
		if games, err := st.ListFreshCachedGames(ctx, time.Now()); err == nil {
			for _, g := range games {
				ifdbClient.SeedCache(g.TUID, g.Payload, g.ExpiresAt)
			}
			slog.Info("IFDB cache warmed", "entries", len(games))
		}
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

	// --- Auth verifier ---
	authVerifier, err := auth.NewVerifier(ctx, cfg.ProjectID)
	if err != nil {
		log.Fatalf("auth verifier: %v", err)
	}

	// --- HTTP server ---
	srv := server.New(cfg, st, ifdbClient, runMgr, buildMgr, authVerifier)
	handler := srv.SetupRoutes()

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%s", cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	slog.Info("starting storycloud", "version", cfg.Version, "env", cfg.Environment, "port", cfg.Port)

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
