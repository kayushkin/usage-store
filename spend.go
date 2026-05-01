package usagestore

import (
	"database/sql"
	"fmt"
	"time"
)

// SpendWindow names match the periods exposed on the Usage page.
const (
	SpendWindowDay   = "day"   // last 24 hours
	SpendWindowWeek  = "week"  // last 7 days
	SpendWindowMonth = "month" // last 30 days
)

// SpendSnapshot is one period-summed cost figure pulled from a provider's
// admin cost API. Spend is monetary and period-bounded; rate-limit windows
// (LimitWindow / ProviderLimits) are utilization-of-cap and instantaneous.
// They share storage to nothing; keep them as distinct shapes.
type SpendSnapshot struct {
	Provider    string  `json:"provider"`     // "anthropic", "openai"
	Window      string  `json:"window"`       // SpendWindow*
	PeriodStart int64   `json:"period_start"` // unix seconds
	PeriodEnd   int64   `json:"period_end"`   // unix seconds
	TotalUSD    float64 `json:"total_usd"`
	Currency    string  `json:"currency"` // typically "USD"
	FetchedAt   int64   `json:"fetched_at"`
	Source      string  `json:"source"` // "api"
}

// migrateSpend creates the spend_snapshots table.
func (s *Store) migrateSpend() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS spend_snapshots (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			fetched_at   INTEGER NOT NULL,
			provider     TEXT NOT NULL,
			window       TEXT NOT NULL,
			period_start INTEGER NOT NULL,
			period_end   INTEGER NOT NULL,
			total_usd    REAL NOT NULL,
			currency     TEXT NOT NULL,
			source       TEXT NOT NULL,
			raw_json     TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_spend_snapshots_lookup
			ON spend_snapshots(provider, window, fetched_at DESC);
	`)
	return err
}

// SaveSpend persists one SpendSnapshot. FetchedAt defaults to now if zero.
func (s *Store) SaveSpend(snap SpendSnapshot, rawJSON []byte) error {
	if snap.Provider == "" || snap.Window == "" {
		return fmt.Errorf("provider and window are required")
	}
	if snap.FetchedAt == 0 {
		snap.FetchedAt = time.Now().Unix()
	}
	if snap.Currency == "" {
		snap.Currency = "USD"
	}
	_, err := s.db.Exec(`
		INSERT INTO spend_snapshots
			(fetched_at, provider, window, period_start, period_end, total_usd, currency, source, raw_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.FetchedAt, snap.Provider, snap.Window,
		snap.PeriodStart, snap.PeriodEnd, snap.TotalUSD,
		snap.Currency, snap.Source, string(rawJSON),
	)
	return err
}

// LatestSpend returns the most recent snapshot for (provider, window).
// Returns (nil, nil) when no snapshot exists.
func (s *Store) LatestSpend(provider, window string) (*SpendSnapshot, error) {
	row := s.db.QueryRow(`
		SELECT fetched_at, period_start, period_end, total_usd, currency, source
		FROM spend_snapshots
		WHERE provider = ? AND window = ?
		ORDER BY fetched_at DESC LIMIT 1`,
		provider, window,
	)
	out := &SpendSnapshot{Provider: provider, Window: window}
	err := row.Scan(&out.FetchedAt, &out.PeriodStart, &out.PeriodEnd, &out.TotalUSD, &out.Currency, &out.Source)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}
