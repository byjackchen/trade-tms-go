# TMS API contract (`tms api`)

The UI-facing HTTP + WebSocket API served by `internal/api` and launched with
`tms api`. This document is the **authoritative wire contract** for the UI
builder; the executable companion is `internal/api/handlers_test.go` (every
endpoint, auth, validation and happy path) plus `internal/api/ws_test.go`.

- Container-internal listen address: `:8080` (`TMS_API_ADDR`, default `:8080`).
- Host port: **18080** (compose maps `18080:8080`).
- Base path for the data/jobs/universe API: `/api/v1`.

## Authentication

> **Note — extends `docs/spec/api-ws-redis.md §1.1`.** The read-only UI API has
> *no* authentication (CORS only). This control plane mutates state (enqueues
> data refresh / universe rebuild / job cancel jobs), so it requires a static
> bearer token. Trading mutation endpoints remain absent, per the spec's
> "read-only forever" rule.

- Every `/api/*` route requires `Authorization: Bearer <TMS_API_TOKEN>`.
- `/healthz` and `/version` are **public** (no token).
- Browser WebSocket clients cannot set headers, so `/api/v1/ws` also accepts
  the token as the `?token=` query parameter. (Any `/api/*` route accepts
  `?token=` as a fallback; prefer the header for REST.)
- Missing/invalid token → `401` with `WWW-Authenticate: Bearer realm="tms-api"`
  and the standard error envelope (`code: "unauthorized"`). Token comparison is
  constant-time; the token is never written to logs.

The process **fails to start** without `TMS_API_TOKEN` (an unauthenticated API
is a misconfiguration, not a degraded mode).

## CORS

Allowlist policy, origins from `TMS_API_CORS_ORIGINS` (comma-separated; default
empty). For an allowlisted `Origin`:

- `Access-Control-Allow-Origin: <origin>` (echoed), `Vary: Origin`
- `Access-Control-Allow-Methods: GET, POST, OPTIONS`
- `Access-Control-Allow-Headers: Authorization, Content-Type`
- `Access-Control-Max-Age: 600`
- Credentials are **not** allowed (no `Allow-Credentials` header).

Preflight (`OPTIONS` with `Access-Control-Request-Method`) → `204`.
Non-allowlisted origins receive **no** CORS headers (the browser enforces the
block). UI default origin: `http://localhost:13000` / `http://127.0.0.1:13000`.

## Error envelope

All non-2xx JSON responses share one shape:

```json
{ "error": { "code": "validation", "message": "human-readable detail" } }
```

| `code` | HTTP | Meaning |
|---|---|---|
| `unauthorized` | 401 | Missing/invalid bearer token. |
| `validation` | 400 | Malformed query/body or out-of-range parameter. |
| `not_found` | 404 | Unknown ticker / job id / universe snapshot. |
| `internal` | 500 | Server-side failure. The `message` is always generic ("internal error; see server logs") — internals never leak. |

Request bodies must be a single JSON object; unknown fields, trailing data and
bodies over 64 KiB are rejected with `400`. An empty body is treated as `{}`.

All timestamps are RFC 3339 **UTC**. All dates are `YYYY-MM-DD` and represent
the **America/New_York trading date** (P1 locked decision: trading-date logic is
normalized to the NYSE calendar in `internal/data/calendar`).

---

## `GET /healthz` (public)

Process liveness + dependency reachability. **Always `200`** even when a
dependency is down (status `"degraded"`): restarting the API cannot heal
Postgres/Redis, so the container healthcheck only asserts the process serves
HTTP. The `tms api --health` probe (container healthcheck) GETs this endpoint.

```json
{
  "status": "ok",                    // "ok" | "degraded"
  "version": "<build version>",
  "deps": {
    "postgres": { "ok": true },
    "redis":    { "ok": false, "error": "connection refused" }
  }
}
```

`deps.<name>.error` is present only when `ok` is false. A `redis.error` of
`"not configured"` means the deployment runs without Redis (WS events degraded).

## `GET /version` (public)

```json
{ "version": "...", "commit": "...", "build_date": "..." }
```

---

## `GET /api/v1/data/coverage`

Per-table market-data coverage, freshness vs. the latest NYSE session, and
`bars_daily` gap detection.

**Summary mode** (no query params):

```json
{
  "latest_session": "2024-06-12",
  "generated_at": "2024-06-12T15:30:00Z",
  "tables": [
    {
      "table": "tickers",
      "rows": 12345,
      "tickers": 12345,
      "min_date": "2000-01-03",
      "max_date": "2024-06-11"
    },
    {
      "table": "bars_daily",
      "rows": 9000000,
      "tickers": 11000,
      "min_date": "2000-01-03",
      "max_date": "2024-06-11",
      "freshness": { "latest_session": "2024-06-12", "lag_sessions": 1 },
      "gaps": {
        "tickers_scanned": 11000,
        "tickers_with_gaps": 37,
        "missing_days_total": 421,
        "worst": [
          { "ticker": "BAC", "first": "2024-06-06", "last": "2024-06-11",
            "bars": 2, "expected_sessions": 4, "missing_days": 2 }
        ]
      }
    }
  ]
}
```

- Tables reported: `tickers`, `bars_daily`, `fundamentals_sf1`, `events`.
- `min_date`/`max_date` are omitted when the table is empty.
- `freshness` (present when `max_date` exists): `lag_sessions` counts NYSE
  sessions in `(max_date, latest_session]` — `0` means fully fresh.
- `gaps` is attached to `bars_daily` only. For each ticker, the number of NYSE
  sessions over its own `[first, last]` span is compared against its stored bar
  count. `worst` is the top-10 by `missing_days` (ties broken by ticker).
  History before the calendar's start (year 2000) is excluded from the
  expectation, so pre-2000 missing counts are a lower bound.

**Drill-down mode** (`?ticker=AAPL`, case-insensitive): the exact missing NYSE
trading dates within that ticker's bar span.

```json
{
  "ticker": "MSFT",
  "first": "2024-06-06",
  "last": "2024-06-11",
  "bars": 2,
  "expected_sessions": 4,
  "missing_days": 2,
  "missing": ["2024-06-07", "2024-06-10"],
  "missing_truncated": false
}
```

- Unknown ticker (not in `tms.tickers`) → `404 not_found`.
- Known ticker with no bars → `200` with `bars: 0`, `missing: []`.
- `missing` is capped at 1000 dates; `missing_truncated` flags the overflow
  (`missing_days` still reflects the true total).

## `GET /api/v1/data/tickers?q=<term>`

Ticker search by ticker prefix **or** name substring (case-insensitive). Exact
ticker matches rank first, then prefix matches, then alphabetical.

- `q` is **required** (blank → `400 validation`).
- `limit` (optional): default 20, range `[1, 200]` (out of range → `400`).

```json
{
  "query": "app",
  "results": [
    {
      "ticker": "AAPL", "name": "Apple Inc", "exchange": "NASDAQ",
      "is_delisted": false, "category": "Domestic Common Stock",
      "sector": "Technology", "industry": "Consumer Electronics",
      "table": "SF1",
      "first_price_date": "1998-01-02", "last_price_date": "2024-06-11",
      "delist_date": ""
    }
  ]
}
```

Optional text/date fields are `""` when NULL. `results` is `[]` when nothing
matches.

> Note: `tms.tickers.table_name` is `SF1` (common stocks) or `SFP`
> (ETFs/funds). The data-refresh **dataset** vocabulary is the separate
> `TICKERS | SEP | SFP | SF1 | EVENTS` (see below).

## `GET /api/v1/data/sync-runs`

Per-dataset sync watermarks plus the dataset-sync run history.

- `dataset` (optional): one of `TICKERS | SEP | SFP | SF1 | EVENTS`
  (case-insensitive). Unknown → `400 validation`.
- `limit` (optional): default 50, range `[1, 500]`.

```json
{
  "datasets": [
    { "dataset": "SEP", "last_sync": "2024-06-11T22:05:00Z",
      "row_count": 9000000, "schema_version": 1,
      "updated_at": "2024-06-11T22:05:00Z" }
  ],
  "runs": [
    { "id": 42, "dataset": "SEP", "kind": "import",
      "started_at": "2024-06-11T22:00:00Z", "finished_at": "2024-06-11T22:05:00Z",
      "rows_added": 12000, "status": "ok" }
  ]
}
```

`runs` is newest-first. `error` is present on a run only when non-empty.
`last_sync` is `null` when the dataset has never synced.

## `POST /api/v1/data/refresh`

Enqueue a `data.refresh` job. Validated server-side; the worker re-validates
the payload. **At most one active refresh at a time** (dedupe key
`data.refresh`); a duplicate returns the existing job with `deduped: true`.

Request body:

```json
{
  "source": "parquet",                 // REQUIRED: "parquet" | "api"
  "tables": ["SEP", "SF1"],            // optional; TICKERS|SEP|SFP|SF1|EVENTS, upper-cased
  "tickers": ["AAPL", "MSFT"],        // optional; upper-cased, no blank entries
  "since": "2024-01-01",              // optional; YYYY-MM-DD
  "actor": "alice",                    // optional; recorded in the audit trail
  "max_attempts": 3                    // optional; [0, 10], 0 = default of 1
}
```

Response `202 Accepted`:

```json
{ "job": { /* job object, see below */ }, "deduped": false }
```

The actor is stamped into the audit trail as `api:<actor>` (or `api` when
omitted), distinguishing HTTP submissions from CLI/system ones. An
`audit_log` row is written atomically with the enqueue.

Validation `400`s: missing/unknown `source`, unknown `tables` entry, blank
`tickers` entry, malformed `since`, `max_attempts` out of range, unknown JSON
field.

### Sources

- **`parquet`** reuses the P0 importer over `TMS_SHARADAR_CACHE_DIR`;
  `tables`/`tickers`/`since` scope the import.
- **`api`** runs the live Nasdaq Data Link **catchup** (ensure-fresh): the
  watermark-driven incremental refresh of all five datasets through T-1
  (`internal/data/sharadar.Syncer.EnsureFresh`, spec §8). It requires
  `TMS_NASDAQ_DATA_LINK_API_KEY` on the worker; if the key is unset the job
  fails terminally with a clear message. Catchup is whole-universe and
  watermark-driven, so it **cannot be scoped** — a `source: "api"` job that
  also sets `tables`, `tickers` or `since` fails fast with a pointer to
  `tms sync bootstrap` (the bounded/filtered backfill entry point). An
  un-bootstrapped store is reported as `skipped_reason: "not-bootstrapped"`
  (succeeded, no rows) rather than auto-bootstrapping.

The `data.refresh source=api` result object carries
`{flow, did_work, days_attempted, days_succeeded, rows_added{…}, errors[],
skipped_reason?}`.

### CLI twin: `tms sync`

The same engine is exposed for operators (no HTTP needed for the bounded
backfill):

- `tms sync bootstrap --start YYYY-MM-DD --end YYYY-MM-DD [--ticker T …]` —
  backfill TICKERS→SEP→SFP→SF1→EVENTS over a bounded window (a failed step
  aborts; idempotent re-runs converge).
- `tms sync catchup` — the same watermark-driven catchup the worker's
  `source: "api"` job runs.

Both require `TMS_NASDAQ_DATA_LINK_API_KEY`, record `dataset_sync_runs` audit
rows and advance the `dataset_sync` watermark.

---

## `POST /api/v1/data/sync-now`

Force the **daily incremental-sync pipeline** immediately — the manual twin of
the automatic `tms scheduler`. It enqueues, for the current NYSE trading date
(or the most recent prior session on a weekend/holiday), `data.refresh
source=api` followed by `eod.refresh`, exactly as the scheduler does at its
configured time.

**Idempotent per trading day.** Enqueue happens at most once per
`(daily, trading_date)` via the durable `tms.scheduler_runs` ledger: if the
scheduler (or an earlier force) already enqueued the day's pipeline, this is a
no-op and returns `forced: false, deduped: true` with no new jobs. This is a
stronger guarantee than the active-only `data.refresh` dedupe key — it holds
even after the day's jobs have already succeeded.

Request body (optional):

```json
{ "actor": "alice" }   // optional; recorded in the audit trail as api:<actor>
```

Response `202 Accepted`:

```json
{
  "trading_date": "2026-06-15",
  "forced": true,        // false when the day was already enqueued
  "deduped": false,      // = !forced
  "data_job_id": 41,     // 0 when deduped
  "eod_job_id": 42       // 0 when deduped
}
```

`503` when the scheduler force path is not wired (misconfigured
`TMS_SCHEDULER_DAILY_AT` / `TMS_SCHEDULER_TZ`); `400` on an unknown JSON field.

**CLI twin:** `tms sync now` runs the identical force through the same ledger.

---

## Jobs

### `GET /api/v1/jobs`

List jobs, newest-first.

- `kind` (optional): exact dispatch key (e.g. `data.refresh`).
- `status` (optional): `queued | running | succeeded | failed | canceled`
  (unknown → `400`).
- `limit` (optional): default 50, range `[1, 500]`.

```json
{ "jobs": [ { /* job object */ } ] }
```

### `GET /api/v1/jobs/{id}`

One job by id. Unknown id → `404 not_found`; non-integer/`<1` id → `400`.

```json
{ "job": { /* job object */ } }
```

### `POST /api/v1/jobs/{id}/cancel`

Optional body: `{ "reason": "...", "actor": "..." }`.

- A **queued** job is canceled immediately (`outcome: "canceled"`).
- A **running** job has its cooperative cancel flag set
  (`outcome: "cancel_requested"`) — the owning worker observes it on its next
  heartbeat and stops. Idempotent.
- A **terminal** job is a no-op (`outcome: "already_terminal"`).
- Unknown id → `404`.

```json
{ "outcome": "cancel_requested", "job": { /* job object */ } }
```

### `POST /api/v1/jobs/{id}/retry`

Re-run a **failed** or **canceled** job by enqueuing a **new** job that clones
the source's `kind` + `payload`. The source row is never mutated — the failed/
canceled record and its audit trail are preserved; the retry is the same enqueue
path the original used, so the worker re-validates the payload.

- Optional body: `{ "actor": "..." }` (stamped `api[:<actor>]` in the audit
  trail, like the other mutation endpoints).
- The clone carries **no** `dedupe_key` (a manual retry is an explicit operator
  action and must not be silently deduped against a still-active job) and
  inherits the source's `priority` + `max_attempts`.
- Only **failed**/**canceled** jobs are retryable. A `queued`/`running` job is
  still in flight, and a `succeeded` job has nothing to retry → `422`
  (`validation`). Unknown id → `404`.

Response `201 Created`:

```json
{ "job": { /* the NEW job object */ }, "source_job_id": 42 }
```

### Job object

```json
{
  "id": 42,
  "kind": "data.refresh",
  "status": "running",                 // queued|running|succeeded|failed|canceled
  "payload": { "source": "api" },
  "priority": 0,
  "run_at": "2024-06-12T15:30:00Z",
  "attempts": 1,
  "max_attempts": 3,
  "dedupe_key": "data.refresh",        // nullable
  "claimed_by": "worker-1",            // nullable
  "claimed_at": "2024-06-12T15:30:01Z",// nullable
  "heartbeat_at": "2024-06-12T15:30:10Z", // nullable
  "started_at": "2024-06-12T15:30:01Z",// nullable
  "finished_at": null,                 // nullable
  "last_error": null,                  // nullable
  "progress": { "stage": "fetch", "pct": 40 }, // omitted when empty
  "cancel_requested": false,
  "result": { "rows": 1200 },          // omitted when empty
  "created_at": "2024-06-12T15:30:00Z",
  "updated_at": "2024-06-12T15:30:10Z"
}
```

`progress` and `result` are JSON objects written by the worker; they are
omitted from the response when empty.

---

## Audit log

### `GET /api/v1/audit`

The append-only operational audit trail (`tms.audit_log`): every state-changing
action — job lifecycle (`job.enqueued` / `job.claimed` / …), param promotions,
control commands, manual interventions — newest-first. Rows are written
atomically with the change they record and are never updated or deleted. Backs
the Ops UI **Audit log** panel. Implementation: `internal/api/handlers_audit.go`.

- `actor` (optional): exact actor match (e.g. `api:alice`, `system`, `cli`).
- `action` (optional): exact action match (e.g. `job.enqueued`).
- `entity` (optional): exact entity-kind match (e.g. `job`, `strategy`).
- `entity_id` (optional): exact entity-id match (e.g. `42`).
- `before` (optional): keyset cursor — return rows with `id` **strictly less
  than** this id (page by passing the last/oldest row's id back). Must be a
  positive integer (else `400`).
- `limit` (optional): default 50, range `[1, 500]`.

```json
{
  "entries": [
    {
      "id": 105,
      "ts": "2026-06-12T15:30:00Z",
      "actor": "api:alice",
      "action": "job.enqueued",
      "entity": "job",            // omitted when null
      "entity_id": "42",          // omitted when null/empty
      "details": { "kind": "data.refresh" }  // omitted when empty ({})
    }
  ],
  "next_before": 88               // keyset cursor for the next (older) page; null when no more
}
```

`entity` / `entity_id` / `details` are omitted when absent (a `details` of `{}`
is treated as empty). `next_before` is the `id` of the oldest row in this page
when the page is full (`len == limit`), else `null` (the trail's start was
reached). When the API is started without an audit reader the endpoint returns
`503` (`validation`, "audit log reader not configured"). Bearer-auth like every
`/api/*` route.

---

## Universe

### `GET /api/v1/universe/latest`

The most recent universe snapshot.

- `kind` (optional): `live | eod | backtest | manual` (unknown → `400`). When
  omitted, the latest snapshot of any kind is returned.
- No snapshot of the requested kind → `404 not_found`.

```json
{
  "snapshot": {
    "id": 7,
    "as_of": "2024-06-11",
    "kind": "eod",
    "table_filter": "",                // omitted when empty
    "window_start": "2023-06-11",      // omitted when zero
    "window_end": "2024-06-11",        // omitted when zero
    "limit_n": 85,
    "tickers": ["AAPL", "MSFT"],
    "excluded": [],
    "params": { },
    "members": [
      { "ticker": "AAPL", "rank": 1, "score": 9.5, "trend_template_count": 8,
        "breakout_proximity": 0.02, "market_cap_usd": 3.0e12, "reasons": [] }
    ],
    "created_at": "2024-06-11T22:10:00Z"
  }
}
```

### `POST /api/v1/universe/rebuild`

Enqueue a `universe.rebuild` job. **At most one active rebuild at a time**
(dedupe key `universe.rebuild`).

Request body (all optional):

```json
{
  "kind": "eod",        // live|eod|backtest|manual; default "manual"
  "limit": 85,          // null/absent → worker default (TMS_LIVE_UNIVERSE_LIMIT, 85); 0 = empty universe
  "uncapped": false,    // ignore the cap
  "top_k": 0,           // >= 0
  "actor": "alice"
}
```

Response `202 Accepted`: `{ "job": { /* job object */ }, "deduped": false }`.

Validation `400`s: unknown `kind`, negative `top_k`, unknown JSON field.

---

## Strategies

The strategy registry the UI Strategies section renders: the four production
strategies (SEPA / Sector Rotation / Pairs / ORB), their **active** parameter
document and **param schema**, allocation and enabled state. The active document
is resolved with the engine's precedence — DB `active_params` → file
`TMS_STRATEGY_PARAMS_DIR/<id>.json` → embedded baseline (`params_source` reports
which tier won), so a promotion is reflected without an API restart.

Strategy ids are the canonical loader stems
(`sepa|sector_rotation|pairs|intraday_breakout`). Each carries a `backtest_id` —
the token `POST /api/v1/backtests` accepts — which differs only for ORB
(`intraday_breakout` → `orb`). The UI links the detail page by `id` and launches
a run with `backtest_id`.

### `GET /api/v1/strategies`

List all four strategies in a fixed display order (sepa, sector_rotation, pairs,
orb).

```json
{ "strategies": [ {
  "id": "sepa",
  "backtest_id": "sepa",
  "label": "SEPA",
  "description": "Stage 2 breakout per Minervini's Specific Entry Point Analysis",
  "capital_pct": 0.40,            // allocation.capital_pct (0..1); null = unallocated (e.g. ORB)
  "active": true,                 // allocation.active (default true)
  "params_source": "baseline",    // db | file | baseline
  "schema_version": 1,
  "parameters_count": 8,
  "parameters": [ {
    "name": "risk_pct", "type": "float", "default": 1.0,
    "search_low": 1.0, "search_high": 4.0,   // omitted for static params
    "description": "Per-trade risk as % of equity"
  } /* ... in file order */ ],
  "active_values": { "risk_pct": 1.0 /* name -> resolved value */ }
} /* ... */ ] }
```

A per-strategy resolution failure (e.g. a malformed promoted document) keeps the
row with an `"error"` string and empty `parameters`, rather than failing the
whole list.

### `GET /api/v1/strategies/{id}`

One strategy's metadata + active params + full schema (same element shape as the
list, wrapped in `strategy`). `404`
`{"error":{"code":"not_found",...}}` for an unknown id (want
`sepa|sector_rotation|pairs|intraday_breakout`).

```json
{ "strategy": { /* the element shape above */ } }
```

---

## System status

### `GET /api/v1/system`

Aggregated health of every component for the UI **System** page — Postgres,
Redis, the moomoo data feed (inferred), active live sessions, the durable
job-queue depth, and market-data freshness — in one call. **Always `200`**;
degradation is reported in the body (`status` + per-component `status`), so the
page renders red/yellow dots rather than throwing. Implementation:
`internal/api/system.go`; contract: `internal/api/system_test.go`.

- `status` rollup: `"down"` iff Postgres is unreachable (the only fatal
  dependency); `"degraded"` if any other component is `down`/`degraded`;
  otherwise `"ok"`.
- Component `status` values: `ok | degraded | down | idle | unknown |
  not_configured`.
- The moomoo feed is **inferred** from the latest `tms.sessions` row + the
  freshness of the latest `PortfolioHealth` snapshot (the `tms api` process holds
  no OpenD socket — that lives in `tmsgo-live`).

```json
{
  "status": "ok",
  "version": "0.1.0",
  "ts": "2025-06-12T15:30:00Z",
  "components": {
    "postgres":    { "status": "ok", "detail": "reachable" },
    "redis":       { "status": "ok", "detail": "reachable" },
    "moomoo_feed": { "status": "ok", "detail": "data flowing" },
    "sessions":    { "status": "ok", "detail": "1 active session · latest RUNNING (signal)" },
    "jobs":        { "status": "ok", "detail": "3 queued · 1 running" },
    "data":        { "status": "ok", "detail": "latest bar 2025-06-10" }
  },
  "metrics": {
    "jobs_queued": 3,
    "jobs_running": 1,
    "active_sessions": 1,
    "latest_bar_date": "2025-06-10",
    "last_sync_at": "2025-06-12T13:30:00Z",
    "live_mode": "signal",
    "live_session_id": 7,
    "health_age_seconds": 60.0
  }
}
```

---

## Backtests

The result + control plane over the DB source of truth (`research.runs` /
`run_metrics` / `equity_curves` / `trades`, P2 locked decision 4). A backtest is
run by the `backtest.run` job handler (engine → DB persist + legacy
`runs/{ts}/*.json` artifacts). Money is rendered as float64 USD (the legacy
artifact shape); `sharpe`/`calmar`/`max_drawdown_pct` are float64 exactly as the
metrics package computes them (population std-dev, 252 periods/yr — see
`docs/spec/hyperopt-metrics.md §1`).

### `POST /api/v1/backtests`

Enqueue a `backtest.run` job (audited; the actor is stamped `api[:<actor>]`).

Request body:

```json
{
  "start": "2024-01-02",          // required (YYYY-MM-DD)
  "end":   "2024-12-31",          // required
  "tickers": ["AAPL","KO"],        // explicit list, OR
  "universe": {"start":"...","end":"...","table":"SF1"}, // survivor-bias-free window
  "starting_balance": 100000.0,    // USD; default 100000
  "fill_profile": "realistic",       // or "close-fill"; default realistic
  "strategy": "scripted",          // scripted|sepa|sector_rotation|pairs|orb|multi
  "orb_symbol": "SPY",             // required for strategy "orb" (or exactly one ticker)
  "intents": [ {"date":"2024-01-03","ticker":"AAPL","side":"LONG","qty":100} ], // scripted only
  "kind": "multi-strategy",        // run-kind badge
  "seed": 0,                       // reserved for stochastic models
  "run_ts": "2026-..._..-..-..",  // optional idempotency key
  "realistic": {"slippage_bps":1.0,"commission_bps":0.0,"commission_per_share":0.0},
  "actor": "alice",                // audit
  "max_attempts": 1,               // queue retry budget (0..10; 0 -> 1)
  "dedupe_key": ""                 // optional: at most one active job per key
}
```

Response `202 Accepted`: `{ "job": { /* job object */ }, "deduped": false }`.
Track progress via the `job` object (`progress` carries
`{phase, bars_processed, bars_total, percent}`) and the WebSocket job stream;
the job `result` carries `{run_id, run_ts, final_balance, sharpe, ...}` on
success.

`strategy` selects the signal source: `scripted` replays the supplied `intents`;
`sepa`/`sector_rotation`/`pairs`/`orb` run a single production strategy; `multi`
runs the SEPA + Sector + Pairs portfolio with its allocations. Only `scripted`
requires `tickers`/`universe`; `sepa`/`multi` accept them as the stock universe
(optional — the engine resolves a point-in-time universe otherwise);
`sector_rotation`/`pairs` derive their instruments from the active params; `orb`
needs `orb_symbol` (or exactly one ticker).

Validation `400`s: missing `start`/`end`; `scripted` with neither `tickers` nor
`universe`; `orb` with no `orb_symbol` and not exactly one ticker; unknown
`fill_profile`; unsupported `strategy`; `max_attempts` out of `[0,10]`; unknown
JSON field.

### `GET /api/v1/backtests`

List runs newest-first by `run_ts`. Query: `?status=` (`RUNNING|COMPLETE|
INTERRUPTED|FAIL`), `?kind=`, `?limit=` (1..500, default 50).

```json
{ "backtests": [ {
  "id": 7, "run_ts": "2026-...", "kind": "multi-strategy", "status": "COMPLETE",
  "start_date": "2024-01-02", "end_date": "2024-12-31",
  "starting_balance_usd": 100000.0, "final_balance_usd": 105000.0,
  "total_pnl_usd": 5000.0, "strategies": ["Scripted-000"],
  "created_at": "...", "updated_at": "..." } ] }
```

### `GET /api/v1/backtests/{id}`

Run detail: summary + portfolio `metrics` + per-strategy `strategy_metrics` +
the run `config`. `404` `{"error":{"code":"not_found",...}}` for an unknown id.

```json
{
  "backtest": { /* summary, as in the list */ },
  "config": { /* the backtest.run payload, verbatim */ },
  "metrics": {
    "final_balance_usd": 105000.0, "total_pnl_usd": 5000.0,
    "sharpe": 1.5, "calmar": 2.5, "max_drawdown_pct": -3.2,
    "num_orders": 4, "num_filled_orders": 4, "num_rejected_orders": 0,
    "num_positions": 2
  },
  "strategy_metrics": { "Scripted-000": { /* same shape */ } }
}
```

### `GET /api/v1/backtests/{id}/equity[?strategy=<id>]`

Equity-curve points, ascending by `ts`. Default scope is `portfolio` (account
equity); `?strategy=<id>` selects that strategy's cumulative-PnL curve.

```json
{ "scope": "portfolio", "points": [ {"ts":"...","balance_usd":100000.0}, ... ] }
```

### `GET /api/v1/backtests/{id}/trades`

Round-trip trades, ordered by `(strategy_id, symbol, entry_ts)`. `exit_ts` /
`exit_px` are `null` for positions still open at run end.

```json
{ "trades": [ {
  "id": 1, "strategy_id": "Scripted-000", "symbol": "AAPL", "side": "LONG",
  "qty": 100, "entry_ts": "...", "exit_ts": "...", "entry_px": 10.0,
  "exit_px": 12.0, "realized_pnl_usd": 200.0 } ] }
```

### `GET /api/v1/backtests/{id}/orders`

The submitted orders, as a JSON **array** (opaque pass-through of the engine's
order list; quantities are strings, prices numbers — api-ws-redis §7.2). `[]`
when none. `404` for an unknown id.

---

## Hyperopt

NSGA-II walk-forward hyper-parameter studies over the deterministic backtest
engine (hyperopt-metrics spec §6–§9). A study runs the self-written, seeded
NSGA-II where each candidate's `(sharpe, calmar)` objective is the aggregate of
per-fold backtest metrics over a shared read-only bar dataset (locked decision
5). Trials persist to `research.hyperopt_studies` / `research.hyperopt_trials`
(DB source of truth) plus a byte-compatible `runs/hyperopt/<study_ts>/` artifact
tree (`study.json`, `progress.json`, `trials/trial_*.json`, `best_params/`).

`{id}` is the study timestamp `study_ts` (UTC `%Y-%m-%d_%H-%M-%S`, the artifact
directory name and table PK).

### `POST /api/v1/hyperopt`

Enqueue a `hyperopt.run` study job. Body:

```json
{
  "strategy":     "sepa",            // sepa | sector_rotation | pairs | joint (required)
  "start":        "2023-01-02",      // required (YYYY-MM-DD)
  "end":          "2023-12-29",      // required
  "population":   50,                // NSGA-II generation size; default 50
  "generations":  5,                 // generations; default 5
  "seed":         42,                // PRNG seed (deterministic); default 42
  "workers":      0,                 // eval parallelism; default min(cores-2,16)
  "walk_forward": true,              // default true
  "folds":        5,                 // default 5
  "embargo_days": 5,                 // default 5
  "tickers":      ["AAPL","MSFT"],   // SEPA/joint stock universe (or "universe" window)
  "universe":     {"start":"...","end":"...","table":"SF1"},
  "starting_balance": 100000.0,      // USD; default 100000
  "study_ts":     "2026-..._..-..-..", // optional idempotency key
  "actor": "...", "dedupe_key": "...", "max_attempts": 1
}
```

`sepa`/`joint` require a stock universe (`tickers` or `universe`). `202` with the
created `{ "job": <job>, "deduped": <bool> }`. `400` for an unknown strategy /
missing window / missing universe.

### `GET /api/v1/hyperopt[?strategy=<s>&limit=<n>]`

Lists studies newest-first (by `study_ts`), optionally filtered by strategy.

```json
{ "studies": [ {
  "ts": "2026-06-13_12-00-00",
  "config":   { "version":1, "study_name":"...", "strategy":"pairs", "start":"...", "end":"...",
                "directions":["maximize","maximize"], "objectives":["sharpe","calmar"],
                "seed":42, "n_trials":250, "workers":14,
                "walk_forward":{"enabled":true,"folds":5,"embargo_days":5},
                "created_at":"...","updated_at":"..." },
  "progress": { "status":"COMPLETE", "completed_trials":250, "failed_trials":0,
                "running_trials":0, "total_trials":250, "workers":14,
                "started_at":"...", "last_heartbeat_at":"...", "coordinator_pid":1234,
                "current_best":{"trial":22,"sharpe":1.8,"calmar":2.4}, "last_error":null }
} ] }
```

### `GET /api/v1/hyperopt/{id}`

Study detail: `{ "study": { "ts", "config", "progress" } }` (same shape as a list
element). `400` for a malformed `study_ts`; `404` for an unknown study.

### `GET /api/v1/hyperopt/{id}/trials`

Every trial of the study (ascending number), each carrying a `pareto_front`
boolean (non-dominated over `(sharpe, calmar)`, both maximized — weak dominance
with strict improvement, hyperopt-metrics §10) and the per-fold metric breakdown:

```json
{ "trials": [ {
  "number": 7, "optuna_number": 7, "strategy": "pairs",
  "params":  { "lookback": 60, "entry_z": 2.1, "exit_z": 0.5, ... },
  "metrics": { "final_balance_usd": ..., "sharpe": ..., "calmar": ..., ... },
  "folds":   [ {"fold":0, "sharpe":..., "calmar":..., ...}, ... ],
  "state":   "COMPLETE",
  "sharpe":  1.8, "calmar": 2.4,
  "started_at":"...", "finished_at":"...", "duration_sec":9.2,
  "run_dump_ts": null, "error": null,
  "pareto_front": true
} ] }
```

`404` for an unknown study.

### `POST /api/v1/hyperopt/{id}/promote`

Promote a chosen trial's params to `tms.active_params` with full audit
(`promoted_by` / `promoted_at` / `source_study` / `source_trial`), via an
immutable tuned `tms.param_sets` row (the §8.2 metadata-rewritten baseline). For
a `joint` study every sub-strategy (`sepa`, `sector_rotation`, `pairs`) is
promoted. The effect is next-run-only (live processes read params at startup).
Idempotent: re-promoting the same `(study, trial)` reuses the param_set. Body:

```json
{ "trial_id": 22, "actor": "alice" }
```

Response: `{ "study_ts": "...", "trial_id": 22, "promoted": [ {"strategy":"pairs","param_set_id":12,"version":1} ] }`.
`404` for an unknown study; `422` when the trial is missing / not `COMPLETE` /
has no tunable params (`validation` code); `400` for a missing `trial_id`.

### CLI twin: `tms hyperopt`

`tms hyperopt run --strategy pairs --start 2023-01-02 --end 2023-12-29
[--population 50 --generations 5 --seed 42 --workers N --folds 5
--tickers AAPL,MSFT --enqueue]` runs (or enqueues) a study; `tms hyperopt list
[--strategy s]` lists studies; `tms hyperopt promote --study <ts> --trial <n>
[--by <id>]` promotes a trial.

---

## Trade (P5)

The trade cockpit read surface plus the audited command-enqueue endpoint. The
**trading mutation surface stays out of the HTTP API** (read-only forever); the
ONLY write here enqueues an `ops.commands` row that the `tmsgo-live` node executes
under full audit. Reads come from PostgreSQL (the durable truth); Redis is
transport-only. The trade read endpoints return `503` when the API was started
without a trade reader.

> **Back-compat:** these endpoints were formerly `/api/v1/live/*` (and the CLI
> `tms live`). The `live`→`trade` refactor renamed the surface to `/api/v1/trade/*`
> and `tms trade run` / `tms trade preflight`; the old `/api/v1/live/*` read/control
> paths **301-redirect** (query string preserved) to their `/trade/*` equivalents
> so a not-yet-updated client keeps working. The aliases are transitional.

The account-aware reads (orders / fills / positions / account / reconciliation)
take an optional **`account_id=<id>`** query param that filters to a single
registered account (`tms.accounts`); omitted, they return the unfiltered book
(all accounts, incl. unattributed rows). The cockpit/desk account selector is
backed by `GET /api/v1/trade/accounts`.

### `GET /api/v1/trade/session`

The most recent trading session with its active halt (if any):

```json
{
  "id": 12,
  "trader_id": "SIGNAL-001",
  "mode": "signal",          // signal | paper | live (paper/live deferred to P6)
  "status": "RUNNING",       // RUNNING | STOPPED | CRASHED
  "started_at": "2026-06-12T13:30:00Z",
  "ended_at": null,
  "config": { },
  "halt": {                  // null when not halted
    "kind": "manual",        // manual | daily_loss | reconciliation | data | broker | other
    "reason": "operator stop",
    "triggered_at": "2026-06-12T14:05:00Z"
  }
}
```

When no session has ever run: `{ "session": null }`.

### `GET /api/v1/trade/intents?strategy_id=<id>&limit=<n>`

Recent signal intents from `tms.signal_intents`, newest first. `strategy_id`
(`sepa` | `pairs` | `sector_rotation` | `intraday_breakout`) is optional;
`limit` defaults to 100, max 1000.

```json
{ "intents": [
  { "strategy_id": "sepa", "symbol": "AAPL", "state": "buy", "strength": 75.0,
    "generation": 7, "intent": { }, "ts": "2026-06-12T20:00:00Z",
    "ts_event": 1781812800000000000 }
] }
```

`intent` is the unwrapped `SignalIntentUnion` variant (the full per-strategy
payload, snake_case — api-ws-redis.md §5.9).

### `GET /api/v1/trade/health`

The latest portfolio-health snapshot. In signal mode there are no positions, so
the snapshot is the flat-book informational NAV (day P&L 0, no halt — decision
6). Returns `503 {"error":{"code":"no_health",...}}` when no session exists.

```json
{ "day_pnl": 0, "day_pnl_pct": 0, "daily_loss_halt": false,
  "halt_headroom_pct": 0, "concentration_pct": 0, "ts": "2026-06-12T13:30:00Z" }
```

### `GET /api/v1/trade/preflight?mode=&strategy=&tickers=&orb_symbol=&check_opend=&max_stale_days=`

The **go-live preflight** report: the structured precondition checks that must
pass before a paper/live (and, with relaxed severity, a signal) session is
started. Always `200` with the report in the body — a failing preflight is a
valid, expected response (the `ok` bool is the go/no-go bit), not an HTTP error.
`503 {"error":{"code":"unavailable",...}}` when the API has no preflight runner
wired (signal-only deployment).

Query params (all optional): `mode` (`signal`\|`paper`\|`live`, default
`signal`), `strategy` (`sepa`\|`sector_rotation`\|`pairs`\|`orb`\|`multi`,
default `multi`), `tickers` (comma-separated SEPA universe; empty resolves the
default SF1 window universe), `orb_symbol`, `check_opend` (`1`/`true` to probe
OpenD even in signal mode), `max_stale_days` (DATA_CURRENT tolerance, default
`1`).

```json
{
  "mode": "paper", "strategy": "multi", "ts": "2026-06-12T13:30:00Z", "ok": false,
  "checks": [
    { "check": "PG_REACHABLE",            "status": "pass", "severity": "blocker", "detail": "reachable" },
    { "check": "REDIS_REACHABLE",         "status": "pass", "severity": "blocker", "detail": "reachable" },
    { "check": "DATA_CURRENT",            "status": "fail", "severity": "blocker", "detail": "data frontier 2026-05-27 is 9 trading day(s) behind T-1 2026-06-11 (tolerance 1) — run a sync before go-live" },
    { "check": "UNIVERSE_RESOLVABLE",     "status": "pass", "severity": "blocker", "detail": "412-name SEPA universe resolvable for [2025-05-08, 2026-06-11]" },
    { "check": "MARKET_DATA_FUNDAMENTALS","status": "pass", "severity": "blocker", "detail": "401/412 SEPA names have market caps (97%)" },
    { "check": "WARMUP_AVAILABLE",        "status": "pass", "severity": "blocker", "detail": "all 3 strategies have enough warmup bars (45 symbols probed)" },
    { "check": "PARAMS_PROMOTED",         "status": "warn", "severity": "warn",    "detail": "running on un-promoted params: pairs (baseline) — live runs but consider promoting tuned params" },
    { "check": "OPEND_REACHABLE",         "status": "pass", "severity": "blocker", "detail": "OpenD connected, GetGlobalState ok" }
  ]
}
```

Severity is resolved **per mode**: `DATA_CURRENT` and `OPEND_REACHABLE` are
`blocker` for paper/live but advisory (`warn`, and `OPEND` is `skip`ped without
`check_opend`) for signal. `PARAMS_PROMOTED` is always a `warn` (live still runs
on baseline params; the operator is flagged). `ok` is `true` iff **no blocker
check failed**. The same report is what `tms trade run` enforces at startup and
`tms trade preflight` prints. At `tms trade run` startup the gate is **mandatory and
non-overridable for `--mode live`** (real money): `--skip-preflight` is accepted
only for paper/signal and refused with a hard error for a live start.

### `GET /api/v1/watchlist`

The distinct symbols the recent sessions emitted intents for (the tracked
universe): `{ "symbols": ["AAPL", "MSFT", ...] }`.

### `POST /api/v1/trade/commands`

Enqueue an **audited** control command (the audited side channel for the trading
mutation surface). Body:

```json
{ "name": "halt", "reason": "operator stop" }
```

| `name` | args | confirmation |
|---|---|---|
| `start` / `resume` | — | none |
| `stop` | `reason` | none |
| `halt` | `reason` | none (safety action — never blocked) |
| `kill` | `reason` | none (kill switch — never blocked) |
| `set_mode` | `mode` (`signal`\|`paper`\|`live`) | `confirm_token` required for `paper`/`live` |
| `flatten` | `reason` | **`confirm_token` required** — closes ALL positions |
| `emergency_kill` | `reason` | **`confirm_token` required** — halt + flatten + stop |
| `reconcile` | — | none (read-only; broker vs strategy books) |

- `202 { "command_id": 7, "status": "pending" }` on enqueue.
- `412 {"error":{"code":"confirmation_required",...}}` for a `set_mode` to
  `paper`/`live`, or `flatten` / `emergency_kill`, without `confirm_token`.
- `400` for an unknown command or invalid mode.

The `confirm_token` is consumed at the boundary and is **never persisted** (no
secrets in the durable `ops.commands` row). The `tmsgo-live` consumer applies the
command idempotently (halt/resume/kill stop or resume **new-intent emission /
opening orders** + set/clear halt state; in paper/live, `flatten` submits FLAT
market orders closing every open position, `emergency_kill` halts + flattens +
stops, `reconcile` compares broker vs strategy books) and writes a
`tms.audit_log` row for every applied/rejected command.

## Trade trading (P6, paper/live)

The paper/live trading read surface. All reads come from PG (the durable
system-of-record); the cockpit follows the Redis `data.*` streams live and
reconstructs from these on (re)connect. **READ-ONLY** (the trading mutation
surface stays on the audited command channel above).

### `GET /api/v1/trade/orders?symbol=<sym>&limit=<n>&account_id=<id>`

`{ "orders": [ { client_order_id, venue_order_id, strategy_id, symbol, side,
qty, filled_qty, avg_fill_px, status, reason, ts } ] }` — newest first. Prices
are floats (USD); `status` is the order lifecycle state
(`SUBMITTED`/`ACCEPTED`/`PARTIALLY_FILLED`/`FILLED`/`REJECTED`/`CANCELED`).
Optional `account_id` filters to one registered account.

### `GET /api/v1/trade/fills?symbol=<sym>&limit=<n>&account_id=<id>`

`{ "fills": [ { trade_id, symbol, qty, price, commission, ts } ] }` — newest
executions first. Optional `account_id` filters to one registered account.

### `GET /api/v1/trade/positions?account_id=<id>`

`{ "positions": [ { strategy_id, symbol, signed_qty, avg_entry_px,
realized_pnl, status } ] }` — the open (non-flat) position book. Optional
`account_id` filters to one registered account (positions key on
`(account_id, strategy_id, symbol)`); omitted, the full book is returned.

### `GET /api/v1/trade/accounts`

`{ "accounts": [ { id, venue, env, broker_acc_id, label } ] }` — the registered
trading accounts from the `tms.accounts` registry (the first-class account
dimension added in the `live`→`trade` refactor). `env ∈ {sim, simulate, real}`
is the paper-vs-real discriminator. This backs the cockpit/desk **account
selector**; selecting an account drives the `account_id` filter on the reads
above. Returns `503` when the API has no trade reader.

### `GET /api/v1/trade/account`

`{ total_assets, cash, available_funds, market_value, day_pnl, ts }` — the
account / buying-power + day-P&L snapshot. Live buying-power / market-value
ride the Redis `data.AccountUpdate` stream (broker funds); this endpoint derives
day-P&L from the persisted position book.

### `GET /api/v1/trade/reconciliation`

`{ ts, has_issues, tolerance_shares, matched, mismatches: [ { symbol,
strategy_books_sum, broker_net, diff } ], symbols_only_in_strategies,
symbols_only_at_broker }` — the latest reconciliation report (broker positions
vs strategy books). `diff = broker_net − strategy_books_sum`. A mismatch
**halts** the node + surfaces here; it is **never** auto-corrected by trading.
Trigger an on-demand reconcile with the `reconcile` command.

## Manual trading desk (operator-driven)

The **only** broker-mutation surface in the HTTP API. The strategy-driven trade
flow stays out of the API (orders come from strategies + the `flatten` command);
this adds a tightly-gated **discretionary** desk so an operator can place / cancel
/ close orders **by hand** against a paper or live account, independent of the
strategy execution mode (in signal mode the operator *is* the executor; in
paper/live it is an override alongside the auto book). Manual orders are
attributed to a `MANUAL` pseudo-strategy so reconciliation + per-strategy
accounting stay clean.

These endpoints are served by the **live-node process** (it holds the broker
connection), on a separate bearer-guarded listener (`--manual-api-addr`, default
`127.0.0.1:18091`), enabled with `tms trade run --manual-mode paper|live`. When no
manual desk is connected every endpoint returns **503**.

**Topology (single host).** The main API process cannot itself hold a broker
connection, so when `TMS_MANUAL_TRADE_URL` points the API at the live node's manual
listener (compose: `http://tmsgo-live-manual:18091`) the API **reverse-proxies**
`/api/v1/trade/*` onto it — the UI (`/api/proxy/trade/*`) and the e2e suite hit one
host (`:18080`) while the broker-connected live node executes. The proxy adds no
trust: every safety gate runs in the live node's controller and the bearer token is
re-authenticated upstream. When the live node is down (or `TMS_MANUAL_TRADE_URL` is
unset, e.g. a signal-only `app` stack) `/trade/*` returns **503** (no desk
connected). Run the full lifecycle in compose with:

```
docker compose --profile app --profile manual up -d --build --wait
```

which brings up the standalone mock OpenD trading venue (`tms mock-opend`) + a paper
live node with the operator manual desk (`tms trade run --mode signal --manual-mode
paper`) over that mock.

**SAFETY (paramount — this can place real orders):**

- A **live** (real-money) manual order requires the **full 4-factor live
  activation** (real acc id + `TMS_LIVE_CONFIRM` phrase + `UnlockTrade` + the
  `TMS-LIVE-REAL-001` trader id — proven by the desk's live-bound executor)
  **plus** a **per-order** typed confirmation phrase in `confirm_token`
  (`I CONFIRM THIS REAL MONEY MANUAL ORDER`). Missing/wrong ⇒ **412
  `confirmation_required`** and **no order is placed**.
- A **paper** manual order requires the **trade password** in `confirm_token`
  (`TMS_MOOMOO_TRADE_PASSWORD`). Missing/wrong ⇒ **412**.
- **Risk gate:** an **opening** order runs `Portfolio.check` (allocator budget +
  concentration + daily-loss-halt). A violation ⇒ **422 `risk_violation`** and the
  order is rejected **unless** `override: true` is set (an audited operator
  decision → `risk_events` + `audit_log`). A **closing** order bypasses the budget
  (closes always proceed), but a **live** close still requires `confirm_token`.
  The order is priced for the gate from the explicit `limit_price`, else the
  broker/market-data quote, else the manual book's last fill price. An order the
  gate **cannot price at all** (no limit, no quote) **fails closed** (422
  `risk_violation`, rule `risk.unpriced_symbol`) rather than passing at 0 notional;
  supply a `limit_price` or `override`.
- **Broker rejection:** if the order passes the risk gate but the **broker/venue**
  rejects it for a business reason (insufficient buying power, market closed,
  unknown symbol), the response is **422 `order_rejected`** carrying the venue's
  reason — never a 500. No order is placed.
- **Idempotent:** `idempotency_key` makes the client-order-id deterministic; a
  retried request never double-submits at the broker.
- **Audited:** every place / cancel / close writes a `tms.audit_log` row
  (operator, action, symbol, side, qty, override?, live?, ts).

### `POST /api/v1/trade/order`

Place a manual order. Body:

```json
{ "idempotency_key": "op-1712", "symbol": "AAPL", "side": "BUY",
  "qty": 100, "type": "MARKET", "limit_price": 0,
  "override": false, "confirm_token": "…", "reason": "discretionary add" }
```

`side` ∈ `BUY|SELL`; `type` ∈ `MARKET|LIMIT` (LIMIT requires `limit_price > 0`).
Responses: **200** `{ client_order_id, submitted, status:"submitted" }` ·
**400** validation · **412** `confirmation_required` (live confirm / paper
password) · **422** `risk_violation` (gate rejected without `override`) · **422**
`order_rejected` (broker business rejection — buying power / market closed /
unknown symbol; no order placed) · **503** no desk connected.

### `POST /api/v1/trade/order/{coid}/cancel`

Cancel a working manual order by client-order-id. Idempotent (cancelling an
unknown / already-terminal order is a no-op success). The cancel confirms via the
normal order-update push (`CANCELLED_ALL`). **200**
`{ client_order_id, status:"cancel_requested" }` · **501 `cancel_unsupported`**
on a wire build without the modify-order proto (the operator is **never** falsely
told a working real order was cancelled).

### `POST /api/v1/trade/position/{symbol}/close`

Close (flatten) the `MANUAL` position in one symbol. Body
`{ "qty": 0, "confirm_token": "…", "idempotency_key": "…" }` — `qty` omitted/`0` ⇒
full close; a positive `qty` partial-closes (clamped to the open size). Bypasses the
budget (a close), but a **live** close requires `confirm_token` (412 without it).
**Idempotent (no real-money oversell):** the close client-order-id is derived from
the optional `idempotency_key` (when supplied) or otherwise from `(symbol, current
net)` — **never** from a wall clock — so a double-click / client retry / two
operators acting on the same symbol re-derive the **same** coid and dedupe at the
broker (exactly **one** SELL). **200**
`{ client_order_id, submitted, symbol, status:"close_submitted" }`. Closing an
already-flat symbol is an idempotent no-op (`submitted:false`).

### `GET /api/v1/trade/status`

Desk-availability probe on the **actual mutation surface**: **503** when no desk is
connected, **200** `{ connected:true, mode:"paper"|"live", live:false|true }` once a
desk is bound. Clients (the e2e skip-guard, the UI) read **this** to decide whether
the desk is reachable — **not** `GET /api/v1/trade/account`, which reuses the
always-present live-account reader and returns 200 even with no desk connected.

### `POST /api/v1/trade/sync`

**Direction 2 — broker → TMS (the primary case).** The operator trades **directly
in the moomoo app** (no order placed via TMS); they then come back to TMS and call
this to pull the account's **actual** state (`Trd_GetPositionList` +
`Trd_GetOrderList` + `Trd_GetOrderFillList` + `Trd_GetFunds`) and **reflect** it
into TMS, so externally-placed trades show up in the positions/orders/fills/account
views. The synced broker truth is reflected under the **`MANUAL`/EXTERNAL** book
(so the auto-strategy books are not corrupted) and then **reconciled** vs the
strategy books (P6 reconciliation → `reconciliation_reports`) so drift is reported.

- **READ-ONLY at the broker:** this places **no** orders. It is therefore safe in
  **all** modes, **including signal** (a signal-mode operator can sync to see /
  manage what they actually hold) — no per-order confirm or risk gate applies
  because nothing crosses the wire to the venue.
- **Idempotent:** the broker net is reflected as the **delta** vs the current
  `MANUAL` book net per symbol, so **re-syncing the same broker state reflects
  nothing** (no duplicated rows / no double-counted positions). A symbol the book
  holds but the broker no longer reports is driven back to flat (an external close).
- **Audited:** writes a `tms.audit_log` row (operator, action `sync`, ts).

No body required. Responses: **200**
`{ status:"synced", positions_observed, orders_observed, fills_observed,
reflected, read_only:true, reconciliation:{ has_issues, summary, matched,
mismatches } }` · **503** no desk connected.

### `GET /api/v1/trade/account`

Account / buying-power + day-PnL view (alias of `GET /api/v1/trade/account`).

### CLI twins

`tms eod --as-of <YYYY-MM-DD> [...]` runs (or enqueues) the idempotent EOD
engine-replay refresh. `tms trade run --mode signal|paper|live --trader-id <id>
[--strategy ... --tickers ... --moomoo-addr ... --bar-seconds 86400]` runs the
live node (paper/live require the broker creds in `secrets/moomoo.env`). Add
`--manual-mode paper|live [--manual-api-addr 127.0.0.1:18091]` to attach the
operator MANUAL trade desk (serves `POST /api/v1/trade/*`, bearer-guarded by
`TMS_API_TOKEN`); a `live` desk re-runs the full 4-factor activation at connect.
`tms ctl <reconcile|flatten|emergency-kill|halt|resume|stop|kill|set-mode>
[--confirm]` enqueues an audited control command (the CLI twin of
`POST /api/v1/trade/commands`).

`tms trade <place|cancel|close|sync> [--addr http://127.0.0.1:18091]` is the HTTP
client for the manual desk (bearer-guarded by `TMS_API_TOKEN`): `tms trade place
--symbol AAPL --side buy --qty 10 [--type limit --limit 195] [--override]
[--confirm <phrase|trade-pw>] [--key <idem>]`, `tms trade cancel <coid>`, `tms
trade close --symbol AAPL [--qty N] [--confirm …]`, and **`tms trade sync`** — the
one-shot **broker → TMS** sync (read-only pull + reflect + reconcile; places no
orders; safe in all modes incl signal).

---

## `GET /api/v1/ws` — WebSocket event stream

A fan-out of live **job** and **dataset-sync** events, bridged from Redis
pub/sub. Authentication is the bearer token via `?token=` (browser clients
cannot send headers). The Origin must be in the CORS allowlist.

Delivery is **best-effort by design**: the durable job state lives in
PostgreSQL. After a reconnect, clients reconcile via `GET /api/v1/jobs`. A
client whose outbound queue (256 frames) overflows is **evicted** (close code
`1008` policy violation); on server shutdown clients are closed with `1001`
going-away.

Every frame is one JSON text message with the envelope:

```json
{ "type": "hello", "ts": "2024-06-12T15:30:00Z", "payload": { } }
```

| `type` | `payload` | Source |
|---|---|---|
| `hello` | `{ "channels": [...] }` | Sent once on connect — confirms the subscription. |
| `job` | a job `Event` object (below) | Redis channel `tms:jobs:events`. |
| `sync` | a dataset-sync event object | Redis channel `tms:data:sync`. |
| `signal_intent` | `{strategy_id, symbol, intent_json, ts_event, ts_init}` | Redis stream `trader-{id}:stream:data.SignalIntentUpdate`. |
| `strategy_state` | `{strategy_id, state_json, ts_event, ts_init}` | Redis stream `…:data.StrategyStateUpdate`. |
| `portfolio_health` | `{day_pnl, day_pnl_pct, daily_loss_halt, halt_headroom_pct, concentration_pct, ts_event, ts_init}` | Redis stream `…:data.PortfolioHealthUpdate`. |
| `watchlist` | `{symbols, ts_event, ts_init}` | Redis stream `…:data.WatchlistUpdate`. |
| `position` | `{positions, ts_event, ts_init}` (empty in signal mode) | Redis stream `…:data.PositionUpdate`. |
| `order_update` | `{client_order_id, venue_order_id, strategy_id, symbol, side, qty, filled_qty, avg_fill_px, status, reason, ts_event, ts_init}` | Redis stream `…:data.OrderUpdate` (P6 paper/live). |
| `fill_update` | `{trade_id, client_order_id, venue_order_id, strategy_id, symbol, side, qty, price, commission, ts_event, ts_init}` | Redis stream `…:data.FillUpdate` (P6). |
| `live_position` | `{positions:[{strategy_id, symbol, signed_qty, avg_px, realized_pnl}], ts_event, ts_init}` | Redis stream `…:data.LivePositionUpdate` (P6 — full book snapshot). |
| `account_update` | `{total_assets, cash, available_funds, market_value, day_pnl, ts_event, ts_init}` | Redis stream `…:data.AccountUpdate` (P6 — broker funds / buying power). |

The live-stream channels bridge the per-trader Redis **streams**
(`trader-{id}:stream:{topic}`, the canonical key shape — api-ws-redis.md
§2.1/§4.1) into the same WS hub: the server tails each topic with `XREAD BLOCK`
from `$` (only new entries, no history replay), and forwards each entry's
`payload` JSON under the matching `type`. The bridged trader id is
`TMS_LIVE_TRADER_ID` (default `SIGNAL-001`). A missing-`payload` or invalid-JSON
entry is skipped; a Redis read failure keeps the WS open and retries.

The client sends nothing meaningful; the server only reads to detect
disconnects and service control frames.

### `job` payload (`jobs.Event`)

```json
{
  "job_id": 42,
  "kind": "data.refresh",
  "event": "progress",   // enqueued|claimed|progress|succeeded|failed|requeued|released|canceled|cancel_requested|reaped
  "status": "running",   // job status after the transition
  "worker": "worker-1",  // when applicable
  "progress": { },        // for progress events
  "error": "...",         // for failure/cancel events
  "ts": "2024-06-12T15:30:10Z"
}
```

### `sync` payload

Reserved for the dataset-sync engine; same envelope contract. Any well-formed
JSON object published on `tms:data:sync` is forwarded verbatim as the
`payload`. Non-JSON publishes are skipped (the stream is never severed by one
bad message).
```
