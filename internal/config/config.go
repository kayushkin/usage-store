package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	ListenAddr      string
	LimitsDBPath    string        // limit_snapshots DB (canonical for usage-store)
	TokensDBPath    string        // token aggregation DB (read-only; currently model-store's)
	RefreshInterval time.Duration // background refresh cadence (limits)
	CodexMaxAge     time.Duration // when a codex rollout snapshot becomes stale

	// Spend (admin-API cost reporting). Empty cred IDs disable that provider's
	// spend collector — the rest of the service still runs.
	AuthStoreURL          string
	AuthStoreToken        string
	AnthropicAdminCredID  string
	OpenAIAdminCredID     string
	SpendRefreshInterval  time.Duration
}

func Load() Config {
	return Config{
		ListenAddr:           envOr("USAGE_STORE_LISTEN_ADDR", ":8185"),
		LimitsDBPath:         expandHome(envOr("USAGE_STORE_DB", "~/.config/usage-store/usage.db")),
		TokensDBPath:         expandHome(envOr("USAGE_STORE_TOKENS_DB", "~/.config/model-store/store.db")),
		RefreshInterval:      envDur("USAGE_STORE_REFRESH_INTERVAL", 60*time.Second),
		CodexMaxAge:          envDur("USAGE_STORE_CODEX_MAX_AGE", 2*time.Hour),
		AuthStoreURL:         envOr("AUTH_STORE_URL", "http://127.0.0.1:8303"),
		AuthStoreToken:       os.Getenv("AUTH_STORE_TOKEN"),
		AnthropicAdminCredID: os.Getenv("USAGE_STORE_ANTHROPIC_ADMIN_CRED_ID"),
		OpenAIAdminCredID:    os.Getenv("USAGE_STORE_OPENAI_ADMIN_CRED_ID"),
		SpendRefreshInterval: envDur("USAGE_STORE_SPEND_REFRESH_INTERVAL", time.Hour),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDur(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
