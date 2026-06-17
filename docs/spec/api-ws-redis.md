# API + WebSocket + Redis Transport Specification

This repo's definition of the read-mostly HTTP/WS API: its endpoints, Redis data
model, WebSocket fan-out, and JSON wire shapes. Implemented in Go
(`github.com/byjackchen/trade-tms-go`). The behaviors below are invariants of
this system; where a known weakness is called out, the better behavior this repo
adopts is described alongside it.

---

## 1. Service overview

A single read-mostly HTTP/WS API process ("UI API") that serves:

1. **Live state** read out of Redis, where the live trading node mirrors its Cache + MessageBus.
2. **Backtest artifacts** read from `runs/{ts}/` JSON dumps.
3. **Hyperopt artifacts** read from `runs/hyperopt/{ts}/`.
4. **Live broker truth** via a read-only proxy to the moomoo OpenD gateway.
5. **WebSocket fan-out** of Redis Streams to browser clients.
6. **Strategy params catalog + source switcher** (the only mutating endpoint, a `PUT`).

App metadata: title `"tms UI API"`, description `"Read-only API serving live Redis state + backtest dumps."`, version = `SCHEMA_VERSION` = `"1.0.0"`. `/api/health` reports this version; the UI checks compatibility.

Go deployment serves it on host port **18080** (project port reservation); container-internal port is free choice. The UI receives the base URLs via build-time env (`NEXT_PUBLIC_API_BASE_URL` / `NEXT_PUBLIC_WS_BASE_URL`, `docker-compose.yml:66-71`).

### 1.1 Auth

**There is no authentication of any kind.** CORS is the only restriction; `allow_credentials=False` is explicit "no auth in P-UI v1". The broker proxy intentionally wraps only read-only SDK methods — `place_order`/`cancel_order`/`modify_order` are NOT exposed; the UI is "read-only forever" per planned ADR-005. Do not add trading mutation endpoints. No auth middleware.

### 1.2 CORS:

| Setting | Value |
|---|---|
| allow_origins | `http://localhost:3000`, `http://127.0.0.1:3000` |
| allow_credentials | `false` |
| allow_methods | `GET`, `PUT`, `OPTIONS` |
| allow_headers | `*` |

 Earlier designs hardcoded port 3000. This repo: same allowlist semantics but origins must include the project's UI port `13000` (`http://localhost:13000`, `http://127.0.0.1:13000`); make origins configurable via env with that default. Methods/credentials/headers behavior.

### 1.3 Configuration (env vars)

All resolved per the dependency layer:

| Env var | Default | Used by |
|---|---|---|
| `REDIS_HOST` | `127.0.0.1` | sync + async Redis clients |
| `REDIS_PORT` | `6379` | same |
| `TMS_TRADER_ID` | `PAPER-SMOKE-001` | Redis key namespace |
| `TMS_RUNS_DIR` | `runs` | RunsReader, HyperoptReader (`<runs>/hyperopt`), SourceManager (`<runs>/active_params`) |
| `MOOMOO_HOST` | `127.0.0.1` | BrokerProxy |
| `MOOMOO_PORT` | `11111` | BrokerProxy |
| `MOOMOO_TRADE_PASSWORD` | `""` (empty) | BrokerProxy unlock |
| `BROKER_CACHE_TTL_SEC` | `5.0` | BrokerProxy TTL cache |
| `MOOMOO_PAPER_ACC_ID` | `19072680` | broker REST endpoints; **read at request time** (so tests/ops can change without restart) |
| `TMS_STRATEGY_PARAMS_DIR` | unset | registry "tuned" resolution (via config loader) |

Redis client config: `decode_responses=True`, `socket_connect_timeout=5` seconds, both sync and async. Both clients are process-wide singletons (`@lru_cache(maxsize=1)`). key/value handling is string-based (decoded UTF-8), connect timeout 5 s.

 `MOOMOO_PAPER_ACC_ID` must be re-read from the environment on every request, not cached at startup.

---

## 2. Redis data model

### 2.1 Key patterns (trader-id namespacing)

All live state lives under the `trader-{trader_id}:` prefix:

| Key pattern | Redis type | Content |
|---|---|---|
| `trader-{id}:positions:{position_id}` | LIST | JSON strings; one position lifecycle event per element, chronological (oldest first) |
| `trader-{id}:orders:{order_id}` | LIST | JSON strings; one order lifecycle event per element, chronological |
| `trader-{id}:accounts:{account_id}` | LIST | similar (documented but **not read by any API endpoint**) |
| `trader-{id}:index:*` | sets/hashes | lookup indexes (not read by API) |
| `trader-{id}:stream:{topic}` | STREAM | event streams; see 2.2 |

ID listing: `KEYS trader-{id}:positions:*` / `KEYS trader-{id}:orders:*`, then strip the prefix. The suffix is everything after the prefix — IDs containing `:` are preserved intact (`_suffix_after_prefix`). prefix-strip semantics, including the defensive fallback `key.rsplit(":", 1)[-1]` when the prefix unexpectedly doesn't match.

 `KEYS` is O(entire keyspace) and blocks Redis. Go should use `SCAN` with `MATCH` (cursor iteration) producing the identical result set; ordering of returned IDs is not guaranteed by Redis in either case and no endpoint sorts them — preserve "no defined order" (clients don't rely on it).

### 2.2 Stream entry envelope

The live node runs its message bus with Redis as the database, JSON encoding, and one Redis Stream per topic. Every stream entry's field map contains (at least) the fields `topic` and `payload`; only `payload` is consumed — a JSON document string.

 Entries whose field map has **no `payload` field** are skipped with a warning log (never an error to the client). Entries whose `payload` is not valid JSON:
- `latest_stream_entry`: returns `{"raw": "<payload string>"}`.
- `recent_stream_entries`: skipped with warning.
- WS subscribers: skipped with warning.

 List element JSON parsing (`_safe_json`): invalid JSON → `{"raw": "<raw string>"}`; valid JSON that is not an object → `{"value": <parsed>}`.

### 2.3 The literal `*` suffix fallback

The live node publishes custom Data types to stream keys with a **literal `*` character appended** (e.g. key `trader-X:stream:data.QuoteUpdate*`) when the msgbus topic contains a wildcard. The sync reader therefore resolves topics in two steps:

1. `XREVRANGE trader-{id}:stream:{topic} + - COUNT n` — if non-empty, use it.
2. Otherwise `XREVRANGE trader-{id}:stream:{topic}* + - COUNT n` — note this is an **exact key whose last character is `*`**, NOT a pattern.

 This two-step fallback applies to both `latest_stream_entry` and `recent_stream_entries`, i.e. every snapshot REST read of a stream. The probe is "first result set empty" (XREVRANGE on a missing key returns an empty array).

 The async WS subscribers do **NOT** apply this fallback: `AsyncRedisStreamSubscriber.stream` XREADs only the exact key `trader-{id}:stream:{topic}`, and the multi-stream subscriber discovers keys with `KEYS {prefix}*` (a real pattern this time,). Replicate this asymmetry exactly (see Open questions Q1 for the consequence).

### 2.4 Stream topics consumed by the API

| Topic | Producer | Payload model (§5) |
|---|---|---|
| `data.RegimeUpdate` | RegimeActor | RegimeUpdatePayload |
| `data.MarketCapUpdate` | FundamentalsActor (multi-ticker, one stream) | MarketCapUpdatePayload |
| `data.EarningsBlackoutUpdate` | EarningsActor (multi-ticker; publishes on transitions + first observation) | EarningsBlackoutUpdatePayload |
| `data.StrategyStateUpdate` | BaseSignalRunner per bar (all strategies interleaved) | StrategyStateUpdatePayload |
| `data.SignalIntentUpdate` | BaseSignalRunner | outer `{strategy_id, symbol, intent_json, ts_event, ts_init}` |
| `data.QuoteUpdate` | QuoteActor | QuoteUpdate wire (§5.10) |
| `data.PortfolioHealthUpdate` | PortfolioHealthActor (V3-C) | PortfolioHealthUpdate wire (§5.11) |
| `data.DataIngestionUpdate` | SharadarHealthActor per SPY heartbeat (live only) | DataIngestionPayload |
| `data.BrokerConnectionUpdate` | BrokerHealthActor per SPY heartbeat (live only) | BrokerConnectionPayload |
| `data.ActorStatsUpdate` | each Context Actor per SPY heartbeat | ActorStatsPayload |
| `events.order.{strategy_id}` | order events per strategy | order event blobs |
| `events.position.{strategy_id}` | position events per strategy | position event blobs |
| `events.fills.{instrument_id}` | fills per instrument (instrument_id includes venue, e.g. `AAPL.MOOMOO`) | fill event blobs |
| `events.system.{component}` | component state-change events, one stream per component | SystemEventPayload source dict (§5.6) |

Timestamps: `ts_event` / `ts_init` are **integer nanoseconds since Unix epoch, UTC** throughout. All datetime conversions in the API are `datetime.fromtimestamp(ns/1e9, tz=UTC)` — i.e. UTC, never local time.

---

## 3. REST endpoints

General notes:
- FastAPI error body shape is `{"detail": "<message>"}` for all `HTTPException`s and the broker 503 handler. Pydantic request-validation failures return 422 with FastAPI's standard `{"detail": [...]}` array. the `detail` envelope and status codes; Go error messages should match the literal formats given below (the UI may display them).
- Response models marked with a Pydantic model serialize **all fields**, including `null` optionals (FastAPI default `exclude_none=False`). Endpoints returning raw `list[dict]` pass blobs through untouched.

### 3.1 `GET /api/health`

 → 200 always:

```json
{"status": "ok", "schema_version": "1.0.0", "redis_reachable": true}
```

- `status` is the literal `"ok"` even when Redis is down — reachability is reported only via `redis_reachable` (= Redis `PING` succeeded; any exception → `false`,).

### 3.2 `GET /api/live/positions/strategy`

 Returns `list[dict]` — for every position ID found via `KEYS`, the **last element** of its event list (latest event = current state), JSON-parsed, passed through verbatim. Positions whose list is empty are skipped. Empty Redis → `[]`. No response model; arbitrary event fields are preserved. (including: closed positions are included — caller filters).

### 3.3 `GET /api/live/orders`

 Identical pattern over order keys: latest event per order_id, raw pass-through, `[]` when none.

### 3.4 `GET /api/live/orders/{order_id}/events`

 Full event history, oldest first, from list `trader-{id}:orders:{order_id}` (`LRANGE key 0 -1`).
- 404 `{"detail": "Order not found: {order_id}"}` when the events list is empty **AND** `order_id` is not in the `KEYS`-derived id list. (Since Redis deletes empty lists, an existing key implies non-empty; the double check is defensive.)
- Response model `list[OrderEventPayload]` with `extra="allow"` — every field of every event is preserved in output; the typed fields are validated: `type: str` required; `ts_event: int` required; `ts_init`, `client_order_id`, `strategy_id`, `instrument_id` optional/nullable. A malformed event (e.g. missing `type`) raises a Pydantic error → FastAPI 500. required/optional split; pass-through of unknown fields.

### 3.5 `GET /api/live/positions/{position_id}/events`

 Same as 3.4 for positions. 404 `"Position not found: {position_id}"`. Response model `list[PositionEventPayload]`: ALL typed fields optional/nullable (`type`, `ts_event`, `position_id`, `strategy_id`, `instrument_id`, `signed_qty: float|null`), extras preserved. signed_qty is float-typed here (events may carry `0.0`).

### 3.6 `GET /api/live/context`

 Response model `LiveContext`:

```json
{"regime": "bull"|null, "market_cap": {"TICKER": "123.0"}, "earnings_blackout": {"TICKER": true}, "regime_last_updated": "2026-...T...Z"|null}
```

Algorithm:
1. `regime_payload` = latest entry of `data.RegimeUpdate`; `regime` = its `value` field, or null if stream empty / field missing.
2. `market_cap_entries` = up to 1000 newest-first entries of `data.MarketCapUpdate`; aggregate **first-seen wins per `ticker`** (= latest; because input is newest first; `_aggregate_latest_per_ticker`;; entries without `ticker` skipped).
3. `market_cap` map value = `str(entry.get("value", "0"))` — wire float stringified with shortest-round-trip formatting (e.g. `2.8e+12` for huge values, `123.0` not `123`). See Open questions Q2 for float-format fidelity.
4. `earnings_blackout` analogous with `bool(entry.get("value", False))`.
5. `regime_last_updated` = `ts_event` ns → UTC ISO datetime; on int-conversion failure (TypeError/ValueError) → null.

### 3.7 `GET /api/live/strategies/{strategy_id}/state`

 Reads up to **1000** newest-first entries of `data.StrategyStateUpdate`; first entry with matching `strategy_id` wins.
- `state` = `json.loads(entry.state_json)` if `state_json` is a string, else used as-is; missing key defaults to `"{}"` → `{}`. Invalid JSON in `state_json` → unhandled exception → 500.
- `ts_event` = ns → UTC datetime; conversion failure → epoch 0 (`1970-01-01T00:00:00Z`).
- No match in window → 404 `"No state found for {strategy_id}"`.
- Response model `LiveStrategyState {strategy_id, state: object, ts_event: datetime}`.

### 3.8 `GET /api/live/quotes`

 Reads up to **2000** newest-first entries of `data.QuoteUpdate`; first-seen per `symbol` wins; entries without `symbol` skipped.
- For each kept entry, derive `ts` = ISO 8601 of `ts_event` ns (failure → epoch 0) and validate `{**entry, "ts": iso}` against `QuoteSnapshot` (§5.10): strings `last`/`bid`/`ask`/`prev_close` parsed into Decimal; unknown extra wire fields (`ts_event`, `ts_init`, `topic`…) are **dropped** (Pydantic default ignore-extra). Decimal values serialize as JSON **strings** preserving the original lexical form (Pydantic v2 serializes Decimal as str).
- Any row failing validation → 502 `{"detail": "Malformed quote payload: <pydantic error text>"}`. status; error text format: Go should produce an equivalent descriptive message; byte-equality with Pydantic's error prose is not required.
- Cold start (no entries) → `[]`.
- Result ordering = first-seen order of symbols while scanning newest→oldest (insertion order).

### 3.9 `GET /api/live/signals[?strategy=<id>]`

 Reads up to **2000** newest-first entries of `data.SignalIntentUpdate`.
- Skip entries missing/empty `strategy_id` or `symbol` (falsy check). Optional `?strategy=` filters on exact outer `strategy_id`. Dedup key = `(symbol, strategy_id)`, first-seen wins.
- The kept value is the **unwrapped** `intent_json` (JSON-parse if string, else as-is; default `""` if absent). Unparseable `intent_json` → **immediate** 502 `"intent_json not parseable for {sym}/{sid}: {err}"` (aborts whole response).
- All unwrapped rows are validated against the discriminated union `SignalIntentUnion` (discriminator field `strategy_id` ∈ `sepa|pairs|sector_rotation|intraday_breakout`, §5.9). Any failure → 502 `"Malformed signal payload: <err>"`. Unknown discriminator value rejects (test).

### 3.10 `GET /api/live/portfolio-health`

 Reads **1** newest entry of `data.PortfolioHealthUpdate`.
- Empty stream → 503 `"PortfolioHealthUpdate stream is empty — no live producer running"`. (em-dash included).
- Response `PortfolioHealth` (§5.11): `day_pnl`, `day_pnl_pct`, `halt_headroom_pct`, `concentration_pct` are `str(<wire value>)` — wire carries floats, so output is the shortest-round-trip float-to-string form (see Open questions Q2). `daily_loss_halt` = `bool(...)`. `ts_event` = int ns. `last_update_age_ms = max(0, (now_ns - ts_event) // 1_000_000)` computed at request time. clamp-at-0 and integer floor-division to ms.
- Missing key / bad int → 502 `"Malformed portfolio-health payload: <err>"`.

### 3.11 `GET /api/hyperopt/studies`

(; reader) Scans `<runs>/hyperopt/*/study.json`, **newest first by directory name** (lexicographic descending; dir names are `YYYY-MM-DD_HH-MM-SS`). Non-dirs and dirs without parseable `study.json` (must be a JSON object) are skipped. Each study dict gets `ts = <dirname>` injected (overwriting any existing `ts`). Validated into `HyperoptStudySummary` (§5.12); missing required fields → 500. Missing hyperopt dir → `[]`.

### 3.12 `GET /api/hyperopt/studies/{ts}`

(reader`) 404 `"Hyperopt study not found: {ts}"` when `study.json` absent/unparseable. Otherwise `HyperoptStudyDetail` = summary + `progress` + `trials`:
- `progress` = `progress.json` parsed, or the default object `{status:"UNKNOWN", completed_trials:0, failed_trials:0, running_trials:0, total_trials:<study.n_trials|0>, workers:<study.workers|0>, started_at:null, updated_at:null, last_heartbeat_at:null, coordinator_pid:null, current_best:null, last_error:null}` when the file is missing.
- **Staleness override**: if `progress.status == "RUNNING"` AND heartbeat timestamp (`last_heartbeat_at`, falling back to `updated_at`) parses as ISO (naive → assume UTC) AND `now_utc - ts > 60s` AND the `coordinator_pid` process is **not alive** (POSIX `kill(pid, 0)`; any of PermissionError/ProcessLookupError/OSError → not alive; pid null → not alive), then status is rewritten to `"INTERRUPTED"`. Unparseable/missing timestamp → not stale. threshold `60 s` and the precedence order. (Note: treating `PermissionError` as "not alive" is arguably wrong — a live root-owned process yields EPERM. This repo treats `EPERM` as alive. Wire shape unchanged.)
- `trials` = each `trials/trial_*.json` (sorted ascending by filename), unparseable files skipped, validated as `HyperoptTrial` (§5.12).

### 3.13 `GET /api/hyperopt/studies/{ts}/best-params`

(reader`) Returns `{ "<file stem>": <parsed JSON object> }` for every parseable `*.json` in `<hyperopt>/<ts>/best_params/`, filenames sorted ascending. Missing dir **or zero parseable files** → 404 `"best_params not found for study: {ts}"` (note `result or None`).

### 3.14 `GET /api/backtest/runs`

(reader`) Scans `<runs>/*/meta.json`, newest-first by directory name. Skips non-dirs, missing/unparseable/non-object metas. Each is projected into `BacktestRunSummary {ts, start_date, end_date, total_pnl_usd, strategies, kind}` where `kind` defaults to `"multi-strategy"` when absent (back-compat). Missing any of the other keys → KeyError → 500. Note: `ts` etc. come from the **file contents**, not the directory name — but sort order comes from the directory name. The `runs/hyperopt` and `runs/active_params` subdirectories are naturally excluded because they lack `meta.json`.

### 3.15 `GET /api/backtest/runs/{ts}`

 `meta.json` validated into `BacktestRunMeta {version:int, ts, start_date, end_date, starting_balance_usd:float, final_balance_usd:float, total_pnl_usd:float, strategies:[str]}` (extra keys like `kind` are dropped by the model —, the detail endpoint does NOT return `kind`). 404 `"Run not found: {ts}"`.

### 3.16 `GET /api/backtest/runs/{ts}/orders` and `/positions`

 Raw pass-through of `orders.json` / `positions.json` (must parse as JSON **array**, else treated as missing). 404 `"orders.json not found for {ts}"` / `"positions.json not found for {ts}"`. File contents are arbitrary order/position serializations (see §7.2/§7.3).

### 3.17 `GET /api/backtest/runs/{ts}/equity-curve`

 From `account.json` (list). 404 `"account.json not found for {ts}"` when file missing/not-a-list; `[]` when present but empty. Per entry:
1. `raw_ts = entry.get("ts") or entry.get("ts_event")` — falsy-value fallthrough: `ts` of `""`/`0`/`null` falls through to `ts_event` (the falsy-fallthrough quirk is intentional).
2. `raw_ts` missing → skip entry. Numeric (int/float) → treat as ns epoch → ISO 8601 UTC string (`datetime.fromtimestamp(int(raw)/1e9, UTC).isoformat`, producing `+00:00` offset form); otherwise `str(raw_ts)` as-is.
3. `raw_balance = entry.get("balance_usd") or entry.get("total") or entry.get("balance_total")` — same truthiness chain; a literal `0`/`0.0` balance falls through and, if no other field, the entry is **skipped**. the chain order is load-bearing: a genuine $0 balance is silently dropped; add a code comment + structured log when an entry is skipped due to a falsy-but-present balance.
4. `float(raw_balance)`; failure → skip entry.
5. Sort ascending by the **string** `ts` (lexicographic; ISO strings sort chronologically). The sort is stable.

Output: `[{"ts": "<string>", "balance_usd": <float>},...]` (`EquityPoint`).

### 3.18 `GET /api/backtest/runs/{ts}/account`

 Raw pass-through of `account.json` list. 404 `"account.json not found for {ts}"`.

### 3.19 `GET /api/backtest/runs/{ts}/regime-distribution`

 `regime_samples.json` parsed as object → returned as `{regime_label: count}`. 404 `"regime_samples.json not found for {ts}"` when missing/unparseable/not-an-object. Values are ints (`dict[str,int]` annotation; FastAPI coerces). May legitimately be `{}`.

### 3.20 `GET /api/backtest/runs/{ts}/strategy-summaries`

(reader`) Returns sorted (ascending) `*.json` file stems in `<runs>/{ts}/strategy_summaries/`. Missing dir → `[]` (no 404). Note: stems are the **sanitized** names (see 3.21).

### 3.21 `GET /api/backtest/runs/{ts}/strategy-summaries/{strategy_id}`

(reader`) Filename = `strategy_id` with `:` → `_` and `/` → `_` (matching the dumper's sanitization,). (note: a `/` in a path param requires URL-encoding `%2F` to reach this handler). File must parse as a JSON list; each element validated as `BacktestStrategySummary {ts: datetime, summary: object}`. 404 `"summaries not found for {strategy_id}"`. `ts` is parsed to datetime and re-serialized by FastAPI (ISO 8601; microseconds preserved).

### 3.22 `GET /api/backtest/runs/{ts}/strategies/{strategy_id}/equity-curve`

(reader`) From `strategy_equity/{sanitized_id}.json` (same `:`/`/` sanitization). 404 `"strategy equity curve not found for {strategy_id} in run {ts}"` when missing/not-a-list. Per entry: requires both `ts` and `balance_usd` keys non-None (no fallback variants here, unlike 3.17); `float` failure skips; `ts` stringified verbatim; sort ascending by ts string. the asymmetry with 3.17 (no `ts_event`/`total` fallbacks, no ns conversion).

### 3.23 `GET /api/live/positions/broker`

 Calls `BrokerProxy.get_positions(acc_id)` (§6). `None`/empty DataFrame → `[]`. Per row → `BrokerPosition`:
- `code` = `str(row["code"])`, default `""`.
- `qty` = `int(row["qty"])`, default/parse-failure → `0`.
- `cost_price`, `market_val`, `unrealized_pl`, `position_side`: optional-string extraction — absent column → null; pandas-NA → null; otherwise `str(value)` (`_opt_str`). For Go (no pandas): the broker payload will be whatever the Go moomoo bridge returns; "NA → null, else stringify" semantics.
- Extra SDK columns are dropped (stable schema for TS codegen).

### 3.24 `GET /api/live/account`

 Calls `BrokerProxy.get_account_balance(acc_id)` → a flat map (first row of the SDK accinfo DataFrame,). Projects exactly `cash`, `total_assets`, `market_val`, `power`, `currency` with normalization: key absent → null; value `None` → null; `str(value).strip` equal to `""` or `"N/A"` → null; else the stripped string. (test: NaN/N-A currency coerced to null).

### 3.25 `GET /api/live/reconciliation`

 Compares the strategy (Redis) books vs broker positions.

Strategy side:
- For each position id: latest event; `strategy_id = str(ev.get("strategy_id", "?"))`; symbol = `instrument_id` (default `"?"`) split on first `.` (strips venue: `AAPL.MOOMOO` → `AAPL`); `signed_qty = int(ev.get("signed_qty", 0))` — parse failure skips the event; `signed_qty == 0` (closed) skipped; sum per `(strategy_id, symbol)`.

Broker side: `broker_positions_from_moomoo`:
- empty/None frame → `{}`; missing `position_side` column on non-empty data → `ValueError` (→ 500) with message listing expected columns; per row: empty `code` skip; strip leading `"US."`; `int(qty)` failure or 0 → skip; `position_side` upper-cased `== "SHORT"` → negate; sum per symbol.

Core compare `reconcile`:
- Aggregate strategy books per symbol (drop zero entries); union of symbols sorted ascending; per symbol with `s_sum` / `b_net` / `diff = b_net - s_sum`:
 - `s_sum != 0 and b_net == 0` → `symbols_only_in_strategies`
 - `s_sum == 0 and b_net != 0` → `symbols_only_at_broker`
 - `abs(diff) <= tolerance (0)` → `matched`
 - else → mismatch `{symbol, strategy_books_sum, broker_net, diff}`
- `has_issues` = any of the three issue lists non-empty.

Response (`ReconciliationResponse`): `ts` = request-time `datetime.now(UTC).isoformat`, plus the four lists + `has_issues`. List order: symbols ascending within each category (consequence of sorted union).

### 3.26 `GET /api/system/data-ingestion`

 Latest entry of `data.DataIngestionUpdate`. Empty stream → HTTP 200 body `null`. (literal JSON `null`, not 404). Validation into `DataIngestionPayload` (§5.4); numeric strings are coerced to int (Pydantic lax mode). Failure → 502 `"Malformed DataIngestionUpdate payload: <err>"`.

### 3.27 `GET /api/system/broker-connection`

 Same pattern for `data.BrokerConnectionUpdate` → `BrokerConnectionPayload` (§5.5). Empty → `null`; malformed → 502 `"Malformed BrokerConnectionUpdate payload: <err>"`.

### 3.28 `GET /api/system/actor-stats`

 Up to **1000** newest-first entries of `data.ActorStatsUpdate`; first-seen per `actor_name` wins; entries with no `actor_name` skipped. Each validated into `ActorStatsPayload`; **malformed entries are skipped with a warning log** (not 502 — asymmetric with 3.26/3.27; deliberate). Empty stream → `{"actors": []}`. Output order = first-seen order over the newest-first scan.

### 3.29 `GET /api/strategies/registered`

(; reader) Scans the **baseline params dir** (`internal/hyperopt/baseline/*.json`; this directory ships with the binary/repo — current files: `intraday_breakout.json`, `pairs.json`, `sector_rotation.json`, `sepa.json`), filenames sorted ascending. Per file → `RegisteredStrategy`:

| Field | Source | Rule |
|---|---|---|
| `name` | JSON `strategy` field | must be non-empty string, else file skipped w/ warning |
| `display_name` | derived | override map `{"sepa": "SEPA"}`; else `snake_case` → `Title Case` via replace(`_`,` `) + title-casing |
| `description` | `display.description` | only if a string, else null |
| `capital_pct` | `allocation.capital_pct` | `float` only if int/float typed, else null |
| `active` | `allocation.active` | `bool(...)`, default false (also when no allocation block) |
| `parameters_count` | `len(parameters)` | 0 if not a dict |
| `schema_version` | `schema_version` | `int(...)`, fallback 1 on any failure |
| `params_source` | env dir check | `"tuned"` iff `TMS_STRATEGY_PARAMS_DIR` resolves AND `<env_dir>/<strategy>.json` exists, else `"baseline"` (per-strategy) |

Malformed baseline JSON files are skipped with a warning. Missing baseline dir → `[]`. all rules; note `params_source` keys off the JSON `strategy` field, not the filename. Env dir resolution happens **per request** (not cached) unless explicitly injected.

### 3.30 `GET /api/strategies/registered/{name}`

 404 `"Unknown strategy: {name}"` when `<baseline>/<name>.json` doesn't exist (filename lookup this time). Detail = summary fields + `parameters`: read from `<env_dir>/<name>.json` when it exists else baseline; malformed active JSON → warn + fall back to baseline content; `parameters` field must be a dict else `{}`. including the baseline-fallback on malformed tuned file.

### 3.31 `GET /api/strategies/registered/{name}/available-sources`

 404 `"Unknown strategy: {name}"` if not registered. Response `StrategySourceList`:
- `options[0]` is always `{source_id:"baseline", kind:"baseline", label:"Baseline"}` with null metrics.
- Then, for each hyperopt study **newest-first** (list_studies order): include iff `best_params/<strategy>.json` exists and parses (membership in `get_best_params(ts)`). Option fields: `source_id = "hyperopt:<ts>"`; metrics from `progress.current_best` — `sharpe`/`calmar` only when numerically typed; `best_trial` from `current_best.trial` only when int; `label` = `"<ts>"` or `"<ts> (Sharpe %.2f, Calmar %.2f)"` (each metric included only when present; joined with `", "`; rounding is round-half-even at the binary level via Go `fmt.Sprintf("%.2f")`); `created_at` from study.json.
- `active` resolution: no `<active_dir>/<strategy>.json` → `"baseline"`; malformed JSON → `"external"`; `metadata.tuned_from_study` missing/not-string → `"external"`; study name must match regex `^hyperopt-([^-]+(?:_[^-]+)*)-(?P<ts>\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})$` to extract ts, else `"external"`; if `hyperopt:<ts>` is among the computed options → that id, else `"external"`. the regex (strategy names with hyphens won't match — by design).

### 3.32 `PUT /api/strategies/registered/{name}/source`

 Body: `{"source": "<id>"}`.
- 404 `"Unknown strategy: {name}"` if not registered.
- Parse source_id: `"baseline"` → delete `<active_dir>/<name>.json` if present (idempotent); `"hyperopt:<ts>"` with non-empty ts → copy `<hyperopt>/<ts>/best_params/<name>.json` into the active dir **atomically** (copy to `<name>.tmp` then rename/`os.replace`; on failure remove tmp, keep pre-existing target). Source file missing → `FileNotFoundError` → 404 with message `"hyperopt best_params not found: <full path>"`. Any other shape (incl. `"hyperopt:"` empty ts) → `ValueError` → **422** with message starting `"invalid source_id"`. status mapping (404 vs 422) and atomic write.
- Success → returns the refreshed `StrategySourceList` (same as 3.31).

This is the **only mutating endpoint** in the API.

---

## 4. WebSocket endpoints

### 4.1 Single-stream bridge (11 endpoints)

All use `stream_to_websocket` over `AsyncRedisStreamSubscriber`:

| WS path | Redis topic |
|---|---|
| `/api/live/stream/orders/{strategy_id}` | `events.order.{strategy_id}` |
| `/api/live/stream/positions/{strategy_id}` | `events.position.{strategy_id}` |
| `/api/live/stream/fills/{instrument_id}` | `events.fills.{instrument_id}` |
| `/api/live/stream/regime` | `data.RegimeUpdate` |
| `/api/live/stream/quotes` | `data.QuoteUpdate` |
| `/api/live/stream/signals` | `data.SignalIntentUpdate` |
| `/api/live/stream/strategy-state` | `data.StrategyStateUpdate` |
| `/api/live/stream/portfolio-health` | `data.PortfolioHealthUpdate` |
| `/api/live/stream/data-ingestion` | `data.DataIngestionUpdate` |
| `/api/live/stream/broker-connection` | `data.BrokerConnectionUpdate` |
| `/api/live/stream/actor-stats` | `data.ActorStatsUpdate` |

Protocol:
- Server accepts the WS handshake immediately, then tails the stream.
- **Frame = one text message per Redis stream entry**, body = the entry's `payload` JSON re-serialized (`json.dumps(payload, default=str)` — non-JSON-native values such as datetimes/Decimals fall back to their string form). There is **no envelope** — the frame is the raw payload object. Frames preserve stream order (test).
- Subscription starts at **`$`** (only entries appended after subscribe; no history replay). `"0-0"` replay exists in code but no endpoint exposes it. default `$`. Go may add an optional `?start=0-0` query for history replay, off by default.
- Tail loop: `XREAD BLOCK 1000 STREAMS <key> <last_id>`; empty result → loop again (idle steady state). `block_ms` default 1000.
- Entry handling: missing `payload` field → skip + warn; invalid JSON payload → skip + warn (one bad publish must not sever the stream).
- Error handling: client disconnect → log info, exit quietly. Any other exception → log with traceback, attempt `ws.close(code=1011)`, swallow secondary errors. close code 1011.
- Redis read failure: first failure logs at error-with-traceback, consecutive failures log at warning level; 1 s sleep backoff between retries; counter resets on success. The WS connection **stays open** during Redis outage. stay-open + backoff; log-level throttling is -grade (replicate the spirit: no traceback spam).
- `last_id` advances to each delivered entry id, including across blocking iterations.

### 4.2 Fan-in system events: `/api/live/stream/system`

(`AsyncMultiStreamSubscriber`; `) Merges all `events.system.*` component streams into one WS.

- Key discovery: `KEYS trader-{id}:stream:events.system.*` at most once per **5.0 s** (`_DISCOVERY_INTERVAL_SEC`). New streams are picked up within one window; deleted keys are pruned from the cursor map (so a recreated stream starts fresh at `start`); new streams start at `$`. prune+restart semantics. use `SCAN` instead of `KEYS`.
- No matching streams → sleep 0.5 s and re-check.
- Poll: `XREAD BLOCK 1000` over **all** current keys with per-key last-ids. Discovery failure / read failure → throttled logs + 1 s backoff, same as 4.1.
- Suffix derivation: portion of the stream key after the prefix `trader-{id}:stream:events.system.`; fallback `rsplit(".",1)[-1]` if the prefix doesn't match.
- Frame: JSON of `SystemEventPayload`:

```json
{"component": "<payload.component_id || suffix>", "component_type": "<payload.component_type || suffix>", "state": "<payload.state || \"UNKNOWN\">", "ts_event": <int(payload.ts_event || 0)>, "ts_init": <int|null>, "event_id": "<uuid|null>"}
```

 Serialized with `json.dumps(model_dump, default=str)`. Note this endpoint **does** apply a typed envelope, unlike 4.1. The source entries carry `ComponentStateChanged.to_dict` keys: `type`, `component_id`, `component_type`, `state` (e.g. `RUNNING`/`STOPPED`/`READY`), `config`, `event_id` (UUID), `ts_event`, `ts_init`; `type`/`config` are dropped.
- Disconnect/error handling identical to 4.1 (info log / 1011 close).

---

## 5. JSON schema reference (field tables)

`SCHEMA_VERSION = "1.0.0"`. The OpenAPI document additionally injects all WS payload models into `components.schemas` (RegimeUpdatePayload, MarketCapUpdatePayload, EarningsBlackoutUpdatePayload, StrategyStateUpdatePayload, SystemEventPayload, OrderEventPayload, PositionEventPayload, ActorStatsPayload, DataIngestionPayload, BrokerConnectionPayload) so the UI's TS codegen sees them. if Go serves OpenAPI for the UI codegen; otherwise generate equivalent TS types by other means.

### 5.1 RegimeUpdatePayload
| field | type | notes |
|---|---|---|
| value | str | `"bull"|"bear"|"neutral"|"warning"` |
| ts_event | int | ns epoch |
| ts_init | int | ns epoch |

### 5.2 MarketCapUpdatePayload: `ticker: str`, `value: float` (market cap USD), `ts_event: int`, `ts_init: int`.

### 5.3 EarningsBlackoutUpdatePayload: `ticker: str`, `value: bool`, `ts_event: int`, `ts_init: int`.

### 5.4 DataIngestionPayload: `source: str` (e.g. `"sharadar"`), `fetch_count: int`, `cache_hit_count: int`, `cache_miss_count: int`, `last_fetch_ts: int` (ns; 0 = never), `ts_event: int`, `ts_init: int`.

### 5.5 BrokerConnectionPayload: `connected: bool`, `last_ping_ts: int` (ns; 0 = none), `quote_context_alive: bool`, `trade_context_alive: bool`, `ts_event: int`, `ts_init: int`.

### 5.6 SystemEventPayload: `component: str`, `component_type: str`, `state: str`, `ts_event: int`, `ts_init: int|null`, `event_id: str|null`.

### 5.7 ActorStatsPayload: `actor_name: str` (`"regime"|"fundamentals"|"earnings"`), `publish_count: int`, `last_publish_ts: int` (ns; 0 = never), `last_value_json: str` (opaque JSON string; shape per actor — regime: JSON-encoded string or `"null"`; fundamentals: `{"TICKER": <float>}`; earnings: `{"TICKER": <bool>}`;), `ts_event: int`, `ts_init: int`. `ActorStatsResponse = {actors: [ActorStatsPayload]}`.

### 5.8 StrategyStateUpdatePayload: `strategy_id: str`, `state_json: str` (opaque JSON), `ts_event: int`, `ts_init: int`.

### 5.9 SignalIntent discriminated union

Shared base fields (all variants): `symbol: str`; `strategy_id` (discriminator); `state` ∈ `no_setup|forming|buy|hold|exit|stop_hit`; `strength: float` constrained **0 ≤ x ≤ 100** (validation error outside range → endpoint 502); `proximity_to_trigger_pct: float|null` (default null); `updated_at: datetime` (ISO 8601 in JSON); `generation: int ≥ 0`.

| Variant (`strategy_id`) | Extra fields (defaults) |
|---|---|
| `sepa` | `grade:int=0`, `trend_template_pass:bool=false`, `base_age_days:int?`, `base_depth_pct:float?`, `volume_dryup:bool?`, `pivot_price:Decimal?`, `stop_price:Decimal?`, `rs_rank:int?` |
| `pairs` | `pair_id:str=""`, `leg_role:"long"|"short"="long"`, `z_score:float=0`, `z_entry_threshold:float=2.0`, `z_exit_threshold:float=0.5`, `hedge_ratio:float=1.0`, `half_life_days:float=0` |
| `sector_rotation` | `momentum_score:float=0`, `rank:int=0`, `target_weight:float=0`, `current_weight:float=0` |
| `intraday_breakout` | `orb_high:Decimal?`, `orb_low:Decimal?`, `atr_at_open:Decimal?`, `entry_window_end:datetime?` |

 Decimal fields accept JSON numbers or strings on input and serialize as **strings** on output (Pydantic v2 Decimal convention; test). Unknown `strategy_id` → validation error → 502.

### 5.10 QuoteSnapshot — REST output of `/api/live/quotes`

| field | type | wire source |
|---|---|---|
| symbol | str | `symbol` |
| last | Decimal (JSON string) | str `last` |
| bid, ask | Decimal\|null | str `bid`/`ask` |
| volume | int | int |
| change_pct | float | float |
| prev_close | Decimal | str |
| market_session | `pre|regular|post|closed` | str |
| ts | datetime (ISO) | derived from `ts_event` ns by the endpoint |
| generation | int | process-monotonic counter for client ordering |

The **WS** `/api/live/stream/quotes` carries the raw wire shape (strings + `ts_event`/`ts_init`, no `ts`), NOT QuoteSnapshot. this REST-vs-WS difference.

### 5.11 PortfolioHealth — REST: `day_pnl: str`, `day_pnl_pct: str`, `daily_loss_halt: bool`, `halt_headroom_pct: str`, `concentration_pct: str`, `ts_event: int` (ns), `last_update_age_ms: int`. Wire (WS + stream) carries floats. Semantics: `day_pnl_pct` fraction of NAV at first-bar-of-day; `daily_loss_halt` true below the −5%-NAV default threshold; `halt_headroom_pct` clamped at 0 when halted; `concentration_pct` = largest single-symbol net exposure / NAV.

### 5.12 Hyperopt models

`HyperoptStudySummary`: `ts, version:int, study_name, strategy, start, end, directions:[str], objectives:[str], seed:int, n_trials:int, workers:int, walk_forward:object, created_at, updated_at` (all string dates ISO).
`HyperoptProgress`: `status:str` (`RUNNING|UNKNOWN|INTERRUPTED|...`), `completed_trials:int, failed_trials:int, running_trials:int, total_trials:int, workers:int`, nullable `started_at, updated_at, last_heartbeat_at, coordinator_pid:int?, current_best:object?, last_error:str?`.
`HyperoptTrial`: `number:int, strategy:str, params:object, metrics:object, folds:[object], state:str` (e.g. `FAIL`), `started_at:str, finished_at:str?, duration_sec:float, run_dump_ts:str?, error:str?`.
`HyperoptStudyDetail` = summary + `progress` + `trials`.

### 5.13 Broker / registry / source models — see §3.23-3.25, §3.29-3.32 field tables; full Pydantic definitions at.

Unused-but-declared models (`LivePosition`; `LiveOrder`; `LiveAccount`) are not referenced by any route; do not implement endpoints for them.

---

## 6. BrokerProxy semantics

 The API holds its own moomoo SDK connection (separate from the trading node) to the same OpenD daemon.

| Parameter | Default | Cite |
|---|---|---|
| cache_ttl_sec | 5.0 (env `BROKER_CACHE_TTL_SEC`) |
| connect_timeout_sec | 10.0 |
| connect_cooldown_sec | 30.0 |
| request_timeout_sec | 10.0 |

 behaviors:
1. **Lazy connect** on first broker request, never at startup — the API must boot and serve all non-broker endpoints with OpenD down.
2. **Connect mutex**: concurrent first-requests must not open two broker connections (asyncio.Lock). Double-check after acquiring.
3. **Bounded connect** (`connect_timeout_sec`); on timeout or any error: record failure time + reason, raise `BrokerUnreachableError`.
4. **Failure cooldown**: within `connect_cooldown_sec` of the last failure, requests fast-fail with 503 `"BrokerProxy: OpenD unreachable {N}s ago ({reason}); cooling down {M}s before retrying"` without touching the SDK. Successful connect clears the cache. Checked both before and inside the lock.
5. **Per-call timeout** (`request_timeout_sec`) on balance/positions; timeout → `BrokerUnreachableError` `"BrokerProxy: {what} timed out after {N}s — OpenD may be unreachable"`.
6. **TTL response cache** keyed `balance:{acc_id}` / `positions:{acc_id}`; entry valid for `cache_ttl_sec` from write (strictly `now - ts > ttl` → miss); cache hit skips connect entirely.
7. **503 mapping**: `BrokerUnreachableError` → HTTP 503 `{"detail": "<message>"}` via a global exception handler, not 500.
8. If an unlock password is configured (non-empty), `trade_unlock(password)` runs right after connect.
9. Trade environment: `TrdEnv.SIMULATE` is the SDK default used for both queries — paper account.

 A failure cache stops unbounded SDK retry threads (~27k threads observed in earlier designs). In Go, a context-cancelled dial doesn't leak goroutines the same way, but keep the cooldown anyway: it protects OpenD from request storms and keeps 503 latency flat. Go must still never cancel-and-retry-tight-loop.

Note: the upstream moomoo SDK has no native Go binding, so this repo needs a broker bridge (e.g. sidecar process or FFI) exposing `get_account_balance(acc_id) -> map`, `get_positions(acc_id) -> rows`; the proxy semantics above apply regardless of transport.

---

## 7. `runs/{ts}/` artifact formats (backtest dumps)

Producer: (`write_run_dump`). Directory name = UTC `time.Now.UTC.Format("2006-01-02_15-04-05")` equivalent (`%Y-%m-%d_%H-%M-%S`). `SCHEMA_VERSION = 1`. All files written with `json.dumps(..., indent=2, default=str)` — 2-space indent, non-serializable values stringified. for the Go dumper if/when it writes runs; the API reader must accept this format.

```
runs/{ts}/
 meta.json
 orders.json
 positions.json
 account.json
 regime_samples.json
 strategy_summaries/{sanitized_strategy_id}.json
 strategy_equity/{sanitized_strategy_id}.json # only when non-empty (Q1.2)
```

Filename sanitization: strategy_id with `:` → `_`, `/` → `_`. `strategy_equity/` is only created when there is at least one strategy with points.

### 7.1 meta.json (real sample `runs/2026-05-13_16-10-49/meta.json`)

```json
{
 "version": 1,
 "ts": "2026-05-13_16-10-49",
 "start_date": "2025-01-01",
 "end_date": "2025-12-31",
 "starting_balance_usd": 100000.0,
 "final_balance_usd": 105247.31,
 "total_pnl_usd": 5247.309999999998,
 "strategies": ["SEPA-000", "SectorRotation-001", "Pairs-002"],
 "kind": "multi-strategy"
}
```

`kind` values observed: `"multi-strategy"` (default), `"multi-strategy-universe"`, `"smoke-{strategy}"`, `"smoke-intraday"`. Old dumps may lack `kind` → list endpoint defaults it.

### 7.2 orders.json — JSON array of order serializations. Observed fields (sample run): `trader_id, strategy_id, instrument_id, client_order_id, venue_order_id, position_id, account_id, last_trade_id, type ("MARKET"), side ("BUY"/"SELL"), quantity (string!), time_in_force, is_reduce_only, is_quote_quantity, filled_qty (string), liquidity_side, avg_px (number), slippage (number), commissions (["0.00 USD"]), emulation_trigger, status ("FILLED"), contingency_type, order_list_id (null), linked_order_ids, parent_order_id, exec_algorithm_id, exec_algorithm_params, exec_spawn_id, tags, init_id (uuid), ts_init (ns int), ts_last (ns int)`. The API passes these through opaquely — do not re-type; quantities are strings, prices numbers, exactly as dumped.

### 7.3 positions.json — JSON array of position serializations. Observed fields: `position_id, trader_id, strategy_id, instrument_id, account_id, opening_order_id, closing_order_id, entry ("BUY"), side ("FLAT"/"LONG"/"SHORT"), signed_qty (number), quantity (string), peak_qty (string), ts_init/ts_opened/ts_last/ts_closed (ns ints), duration_ns (int), avg_px_open/avg_px_close (numbers), quote_currency, base_currency (null), settlement_currency, commissions (array of strings), realized_return (number), realized_pnl ("10579.14 USD")`. Pass-through.

### 7.4 account.json — array of `{"ts": "<ISO 8601 with +00:00>", "balance_usd": <float>}` produced by `account_history_from_cache`: one point per account-state event carrying a USD balance; `ts` = `ts_event` ns → UTC `.isoformat` (offset form `+00:00`, no `Z`). Older dump versions may instead carry `ts_event` (ns int) and/or `total`/`balance_total` balances — the equity-curve endpoint supports those variants (§3.17), the raw `/account` endpoint passes through whatever exists.

### 7.5 regime_samples.json — object `{"<regime label>": <int count>}`; may be `{}`.

### 7.6 strategy_summaries/{id}.json — array of `{"ts": "<ISO 8601 UTC>", "summary": {<opaque per-strategy state_summary dict>}}`. Backtests write exactly one end-of-run sample. Summary shape varies per strategy (e.g. SEPA: `active_set`, `active_count`, `tracked_count`, `subscription_cap`, `active_cap`).

### 7.7 strategy_equity/{id}.json — array of `{"ts": "<ISO 8601 +00:00>", "balance_usd": <float>}` (cumulative realized P&L in USD; NOT account balance). Absent when a strategy had no closed positions or run predates Q1.2.

### 7.8 `runs/hyperopt/{ts}/` layout (read-only for the API)

```
runs/hyperopt/{ts}/
 study.json # HyperoptStudySummary fields minus ts (ts injected from dirname)
 progress.json # HyperoptProgress fields
 trials/trial_%04d.json
 best_params/{strategy}.json # only after a study produces a best
 optuna_journal.log # ignored by API
```

`best_params/{strategy}.json` shape (real sample `runs/hyperopt/2026-05-08_00-18-38/best_params/sepa.json`): `{strategy, schema_version, metadata: {source: "tuned", created_at, tuned_from_study: "hyperopt-<strategy>-<ts>", tuned_from_trial:int}, parameters: {<name>: {default, type, search:{low,high}, description}}}`. The `metadata.tuned_from_study` string is what `get_active` parses (§3.31).

### 7.9 `runs/active_params/{strategy}.json` — verbatim copies of best_params files installed by the PUT endpoint (§3.32); consumed by the strategy params loader via `TMS_STRATEGY_PARAMS_DIR=<runs>/active_params` (docker-compose.yml:42).

---

## 8. Signal-mode vs trade-mode differences

Modes are a property of the **trading node**, not the API process. The API code has zero mode branches — observable differences are emergent from what the node publishes. keep the API mode-agnostic.

| Aspect | signal mode | paper / live (trade) mode |
|---|---|---|
| Exec client | none registered (`exec_clients = {}`) | moomoo exec client (paper flag `is_paper = (mode=="paper")`) |
| Password / acct | password ignored; no `PAPER_ACC_ID` needed | non-empty `MOOMOO_TRADE_PASSWORD` + `PAPER_ACC_ID` required, else startup ValueError |
| trader_id default | `PAPER-SMOKE-001` | paper: `PAPER-SMOKE-001`; live: `TMS-LIVE-REAL-001`; `TMS_TRADER_ID` env overrides |
| Orders submitted by strategies | fail at the RiskEngine (no venue route) — **by design**; operator trades manually from cockpit signals | routed to broker |
| Consequence for API | `trader-{id}:orders:*` / `positions:*` keys and `events.order.*` / `events.position.*` / `events.fills.*` streams stay **empty** → `/api/live/orders`, `/api/live/positions/strategy` return `[]`; order/position WS streams are silent; `/api/live/reconciliation` has empty strategy books (only `symbols_only_at_broker` can be non-empty) | fully populated |
| Data streams (`data.*`: regime, quotes, signals, strategy-state, actor stats, ingestion, broker-connection, portfolio-health) | published normally — this is the whole point of signal mode | same |
| Broker REST endpoints (`/api/live/account`, `/positions/broker`, `/reconciliation`) | still work — the BrokerProxy has its own OpenD connection independent of node mode | same |

 trader-id coupling: the API must be configured with the same `TMS_TRADER_ID` namespace as the node; live mode's distinct default (`TMS-LIVE-REAL-001`) means the operator must align the API's `TMS_TRADER_ID` to see real-money streams.

Redis wiring on the node side (context for the Go system): `TMS_USE_REDIS=1` default; cache `encoding=json, flush_on_start=False`; msgbus `encoding=json, stream_per_topic=True`; Redis `connection_timeout=5, response_timeout=5`.

---

## 9. Logging / robustness requirements (cross-cutting)

- All skip/malformed paths described above log warnings with enough context (topic, entry id, actor name) — never crash the request (multiple cites above). the no-crash semantics; Go uses structured logging (slog).
- WS handlers must never take the server down: every handler catches all errors, attempts a 1011 close, and swallows secondary failures.
- Redis outage during WS streaming: stay connected, retry with 1 s backoff, throttle logs (§4.1).
- No panics in normal paths; context cancellation must terminate WS tails within ~`block_ms`: cancellation is responsive because the XREAD block window is 1 s. Go uses XREAD with a 1 s block inside a context-checked loop, or a cancellable blocking read.

---

## 10. Endpoint inventory checklist (completeness gate)

REST (33): health; live: positions/strategy, orders, orders/{id}/events, positions/{id}/events, context, strategies/{id}/state, quotes, signals, portfolio-health, positions/broker, account, reconciliation; hyperopt: studies, studies/{ts}, studies/{ts}/best-params; backtest: runs, runs/{ts}, orders, positions, equity-curve, account, regime-distribution, strategy-summaries, strategy-summaries/{id}, strategies/{id}/equity-curve; system: data-ingestion, broker-connection, actor-stats; strategies: registered, registered/{name}, registered/{name}/available-sources, PUT registered/{name}/source.

WS (12): orders/{strategy_id}, positions/{strategy_id}, fills/{instrument_id}, regime, quotes, signals, strategy-state, portfolio-health, data-ingestion, broker-connection, actor-stats, system.

Anything absent from this list is out of scope and must not be silently invented; anything on this list must exist.

---

## 11. Open questions

1. **WS `*`-suffix asymmetry.** The sync reader falls back to the literal `*`-suffixed stream key (e.g. `…:stream:data.QuoteUpdate*`) but the async WS subscribers XREAD only the bare key (vs). If the live node actually publishes quotes to the `*`-suffixed key, the WS endpoints for those topics would never deliver frames while the REST snapshots work. Is this a latent bug, or do live publishes land on the bare key (and only some topics get the `*` form)? Recommendation: verify against a live node's keyspace; implement the asymmetry as-is, and consider making the WS subscriber probe both keys.
2. **Float-to-string fidelity.** Several REST fields are produced by stringifying a wire float (LiveContext.market_cap §3.6; PortfolioHealth §3.10), and JSON floats are emitted via the shortest-round-trip algorithm (e.g. `5247.309999999998` in meta.json). Go's `strconv.FormatFloat(v,'g',-1,64)` produces the same shortest-round-trip digits but differs in exponent/format edge cases (`100000.0` with a trailing `.0` vs Go `'g'` giving `100000` without `.0`). Decide: is byte-equality required for these strings, or only numeric equality? If byte-equality, the two stringify sites need a fixed formatter.
3. **`PermissionError` in pid-aliveness** (§3.12): classifying EPERM as "process not alive" can mark a study INTERRUPTED while its coordinator is alive under another uid. This repo treats EPERM as alive.
4. **Equity-curve falsy-balance skip** (§3.17): a real `balance_usd: 0.0` entry is dropped from the curve. This is a deliberate invariant — confirm whether a behavior change (keeping the entry) is wanted instead.
5. **`/api/live/orders/{id}/events` 500 on missing `type`** (§3.4): order events lacking `type` or `ts_event` produce a 500. Is a 502 with a clear message preferred in Go (would deviate)?
6. **OpenAPI surface**: does the Go port need to serve a FastAPI-compatible `/openapi.json` (the Next.js `pnpm codegen` consumes it)? If yes, the injected WS payload schemas (§5) must appear under `components.schemas` with identical names.
7. **moomoo bridge**: the broker endpoints depend on the upstream moomoo SDK (no native Go binding). Confirm the planned Go-side transport (sidecar vs skip-in-P0) — the 503 cooldown semantics in §6 apply either way.
8. **`MOOMOO_OPEND_HOST`/`MOOMOO_OPEND_PORT`** appear in docker-compose env (docker-compose.yml:36-37) but the API reads only `MOOMOO_HOST`/`MOOMOO_PORT`; the OPEND pair is presumably consumed by other tooling. Confirm Go only needs the latter.
