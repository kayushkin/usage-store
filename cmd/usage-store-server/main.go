package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	usagestore "github.com/kayushkin/usage-store"
	"github.com/kayushkin/usage-store/anthropic"
	"github.com/kayushkin/usage-store/codex"
	"github.com/kayushkin/usage-store/internal/authstore"
	"github.com/kayushkin/usage-store/internal/config"
	"github.com/kayushkin/usage-store/internal/server"
	"github.com/kayushkin/usage-store/spend"
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

	sc := buildSpendCollectors(cfg)

	srv := server.New(store, cfg.TokensDBPath, ant, cx, sc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runRefresher(ctx, store, ant, cx, cfg.RefreshInterval)
	go runSpendRefresher(ctx, store, sc, cfg.SpendRefreshInterval)

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

	log.Printf("[usage-store] listening on %s (limits db: %s, tokens db: %s, refresh: %s, spend refresh: %s, anthropic-admin: %s, openai-admin: %s)",
		cfg.ListenAddr, cfg.LimitsDBPath, cfg.TokensDBPath, cfg.RefreshInterval, cfg.SpendRefreshInterval,
		boolStr(sc.Anthropic != nil), boolStr(sc.OpenAI != nil))
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[usage-store] server error: %v", err)
	}
}

// buildSpendCollectors wires each spend collector to a lazy auth-store lookup
// against the configured credential ID. A missing credential ID disables that
// provider's spend collector — the rest of usage-store still runs.
func buildSpendCollectors(cfg config.Config) server.SpendCollectors {
	out := server.SpendCollectors{}
	if cfg.AnthropicAdminCredID == "" && cfg.OpenAIAdminCredID == "" {
		return out
	}

	asClient := authstore.New(cfg.AuthStoreURL, cfg.AuthStoreToken, "usage-store")

	if cfg.AnthropicAdminCredID != "" {
		credID := cfg.AnthropicAdminCredID
		out.Anthropic = spend.NewAnthropic(func() (string, error) {
			r, err := asClient.ResolveByID(credID, "fetch:anthropic-cost-report")
			if err != nil {
				return "", err
			}
			return r.APIKey, nil
		})
	}
	if cfg.OpenAIAdminCredID != "" {
		credID := cfg.OpenAIAdminCredID
		out.OpenAI = spend.NewOpenAI(func() (string, error) {
			r, err := asClient.ResolveByID(credID, "fetch:openai-costs")
			if err != nil {
				return "", err
			}
			return r.APIKey, nil
		})
	}
	return out
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
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

// runSpendRefresher polls each configured provider's admin cost API on a
// longer cadence (default 1h). Spend rollups don't change minute-to-minute and
// the admin endpoints are rate-limited per docs. Failures are logged but do
// not stop the loop — credential rotation or transient 5xx shouldn't take the
// service down.
func runSpendRefresher(ctx context.Context, s *usagestore.Store, sc server.SpendCollectors, interval time.Duration) {
	if sc.Anthropic == nil && sc.OpenAI == nil {
		return
	}
	tick := func() {
		if sc.Anthropic != nil {
			if err := saveSpend(s, "anthropic", sc.Anthropic.Fetch); err != nil {
				log.Printf("[spend-refresh] anthropic: %v", err)
			}
		}
		if sc.OpenAI != nil {
			if err := saveSpend(s, "openai", sc.OpenAI.Fetch); err != nil {
				log.Printf("[spend-refresh] openai: %v", err)
			}
		}
	}

	tick()
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

func saveSpend(s *usagestore.Store, provider string, fetch func() ([]usagestore.SpendSnapshot, [][]byte, error)) error {
	snaps, raws, err := fetch()
	if err != nil {
		return err
	}
	for i, snap := range snaps {
		if err := s.SaveSpend(snap, raws[i]); err != nil {
			return fmt.Errorf("save %s/%s: %w", provider, snap.Window, err)
		}
	}
	return nil
}
