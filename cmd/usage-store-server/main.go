package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	usagestore "github.com/kayushkin/usage-store"
	"github.com/kayushkin/usage-store/anthropic"
	"github.com/kayushkin/usage-store/codex"
	"github.com/kayushkin/usage-store/internal/config"
	"github.com/kayushkin/usage-store/internal/server"
)

func main() {
	cfg := config.Load()

	store, err := usagestore.Open(cfg.LimitsDBPath)
	if err != nil {
		log.Fatalf("[usage-store] open store: %v", err)
	}
	defer store.Close()

	ant := anthropic.New()
	cx := codex.New()
	cx.MaxAge = cfg.CodexMaxAge

	srv := server.New(store, cfg.TokensDBPath, ant, cx)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runRefresher(ctx, store, ant, cx, cfg.RefreshInterval)

	httpSrv := &http.Server{Addr: cfg.ListenAddr, Handler: srv}

	// Graceful shutdown on SIGINT / SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("[usage-store] shutdown signal received")
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		httpSrv.Shutdown(shutdownCtx)
		cancel()
	}()

	log.Printf("[usage-store] listening on %s (limits db: %s, tokens db: %s, refresh: %s)",
		cfg.ListenAddr, cfg.LimitsDBPath, cfg.TokensDBPath, cfg.RefreshInterval)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[usage-store] server error: %v", err)
	}
}

// runRefresher polls each provider on the configured interval and persists snapshots.
//
// Anthropic: hits the OAuth usage API (cheap; no quota cost). Logs but doesn't
// crash on failure — we want the service to stay up if creds expire.
//
// Codex: rescans local rollout JSONLs. We never probe (stale-skip per design).
// If the latest rollout is older than CodexMaxAge the snapshot is still saved
// but the IsStale flag will be true on read, so consumers can decide what to do.
func runRefresher(ctx context.Context, s *usagestore.Store, ant *anthropic.Collector, cx *codex.Reader, interval time.Duration) {
	tick := func() {
		if snap, raw, err := ant.Fetch(); err != nil {
			log.Printf("[refresh] anthropic: %v", err)
		} else if err := s.SaveLimits(*snap, raw); err != nil {
			log.Printf("[refresh] anthropic save: %v", err)
		}
		if snap, raw, err := cx.Latest(); err != nil {
			log.Printf("[refresh] codex: %v", err)
		} else if snap != nil {
			if err := s.SaveLimits(*snap, raw); err != nil {
				log.Printf("[refresh] codex save: %v", err)
			}
		}
	}

	tick() // fire immediately on start
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
