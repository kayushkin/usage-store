package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
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

	log.Printf("[usage-store] listening on %s (limits db: %s, tokens db: %s, refresh: %s, spend refresh: %s, anthropic-admin: %s)",
		cfg.ListenAddr, cfg.LimitsDBPath, cfg.TokensDBPath, cfg.RefreshInterval, cfg.SpendRefreshInterval,
		boolStr(sc.Anthropic != nil))
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[usage-store] server error: %v", err)
	}
}

// buildSpendCollectors discovers admin credentials in auth-store rather than
// taking explicit credential IDs. It scans every credential, resolves the
// api_key, and matches by prefix (sk-ant-admin- for Anthropic, sk-admin- for
// OpenAI). The first match wins. Future rotations are picked up because the
// resolve happens lazily on every Fetch.
func buildSpendCollectors(cfg config.Config) server.SpendCollectors {
	out := server.SpendCollectors{}

	if cfg.AuthStoreToken == "" {
		log.Printf("[spend] AUTH_STORE_TOKEN not set; spend collection disabled")
		return out
	}
	asClient := authstore.New(cfg.AuthStoreURL, cfg.AuthStoreToken, "usage-store")

	creds, err := asClient.ListCredentials()
	if err != nil {
		log.Printf("[spend] auth-store list failed: %v", err)
		return out
	}

	anthropicAdminID := findAdminCredID(asClient, creds, "anthropic", "sk-ant-admin-")
	if anthropicAdminID != "" {
		log.Printf("[spend] anthropic admin credential discovered: %s", anthropicAdminID)
		out.Anthropic = spend.NewAnthropicKey(func() (string, error) {
			r, err := asClient.ResolveByID(anthropicAdminID, "fetch:anthropic-spend-per-key")
			if err != nil {
				return "", err
			}
			return r.APIKey, nil
		})
		out.Anthropic.OnUnknownModel = func(model string) {
			log.Printf("[spend] unknown anthropic model %q (priced as $0; add to spend/pricing.go)", model)
		}
	}
	return out
}

// findAdminCredID resolves each credential of the given provider until one
// returns an api_key with the expected admin prefix. Auditing every resolve
// is intentional — auth-store's audit trail will show this discovery sweep.
func findAdminCredID(c *authstore.Client, creds []authstore.CredentialSummary, provider, prefix string) string {
	for _, cred := range creds {
		if cred.Provider != provider || cred.AuthType != "api_key" {
			continue
		}
		r, err := c.ResolveByID(cred.ID, "discover:admin-key")
		if err != nil {
			log.Printf("[spend] resolve %s for discovery failed: %v", cred.ID, err)
			continue
		}
		if strings.HasPrefix(r.APIKey, prefix) {
			return cred.ID
		}
	}
	return ""
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// runRefresher polls each provider on the configured interval and persists snapshots.
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

// runSpendRefresher polls each configured provider's per-key spend on a
// longer cadence (default 1h). Failures are logged but never stop the loop.
func runSpendRefresher(ctx context.Context, s *usagestore.Store, sc server.SpendCollectors, interval time.Duration) {
	if sc.Anthropic == nil {
		return
	}
	tick := func() {
		snaps, err := sc.Anthropic.Fetch()
		if err != nil {
			log.Printf("[spend-refresh] anthropic: %v", err)
			return
		}
		for _, snap := range snaps {
			if err := s.SaveKeySpend(snap); err != nil {
				log.Printf("[spend-refresh] save %s: %v", snap.APIKeyID, err)
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
