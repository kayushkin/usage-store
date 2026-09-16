# usage-store

Token usage tracking + subscription-limit aggregator. Library + service.

## Library

`github.com/kayushkin/usage-store` exposes `Open`, `Track`, `Query`, `Stats`, `Summary`, `TotalCost` for per-request token accounting (used by `model-store`), plus `SaveLimits`, `LatestLimits`, `HistoryLimits` for subscription-limit snapshots.

Sub-packages:
- `anthropic` — fetches Claude OAuth subscription usage from `https://api.anthropic.com/api/oauth/usage` using the token in `~/.claude/.credentials.json`.
- `codex` — starts `codex app-server` (the Codex CLI's JSON-RPC interface) each refresh and asks it `account/rateLimits/read`. The CLI answers from OpenAI with its own login and refreshes that login itself, so usage-store holds no token, and the numbers cover every Codex client on the account, not only sessions run on this host. Needs `USAGE_STORE_CODEX_COMMAND` (a path or a name on `PATH`; the npm `codex` also needs `node` on `PATH`) — unset or not found, codex limits are off and the startup log says so. `USAGE_STORE_CODEX_TIMEOUT` bounds one read (default 30s).

## Service (`usage-store-server`)

HTTP front for the library. Runs as a user systemd unit on `:8185` (default).

Endpoints (all under `/api/usage`):

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/usage` | Token aggregates (`day` / `week` / `month`) — same shape dash already consumes |
| GET | `/api/usage/limits` | Latest snapshot for every provider |
| GET | `/api/usage/limits/{provider}` | Latest snapshot for one provider |
| GET | `/api/usage/limits/{provider}/history?window=&since=&until=&limit=` | Snapshot history (for graphing) |
| POST | `/api/usage/limits/refresh?provider=anthropic\|codex` | Force a fresh fetch + persist |
| GET | `/health` | Liveness |

Background ticker fires `Anthropic.Fetch` + `Codex.Latest` every `USAGE_STORE_REFRESH_INTERVAL` (default `60s`) and writes a row per (provider, window).

### Env vars

| Var | Default |
|---|---|
| `USAGE_STORE_LISTEN_ADDR` | `:8185` |
| `USAGE_STORE_DB` | `~/.config/usage-store/usage.db` (limit snapshots) |
| `USAGE_STORE_TOKENS_DB` | `~/.config/model-store/store.db` (token-usage source, read-only) |
| `USAGE_STORE_REFRESH_INTERVAL` | `60s` |
| `USAGE_STORE_CODEX_MAX_AGE` | `2h` |

### Deploy

```bash
./deploy.sh
```

Builds the binary, installs to `~/bin/usage-store-server`, drops the systemd unit at `~/.config/systemd/user/usage-store.service`, and starts it.

## Snapshot schema

`limit_snapshots` rows are append-only. Each `SaveLimits` call writes one row per window; reconstruct a `ProviderLimits` with `LatestLimits(provider)` (returns the most recent `snapshot_at` group). `StaleAfter` is computed at read time from the per-provider freshness budget (`codex` = 10m — ten missed refreshes, since a live read should never be older than one interval; `anthropic` = none).
