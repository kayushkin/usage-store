package spend

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	usagestore "github.com/kayushkin/usage-store"
)

// OpenAICostsURL is the admin-API endpoint for cost reporting. Requires a
// sk-admin-... key sent as a Bearer token.
const OpenAICostsURL = "https://api.openai.com/v1/organization/costs"

type OpenAICollector struct {
	APIKeyFn   func() (string, error)
	HTTPClient *http.Client
}

func NewOpenAI(apiKeyFn func() (string, error)) *OpenAICollector {
	return &OpenAICollector{
		APIKeyFn:   apiKeyFn,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// rawOpenAIResponse matches the documented shape of /v1/organization/costs.
// Each bucket contains zero or more results; each result has an Amount object.
type rawOpenAIResponse struct {
	Object  string `json:"object"`
	Data    []struct {
		Object    string `json:"object"`
		StartTime int64  `json:"start_time"`
		EndTime   int64  `json:"end_time"`
		Results   []struct {
			Object string `json:"object"`
			Amount struct {
				Value    float64 `json:"value"`
				Currency string  `json:"currency"`
			} `json:"amount"`
		} `json:"results"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

// Fetch returns one snapshot per (day, week, month) window.
func (c *OpenAICollector) Fetch() ([]usagestore.SpendSnapshot, [][]byte, error) {
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
			return nil, nil, fmt.Errorf("openai spend %s: %w", w.name, err)
		}
		out = append(out, *snap)
		raws = append(raws, raw)
	}
	return out, raws, nil
}

func (c *OpenAICollector) fetchOne(window string, start, end time.Time) (*usagestore.SpendSnapshot, []byte, error) {
	apiKey, err := c.APIKeyFn()
	if err != nil {
		return nil, nil, err
	}

	total := 0.0
	currency := "usd"
	var firstRaw []byte
	page := ""

	for {
		req, err := http.NewRequest("GET", OpenAICostsURL, nil)
		if err != nil {
			return nil, nil, err
		}
		q := req.URL.Query()
		q.Set("start_time", fmt.Sprintf("%d", start.Unix()))
		q.Set("end_time", fmt.Sprintf("%d", end.Unix()))
		q.Set("bucket_width", "1d")
		q.Set("limit", "31") // enough buckets for a 30-day window at 1d granularity
		if page != "" {
			q.Set("page", page)
		}
		req.URL.RawQuery = q.Encode()
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := c.HTTPClient.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("http: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			return nil, body, fmt.Errorf("openai returned %d: %s", resp.StatusCode, string(body))
		}
		if firstRaw == nil {
			firstRaw = body
		}

		var raw rawOpenAIResponse
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, body, fmt.Errorf("parse: %w", err)
		}

		for _, bucket := range raw.Data {
			for _, r := range bucket.Results {
				total += r.Amount.Value
				if r.Amount.Currency != "" {
					currency = r.Amount.Currency
				}
			}
		}

		if !raw.HasMore || raw.NextPage == "" {
			break
		}
		page = raw.NextPage
	}

	return &usagestore.SpendSnapshot{
		Provider:    "openai",
		Window:      window,
		PeriodStart: start.Unix(),
		PeriodEnd:   end.Unix(),
		TotalUSD:    total,
		Currency:    currency,
		FetchedAt:   time.Now().Unix(),
		Source:      "api",
	}, firstRaw, nil
}
