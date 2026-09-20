# About usage-store

## What it is

usage-store answers two questions for this host: how much of each subscription's limit is used, and what each API key has cost. It is a Go library (`github.com/kayushkin/usage-store`, package `usagestore`) and a service, `usage-store-server`, that serves the library over HTTP on `:8185`. It owns the `limit_snapshots`, `spend_keys`, `spend_daily` and `spend_topups` tables in `~/.config/usage-store/usage.db`. `README.md` holds the route table and the settings table; keep them true when you change either.

## Who calls it

dash proxies it (`dash/server/api_usage.go`) for the usage page, which bridge-ui draws (`BridgeUsage.tsx`). The scheduler's autoworker reads `GET /api/usage/limits` before it dispatches work, and skips a provider whose snapshot is stale. healthcheck watches the `usage-store` unit with `auto_restart`. No `go.mod` under `~/repos` requires the library (measured 2026-09-20): only this repo's own server uses it.

## The token-accounting half is dead

`Track`, `Query`, `Stats`, `Summary` and `TotalCost` have no caller outside this repo's tests. `GET /api/usage` reads the `usage` table in model-store's `~/.config/model-store/store.db`, read-only, and the newest row there is dated 2026-04-26. The `usage` table in this service's own database has never been written. Do not build on either until noteboard todo `c211d55a-21fb-4a03-927a-099b62055da4` is decided. Live per-request accounting is not here.

# How it works

## Subscription limits

`internal/limitsrefresh` reads every provider each `USAGE_STORE_LIMITS_IDLE_REFRESH_INTERVAL` (15m). While the usage page is open it calls `POST /api/usage/limits/watch` about once a minute, and reads speed up to every 2m until 3m pass with no call. A failed read backs off, doubling up to the idle interval. Nothing may poll faster: Anthropic's usage endpoint shares its rate limit with every Claude Code process on the login.

- `anthropic` reads `https://api.anthropic.com/api/oauth/usage` with the OAuth token in `~/.claude/.credentials.json`.
- `codex` starts `codex app-server` on every read and asks it `account/rateLimits/read`. The Codex CLI holds and refreshes the login; usage-store holds no OpenAI token. It needs `USAGE_STORE_CODEX_COMMAND`, and the npm `codex` needs `node` on `PATH` too — the unit's `PATH` line is there for that. With the variable unset or the command missing, codex limits are off and the start-up log says so. It no longer reads `~/.codex/sessions/`.

The server sets `StaleAfter` when it answers, to the snapshot time plus three idle intervals (45m).

## Snapshots are append-only and the table is large

Each `SaveLimits` call writes one row per window and nothing prunes them. Measured 2026-09-20: `usage.db` is 647 MB, 585 MB of it `limit_snapshots` (about 540,000 rows since April). Most codex rows repeat the one before — noteboard todo `ca905a20-4a1b-4c06-a917-236146ddd815`. The history route has no default page size, so always send `limit` and `since` (todo `4c70597a-887c-480d-ae31-2bcbd229334d`). Never copy or dump the live database in a test; `scripts/e2e-smoke.sh` shows how to run against temp ones.

## Spend per API key

Spend is off unless `AUTH_STORE_TOKEN` is set. On this host it is set in the host-local drop-in `~/.config/systemd/user/usage-store.service.d/spend.conf` — keep it mode 600, and never put the token in the tracked unit file. At start the server lists auth-store's credentials, resolves each Anthropic `api_key` credential, and takes the first whose key starts `sk-ant-admin`; every resolve shows in auth-store's audit trail on purpose. It refreshes hourly (`USAGE_STORE_SPEND_REFRESH_INTERVAL`). A model missing from `spend/pricing.go` is priced at $0 and logged, so add new models there. Top-ups are rows a person adds through `/api/usage/spend/topups`.

# Access and operations

## Live service

`usage-store.service` is a user unit running `~/bin/usage-store-server`. It listens on every interface at `:8185` with no auth, so treat every route as public to the network. `GET /health` is liveness only. Logs: `journalctl --user -u usage-store`. The start-up line names both database paths, both refresh intervals and whether codex and spend are on — read it before you guess at configuration. `~/.config/usage-store/limits.db` is an empty leftover; nothing opens it.

## Settings

`internal/config` reads plain environment variables with `os.Getenv`; it does not use `llm-bridge/servicesettings` yet, so this service has no `/settings` route (noteboard todo `0554d4f2-2a8a-4c82-85ed-fdab3d212988`). ⚠️ `envDur` drops a duration it cannot parse and uses the default without a word, so a typo in a unit file changes nothing and logs nothing. Check the start-up line after you change an interval.

# Working in this repo

## Build, test and deploy

`go build ./... && go vet ./... && go test ./...` — green on `main` on 2026-09-20. `scripts/e2e-smoke.sh` builds the server, boots it on a temp port against temp databases with `HOME` pointed away, and calls every route; run it after you add or change a route, because a clashing `ServeMux` pattern compiles and then panics at start. Put the mise shims on `PATH` for Go: `export PATH="$HOME/.local/share/mise/shims:$PATH"`.

Deploy with `./deploy.sh` from the main clone, on a clean, pushed `main`; it runs the shared deploy gate first and records the deploy in repo-store's ledger last. The gate was added on 2026-09-20 and no deploy has run since, so the ledger has no row for this repo yet and the running binary's commit is not recorded anywhere. The script stops the service before it copies the binary, so a failed start leaves the usage page and the autoworker's limit check down until you fix it.

## Parked branches

The repo holds about ten pushed `fix/`, `test/` and `guard/` branches from nightly passes, each one to eight commits ahead of an older `main`. Whether and how they land is an open decision — noteboard todo `21cacf42-d867-4b54-aa6a-3828408d8e9e` — and several have a todo of their own waiting on the operator. Do not merge them as a side effect of other work, and do not start a second branch for a fix one of them already holds: run `git branch -a --no-merged main` and read the branch first.
