# Spec: UI Surface, Runner Modes, EOD Pipeline, CLI

This repo's definition of the UI surface, runner modes, the EOD pipeline, and the
CLI. The behaviors below are invariants of this system (values, ordering, edge
cases, string formats); wire-visible semantics that the UI/Redis consumers depend
on must not change silently. Where a known weakness is called out, the better
behavior this repo adopts is described alongside it.

---

## Part A — UI surface (`src/ui`)

### A.1 Build tooling

| Item | Value | Source |
|---|---|---|
| Framework | Next.js 16.2.4, App Router, React 19.2.4, TypeScript strict | `src/ui/package.json:19-23` |
| Package manager | pnpm 9.15.0 (`packageManager` field) | `src/ui/package.json:5` |
| Styling | Tailwind CSS 4 + shadcn/ui (style `base-nova`, baseColor `neutral`, lucide icons) | `src/ui/components.json:4-12` |
| Data fetching | TanStack Query v5; charts: Recharts 3; theme: next-themes (dark default) | `src/ui/package.json:13-25`, `src/ui/src/app/providers.tsx:22` |
| Output mode | `output: "standalone"` (self-contained `node server.js` for Docker) | `src/ui/next.config.ts:6` |
| Scripts | `dev` = `next dev`; `build` = `next build`; `start` = `next start`; `lint` = `eslint`; `codegen` = `node scripts/codegen.mjs` | `src/ui/package.json:6-11` |
| Type codegen | fetch `${API_BASE_URL}/openapi.json` (default `http://127.0.0.1:8000`, env `API_BASE_URL`), save to `openapi.json`, then `pnpm exec openapi-typescript openapi.json -o src/lib/api/types.ts`; falls back to saved snapshot if API down, errors (exit 1) if neither | `src/ui/scripts/codegen.mjs:24-46` |
| Docker image | 3-stage node:22-alpine; `NEXT_PUBLIC_API_BASE_URL` / `NEXT_PUBLIC_WS_BASE_URL` are **build args** baked into client bundles (compose passes `http://localhost:3000`-style host URLs); runtime: non-root `nextjs` user, PORT=3000, `node server.js` | `docker/ui.Dockerfile:1-56`, `docker-compose.yml:62-75` |

 Client env resolution: `API_BASE_URL = NEXT_PUBLIC_API_BASE_URL || "http://127.0.0.1:8000"`, `WS_BASE_URL = NEXT_PUBLIC_WS_BASE_URL || "ws://127.0.0.1:8000"` (`src/ui/src/lib/api/client.ts:15-20`). `apiGet` throws `Error("API <path> -> HTTP <status>")` on non-2xx (`client.ts:24-29`).

 Global query defaults: `refetchInterval: 5000`, `staleTime: 2000` (`src/ui/src/app/providers.tsx:13-16`). Theme: `defaultTheme="dark"`, `enableSystem`, class attribute (`providers.tsx:22`).

> Port note: the host ports for this stack are api **18080** and ui **13000** (project ground rule). Mapping ports is an allowed deployment choice; the URL-resolution *logic* is the invariant.

### A.2 Route map

Nav links (`src/ui/src/components/site-header.tsx:10-17`), in order: `/` (Live), `/watchlist`, `/strategies`, `/backtest`, `/hyperopt`, `/system`. Active-link rule: `/` requires exact match; others use `pathname.startsWith(href)` (`site-header.tsx:43-46`).

| Route | File | Purpose |
|---|---|---|
| `/` | `src/ui/src/app/page.tsx` | Live cockpit |
| `/watchlist` | `src/ui/src/app/watchlist/page.tsx` | Quotes + signals table with strategy filter |
| `/strategies` | `src/ui/src/app/strategies/page.tsx` | Registered strategies list |
| `/strategies/[id]` | `src/ui/src/app/strategies/[id]/page.tsx` | Strategy detail + params-source switcher (the only mutating UI) |
| `/backtest` | `src/ui/src/app/backtest/page.tsx` | Backtest run list |
| `/backtest/runs/[ts]` | `src/ui/src/app/backtest/runs/[ts]/page.tsx` | Run detail |
| `/backtest/runs/[ts]/strategies/[id]` | `.../strategies/[id]/page.tsx` | Per-strategy run detail |
| `/hyperopt` | `src/ui/src/app/hyperopt/page.tsx` | Optuna study monitor (read-only) |
| `/system` | `src/ui/src/app/system/page.tsx` | System health widgets |

The UI is read-only by design except the params-source PUT (`src/ui/README.md:8-11`).

### A.3 API endpoints consumed (complete)

REST (all `GET` unless noted; from `src/ui/openapi.json` paths + page code):

| Endpoint | Consumers |
|---|---|
| `/api/live/quotes` | watchlist, ActionableSignals |
| `/api/live/signals` and `/api/live/signals?strategy=<id>` | watchlist (server-side filter when not "all"), ActionableSignals |
| `/api/live/account` | AccountStrip (poll 5 s, `retry: false`) |
| `/api/live/portfolio-health` | usePortfolioHealthStream initial snapshot (503 expected pre-first-publish → swallowed) |
| `/api/live/positions/strategy` | StrategyCard (poll 10 s), strategy detail page |
| `/api/live/positions/broker` | OpenPositionsCompact (poll 10 s, `retry:false`) |
| `/api/live/reconciliation` | HealthStrip (poll 30 s, `retry:false`) |
| `/api/live/orders/{order_id}/events` | OrderEventDrawer (`src/ui/src/components/OrderEventDrawer.tsx:27-29`) |
| `/api/live/positions/{position_id}/events` | PositionEventDrawer (`PositionEventDrawer.tsx:27-30`) |
| `/api/system/data-ingestion` | /system SharadarWidget |
| `/api/system/broker-connection` | /system BrokerWidget |
| `/api/system/actor-stats` | /system ActorsWidget (404 → "needs live trading node" message, `system/page.tsx:243`) |
| `/api/strategies/registered` | /strategies |
| `/api/strategies/registered/{name}` | /strategies/[id] |
| `/api/strategies/registered/{name}/available-sources` | /strategies/[id] |
| `PUT /api/strategies/registered/{name}/source` (body `{"source": <id>}`) | /strategies/[id] Apply button (`strategies/[id]/page.tsx:96-109`) |
| `/api/backtest/runs` | /backtest (no refetch, `staleTime: Infinity`) |
| `/api/backtest/runs/{ts}` `/equity-curve` `/regime-distribution` `/strategy-summaries` | /backtest/runs/[ts] |
| `/api/backtest/runs/{ts}/strategy-summaries/{id}`, `/api/backtest/runs/{ts}/strategies/{id}/equity-curve` | /backtest/runs/[ts]/strategies/[id] |
| `/api/hyperopt/studies`, `/api/hyperopt/studies/{ts}`, `/api/hyperopt/studies/{ts}/best-params` (404 → null, not error) | /hyperopt |

Endpoints present in the API but not currently consumed by any page (must still exist in the API): `/api/health`, `/api/live/context`, `/api/live/orders`, `/api/live/strategies/{strategy_id}/state`, `/api/backtest/runs/{ts}/account`, `/api/backtest/runs/{ts}/orders`, `/api/backtest/runs/{ts}/positions` (`src/ui/openapi.json` paths).

WebSocket streams (paths from hooks):

| WS path | Hook | Keying / dedupe |
|---|---|---|
| `/api/live/stream/quotes` | `use-quote-stream.ts:27-30` | by `symbol`; drop if `existing.generation >= incoming.generation` |
| `/api/live/stream/signals` | `use-signal-stream.ts:49-52` | by `` `${symbol}:${strategy_id}` ``; parse `intent_json` (drop malformed); drop if `existing.generation >= inner.generation`; optional client-side `strategyFilter` on `frame.strategy_id` |
| `/api/live/stream/strategy-state` | `use-strategy-state-stream.ts:27-37` | filter `payload.strategy_id !== strategyId`; parse `state_json` (warn on failure) |
| `/api/live/stream/regime` | `use-regime-stream.ts:14-17` | last-write-wins, payload `{value, ts_event?}` |
| `/api/live/stream/system` | `use-system-events.ts:19-29` | rolling tail, `MAX_EVENTS = 100` |
| `/api/live/stream/broker-connection` | `use-broker-connection-stream.ts:14-39` | last-write-wins; tracks wall-clock `staleMs`, 1 s tick |
| `/api/live/stream/data-ingestion` | `use-data-ingestion-stream.ts:8-44` | same pattern (SharadarHealthActor heartbeat) |
| `/api/live/stream/portfolio-health` | `use-portfolio-health-stream.ts:10-66` | REST snapshot on mount + WS overlay, last-write-wins, no generation |

 WS reconnect (shared `use-websocket-stream.ts:36-71`): on close OR error, `attempts += 1; delay = min(30_000, 500 * 2^min(attempts, 6))` ms, then reconnect; `attempts` resets to 0 on successful open; JSON parse errors on frames are silently swallowed; cleanup cancels timer and closes socket.

 `staleMs` semantics (broker/data/portfolio-health hooks): `Number.POSITIVE_INFINITY` until first frame, else `max(0, now - lastWallMs)` with `now` ticking at 1 Hz.

### A.4 Cockpit (`/`) — components and exact rules

Layout (`src/ui/src/app/page.tsx:18-48`): each panel wrapped in `CockpitPanel` — a class-component error boundary that renders an inline red "Panel error: <name> … reload page" card on child render error so siblings survive (`components/cockpit/CockpitPanel.tsx:17-50`). Panel order: health, account, 3 strategy cards (grid), signals + positions (grid), events (compact `SystemEventsLog` with `maxRows={6}`).

 Strategy registry — frontend source of truth, must match what the live runner registers (`src/ui/src/lib/strategies.ts:18-33`):

| runner `id` | label | `signalStrategyId` (lowercase intent discriminator) |
|---|---|---|
| `SEPA-UNIVERSE-001` | SEPA | `sepa` |
| `SectorRotation-001` | Sector Rotation | `sector_rotation` |
| `Pairs-001` | Pairs | `pairs` |

 This registry is hardcoded; comment defers auto-discovery via `GET /api/live/strategies` (`strategies.ts:4-6`). Go may add such an endpoint, but the three IDs above are wire-contract defaults and must stay.

**HealthStrip** — five indicators with pure color functions (`components/cockpit/HealthStrip.tsx:42-100`), all:

- `brokerColor`: null→gray; `!connected`→red; staleMs > 48 h→red; > 25 h→yellow; else green.
- `dataColor`: null→gray; staleMs > 48 h→red; > 25 h→yellow; else green.
- `regimeColor`: null→gray; value `"bear"`→red; `"warning"`→yellow; else green (no staleness).
- `riskColor`: null→gray; `daily_loss_halt`→red; staleMs > 10 min→red; > 3 min→yellow; `Number(halt_headroom_pct) < 0.02`→yellow; else green.
- `reconcileColor`: query error→gray; no data→gray; `has_issues`→red; else green; sublabel "`N` mismatches" from `mismatches.length`.
- Whole strip gets red border/background when `risk.latest.daily_loss_halt === true`.

**AccountStrip** (`AccountStrip.tsx:11-31`): `/api/live/account` poll 5 s; Total = `Number(total_assets)`, Cash = `Number(cash)`; Day P/L `formatUsd(Number(day_pnl))` + ` (xx.xx%)` from `day_pnl_pct * 100` toFixed(2); Halt-headroom `(halt_headroom_pct*100).toFixed(1)%` or red "HALTED". Red border when halted.

**StrategyCard** (`StrategyCard.tsx:38-77`): per registry entry — strategy-state WS (running dot = any frame received), positions filtered by exact `String(p["strategy_id"]) === strategyId`, signal counts over the signal stream filtered by `signalStrategyId`: tally `state` into `{buy, forming, hold}` (only keys already in the accumulator count). Shows BUY/FORMING counts; "all strategies — no new orders" line when halted.

**ActionableSignals** (`ActionableSignals.tsx:54-83`): merge REST snapshot + stream (stream wins when `existing.generation <= incoming.generation` — note ties go to the stream here, vs strict `>` in the hooks); filter to `state === "buy" || state === "forming"`; sort: buy before forming, then `symbol.localeCompare`. (including the `<=` vs `>=` asymmetry between page-merge and hook-dedupe).

**OpenPositionsCompact** (`OpenPositionsCompact.tsx:23-40`): `/api/live/positions/broker` poll 10 s; unrealized P/L formatted `(n>=0?"+":"") + n.toFixed(2)`, non-finite → raw string, null → "—". Error state → "Broker unreachable".

**SystemEventsLog** (`SystemEventsLog.tsx:12-141`): badge variant by uppercased state: ERROR/FAULT→destructive; STOPPED/DEGRADED/WARNING→secondary; else outline. Timestamp render: `new Date(ts_ns / 1_000_000).toISOString.replace("T"," ").replace("Z","")`, `0`→"—". Compact mode (cockpit) vs full collapsible mode (100-event tail, auto-scroll).

### A.5 Watchlist (`/watchlist`)

(`src/ui/src/app/watchlist/page.tsx`) — strategy filter buttons: `all | sepa | pairs | sector_rotation | intraday_breakout` (`watchlist/page.tsx:35-40`). Filtering is server-side for the REST snapshot (`?strategy=`), client-side for the WS stream.

 Row merge algorithm (`watchlist/page.tsx:86-127`): quote map keyed by symbol (snapshot floor, stream wins on `existing.generation <= incoming`); signal map keyed `symbol:strategy_id` (same rule); rows are one-per-symbol — for a symbol with multiple strategies' signals, the signal with the **higher generation** wins the row's signal column; rows sorted by `symbol.localeCompare`.

 State badge classes keyed by signal state: `no_setup, forming, buy, hold, exit, stop_hit` (`watchlist/page.tsx:42-51`); default state when no signal: `"no_setup"`. Columns: Symbol, Last (`quote.last`), %Chg (`change_pct.toFixed(2)%`, green ≥ 0 else red), State badge, Strategy (`signal.strategy_id`), Strength (`strength.toFixed(1)`), Proximity (`proximity_to_trigger_pct.toFixed(2)%`, null → "—"), Session (`quote.market_session`). Disconnect banner when either WS is down.

### A.6 Strategies pages

`/strategies` (`strategies/page.tsx:34-118`): table of `RegisteredStrategy` — display_name (link), description, Allocation = `capital_pct*100` toFixed(0)% when `active` else "inactive", parameters_count, source badge (`tuned` → default variant, else secondary).

`/strategies/[id]` (`strategies/[id]/page.tsx`): detail + Parameters table (name, `default` value, `type`, search range `low – high`, description) + Params Source radio selector with PUT mutation, "external" pseudo-option rendered disabled when active source is `external` but not in options (`strategies/[id]/page.tsx:228-249`); Live Positions count filtered by `strategy_id.toLowerCase.startsWith(id.toLowerCase)` (`strategies/[id]/page.tsx:78-84` — note prefix match, unlike the cockpit's exact match). including the note "Applies immediately. Already-running trader processes use their existing params until restart."

### A.7 Backtest pages

`/backtest` (`backtest/page.tsx:48-52`): list, no auto-refetch. Kind badge: `multi-strategy` → default variant, else secondary. P&L cell green/red via `Intl.NumberFormat("en-US", {style:"currency",currency:"USD"})`.

`/backtest/runs/[ts]`: 4 parallel queries (meta, equity-curve, regime-distribution, strategy-summaries), all `staleTime: Infinity`. Regime distribution rendered as a bar chart of `{regime, count}` entries from a `Record<string, number>`.

`/backtest/runs/[ts]/strategies/[id]`: per-strategy equity curve + state snapshot cards, timestamp formatted `toLocaleString("en-US", {timeZone:"UTC",...})`.

 `EquityCurveChart` (`components/charts/EquityCurveChart.tsx:22-41`): thins to ≤ 120 evenly spaced points via `step = ceil(n/120)`, always appends the true last point if not already included; Y tick `$${(v/1000).toFixed(0)}K`; X tick = first 10 chars of ISO ts.

 Strategy-state discriminator (`components/strategies/discriminate.ts:74-98`), checked in order:
1. SEPA: has all of `current_grade`, `vcp_detected`, `bars_in_history`.
2. Pairs: `Array.isArray(state.pairs)`.
3. SectorRotation: `current_holdings` is a non-null non-array object.
4. IntradayBreakout: has `range_high` AND `session_date`.
5. else `unknown` → raw `JSON.stringify(state, null, 2)` dump.

View formatting rules: SEPA market cap `$x.xxT/B/M` thresholds 1e12/1e9/1e6 else `$<v.toFixed(0)>` (`SepaView.tsx:4-10`); SEPA `inPosition = position_qty !== 0`; Pairs z/β `.toFixed(3)`, state badge LONG_SPREAD→default, SHORT_SPREAD→destructive, else secondary; IntradayBreakout volume `x.xM/x.xK` thresholds 1e6/1e3, `inPosition = position_qty > 0` (note: `>` here vs `!==` for SEPA).

### A.8 Hyperopt page

(`hyperopt/page.tsx`) — study list (auto-select first), detail refetch every 3 s **only while** `progress.status === "RUNNING"` (`hyperopt/page.tsx:97-99`); best-params fetched only when status COMPLETE, 404 → null (normal). Pareto frontier (`hyperopt/page.tsx:60-81`): over COMPLETE trials with both `sharpe` and `calmar` metrics; trial dominated iff another trial has `sharpe >= s && calmar >= c && (sharpe > s || calmar > c)`; missing metrics coerce to 0 inside the comparison. Top-trials table = top 10 by sharpe desc. Status badge: COMPLETE→default; FAILED/FAIL/INTERRUPTED→destructive; else secondary. Best-params card shows the `.env` line `TMS_STRATEGY_PARAMS_DIR=runs/hyperopt/<ts>/best_params`.

### A.9 System page

(`system/page.tsx`) — three widgets (Sharadar ingestion, Broker connection, Context actors). hit ratio: `fetch_count > 0 ? round(cache_hit_count / (cache_hit_count + cache_miss_count || 1) * 100)%: "—"` (`system/page.tsx:79-84`). Actor labels map: `regime→RegimeActor, fundamentals→FundamentalsActor, earnings→EarningsActor` (`system/page.tsx:178-182`). `last_value_json` preview: parse; string→as-is; object with ≤3 keys→full JSON; >3 keys→`{k1, k2,...+N}`; parse failure→raw. `data === null` → "needs live trading node" messages.

### A.10 Shared formatting

`src/ui/src/lib/format.ts:1-30`:
- `formatUsd`: `Intl.NumberFormat("en-US", {style:"currency", currency:"USD", minimumFractionDigits:2, maximumFractionDigits:2})`.
- `formatRelativeTime(ts_ns)`: `0`→`"never"`; negative diff→`"just now"`; `<60s`→`Ns ago`; `<60m`→`Nm ago`; `<24h`→`Nh ago`; else `Nd ago` (floor division at each step; input is **nanoseconds**, converted `ts_ns / 1_000_000` to ms).

### A.11 Signal mode vs trade mode rendering

There is **no mode switch in the UI code** — the same pages render in all node modes. The differences are entirely data-driven:

1. Redis namespace: signal and paper modes default to trader_id `PAPER-SMOKE-001`, which matches the API container's `TMS_TRADER_ID` default, so cockpit/watchlist see streams with zero reconfiguration; live mode uses `TMS-LIVE-REAL-001` and the operator must align the API container's `TMS_TRADER_ID` to see real-money streams.
2. In signal mode no exec client exists, so order submission fails at the RiskEngine by design; the broker-positions/account panels still populate via the API's OpenD REST proxy (reflecting whatever the operator trades manually), and `ActionableSignals` (BUY/FORMING) is the operator's manual-trade prompt.
3. EOD-refreshed signals land in the same streams, so a populated watchlist does not imply a running live node.

---

## Part B — Runner modes (`src/runner`)

### B.1 `build_backtest_engine`

 Signature defaults: `trader_id="MULTI-STRAT-001"`, `log_level="WARNING"`, `bypass_logging=False`, `account_type=AccountType.MARGIN` (required for Pairs shorts), `oms_type=OmsType.NETTING`.

 Wiring sequence:
1. Construct `BacktestEngine` with `BacktestEngineConfig(trader_id, LoggingConfig(log_level, bypass_logging))`.
2. `engine.add_venue(venue, oms_type, account_type, base_currency=starting_balance.currency, starting_balances=[starting_balance])`.
3. For each `(ticker, df)` in `bars_by_ticker` (dict iteration order = insertion order): create equity instrument for `(symbol, venue)`; `engine.add_instrument`; `BarType.from_str(f"{instrument.id}-1-DAY-LAST-EXTERNAL")`; wrangle df → bars; `engine.add_data(bars)`.
4. Return `(engine, bar_type_by_ticker)`.

Caller responsibilities are explicitly NOT in this function (pre-load bars, construct runners with returned bar types, register actors BEFORE strategies, register strategies, attach Portfolio, `engine.run`) —.

 `bypass_logging=True` exists because the Rust logging subsystem may only init once per process (tests constructing multiple engines); production leaves it False. Go analogue: logger re-init must be safe across multiple engine constructions in one test binary.

### B.2 Full backtest sequence (`::run_backtest`; lines 361-825)

Defaults: `start="2024-01-02"`, `end="2024-12-31"`, `starting_balance_usd=100_000.0`, `dump=True`, venue `Venue("SIM")`. CLI: positional `[START] [END]` only (`:828-831`).

 Override validation: reserved keys per strategy — sepa/sector_rotation: `{strategy_id, bar_types}`; pairs: `{strategy_id, bar_types, pairs_spec}`; violation raises `ValueError("reserved override keys for <name>: [...]")` (`:106-126`).

 Exact step order:
1. Resolve cache: production = `SharadarUniverseCache(CacheLayout.from_config(load_config))`; injected test cache recovers `layout = getattr(cache, "layout", None)` (`:374-389`).
2. Universe: `cache.list_universe_for_window(start_dt, end_dt, table="SF1")` — survivor-bias-free (includes mid-window delistings; ETFs excluded). 0 tickers → `RuntimeError` (`:391-403`).
3. Per-ticker bars in one pull over `[start_dt - 400 days, end_dt]` (calendar days; ≈270 trading days ≥ the 200-bar TrendTemplate/SEPA threshold), split at `pd.Timestamp(start_dt, tz="UTC")` into warmup (`<`) and run (`>=`) slices (`:405-434`). Skip rules, in order: empty df → skip; empty run slice → skip; run slice price-cap overflow → record in `skipped_overflow`; run slice NaN → record in `skipped_nan`. Warmup slice kept only if non-empty AND NaN-free (otherwise lose pre-warm, never crash ingestion).
 - Price cap `_NAUTILUS_PRICE_MAX = 17_014_118_346_046.0`; overflow = any of open/high/low/close `max >` cap. Rationale: reverse-split-adjusted micro-caps overflow the engine's price type.
 - NaN check covers open/high/low/close/**volume** (`:139-151`).
 - Bar frame normalization `_bars_to_engine_format` (`:206-221`): long-format (has `date` column) → index = `to_datetime(date).tz_localize("UTC")`, keep `[open,high,low,close,volume]`; already-indexed naive → `tz_localize("UTC")`; tz-aware → pass through columns only.
4. ETF + pair bars over run window only (no warmup): `DEFAULT_UNIVERSE` (11 SPDR ETFs, from `strategies/sector_rotation`) and `pair_tickers = sorted(set(legs of DEFAULT_PAIRS))`. Missing/empty or NaN ETF/pair data → hard `RuntimeError` (partial universes are not allowed) (`:270-333`). ETFs overwrite any same-ticker entry (`bars_by_ticker.update(sector_bars)`); pair legs only fill gaps (`if t not in bars_by_ticker`) (`:445-449`).
5. SPY: warmup window = `run_start - 500 days` (calendar; ≈340 trading days > RegimeActor's 200-bar need); single pull split at run_start into `(spy_warmup, spy_run)`; missing → `RuntimeError` (`:335-358`). Context preload (`_load_sf1_mrt` + `_load_earnings`) wrapped in try; on RuntimeError → `actors_wired = False` and continue without actors (`:457-470`). `bars_by_ticker["SPY"] = spy_run` only when actors wired.
 - `_load_sf1_mrt` (`:223-244`): pyarrow dataset scan of `layout.sf1_root` (hive partitioning), filter `ticker in tickers AND dimension == "MRT"`, keep columns `ticker, datekey, dimension, marketcap` (those present); missing root → empty df.
 - `_load_earnings` (`:246-267`): scan `layout.events_root`, filter ticker set; earnings = rows whose `eventcodes` string split by `"|"` contains `"22"`; rename `date` → `report_date`; missing root / no `eventcodes` column → empty df.
6. Engine: `build_backtest_engine(venue=SIM, starting_balance=Money(starting_balance_usd, USD), bars_by_ticker, bypass_logging)` (`:517-522`).
7. `stock_tickers_tuple` = bars_by_ticker keys minus `{"SPY", *sector_tickers}` (`:529-532`). Screener = `SEPAScreener(SEPAScreenerConfig(market_cap_lookup=_market_cap_lookup_factory(cache)))`.
 - `_market_cap_lookup_factory` (`:182-203`): memoized per-ticker; latest SF1 row by `datekey`; missing/NaN/absent `marketcap` column → `0.0` (screener treats 0 as fail-rule-8).
8. Strategy kwargs: JSON defaults via `defaults_dict(load_strategy_params(<name>))`; sector drops `universe` key, pairs drops `pairs` key (runner derives them); `pairs_spec_from_json = tuple(tuple(p) for p in pairs_defaults["pairs"])`; merge order `{"strategy_id": <ID>, **defaults, **overrides}` (later wins) with IDs `SEPA-UNIVERSE-001`, `SectorRotation-001`, `Pairs-001` (`:544-575`).
9. Assembly: if SPY bar type present and actors wired → `_build_actors(engine, mode="backtest",...)` (actors BEFORE strategies — SEPA's on_start subscriptions need publishers wired first, `:524-528`); always `_build_strategies(...)` then `_build_portfolio(...)` (`:583-612`).
10. Instrument `runner.on_pre_open` with a counting wrapper (`:614-623`) — `pre_open_count` is part of `BacktestResult`.
11. `EquityCurveSamplerActor` registered when actors wired and SPY bar type exists; heartbeat = SPY daily bar type; tracks the 3 actual strategy IDs (`:626-637`).
12. Pre-warm: for each warmup ticker with a known bar type, `runner.warmup_ticker(bt.instrument_id, warmup_df)` (SEPA universe runner only) (`:639-651`).
13. `engine.run` inside try; `finally: engine.dispose` (`:653,824-825`).
14. Metrics (`:678-718`): `final_balance = float(account.balance_total(USD))`; equity curve from sampler: sum `balance_usd` per `ts` across strategies into `pnl_by_ts`, curve = `[starting + pnl_by_ts[ts] for ts in sorted(pnl_by_ts)]` (lexicographic ISO sort); fallback degenerate `[starting, final_balance]`. `BacktestMetrics{final_balance_usd, total_pnl_usd, sharpe, calmar, max_drawdown_pct, num_orders, num_filled_orders(=is_closed count), num_rejected_orders(=status REJECTED), num_positions}`.
15. Run dump when `dump` (`:741-806`): orders/positions serialized via `to_dict` when available else minimal fallback dicts; `account_history_from_cache`; `strategy_summaries`: SEPA = single sample `{active_set(sorted symbols), active_count, tracked_count, subscription_cap, active_cap}`; Sector + Pairs = `sg.state_summary` (exceptions swallowed); `RunDump(kind="multi-strategy-universe", regime_samples={}, per_strategy_equity=sampler output)`; `write_run_dump(...)` → `runs/{ts}/` consumed by the /backtest UI.

 Equity-curve fallback `[starting, final]` makes Sharpe/Calmar meaningless on 2 points (acknowledged in comments `:626-629`). keep the same fallback values for stable dump contents, but should log a structured warning.

### B.3 Shared assembly

 Target-agnostic helpers: prefer `target.add_actor` / `target.add_strategy`; else `target.trader.add_actor/add_strategy` (TradingNode facade).

 `_build_actors` (`:101-178`) — registration order matters:
1. `RegimeActor(RegimeActorConfig(spy_bar_type))`; `seed_history(spy_warmup)` iff warmup is not None and not empty. Always registered.
2. `FundamentalsActor` only if `sf1_mrt` non-empty (config: sf1_df, reference_bar_type=spy_bar_type, tickers=stock_tickers).
3. `EarningsActor` only if `earnings_df` non-empty (same shape).
4. live mode only, appended in order: `SharadarHealthActor` (owns its own new `SharadarClient`, spy_bar_type config), `BrokerHealthActor` (shares `moomoo_client`), `QuoteActor` (`component_id="quote_actor"`, shares `moomoo_client`).

Tests pin: backtest with all data → exactly 3 actors in order Regime, Fundamentals, Earnings; empty SF1 + empty earnings → 1 actor (`:90-117`); live mode → 6 actors (`:268`); backtest ignores moomoo_client (`:305`); None warmup OK (`:325`).

 `_build_strategies` (`:180-227`): construct SEPAUniverseRunner (bar_types = tuple over `sepa_stocks` order, plus screener), SectorRotationRunner (bar_types over `sector_etfs` order), PairsRunner (bar_types over `pair_tickers = tuple({legs})` — a **set**, i.e. unordered; see Open Questions — plus `pairs_spec`); registration order SEPA → Sector → Pairs; return `[sepa, sector, pairs]`.

 `_build_portfolio` (`:230-257`): allocations SEPA 0.40 / SectorRotation 0.30 / Pairs 0.20 (10% implicit cash); `RiskConstraintsConfig(max_single_name_pct=0.50, concentration_pct=0.40, daily_loss_halt_pct=0.10)`; then `strategy.set_portfolio_service(portfolio)` on each of the three. Identical config in backtest and live (consistency goal noted at `:236-239`).

 `_build_portfolio_health_actor` (`:260-283`): live mode only; subscribes to **1-MINUTE** SPY bars — `BarType.from_str(f"{spy_instrument_id}-1-MINUTE-LAST-EXTERNAL")`, NOT the daily spy_bar_type — so day-P/L + halt indicators tick at minute cadence. Backtest returns `[]` (pinned by).

 `build_all` (`:286-351`): `mode="live"` with `moomoo_client is None` raises `ValueError("moomoo_client required for live mode...")`; sequence = actors → strategies → portfolio → (live) portfolio-health actor appended to actor list; returns `AssemblyResult(actors, strategies, portfolio)`.

 msgspec custom encodings registered at import: `MoomooClient → "<MoomooClient>"`, `SharadarClient → "<SharadarClient>"` (lossy, logging-only) (`:44-50`). Go analogue: actor-config serialization for logs must tolerate client handles.

### B.4 `portfolio_glue`

 Snapshot construction: `nav = Decimal(str(account.balance_total(base_currency)))`; positions aggregated over `cache.positions_open` keyed `(str(strategy_id), str(symbol))` with `signed = int(pos.signed_qty)` (positive long / negative short), **zero-qty entries skipped**, same-key entries summed; `cash = nav` (simplified — balance_total already nets margin); `realized_pnl_today` / `unrealized_pnl_today` default `Decimal(0)` (daily-loss-halt effectively dormant in backtest until the caller tracks deltas —); `last_close` defensively copied. Pinned by.

 `maybe_check_portfolio` (`:69-98`): no portfolio configured → True; approved → True; rejected → log warning exactly `"[Portfolio] REJECTED {strategy_id}/{symbol} ({side} {qty}): {rule_name} — {reason}"` (when log present) and return False; missing logger must not crash.

### B.5 Live node — `build_node` mode switch

Modes: `Literal["signal", "paper", "live"]` (`:25`).

 Trader-id resolution: `TMS_TRADER_ID` env (stripped, non-empty) wins; else per-mode default: signal→`PAPER-SMOKE-001`, paper→`PAPER-SMOKE-001`, live→`TMS-LIVE-REAL-001` (`:37-41,90-93`; pinned by).

 Mode switch:
- **signal**: `exec_clients = {}` — data client only; strategies run and publish signals; orders die at RiskEngine (no venue route) by design; password ignored; `PAPER_ACC_ID` not required (test `:96-107`).
- **paper / live**: empty/None password → `ValueError` mentioning password (`:115-120`; test `:110-115`); missing `PAPER_ACC_ID` env → `ValueError` (`:121-126`); `paper_acc_id = int(env)` (unguarded int parse — ValueError on garbage); exec client config `{paper_acc_id, unlock_password=password, is_paper=(mode=="paper")}` (`:127-134`).
- Data client always `{"MOOMOO": MoomooDataClientConfig}`; factories: data factory always added, exec factory only when `mode != "signal"` (`:173-176`).

 Redis backing (`:136-170`): enabled iff `TMS_USE_REDIS` env == `"1"` (default "1"); `REDIS_HOST` default `127.0.0.1`, `REDIS_PORT` default `6379` (int). `DatabaseConfig(type="redis", connection_timeout=5, response_timeout=5)`; `CacheConfig(database, encoding="json", flush_on_start=False)`; `MessageBusConfig(database, encoding="json", stream_per_topic=True)`.

> Go port note: host Redis port for the Go stack is **56379** (ground rule); the env-default of 6379 is reference behavior — keep env names/defaults, point deployment env at 56379.

### B.6 Live node — `build_and_run` warmup flow + auto-catchup

 Sequence:
1. `build_node(mode, password)`; banner prints (mode, trader_id, redis host:port, paper_acc for non-signal) (`:213-222`).
2. Cache: `load_config` → `CacheLayout.from_config` → `SharadarUniverseCache` (`:224-227`).
3. **Auto-catchup**: enabled by default; disabled when `--no-sync` OR `TMS_AUTO_SYNC` env (stripped) == `"0"`; when disabled prints which gate fired (`:233-237`). When enabled: `ensure_cache_fresh(SharadarClient(cfg), cache_layout)`; prints skipped_reason / "cache already fresh" / `synced X/Y days; rows added=...; errors=N` + first 5 errors; **any exception → WARN and continue** (operator chose live trading with stale disk over no trading, `:229-251`).
4. Universe + warmup window: `today = date.today` (local date), `warmup_start = today - timedelta(days=2*365)` (`:257-258`). `sector_tickers = tuple(DEFAULT_UNIVERSE)`; `pairs_spec = tuple(tuple(p) for p in DEFAULT_PAIRS)`; `pair_tickers = tuple(sorted({legs}))`.
5. If `--no-warmup`: spy_warmup=None, sf1_mrt/earnings empty, sepa_tickers= (`:262-267`). Else each load step is independently try/excepted with WARN-and-degrade:
 - SPY warmup `cache.get_bars("SPY", warmup_start, today)` — failure → RegimeActor starts cold.
 - SEPA universe: `cache.list_universe_for_window(warmup_start, today, table="SF1")`, exclude `{"SPY", *sector_tickers}` (note: pair legs NOT excluded here, unlike EOD — see Open Questions), then cap via `_apply_universe_limit(tickers, _market_cap_lookup_factory(cache), limit=_resolve_universe_limit)` — failure → empty universe.
 - `_load_sf1_mrt(layout, sepa_tickers)`, `_load_earnings(layout, sepa_tickers)` — failures → empty frames.
6. Bar types: for every ticker in `("SPY",) + sepa + sector + pair`, `BarType.from_str(f"{InstrumentId(Symbol(t), MOOMOO_VENUE)}-1-DAY-LAST-EXTERNAL")` (`:331-340`).
7. Strategy kwargs from JSON defaults, same key-drop rules as backtest, same three strategy_ids (`:349-362`).
8. moomoo client: prefer `node.kernel.data_engine.get_client("MOOMOO")`; on exception fall back to the `MoomooClient` **class object** (not an instance) with a comment that actor wiring will then fail loudly at build_all (`:336-348` region; see below).
9. `build_all(node, mode="live",...)`; print assembled counts; `node.build` (required to wire clients from factories — pre-V3-C bug noted `:378-380`); `node.run` (blocks until SIGINT/stop).

 `_resolve_universe_limit` (`:50-61`): `TMS_LIVE_UNIVERSE_LIMIT` env stripped; empty → 85 (`_DEFAULT_LIVE_UNIVERSE_LIMIT`, derived from moomoo OpenD's hard cap of 100 simultaneous K-line subscriptions minus ~15 for SPY+ETFs+pair legs); non-int → `ValueError("TMS_LIVE_UNIVERSE_LIMIT must be an integer, got...")` (fail fast, no silent fallback — pinned).

 `_apply_universe_limit` (`:63-87`), pinned by:
- `limit <= 0` or empty universe → `` (including negative).
- `len(universe) <= limit` → pass through **unchanged order** (no reshuffle).
- else `sorted(key=market_cap_lookup, reverse=True)[:limit]` — descending market cap; unknown tickers (lookup 0.0) sort last. Note the sort is stable: equal-cap tickers keep input order. Use a stable sort.

 moomoo-client fallback assigns the class, not an instance — acknowledged in-source as deferred construction that "will fail at build_all time with a clear error". Go should construct a proper client instance or return a typed error immediately instead of passing a type-token through.

 `today = date.today` uses the host-local date while EOD uses UTC; Go should use one timezone (UTC) consistently — behavior difference only matters around midnight local.

### B.7 Auto-catchup detail (`::ensure_cache_fresh`; lines 87-216):
- If `meta.last_sync["SEP"]` missing → return `CatchupReport(skipped_reason="not-bootstrapped")` with a warning telling operator to run bootstrap (full backfill is hours, never implicit at startup).
- Target = `(today or now(UTC).date) - 1 day`; day list = pandas business days (Mon–Fri, **no holiday calendar** — US holidays cause harmless zero-row calls, accepted trade-off `:53-66`) from `last_sep.date` **inclusive** through target inclusive; empty → fresh report, no work.
- Per-day loop, order SEP then SFP (SEP first so quota exhaustion mid-loop leaves the bar cache maximally complete `:138-141`); each call try/excepted, errors collected as `"SEP {date}: {exc}"` style strings; a day counts succeeded only if both SEP and SFP succeeded; `meta.save(layout)` after **every** day (crash-resume safety `:166-167`).
- Then once: TICKERS refresh (`write_tickers`); re-read ticker list; empty → record error `"TICKERS list empty — skipping SF1 / EVENTS"`; else SF1 then EVENTS full refresh, each independently try/excepted, meta saved after each.
- `CatchupReport{skipped_reason, days_attempted, days_succeeded, rows_added: {SEP,SFP,SF1,EVENTS,TICKERS}, errors}`; `did_work = days_attempted > 0 or rows_added non-empty`.
- Re-syncing the last-synced day on every start is intentional; the underlying writers are same-day idempotent (pinned by et al.).

### B.8 Daily restart (`scripts/restart_live_node.sh`)

 (operationally — Go equivalent must preserve semantics): `set -euo pipefail`; log file `${TMS_LIVE_LOG:-$HOME/Library/Logs/tms-live-node.log}`, grace `${TMS_RESTART_GRACE_SECS:-30}` s; find the live-node PIDs; SIGTERM all; poll every 2 s up to grace; SIGKILL stragglers + 1 s sleep; then relaunch the live node detached (survives the launchd job exit). Safe when nothing is running. Mode comes from `.env` (`TMS_LIVE_MODE`, default signal). Rationale: in-memory actors read Sharadar warmup once at startup, so a clean restart (not in-process refresh) picks up the fresh cache through auto-catchup (`restart_live_node.sh:7-13`).

---

## Part C — EOD pipeline (`src/runner/eod`)

### C.1 Purpose & architecture

The live node only emits signals when MOOMOO delivers the daily bar at session close; if the node is down at that moment the watchlist stays empty. The EOD refresher replays the local Sharadar cache through the **same `SignalGenerator.on_bar` code path** live mode uses (no parallel state machine) and publishes the **same wire schema** to the same Redis streams — the UI cannot distinguish producers. Pure signal computation, no trading engine.

### C.2 Sink protocol

 Topics (no `*`): `SIGNAL_INTENT_TOPIC = "data.SignalIntentUpdate"`, `STRATEGY_STATE_TOPIC = "data.StrategyStateUpdate"`.

 Stream key: `f"trader-{trader_id}:stream:{topic}*"` — the **literal `*` suffix** matches what the message bus produces for wildcard-topic custom Data; the API's RedisReader already falls back through this form. Field `topic` inside the XADD entry also carries the literal-`*` form (`topic + "*"`).

 Intent serialization: the intent is serialized to JSON (its fields as a map, with non-JSON-native values stringified).

 `RedisIntentSink.publish_intent` XADD payload (single `payload` JSON field + `topic` field):

```json
{"type": "SignalIntentUpdate", "strategy_id": "<sid>", "symbol": "<sym>",
 "intent_json": "<json string of intent fields>",
 "ts_event": "<int ns AS STRING>", "ts_init": "<same string>"}
```

ts stringification is deliberate — i64 values are encoded as JSON strings; RedisReader's `int(entry["ts_event"])` round-trip must be identical for live and EOD producers (; pinned exactly by).

 `RedisStateSink.publish_state` payload: `{"type": "StrategyStateUpdate", "strategy_id", "state_json": json.dumps(state_summary, default=str), "ts_event": str(ts), "ts_init": str(ts)}` (; pinned by). UI parses `state_json` a second time.

 Protocol seam: `SignalIntentSink.publish_intent(strategy_id, symbol, intent, ts_event_ns)` / `StrategyStateSink.publish_state(strategy_id, state_summary, ts_event_ns)`; in-memory doubles capture call dicts for tests. Go: define equivalent interfaces; tests must be able to swap in capturing fakes without Redis.

### C.3 Refresh algorithm (`::refresh_eod_signals`; lines 393-502)

Defaults:

| Param | Default |
|---|---|
| `today` | `datetime.now(UTC).date` when None |
| `warmup_days` | `2 * 365` = 730 calendar days |
| `universe_limit` | 85 |
| `sector_etfs` | `DEFAULT_UNIVERSE` (11 SPDR ETFs) |
| `pairs_spec` | `DEFAULT_PAIRS` |
| `equity_usd` | `Decimal("1000000")` (constant equity_provider; EOD never sizes orders) |
| state-stream strategy ids | `SEPA-UNIVERSE-001` / `SectorRotation-001` / `Pairs-001` |
| intent-stream strategy ids | lowercase `sepa` / `sector_rotation` / `pairs` |

 Step sequence:
1. `warmup_start = today - warmup_days`. Load JSON param defaults for the three strategies (same loader as live).
2. `excluded = {"SPY", *sector_etfs, *pair_tickers}` where `pair_tickers = sorted({legs})` — note pair legs ARE excluded here (live warmup does not exclude them).
3. `_load_sepa_universe` (`:111-135`): `list_universe_for_window(warmup_start, today, table="SF1")` minus excluded; if `limit <= 0` or `len <= limit` → as-is; else if `market_cap_lookup is None` → `filtered[:limit]` (native order, deterministic); else top-N by market cap descending.
4. `_refresh_sepa` (`:137-213`) per ticker, sequential:
 - `cache.get_bars(ticker, warmup_start, today)`; read failure → error `"SEPA {t}: cache read failed: {exc}"`, continue; empty → silently skip (not an error, not counted).
 - Fresh `SEPASignalGenerator` per ticker; SG kwargs filtered to the allowlist `{risk_pct, market_cap_min_usd, hard_stop_pct, pivot_buffer_pct, breakout_volume_multiple, vcp_lookback, history_max_bars, timezone}` (`:161-171`) — extra JSON params are runner-level and dropped.
 - Feed every row via `_bar_from_row` → `sg.on_bar(bar)`. Bar conversion (`:89-103`): `ts = pd.Timestamp(row["date"])`, tz-naive → localize UTC; OHLC via `Decimal(str(value))`; `volume = int(row["volume"])`.
 - `intent = sg.evaluate_intent(as_of=last_bar.ts)`; publish intent with `strategy_id="sepa"` and `ts_event_ns = int(last_bar.ts.timestamp * 1e9)` (bar time, NOT wall clock — pinned by); then publish state with `strategy_id=sepa_strategy_id` (`SEPA-UNIVERSE-001`) and the same ts.
 - Any exception in the SG/publish block → error `"SEPA {t}: {exc}"` + warning log, continue with next ticker.
5. `_refresh_sector_rotation` (`:263-324`): one SG over the ETF tuple (drop `universe` key from params); bars merged via `_drive_multi_symbol_sg` (`:216-260`): per-symbol load (errors collected, empty skipped), tag `_symbol`, `pd.concat` then `sort_values(["date", "_symbol"])` — **chronological with symbol tiebreak**, matching engine feed order; per-row `on_bar`. No frames → `(0, errors)`. Then `intents = sg.evaluate_intent(as_of=last.ts)` (list); publish each with `strategy_id="sector_rotation"`, `symbol=getattr(intent, "symbol", "")`, all sharing the last bar's ts; one state publish (`SectorRotation-001`). evaluate/state failures append errors (state failure does not zero the intent count).
6. `_refresh_pairs` (`:327-390`): symmetric; empty `pairs_spec` → `(0, [])` immediately; `Pair(long_leg=a, short_leg=b)` per spec tuple; symbols = `sorted({legs})`; intents published with `strategy_id="pairs"`; state as `Pairs-001`.
7. `RefreshReport` (`:71-86,481-491`): `{asof=today, sepa_tickers_processed, sepa_intents_published, sector_intents_published, pairs_intents_published, state_summaries_published = sepa_intents + (1 if sector_intents else 0) + (1 if pairs_intents else 0), errors=[sepa..., sector..., pairs...]}` (error list order: SEPA, Sector, Pairs); `total_intents` property = sum of the three intent counts. Log line counts at start and end.

Pinned end-to-end by: one SEPA intent per universe ticker with bars; one sector intent per ETF; 2 pairs intents per pair (one per leg); state ids include all three runner ids; empty cache → all-zero report and zero sink calls; ts_event between first seeded bar and `today`.

### C.4 CLI entry

 Args (argparse):

| Flag | Default | Notes |
|---|---|---|
| `--trader-id` | env `TMS_TRADER_ID` else `PAPER-SMOKE-001` | must match API container's TMS_TRADER_ID |
| `--redis-host` | env `REDIS_HOST` else `127.0.0.1` |
| `--redis-port` | int(env `REDIS_PORT` else `6379`) |
| `--universe-limit` | int(env `TMS_EOD_UNIVERSE_LIMIT` else `85`) |

Flow: logging.basicConfig INFO; build cache from config; `redis.Redis(decode_responses=True)`; `client.ping` — ConnectionError → stderr `ERROR: cannot reach Redis at host:port:...` and **exit 1** (the only non-zero exit); wire Redis sinks; market-cap lookup via `_market_cap_lookup_factory(cache)` with try/except → WARN + `None` fallback (native-order cap); run; print summary line `[eod-refresh] asof=... SEPA=i/p Sector=n Pairs=n state_summaries=n errors=n` + first 10 errors + "... and N more"; **always return 0** after a run — per-strategy errors never fail the process so a transient hiccup doesn't poison the next launchd attempt.

### C.5 launchd schedules

`launchd/com.tms.eod-signal-refresh.plist` (semantics are the invariant):
- Label `com.tms.eod-signal-refresh`; ProgramArguments run the EOD signal-refresh entrypoint, appending to `$HOME/Library/Logs/tms-eod-refresh.log`; WorkingDirectory = repo root; `StartCalendarInterval Hour=17 Minute=0` (**local time** — comments: 17:00 local ≈ 21:00/22:00 UTC depending on PT DST; constraint is "well after 16:00 ET close", Sharadar publishes T-1 ~01:00–05:00 UTC); `RunAtLoad=false`; stdout/stderr to `/tmp/com.tms.eod-signal-refresh.{stdout,stderr}`; PATH env `/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin`.

`launchd/com.tms.live-node-restart.plist`:
- Label `com.tms.live-node-restart`; runs `./scripts/restart_live_node.sh` via `/bin/bash -lc`; `StartCalendarInterval Hour=2 Minute=0` (local); `RunAtLoad=false`, no KeepAlive (shell hands off to nohup'd node); same PATH; logs to `/tmp/com.tms.live-node-restart.{stdout,stderr}` plus the script's own log.
- The plist's prose claims "Fires at 06:00 UTC" but `Hour=2` is interpreted in **local** time (the file itself admits this lower down) — on a PT machine 02:00 local = 09:00/10:00 UTC, which still satisfies "after Sharadar publish, before 13:30 UTC open" but contradicts the header comment. Go scheduling (cron/systemd/launchd template) should state one timezone unambiguously and document the DST drift.

### C.6 Known non-idempotency

The Makefile help advertises `eod-refresh` as "daily, idempotent" (`Makefile:21`), but the implementation is **append-only**: every run XADDs new entries to the two streams with fresh stream IDs and no dedup, no `MAXLEN` trim at the producer. Re-running the same day appends duplicate intents/states (same `ts_event`, new entries). Consumers read latest-per-key so reads are effectively idempotent, and a ~10k MAXLEN cap is assumed elsewhere in the system design (`docs/superpowers/specs/2026-05-13-p-ui-v3-ab-quote-signal-watchlist-design.md:390`), but the EOD producer itself never trims. Additional non-idempotent surface: SEPA publishes **one StrategyStateUpdate per ticker**, all labeled `SEPA-UNIVERSE-001` — the cockpit SEPA card therefore shows whichever ticker's per-symbol state landed last, and `state_summaries_published` for SEPA equals the ticker count rather than 1.
**Go proposal**: (1) keep the wire format byte-identical, (2) add producer-side `XADD... MAXLEN ~ 10000`, (3) optionally skip publishing when an identical payload for the same `(stream, ts_event, strategy_id, symbol)` is already the stream tail, (4) consider one aggregated SEPA state publish — but only behind a flag, since the cockpit's "running" dot currently depends on any frame for `SEPA-UNIVERSE-001` and the per-ticker stream feeds nothing else.

---

## Part D — CLI commands (complete inventory)

### D.1 (make `live-node`)

| Arg | Values / default | Behavior |
|---|---|---|
| `--mode` | `signal\|paper\|live`; default None | Precedence: CLI flag > `TMS_LIVE_MODE` env (lowercased, stripped; default `signal`) > signal. Invalid → `ValueError`. signal is the always-safe default even with password in env. |
| `--no-warmup` | flag | Skip Sharadar warmup load (SEPA starts cold) (`:130-133`) |
| `--no-sync` | flag | Skip auto-catchup; `TMS_AUTO_SYNC=0` equivalent (`:135-141`) |

Mode gates: live → interactive confirmation: print warning block, read input, strip; anything but literal uppercase `LIVE` → "Aborted by user." + `sys.exit(0)` (`:51-72`); then password. paper → password only. signal → neither. Password = `MOOMOO_TRADE_PASSWORD` env; empty → stderr message + `sys.exit(1)` (`:75-90`). `KeyboardInterrupt` from `build_and_run` → clean message + exit 0.

 `_install_signal_handlers` (`:93-114`, SIGINT/SIGTERM → `node.stop_async`) is **defined but never called** from `main` — dead code; SIGTERM (from restart_live_node.sh) currently relies on default handling. Go must implement real signal handling: context cancellation on SIGINT/SIGTERM, graceful node stop within the restart script's 30 s grace.

### D.2 (make `eod-refresh`) — see C.4.

### D.3 (click group; shim; make `sync-universe SUBCMD='...'`)

 Subcommands:
- `bootstrap --start YYYY-MM-DD --end YYYY-MM-DD [--ticker T]...` — full backfill, ordered steps with meta recording after each: [1/4-labelled but actually 5 steps] TICKERS (`write_tickers`) → SEP bootstrap → SFP bootstrap → SF1 bootstrap (per ticker list) → EVENTS bootstrap; with `--ticker` overrides, SEP/SFP are fetched via ticker+date filter and written through the writers' `_normalize` + per-year partition path (smoke-test path). No tickers after TICKERS step → `click.Abort`.
- `update [--asof YYYY-MM-DD]` (default: `date.today`) — TICKERS refresh → SEP update (row_count accumulates: `cur + n`) → SFP update → SF1 refresh → EVENTS refresh; meta saved at end (`:126-166`).
- `stats` — prints cache root and table `Dataset | Rows | Last sync` for TICKERS/SEP/SF1/EVENTS (NB: SFP missing from the stats loop — `:169-184`; Go should include SFP).

Cache root from `TMS_SHARADAR_CACHE_DIR` else `<repo>/cache/sharadar` (`:1-5`).

### D.4 (make `hyperopt STRATEGY=... N_TRIALS=... WORKERS=...`)

 Args: positional `strategy` ∈ `{sepa, sector_rotation, pairs, joint}`; `--n-trials` 20; `--workers` 1; `--start` `2023-01-01`; `--end` `2024-12-31`; `--runs-dir` `runs/hyperopt`; `--seed` 42; `--walk-forward/--no-walk-forward` default True; `--folds` 5; `--embargo-days` 5; `--no-dump-trials`; `--resume <name>`; `--trial-timeout-sec` 600 (0 → disabled/None); `--promote-best` prints (never writes) the `.env` line `TMS_STRATEGY_PARAMS_DIR=<relative best_params dir>` when the dir exists.

### D.5 (make `multi-strategy-backtest [START=][END=]`)

Positional `[START] [END]`, defaults `2024-01-02` / `2024-12-31`; runs `run_backtest(verbose=True)` (see B.2).

### D.6 (make `live-paper-smoke`)

`--mode connect-only` (default, no network) | `manual-test-order` (needs OpenD + `MOOMOO_TRADE_PASSWORD`; connects, unlocks trade context, disconnects) | `sepa` (stub, deferred). Redis backing identical to live_runner (`TMS_USE_REDIS=0` disables). Trader id default `PAPER-SMOKE-001`.

### D.7 Other scripts

- (make `api-smoke`): uvicorn `api.server:app` on `127.0.0.1:8000`, log_level info; env: `REDIS_HOST/REDIS_PORT`, `TMS_TRADER_ID`, `TMS_RUNS_DIR`.
- (make `hello`): synthetic-bar EMACross sanity backtest; no args.
-: validates Redis cache/msgbus backing with trader `SPIKE-001`; no args.
- `make moomoo-spike` → the moomoo spike entrypoint (needs OpenD).
-: pure shim re-exporting the click cli (`:1-12`).
- `scripts/restart_live_node.sh` (make `live-node-restart`): see B.8.

### D.8 Makefile targets (complete; `Makefile:1-141`)

`-include.env` + `export` at top — every target sees `.env` vars automatically (required for the task runner).

| Target | Action |
|---|---|
| `install` | `uv sync` |
| `test` / `lint` / `format` / `check` | pytest / `ruff check src tests scripts` / `ruff format` / lint+test |
| `hello`, `multi-strategy-backtest`, `hyperopt`, `sync-universe` (errors with usage if `SUBCMD` empty, exit 2), `moomoo-spike`, `live-paper-smoke`, `api-smoke` | as above |
| `watchlist-smoke` | the watchlist e2e smoke test (needs API + Redis) |
| `live-node` | prints mode/pre-flight help then runs |
| `live-node-restart`, `eod-refresh` | wrappers |
| `ui-install` / `ui-dev` / `ui-build` / `ui-codegen` | `cd src/ui && pnpm install/dev/build/codegen` |
| `stack-up` | `docker compose up -d` + URL banner (UI:3000, API:8000, Redis:6379); trading node intentionally NOT in compose |
| `stack-down` / `stack-logs` (`-f --tail=100`) / `stack-restart` (api+ui) / `stack-rebuild` (`up -d --build`) | compose ops |
| `clean` | remove pytest/mypy/ruff caches, `__pycache__` |

docker-compose specifics worth porting (`docker-compose.yml`): Redis is an **external shared host container** on network `dev-net` (not a compose service); api joins `default` + `dev-net`, mounts `./src` and `./scripts` read-only (hot reload) and `./runs` read-write; OpenD reached via `host.docker.internal` (with `host-gateway` extra_host for Linux); `NEXT_PUBLIC_*` baked at UI build time. The Go stack replaces this with its own compose on the reserved ports (55432/56379/18080/13000) — wiring topology (api↔redis↔ui, node-on-host) is the part; the shared-external-redis arrangement is -eligible (own the Redis service in compose, since the Go project has a dedicated reserved port).

---

## Parameter quick-reference

| Constant | Value | Source |
|---|---|---|
| Allocations | SEPA 0.40 / Sector 0.30 / Pairs 0.20 / cash 0.10 |
| Risk constraints | single-name 0.50, concentration 0.40, daily-loss halt 0.10 |
| Trader ids | signal/paper `PAPER-SMOKE-001`, live `TMS-LIVE-REAL-001`, backtest `MULTI-STRAT-001`, spike `SPIKE-001` |, |
| Live/EOD universe cap | 85 (env `TMS_LIVE_UNIVERSE_LIMIT` / `TMS_EOD_UNIVERSE_LIMIT` / `--universe-limit`) |, |
| Live/EOD warmup window | 730 calendar days (2×365) |, |
| Backtest warmup | stocks 400 d calendar, SPY 500 d calendar |
| Price cap | 17_014_118_346_046.0 |
| EOD equity stub | Decimal("1000000") |
| Bar types | `<sym>.<venue>-1-DAY-LAST-EXTERNAL` everywhere; PortfolioHealth uses `-1-MINUTE-LAST-EXTERNAL` SPY |, |
| Redis | `TMS_USE_REDIS` default on; timeouts 5 s/5 s; json encoding; `flush_on_start=False`; `stream_per_topic=True` |
| WS reconnect | `min(30000, 500·2^min(attempts,6))` ms | `use-websocket-stream.ts:66` |
| UI polling | global 5 s; positions 10 s; reconciliation 30 s; hyperopt-running 3 s | `providers.tsx:14`, component files |
| Health thresholds | broker/data 25 h yellow, 48 h red; risk 3 min yellow, 10 min red, headroom <0.02 yellow | `HealthStrip.tsx:42-90` |
| System events tail | 100 (cockpit shows last 6) | `use-system-events.ts:9`, `page.tsx:46` |
| Equity chart thinning | ≤120 points | `EquityCurveChart.tsx:23` |
| launchd | EOD 17:00 local daily; node restart 02:00 local daily; restart grace 30 s | plists, `restart_live_node.sh:26` |
| Timezones | bars normalized to UTC; EOD `today` = UTC date; live `today` = local date; launchd = local wall clock; UI backtest ts rendered in UTC | cited above |

---

## Open questions

1. **PairsRunner bar_types ordering** — `pair_tickers = tuple({t for pair in pairs_spec for t in pair})` iterates a *set*, so the bar_types tuple order is unordered/hash-dependent across runs. Does PairsRunner's behavior depend on bar_types order (subscription order could affect first-bar timing)? EOD uses `sorted(...)` for the same legs. Pick **sorted** order deliberately — confirm this doesn't change live behavior.
2. **Live warmup excludes pair legs from SEPA universe?** EOD excludes `{SPY, sector ETFs, pair legs}` but live `build_and_run` only excludes `{SPY, sector ETFs}`, so KO/PEP/etc. can be in the live SEPA universe but never in the EOD one. Intentional or drift? Go must pick one (spec'd here as: replicate each path as-is until answered).
3. **`make eod-refresh` claims idempotency** (`Makefile:21`) vs the append-only XADD reality (C.6). Is the operator relying on duplicate-append being harmless, and is the ~10k MAXLEN trim applied by some other producer (live node) but never by EOD? Affects whether Go should add `MAXLEN` at the EOD producer.
4. **launchd restart hour** — header comment says 06:00 UTC, plist fires 02:00 *local* (C.5). Which was the operator's true intent for the Go scheduler?
5. **`_default_trader_id` for `live` vs API container** — live publishes under `TMS-LIVE-REAL-001` while the API container default reads `PAPER-SMOKE-001`; the cockpit is blank in live mode until the operator aligns `TMS_TRADER_ID`. Should the Go API auto-discover available `trader-*` namespaces instead?
6. **EOD SEPA per-ticker state publishes** under one strategy_id (C.6) — should Go aggregate (one state per strategy) or byte-match the per-ticker flood? Spec defaults to byte-match.
7. **`paper_acc_id = int(env)`** raises an uncaught ValueError on non-numeric input (vs the curated error for missing). Go: same fail-fast but with a typed error message — acceptable?
8. **`stats` subcommand omits SFP** — bug or intentional (SFP added later)? Go spec'd to include SFP under.
9. **IntradayBreakout** appears in the UI (filter, intent type, state view) and in EOD docs but no IntradayBreakout strategy is registered by `strategy_assembly.build_all` or refreshed by the EOD pipeline. Is it dormant (kept for a future runner) such that Go only needs the UI/type surface, not a runner?
10. **`use-signal-stream` vs page merges generation tie-breaking** — hooks drop incoming when `existing.generation >= incoming` (ties keep old) while page merges overwrite when `existing.generation <= incoming` (ties take new). Intentional? Spec'd as byte-match either way.
11. **`equity_curve` ts sort is lexicographic** on the sampler's ts strings; safe only if all ts share one ISO format+timezone. Confirm sampler always emits uniform ISO-8601 UTC.
