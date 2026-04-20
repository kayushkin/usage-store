package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"

	usagestore "github.com/kayushkin/usage-store"
	"github.com/kayushkin/usage-store/anthropic"
	"github.com/kayushkin/usage-store/codex"

	_ "modernc.org/sqlite"
)

// Server exposes usage-store data over HTTP. All routes live under /api/usage.
type Server struct {
	store        *usagestore.Store
	tokensDBPath string
	anthropic    *anthropic.Collector
	codex        *codex.Reader
	mux          *http.ServeMux
}

func New(s *usagestore.Store, tokensDBPath string, ant *anthropic.Collector, cx *codex.Reader) *Server {
	srv := &Server{
		store:        s,
		tokensDBPath: tokensDBPath,
		anthropic:    ant,
		codex:        cx,
		mux:          http.NewServeMux(),
	}
	srv.routes()
	return srv
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/usage", s.handleUsage)
	s.mux.HandleFunc("GET /api/usage/limits", s.handleLimits)
	s.mux.HandleFunc("GET /api/usage/limits/{provider}", s.handleProviderLimits)
	s.mux.HandleFunc("GET /api/usage/limits/{provider}/history", s.handleHistory)
	s.mux.HandleFunc("POST /api/usage/limits/refresh", s.handleRefresh)
	s.mux.HandleFunc("GET /health", s.handleHealth)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	s.mux.ServeHTTP(w, r)
}

// handleUsage returns token-usage aggregates over day/week/month windows.
//
// Mirrors the dash UI's existing /api/usage shape so we can swap dash to point
// here without touching the React side.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	db, err := sql.Open("sqlite", s.tokensDBPath+"?mode=ro")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("open tokens db: %w", err))
		return
	}
	defer db.Close()

	type stats struct {
		Agent        string  `json:"agent"`
		Orchestrator string  `json:"orchestrator"`
		Model        string  `json:"model"`
		Messages     int     `json:"messages"`
		InputTokens  int64   `json:"input_tokens"`
		OutputTokens int64   `json:"output_tokens"`
		TotalTokens  int64   `json:"total_tokens"`
		CostUSD      float64 `json:"cost_usd"`
	}

	queryPeriod := func(days int) []stats {
		query := `SELECT agent, COALESCE(orchestrator, 'inber'), model,
			SUM(input_tokens), SUM(output_tokens), SUM(requests), COALESCE(SUM(cost_usd), 0)
			FROM usage WHERE date >= date('now', ?)
			GROUP BY agent, orchestrator, model
			ORDER BY SUM(input_tokens) + SUM(output_tokens) DESC`
		rows, err := db.Query(query, fmt.Sprintf("-%d days", days))
		if err != nil {
			log.Printf("[usage] period query: %v", err)
			return []stats{}
		}
		defer rows.Close()

		var out []stats
		for rows.Next() {
			var u stats
			if err := rows.Scan(&u.Agent, &u.Orchestrator, &u.Model,
				&u.InputTokens, &u.OutputTokens, &u.Messages, &u.CostUSD); err != nil {
				continue
			}
			u.TotalTokens = u.InputTokens + u.OutputTokens
			out = append(out, u)
		}
		if out == nil {
			out = []stats{}
		}
		return out
	}

	writeJSON(w, map[string]interface{}{
		"day":   queryPeriod(1),
		"week":  queryPeriod(7),
		"month": queryPeriod(30),
	})
}

// handleLimits returns the latest snapshot for every known provider.
func (s *Server) handleLimits(w http.ResponseWriter, r *http.Request) {
	out := map[string]*usagestore.ProviderLimits{}
	for _, p := range []string{"anthropic", "codex"} {
		snap, err := s.store.LatestLimits(p)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("latest %s: %w", p, err))
			return
		}
		out[p] = snap
	}
	writeJSON(w, out)
}

func (s *Server) handleProviderLimits(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	snap, err := s.store.LatestLimits(provider)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if snap == nil {
		http.Error(w, `{"error":"no snapshot"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, snap)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	f := usagestore.LimitHistoryFilter{
		Provider: r.PathValue("provider"),
		Window:   r.URL.Query().Get("window"),
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.Since = n
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			f.Until = n
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	rows, err := s.store.HistoryLimits(f)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if rows == nil {
		rows = []usagestore.LimitHistoryRow{}
	}
	writeJSON(w, rows)
}

// handleRefresh forces an out-of-band fetch for one provider.
//
// For anthropic this hits the upstream API. For codex this rescans the local
// rollout directory (cheap; no network). Persists the result as a new snapshot.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	switch provider {
	case "anthropic":
		snap, raw, err := s.anthropic.Fetch()
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if err := s.store.SaveLimits(*snap, raw); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, snap)
	case "codex":
		snap, raw, err := s.codex.Latest()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if snap == nil {
			http.Error(w, `{"error":"no codex rollout found"}`, http.StatusNotFound)
			return
		}
		if err := s.store.SaveLimits(*snap, raw); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, snap)
	default:
		http.Error(w, `{"error":"unknown provider"}`, http.StatusBadRequest)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
