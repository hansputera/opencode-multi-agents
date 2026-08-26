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
	"github.com/rs/zerolog"
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

	// Build proxy manager(s): external SOCKS5 + multi-account ProtonVPN
	mgr, cleanupMgr, err := buildManager(cfg, &log.Logger)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize proxy manager")
	}
	defer cleanupMgr()

	// Initialize proxy pool manager
	poolMgr := proxy.NewPoolManagerWithManager(mgr, cfg, &log.Logger)
	defer poolMgr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy pool population in the background: a fresh VPN container
	// can take tens of seconds to boot, so the pool fills up asynchronously
	// while the HTTP server below is already accepting connections. Starting
	// the server only after the whole pool is ready would leave the gateway
	// port unlistened for minutes and fail readiness/health checks.
	go func() {
		if err := poolMgr.Start(ctx); err != nil {
			log.Error().Err(err).Msg("Failed to start proxy pool")
		}
	}()

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

	// Initialize config store (runtime accounts, proxies, settings)
	cfgStore, err := config.NewConfigStore("data/config.db")
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize config store")
	}
	defer cfgStore.Close()

	// One-time migration from .env on first boot
	if err := cfgStore.SeedFromConfig(cfg); err != nil {
		log.Warn().Err(err).Msg("Failed to seed config from .env")
	}

	// Create HTTP handler
	h := handler.New(cfg, cfgStore, poolMgr, metricsStore, &log.Logger)

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

// buildManager constructs the appropriate Manager based on config:
//   - Single VPN account + no external proxies → DockerManager directly
//   - Multiple sources → CompositeManager wrapping all
//
// The cleanup function closes all managers.
func buildManager(cfg *config.Config, log *zerolog.Logger) (proxy.Manager, func(), error) {
	accounts := cfg.ParseProtonVPNAccounts()
	socks5Addrs := cfg.ParseSOCKS5Addrs()

	// Build VPN managers (one per account)
	var vpnMgrs []proxy.Manager
	var cleanups []func()
	for i, acct := range accounts {
		accountCfg := *cfg // copy
		accountCfg.ProtonVPNUsername = acct.Username
		accountCfg.ProtonVPNPassword = acct.Password
		// Isolate store per account (first account uses base path)
		if i > 0 {
			accountCfg.ProtonVPNStorePath = fmt.Sprintf("%s_%d", cfg.ProtonVPNStorePath, i)
		}
		// Only the first account uses session cookies
		if i > 0 {
			accountCfg.ProtonVPNSessionCookies = ""
		}

		mgr, err := proxy.NewDockerManager(&accountCfg, log)
		if err != nil {
			// Close previously created managers
			for _, c := range cleanups {
				c()
			}
			return nil, nil, fmt.Errorf("failed to create VPN manager for account %d (%s): %w", i, acct.Username, err)
		}
		vpnMgrs = append(vpnMgrs, mgr)
		cleanups = append(cleanups, func() { mgr.Close() })
	}

	// Build external proxy manager
	var extMgr *proxy.ExternalProxyManager
	if len(socks5Addrs) > 0 {
		extMgr = proxy.NewExternalProxyManager(socks5Addrs, cfg.ProtonVPNIPCheckURL, log)
		cleanups = append(cleanups, func() { extMgr.Close() })
	}

	// Single manager? Return it directly.
	if len(vpnMgrs) == 1 && extMgr == nil {
		return vpnMgrs[0], cleanups[0], nil
	}
	if len(vpnMgrs) == 0 && extMgr != nil {
		return extMgr, cleanups[0], nil
	}

	// Multiple sources → composite
	composite := proxy.NewCompositeManager(extMgr, vpnMgrs, log)
	cleanup := func() {
		for _, c := range cleanups {
			c()
		}
	}
	return composite, cleanup, nil
}
