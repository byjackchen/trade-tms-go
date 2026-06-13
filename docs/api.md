# TMS API contract (`tms api`)

The UI-facing HTTP + WebSocket API served by `internal/api` and launched with
`tms api`. This document is the **authoritative wire contract** for the UI
builder; the executable companion is `internal/api/handlers_test.go` (every
endpoint, auth, validation and happy path) plus `internal/api/ws_test.go`.

- Container-internal listen address: `:8080` (`TMS_API_ADDR`, default `:8080`).
- Host port: **18080** (compose maps `18080:8080`).
- Base path for the data/jobs/universe API: `/api/v1`.

## Authentication

> **[IMPROVE] — deviation from `docs/spec/api-ws-redis.md §1.1`.** The Python
> reference UI API has *no* authentication (CORS only). This Go control plane
> mutates state (enqueues data refresh / universe rebuild / job cancel jobs),
> so it requires a static bearer token. Trading mutation endpoints remain
> absent, per the spec's [MUST-MATCH] "read-only forever" rule.

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
| `hello` | `{ "channels": ["job", "sync"] }` | Sent once on connect — confirms the subscription. |
| `job` | a job `Event` object (below) | Redis channel `tms:jobs:events`. |
| `sync` | a dataset-sync event object | Redis channel `tms:data:sync`. |

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
