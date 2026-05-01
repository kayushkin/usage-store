package usagestore

import (
	"database/sql"
	"fmt"
	"time"
)

// SpendWindow names match the periods exposed on the Usage page. Snapshots
// store all three windows on every row so the UI can render in one read.
const (
	SpendWindowDay   = "day"   // last 24h
	SpendWindowWeek  = "week"  // last 7d
	SpendWindowMonth = "month" // last 30d
)

// KeySpend is one snapshot of cost-attributed-to-a-single-API-key. Cost is
// derived locally from the provider's usage_report (token counts) multiplied
// by per-model pricing — cost_report can't filter by api_key_id and would
// also lump in subscription-imputed value, so we don't use it.
type KeySpend struct {
	Provider     string  `json:"provider"`       // "anthropic", "openai"
	APIKeyID     string  `json:"api_key_id"`     // provider-side ID, e.g. apikey_01...
	APIKeyName   string  `json:"api_key_name"`   // user-given name on the console
	APIKeyHint   string  `json:"api_key_hint"`   // partial key, e.g. sk-ant-api03-J7f...OgAA
	APIKeyStatus string  `json:"api_key_status"` // "active", "archived", etc.
	TotalUSD24h  float64 `json:"total_usd_24h"`
	TotalUSD7d   float64 `json:"total_usd_7d"`
	TotalUSD30d  float64 `json:"total_usd_30d"`
	FetchedAt    int64   `json:"fetched_at"`
	RawJSON      string  `json:"raw_json,omitempty"` // populated only on /raw endpoint
}

// migrateSpend creates the spend_keys table.
func (s *Store) migrateSpend() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS spend_keys (
			fetched_at     INTEGER NOT NULL,
			provider       TEXT NOT NULL,
			api_key_id     TEXT NOT NULL,
			api_key_name   TEXT NOT NULL DEFAULT '',
			api_key_hint   TEXT NOT NULL DEFAULT '',
			api_key_status TEXT NOT NULL DEFAULT '',
			total_usd_24h  REAL NOT NULL,
			total_usd_7d   REAL NOT NULL,
			total_usd_30d  REAL NOT NULL,
			raw_json       TEXT,
			PRIMARY KEY (provider, api_key_id, fetched_at)
		);
		CREATE INDEX IF NOT EXISTS idx_spend_keys_latest
			ON spend_keys(provider, api_key_id, fetched_at DESC);
	`)
	return err
}

// SaveKeySpend persists one snapshot. FetchedAt defaults to now.
func (s *Store) SaveKeySpend(snap KeySpend) error {
	if snap.Provider == "" || snap.APIKeyID == "" {
		return fmt.Errorf("provider and api_key_id are required")
	}
	if snap.FetchedAt == 0 {
		snap.FetchedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(`
		INSERT INTO spend_keys
			(fetched_at, provider, api_key_id, api_key_name, api_key_hint, api_key_status,
			 total_usd_24h, total_usd_7d, total_usd_30d, raw_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.FetchedAt, snap.Provider, snap.APIKeyID, snap.APIKeyName, snap.APIKeyHint, snap.APIKeyStatus,
		snap.TotalUSD24h, snap.TotalUSD7d, snap.TotalUSD30d, snap.RawJSON,
	)
	return err
}

// LatestKeySpend returns the most recent snapshot for every key seen for a
// provider. Keys archived on the provider side are still returned (we want
// to show their final balance).
func (s *Store) LatestKeySpend(provider string) ([]KeySpend, error) {
	rows, err := s.db.Query(`
		SELECT s.provider, s.api_key_id, s.api_key_name, s.api_key_hint, s.api_key_status,
		       s.total_usd_24h, s.total_usd_7d, s.total_usd_30d, s.fetched_at
		FROM spend_keys s
		JOIN (
		    SELECT provider, api_key_id, MAX(fetched_at) AS fetched_at
		    FROM spend_keys WHERE provider = ?
		    GROUP BY provider, api_key_id
		) latest USING (provider, api_key_id, fetched_at)
		ORDER BY s.total_usd_30d DESC`,
		provider,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []KeySpend{}
	for rows.Next() {
		var k KeySpend
		if err := rows.Scan(&k.Provider, &k.APIKeyID, &k.APIKeyName, &k.APIKeyHint, &k.APIKeyStatus,
			&k.TotalUSD24h, &k.TotalUSD7d, &k.TotalUSD30d, &k.FetchedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// LatestKeySpendRaw fetches the raw_json column for one (provider, api_key_id)
// at its most recent snapshot. Empty string if not found.
func (s *Store) LatestKeySpendRaw(provider, apiKeyID string) (string, error) {
	var raw sql.NullString
	err := s.db.QueryRow(`
		SELECT raw_json FROM spend_keys
		WHERE provider = ? AND api_key_id = ?
		ORDER BY fetched_at DESC LIMIT 1`,
		provider, apiKeyID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return raw.String, nil
}
