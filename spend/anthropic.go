// Package spend computes per-API-key spend for Claude API keys. The dollar
// figure is derived locally from token counts × per-model pricing, not from
// the org cost_report (which can't filter by api_key_id and lumps in
// imputed Max-subscription value).
package spend

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	usagestore "github.com/kayushkin/usage-store"
)

const (
	anthropicAPIKeysURL  = "https://api.anthropic.com/v1/organizations/api_keys"
	anthropicUsageURL    = "https://api.anthropic.com/v1/organizations/usage_report/messages"
	anthropicAPIVersion  = "2023-06-01"
)

// AnthropicKeyCollector lists every API key on the org and computes spend
// per key from usage_report/messages. The admin key is fetched lazily so
// rotation is picked up without a process restart.
type AnthropicKeyCollector struct {
	APIKeyFn   func() (string, error)
	HTTPClient *http.Client
	// Logger receives one line per unknown model, so the user notices when a
	// new model needs adding to the pricing table.
	OnUnknownModel func(model string)
}

func NewAnthropicKey(apiKeyFn func() (string, error)) *AnthropicKeyCollector {
	return &AnthropicKeyCollector{
		APIKeyFn:   apiKeyFn,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// rawAPIKey is the slice of /v1/organizations/api_keys we keep.
type rawAPIKey struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	PartialKeyHint string `json:"partial_key_hint"`
	Status         string `json:"status"`
}

type rawAPIKeysResponse struct {
	Data    []rawAPIKey `json:"data"`
	HasMore bool        `json:"has_more"`
	LastID  string      `json:"last_id"`
}

// rawUsageBucket matches the documented shape of usage_report/messages with
// group_by=api_key_id,model. Cache breakdown is in cache_creation; we want
// both the 5m and 1h buckets so cache-write cost is correct.
type rawUsageBucket struct {
	StartingAt string             `json:"starting_at"`
	EndingAt   string             `json:"ending_at"`
	Results    []rawUsageResult   `json:"results"`
}

type rawCacheCreation struct {
	Ephemeral5mInput int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInput int64 `json:"ephemeral_1h_input_tokens"`
}

type rawUsageResult struct {
	UncachedInput       int64            `json:"uncached_input_tokens"`
	CacheRead           int64            `json:"cache_read_input_tokens"`
	CacheCreation       rawCacheCreation `json:"cache_creation"`
	OutputTokens        int64            `json:"output_tokens"`
	APIKeyID            string           `json:"api_key_id"`
	Model               string           `json:"model"`
}

type rawUsageResponse struct {
	Data     []rawUsageBucket `json:"data"`
	HasMore  bool             `json:"has_more"`
	NextPage string           `json:"next_page"`
}

// Fetch returns one KeySpend per active+archived API key on the org. Rather
// than make N requests (one per key), we make one big usage_report request
// for the full 30-day window grouped by (api_key_id, model) and partition
// the results locally. That keeps us inside Anthropic's tight admin-API
// rate limits while still letting us compute every window.
func (c *AnthropicKeyCollector) Fetch() ([]usagestore.KeySpend, error) {
	keys, err := c.listKeys()
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	if len(keys) == 0 {
		return nil, nil
	}

	end := time.Now().UTC().Truncate(24 * time.Hour) // today 00:00 UTC
	start := end.AddDate(0, 0, -30)

	usage, rawByKey, err := c.fetchUsage(start, end)
	if err != nil {
		return nil, fmt.Errorf("usage_report: %w", err)
	}

	day7Cutoff := end.AddDate(0, 0, -7).Format(time.RFC3339)
	day1Cutoff := end.AddDate(0, 0, -1).Format(time.RFC3339)

	out := make([]usagestore.KeySpend, 0, len(keys))
	for _, k := range keys {
		var c30, c7, c1 float64
		for _, b := range usage {
			for _, r := range b.Results {
				if r.APIKeyID != k.ID {
					continue
				}
				cost := costFromTokens(r, c.OnUnknownModel)
				c30 += cost
				if b.StartingAt >= day7Cutoff {
					c7 += cost
				}
				if b.StartingAt >= day1Cutoff {
					c1 += cost
				}
			}
		}
		raw := rawByKey[k.ID]
		out = append(out, usagestore.KeySpend{
			Provider:     "anthropic",
			APIKeyID:     k.ID,
			APIKeyName:   k.Name,
			APIKeyHint:   k.PartialKeyHint,
			APIKeyStatus: k.Status,
			TotalUSD24h:  c1,
			TotalUSD7d:   c7,
			TotalUSD30d:  c30,
			RawJSON:      raw,
			FetchedAt:    time.Now().Unix(),
		})
	}
	return out, nil
}

func (c *AnthropicKeyCollector) listKeys() ([]rawAPIKey, error) {
	apiKey, err := c.APIKeyFn()
	if err != nil {
		return nil, err
	}
	out := []rawAPIKey{}
	afterID := ""
	for {
		u, _ := url.Parse(anthropicAPIKeysURL)
		q := u.Query()
		q.Set("limit", "100")
		if afterID != "" {
			q.Set("after_id", afterID)
		}
		u.RawQuery = q.Encode()

		req, _ := http.NewRequest("GET", u.String(), nil)
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicAPIVersion)
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("http: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("anthropic returned %d: %s", resp.StatusCode, string(body))
		}
		var raw rawAPIKeysResponse
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse: %w", err)
		}
		out = append(out, raw.Data...)
		if !raw.HasMore || raw.LastID == "" {
			break
		}
		afterID = raw.LastID
	}
	return out, nil
}

// fetchUsage retrieves all daily buckets for [start, end] grouped by api_key
// and model. Returns the merged bucket list plus a per-key pretty-printed
// raw JSON snapshot for the /raw endpoint.
func (c *AnthropicKeyCollector) fetchUsage(start, end time.Time) ([]rawUsageBucket, map[string]string, error) {
	apiKey, err := c.APIKeyFn()
	if err != nil {
		return nil, nil, err
	}
	all := []rawUsageBucket{}
	page := ""
	for {
		u, _ := url.Parse(anthropicUsageURL)
		q := u.Query()
		q.Set("starting_at", start.Format(time.RFC3339))
		q.Set("ending_at", end.Format(time.RFC3339))
		q.Set("bucket_width", "1d")
		q.Add("group_by[]", "api_key_id")
		q.Add("group_by[]", "model")
		if page != "" {
			q.Set("page", page)
		}
		u.RawQuery = q.Encode()

		req, _ := http.NewRequest("GET", u.String(), nil)
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicAPIVersion)
		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("http: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, nil, fmt.Errorf("anthropic returned %d: %s", resp.StatusCode, string(body))
		}
		var raw rawUsageResponse
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, nil, fmt.Errorf("parse: %w", err)
		}
		all = append(all, raw.Data...)
		if !raw.HasMore || raw.NextPage == "" {
			break
		}
		page = raw.NextPage
	}

	// Build a per-key raw JSON snapshot so the UI can show "Show raw response"
	// without us having to redo the API call. Filtering on the merged list is
	// cheap and gives consistent debug data even after pagination.
	rawByKey := map[string]string{}
	byKey := map[string][]rawUsageBucket{}
	for _, b := range all {
		// Group results in this bucket per key.
		perKey := map[string][]rawUsageResult{}
		for _, r := range b.Results {
			perKey[r.APIKeyID] = append(perKey[r.APIKeyID], r)
		}
		for kid, results := range perKey {
			byKey[kid] = append(byKey[kid], rawUsageBucket{
				StartingAt: b.StartingAt,
				EndingAt:   b.EndingAt,
				Results:    results,
			})
		}
	}
	for kid, buckets := range byKey {
		b, _ := json.MarshalIndent(map[string]interface{}{
			"api_key_id": kid,
			"buckets":    buckets,
		}, "", "  ")
		rawByKey[kid] = string(b)
	}

	return all, rawByKey, nil
}

// costFromTokens applies pricing × per-model multipliers. Calls onUnknown for
// every model we don't have a price for (with the dollar figure tallied as 0
// — visible gap rather than a silent under-report).
func costFromTokens(r rawUsageResult, onUnknown func(string)) float64 {
	p, ok := LookupAnthropic(r.Model)
	if !ok {
		if onUnknown != nil {
			onUnknown(r.Model)
		}
		return 0
	}
	const M = 1_000_000.0
	cost := 0.0
	cost += float64(r.UncachedInput) / M * p.InputUSDPerMTok
	cost += float64(r.OutputTokens) / M * p.OutputUSDPerMTok
	cost += float64(r.CacheRead) / M * p.CacheReadPerMTok()
	cost += float64(r.CacheCreation.Ephemeral5mInput) / M * p.CacheWrite5mPerMTok()
	cost += float64(r.CacheCreation.Ephemeral1hInput) / M * p.CacheWrite1hPerMTok()
	return cost
}

// parseAmountString is kept exported in case raw amounts ever come back as
// numeric strings on a future field. Currently unused — we compute locally.
func parseAmountString(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}
