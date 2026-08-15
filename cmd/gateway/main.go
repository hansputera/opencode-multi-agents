package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/hansputera/opencode-multi-agents/internal/handler"
	"github.com/hansputera/opencode-multi-agents/internal/logger"
	"github.com/hansputera/opencode-multi-agents/internal/metrics"
	"github.com/hansputera/opencode-multi-agents/internal/proxy"
)

func main() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger
	log := logger.New(cfg.LogLevel, cfg.LogFormat)
	log.Info().Msg("Starting OpenAI-compatible API gateway")

	// Initialize proxy pool manager
	poolMgr, err := proxy.NewPoolManager(cfg, &log.Logger)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize proxy pool manager")
	}
	defer poolMgr.Close()

	// Start proxy pool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := poolMgr.Start(ctx); err != nil {
		log.Fatal().Err(err).Msg("Failed to start proxy pool")
	}

	// Initialize metrics store
	metricsStore, err := metrics.New(cfg.MetricsDBPath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize metrics store")
	}
	defer metricsStore.Close()

	// Background metric pruning
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := metricsStore.Prune(); err != nil {
					log.Debug().Err(err).Msg("Failed to prune metrics")
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Create HTTP handler
	h := handler.New(cfg, poolMgr, metricsStore, &log.Logger)

	// Create HTTP server
	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      h,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second, // Longer for streaming
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Info().Str("addr", cfg.ListenAddr).Msg("HTTP server listening")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal().Err(err).Msg("HTTP server error")
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info().Msg("Shutting down server...")

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("Server shutdown error")
	}

	log.Info().Msg("Server stopped")
}
