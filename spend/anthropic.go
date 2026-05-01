// Package spend collects monetary spend (not utilization) from provider
// admin cost APIs. Auth is an admin-class API key resolved through auth-store
// — regular project keys cannot read these endpoints.
package spend

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	usagestore "github.com/kayushkin/usage-store"
)

// AnthropicCostReportURL is the admin-API endpoint that reports per-bucket
// spend. Requires a sk-ant-admin-... key sent as x-api-key.
const AnthropicCostReportURL = "https://api.anthropic.com/v1/organizations/cost_report"

// AnthropicCollector pulls spend totals from the Anthropic admin API. The
// admin key is fetched lazily on each Fetch via APIKeyFn so rotation in
// auth-store is picked up without a process restart.
type AnthropicCollector struct {
	APIKeyFn   func() (string, error)
	HTTPClient *http.Client
}

func NewAnthropic(apiKeyFn func() (string, error)) *AnthropicCollector {
	return &AnthropicCollector{
		APIKeyFn:   apiKeyFn,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// rawAnthropicResponse covers the shape we sum over. Anthropic's cost_report
// returns one bucket per period; each bucket has a list of `results` entries
// broken out by workspace / model / token type. We sum every entry's `amount`.
//
// The API has been adding fields over time — we only bind what we need; any
// new fields are ignored. If `amount` ever changes shape we want to crash
// loudly rather than silently report $0.
type rawAnthropicResponse struct {
	Data []struct {
		StartingAt string                  `json:"starting_at"`
		EndingAt   string                  `json:"ending_at"`
		Results    []rawAnthropicCostEntry `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

// rawAnthropicCostEntry binds amount loosely because Anthropic has shipped
// it both as a bare number and as a {value, currency} object across docs
// versions. parse() handles both.
type rawAnthropicCostEntry struct {
	Amount   json.RawMessage `json:"amount"`
	Currency string          `json:"currency"`
}

// Fetch returns one snapshot per (day, week, month) window.
func (c *AnthropicCollector) Fetch() ([]usagestore.SpendSnapshot, [][]byte, error) {
	now := time.Now().UTC()
	out := make([]usagestore.SpendSnapshot, 0, 3)
	raws := make([][]byte, 0, 3)

	windows := []struct {
		name string
		dur  time.Duration
	}{
		{usagestore.SpendWindowDay, 24 * time.Hour},
		{usagestore.SpendWindowWeek, 7 * 24 * time.Hour},
		{usagestore.SpendWindowMonth, 30 * 24 * time.Hour},
	}

	for _, w := range windows {
		start := now.Add(-w.dur)
		snap, raw, err := c.fetchOne(w.name, start, now)
		if err != nil {
			return nil, nil, fmt.Errorf("anthropic spend %s: %w", w.name, err)
		}
		out = append(out, *snap)
		raws = append(raws, raw)
	}
	return out, raws, nil
}

func (c *AnthropicCollector) fetchOne(window string, start, end time.Time) (*usagestore.SpendSnapshot, []byte, error) {
	apiKey, err := c.APIKeyFn()
	if err != nil {
		return nil, nil, err
	}

	// Walk pagination until has_more is false. In practice cost_report only
	// returns one bucket per request when bucket_width covers the full range,
	// but the loop is cheap insurance against future API changes.
	total := 0.0
	currency := "USD"
	var firstRaw []byte
	page := ""

	for {
		req, err := http.NewRequest("GET", AnthropicCostReportURL, nil)
		if err != nil {
			return nil, nil, err
		}
		q := req.URL.Query()
		q.Set("starting_at", start.Format(time.RFC3339))
		q.Set("ending_at", end.Format(time.RFC3339))
		q.Set("bucket_width", "1d")
		if page != "" {
			q.Set("page", page)
		}
		req.URL.RawQuery = q.Encode()
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("http: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, body, fmt.Errorf("anthropic returned %d: %s", resp.StatusCode, string(body))
		}
		if firstRaw == nil {
			firstRaw = body
		}

		var raw rawAnthropicResponse
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, body, fmt.Errorf("parse: %w", err)
		}

		for _, bucket := range raw.Data {
			for _, r := range bucket.Results {
				v, err := parseAmount(r.Amount)
				if err != nil {
					return nil, body, fmt.Errorf("parse amount %s: %w", string(r.Amount), err)
				}
				total += v
				if r.Currency != "" {
					currency = r.Currency
				}
			}
		}

		if !raw.HasMore || raw.NextPage == "" {
			break
		}
		page = raw.NextPage
	}

	return &usagestore.SpendSnapshot{
		Provider:    "anthropic",
		Window:      window,
		PeriodStart: start.Unix(),
		PeriodEnd:   end.Unix(),
		TotalUSD:    total,
		Currency:    currency,
		FetchedAt:   time.Now().Unix(),
		Source:      "api",
	}, firstRaw, nil
}

// parseAmount handles both shapes Anthropic has shipped:
//   - bare number / numeric string ("0.123" or 0.123)
//   - object form ({"value": "0.123", "currency": "USD"})
func parseAmount(raw json.RawMessage) (float64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	// Try numeric / numeric-string.
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil {
		return num, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strconv.ParseFloat(s, 64)
	}
	// Object form.
	var obj struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && len(obj.Value) > 0 {
		return parseAmount(obj.Value)
	}
	return 0, fmt.Errorf("unrecognised amount shape: %s", string(raw))
}
