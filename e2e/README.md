# TMS end-to-end suite (Playwright)

Permanent browser-driven e2e tests for the TMS control-plane UI (P1: the Data
workspace). The suite runs against a **live compose stack** — it does not start
the stack itself; the CI gate (and the `itest-full` Make target) does.

## Layout

```
e2e/
  playwright.config.ts   baseURL http://localhost:13000, retries 1,
                         trace on-first-retry, html+line+json reporters -> report/
  fixtures/
    global-setup.ts      blocks the suite until API /healthz (postgres up) and
                         the UI /api/healthz (configured) are application-ready
    test.ts              custom `test` with a `consoleErrors` collector fixture
  lib/
    env.ts               single source of truth for ports/creds (all overridable)
    db.ts                direct postgres client: DB-truth counts + seed helpers
                         (Data coverage AND research.* backtest ground truth)
    api.ts               direct API client (auth probe + GET/POST /api/v1 helpers)
    format.ts            mirrors the UI's Intl integer formatting for DB-truth
    backtests.ts         scripted-launch builder: picks tradable tickers + a
                         covered window from postgres, builds the POST body
  seed/
    seed.sql             idempotent deterministic market-data + sync seed
    seed.ts              runner (`--if-empty` skips when bars_daily is non-empty)
  tests/
    01-data-coverage     UI coverage numbers == postgres COUNT(*) / DISTINCT
    02-gap-heatmap       gap heatmap renders (missing + complete + not-found)
    03-refresh-flow      open dialog, parquet + 2 tickers, submit, job -> terminal,
                         Recent jobs gains the row, jobs table grows
    04-cancel-flow       cancel a job through the UI; DB confirms terminal
    05-auth              token-less /api/* = 401; valid token = 200; /healthz public
    06-console-errors    zero severe console errors across every route +
                         the /backtests/{id} detail route + /strategies and
                         /strategies/{id} (self-skip until built)
    07-backtest-launch   open New-backtest dialog, scripted run (2 tickers, ~3mo,
                         close-fill), job running -> succeeded via the UI,
                         detail link targets the persisted run id (DB-checked)
    08-backtest-detail   detail metric cards + equity points + trades/orders MATCH
                         tms.run_metrics/equity_curves/trades AND the API payloads;
                         equity chart (canvas/svg) + trades + orders tables render
    09-backtest-cancel   launch a long-ish backtest (widest window), cancel mid-run
                         from the UI -> status canceled (race-tolerant, DB-checked)
    10-strategies-list   /strategies lists the four shipped strategies (sepa, pairs,
                         sector_rotation, intraday_breakout); each row's active
                         params == tms.active_params -> param_sets (DB) / API
    11-strategy-detail   /strategies/{id} param table == the active document's
                         parameters[*].default, one param-row per parameter (DB/API)
    12-strategy-backtest launch a REAL single-strategy run (SEPA, handful of tickers,
                         ~1yr) from the UI -> WS progress -> succeeded; detail shows
                         equity + trades and metric cards == GET /backtests/{id}
    13-hyperopt-launch   launch a TINY NSGA-II study (pairs, small pop/gens, 1-2 folds)
                         from /hyperopt -> WS progress -> succeeded; detail trials
                         table + Pareto scatter populate; numbers == tms.hyperopt_*
                         / GET /hyperopt/{id}/trials (DB/API-checked)
    14-hyperopt-detail   /hyperopt/{ts} Pareto front (marked non-dominance ==
                         (sharpe,calmar) front computed from the DB, spec §10) +
                         per-fold drill-down (one fold row per recorded fold)
    15-hyperopt-promote  promote a COMPLETE trial -> confirmation -> success; assert
                         tms.active_params now points at the tuned param_set with a
                         full audit row (promoted_by/source_study/source_trial,
                         source_id=hyperopt:<ts>) AND /strategies/{id} renders the
                         new active params
    16-hyperopt-cancel   launch a larger study, cancel mid-run from the UI -> canceled
                         (study row INTERRUPTED; race-tolerant, DB-checked)
    17-hyperopt-console   zero severe console errors on /hyperopt + /hyperopt/{id}
    18-live-intents       /trade cockpit streams signal intents over the WS; the
                          rendered (strategy_id, symbol) rows MATCH tms.signal_intents
                          (as_of NULL streaming rows); the live-intent count agrees
                          with the DB streaming truth and is append-only (DB-checked)
    19-live-health        /trade session strip == GET /trade/session (mode=signal,
                          status, connected); health strip renders the flat-book
                          informational NAV snapshot (signal mode: no halt; DB-gated)
    20-live-watchlist     /trade watchlist rows == the DB tracked universe (distinct
                          intent symbols); never renders a fabricated/unemitted symbol
    21-live-killswitch    click halt -> confirmation -> command enqueued; tms.halts
                          gains an active row; cockpit reflects halted; NO NEW intents
                          after halt (streaming count freezes; DB-checked)
    22-live-mode-switch   mode-switch-to-paper opens a confirmation-PHRASE dialog whose
                          submit is disabled until the phrase is typed (guard only — no
                          actual switch); API rejects set_mode->paper w/o confirm_token
                          (412 confirmation_required); session mode unchanged (DB-checked)
    23-live-console       zero severe console errors on /trade (placeholder OR real
                          cockpit; opening+dismissing the halt dialog stays clean)
    24-paper-blotter      paper session over the mock venue: blotter rows == tms.orders
                          (>=1 FILLED), positions panel == open book, account day-P&L
                          card == Σ realized (all DB-checked; self-skips if signal-only)
    25-portfolio-gate     an over-budget/over-concentration order is REJECTED: a
                          tms.risk_events row (approved=false, a real reference rule id)
                          exists and the gated order never FILLs (DB-checked; safety)
    26-flatten            flatten WITHOUT confirm_token -> 412 (API guard); the cockpit
                          flatten control's typed-phrase dialog closes ALL positions
                          (open-position count -> 0; paper-only, never live)
    27-daily-loss-halt    day P&L below threshold -> active tms.halts(daily_loss) +
                          halt banner; NEW opens rejected (risk.daily_loss_halt) but no
                          daily_loss rejection carries side=FLAT (FLAT still passes)
    28-live-safety        set_mode->live w/o confirm_token -> 412 (mode unchanged, never
                          live); switch-to-live opens a phrase dialog (wrong phrase never
                          arms; canceled -> no switch); NO direct-to-live affordance
    29-reconciliation     reconciliation panel == latest tms.reconciliation_reports
                          (has_issues, mismatch diff = broker_net - strategy_sum); a
                          mismatch is VISUALLY FLAGGED; clean report is not flagged
    30-paper-console      zero severe console errors on the paper cockpit (trading
                          panels render clean; opening+dismissing flatten stays clean)
    39-account-selector   the account selector lists GET /api/v1/trade/accounts (==
                          the tms.accounts registry) + an "All accounts" sentinel;
                          selecting one sets ?account=<id> and threads account_id=<id>
                          into the positions read (per-account filter); clearing to
                          "All accounts" drops both (DB- + request-checked)
```

### Strategies specs and build order

The Strategies workspace ships after the P1 Data + P2 Backtests workspaces.
Specs 10-12 (and the `/strategies` + `/strategies/{id}` console-error cases in
06) are **permanent** and assert the documented contract (conventional
`data-testid`s: list root `strategies-page` with `strategy-row-<id>` rows;
detail root `strategy-detail` with `param-row-<name>` rows carrying
`data-param-value`; the strategy launch affordance `strategy-backtest-launch`
or a `backtest-strategy` selector on the shared New-backtest dialog). While the
section is still the `coming-soon` placeholder / the route is unbuilt, they
**self-skip** cleanly so the gate stays green; once wired the assertions bind
and never weaken. Every active-param value and metric card is compared to ground
truth queried independently from postgres (tms.active_params -> param_sets) and
the API — no fabricated values. The four canonical strategy ids come from
`internal/params/loader.go` (sepa, pairs, sector_rotation, intraday_breakout).

### Hyperopt specs and build order

The Hyperopt studio ships after the P1 Data + P2 Backtests + P3 Strategies
workspaces. Specs 13-17 are **permanent** and assert the documented contract
(`docs/api.md` Hyperopt; conventional `data-testid`s: list root `hyperopt-page`
with `hyperopt-study-row-<ts>`; launch `hyperopt-launch` / `open-hyperopt-dialog`
-> `hyperopt-dialog` / `hyperopt-form` with `hyperopt-{strategy,start,end,
population,generations,folds,tickers}` + `hyperopt-submit`; the shared
`job-progress` panel drives the `hyperopt.run` job; detail root `hyperopt-detail`
[data-study-ts] with `hyperopt-trials-table` / `hyperopt-trial-row-<n>`
[data-sharpe/data-calmar/data-pareto], `pareto-scatter` [data-pareto-count],
per-fold `trial-fold-<n>-<fold>` / `trial-fold-row-<n>`; promotion
`hyperopt-promote-<n>` -> `hyperopt-promote-confirm` -> `hyperopt-promote-
success`). While the section is still the `coming-soon` placeholder
(`hyperopt-placeholder`) / a route is unbuilt / the stack has no tradable bars
or persisted study, they **self-skip** cleanly so the gate stays green; once
wired the assertions bind and never weaken. The Pareto front the UI marks is
checked against the non-dominance (weak dominance + strict improvement, spec
§10) computed independently in `lib/hyperopt.markPareto` from the DB objective
points, and the promotion's effect is verified against tms.active_params JOIN
tms.param_sets (audit columns promoted_by / source_study / source_trial /
source_id) — no fabricated values.

### Trade cockpit specs and build order

The /trade cockpit ships in P5, after the P1 Data + P2 Backtests + P3
Strategies/Hyperopt workspaces. Specs 18-23 are **permanent** and assert the
documented contract (`docs/api.md` "Trade (P5)"; `docs/spec/portfolio-risk.md`,
`docs/spec/ui-runner-modes-eod.md`). The gate runs `tms trade run --mode signal`
against the in-repo **mock OpenD** (`TMS_MOOMOO_ADDR` → mock), which replays a
day of bars out of Postgres, so a signal session emits intents into
`tms.signal_intents` + the Redis streams the API bridges to the cockpit's WS.
The real-OpenD smoke is deferred to market hours (`docs/runbooks/trade-smoke.md`).

Conventional `data-testid`s the cockpit must expose:
- cockpit root `live-page` (placeholder is `live-placeholder`, the ComingSoon
  testid the suite skips on);
- account selector `account-selector` with the `account-selector-input`
  `<select>` (one `account-option` per `GET /api/v1/trade/accounts` row + an
  "All accounts" sentinel); selecting writes `?account=<id>` which the positions
  panel / blotter / account panel read back as their `account_id` filter;
- session strip `live-session` [`data-mode` / `data-status` / `data-halted`];
  connection indicator `live-connection` [`data-connected`];
- health strip `live-health` [`data-daily-loss-halt`], optional
  `live-health-day-pnl`;
- intents panel `live-intents` [optional `data-intent-count`] with
  `live-intent-row` [`data-strategy-id` / `data-symbol` / `data-state`];
- watchlist `live-watchlist` with `live-watchlist-row` [`data-symbol`];
- kill-switch `live-halt-button` -> `live-halt-confirm` (optional
  `live-halt-reason`) -> `live-halt-confirm-submit` / `live-halt-confirm-cancel`;
  halted banner `live-halted-banner`;
- mode-switch `live-mode-switch-paper` -> `live-mode-confirm` with
  `live-mode-confirm-phrase` + a `live-mode-confirm-submit` DISABLED until the
  phrase is typed + `live-mode-confirm-cancel`.

While the section is still the placeholder, the API has no live reader (live
endpoints return `503` — gated via `liveReaderAvailable()`), or no signal
session has emitted yet, the specs **self-skip** cleanly so the gate stays
green; once wired the assertions bind and never weaken. Every rendered intent /
watchlist symbol / halt is compared to ground truth queried independently from
postgres (`tms.sessions` / `tms.signal_intents` (as_of NULL) / `tms.halts`) and
the API — no fabricated values. The mode-switch guard is asserted at BOTH the UI
(disabled-until-phrase dialog) and the API boundary (412 `confirmation_required`
without a `confirm_token`); neither test ever completes a real mode switch.

### Paper-trading specs and build order (P6)

Specs 24-30 cover the **paper-trading cockpit + safety** and ship in P6, after
the P5 signal cockpit. The gate runs `tms trade run --mode paper` against the in-repo
**mock trading venue** (the P5 mock OpenD extended to accept `Trd_PlaceOrder` and
simulate accept->fill / reject + push `Trd_UpdateOrder` / `Trd_UpdateOrderFill`,
P6 decision 9), so a strategy order flows through the portfolio gate (decision 4),
the `MoomooExecutor` (decision 2) and the order-state machine (decision 3) into
`tms.orders` / `fills` / `positions` / `risk_events` (decision 5, the durable
system-of-record). The real paper/live-account smoke is **deferred** to market
hours (`docs/runbooks/trade-smoke.md`).

Contract these specs assert (`docs/api.md` "Trade trading (P6, paper/live)";
`docs/spec/portfolio-risk.md`). API reads: `GET /trade/{orders,fills,positions,
account,reconciliation}` (503 when no trading reader — gated via
`liveTradingAvailable()`). Commands: `flatten` / `emergency_kill` / `set_mode`
to `paper`/`live` require a `confirm_token` (412 `confirmation_required` without
it). Conventional `data-testid`s the paper cockpit must expose:
- blotter `live-blotter` with `live-blotter-order-row` [`data-client-order-id` /
  `data-symbol` / `data-status` / `data-filled-qty`];
- positions `live-positions` with `live-position-row` [`data-symbol` /
  `data-signed-qty`];
- account `live-account` with `live-account-day-pnl` [`data-day-pnl-usd`];
- gate decisions `live-risk-events` with `live-risk-event-row`
  [`data-rule-name` / `data-symbol` / `data-approved`];
- flatten `live-flatten-button` -> `live-flatten-confirm` with
  `live-flatten-confirm-phrase` + a `live-flatten-confirm-submit` DISABLED until
  the phrase (`FLATTEN`) is typed + `live-flatten-confirm-cancel`;
- reconciliation `live-reconciliation` [`data-has-issues`] with
  `live-reconciliation-mismatch` (the visible flag) + `live-recon-mismatch-row`
  [`data-symbol` / `data-diff` / `data-broker-net` / `data-strategy-sum`];
- the switch-to-live control `control-mode-live` reuses the shared
  `live-mode-confirm` phrase dialog (same guard as paper).

**SAFETY is the top acceptance criterion.** The live-safety spec (28) and the
flatten spec (26) assert the dangerous guards at BOTH the UI (disabled-until-
phrase dialog; a wrong/near-miss phrase never arms) AND the API boundary (412
without a `confirm_token`), and **never** complete a real live switch or a
flatten against a non-paper session — there is no real account in this gate. The
gate (25/27) is proven by durable `tms.risk_events` rejections carrying the
byte-identical reference rule ids (`allocator.budget_exceeded`,
`risk.max_single_name`, `risk.concentration`, `risk.daily_loss_halt`); a gated
order **never** reaches a FILLED state, and a `daily_loss` rejection **never**
carries `side=FLAT` (FLAT/close orders still pass during a halt by design).
Every order / fill / position / day-P&L / reconciliation drift the UI renders is
compared to ground truth queried independently from postgres (lib/db, money
decoded from fixed-point 1e-4) and the API — no fabricated values. While the
paper-trading panels are not built, the API has no trading reader (503), or no
paper session has run / no order filled this run, the specs **self-skip** cleanly
so the gate stays green; once wired the assertions bind and never weaken.

### Manual trading desk specs and build order (P6)

Specs 32-38 cover the **operator-driven MANUAL trading desk** — the ONLY
broker-mutation surface in the HTTP API (`docs/api.md` "Manual trading desk"). The
desk lets the operator place / cancel / close orders **by hand** against a paper
or live account, in ANY strategy mode (signal: the operator IS the executor;
paper/live: an override alongside the auto book), attributed to the `MANUAL`
pseudo-strategy so reconciliation + per-strategy accounting stay clean. It reuses
the `MoomooExecutor` + `Trd_*` client + the order-state machine +
`tms.orders`/`fills`/`positions`/`risk_events` + the **mock trading venue**, run
in **PAPER** in the gate (`--manual-mode paper`). The endpoints live on the live
node's bearer-guarded manual listener (`/api/v1/trade/*`, reached via
`MANUAL_BASE_URL`, default the API host); when no desk is connected every endpoint
returns **503** and the specs self-skip.

The specs (and what each asserts):
- **32 order ticket** — place a paper BUY (`POST /trade/order`, idempotent
  client-order-id); blotter shows submitted -> FILLED; the MANUAL positions panel
  gains the symbol; account/day-P&L renders; every rendered row MATCHES
  `tms.orders`/`positions` (DB truth, money decoded 1e-4); the placement is
  audited (`tms.audit_log`).
- **33 close position** — click Close on a MANUAL position ->
  confirmation -> `POST /trade/position/{symbol}/close`; the symbol's signed qty
  -> 0; a closing order appears in the blotter (a close BYPASSES the budget; paper
  close still types the trade password).
- **34 trade-from-signal** — click Trade on a watchlist signal; the ticket
  PRE-FILLS the symbol (+ side from the intent); submit places the MANUAL order.
- **35 risk override** — an over-limit opening order ⇒ **422 `risk_violation`**
  (durable `MANUAL` `approved=false` `risk_events` row, gate held — no fill);
  `override: true` resubmit is accepted and writes an **approved** `risk_events`
  row (the audited operator decision). Asserted at BOTH the API boundary and the
  inline-violation UI; skips if the stack's budget did not gate this run.
- **36 LIVE SAFETY** (the TOP criterion) — a live manual order WITHOUT the
  per-order confirm phrase (`I CONFIRM THIS REAL MONEY MANUAL ORDER`) ⇒ **412
  `confirmation_required`**; a WRONG/near-miss phrase is ALSO 412; a **paper/signal
  desk targeting live is refused** (never a 200 real order); the UI switch-to-live
  opens a guarded phrase dialog (disabled until the EXACT phrase) and is only ever
  CANCELLED; no direct-to-live affordance exists. **Never** places a real order.
- **37 cancel + console** — rest a BUY LIMIT far from the market, click Cancel on
  its row; the order reaches a terminal `CANCELLED_*` state OR truthfully reports
  **501 `cancel_unsupported`** (never falsely "cancelled"); and the manual trade
  surface renders with ZERO severe console / page errors.
- **38 sync from broker** (DIRECTION 2, broker -> TMS — the operator's PRIMARY
  case) — `POST /trade/sync` pulls the account's ACTUAL state and REFLECTS it under
  the `MANUAL`/EXTERNAL book; **READ-ONLY** at the broker (`read_only:true`, places
  NO order, so it gates ONLY on a connected desk — **not** `manualDeskIsPaper()`,
  safe in every mode incl signal); **audited** (`trade.manual.sync` row);
  **idempotent** — re-syncing the same broker state reflects nothing
  (`reflected:0`, the MANUAL book's distinct-symbol count does not grow, no
  duplicate rows). Proven at BOTH the API boundary (200 shape + audit + idempotency)
  and the desk UI (`manual-sync-button` -> read-only `manual-sync-result` toast;
  reflected positions in `manual-positions` match the DB; re-sync adds no symbol).
  `reconciliation` in the 200 body is OPTIONAL (present only when a reconciler is
  wired). Self-skips until the sync panel ships / no desk is connected.

Conventional `data-testid`s the manual desk must expose (the specs self-skip on
the root testid until the desk ships, then bind hard): desk root `manual-desk`
(coming-soon `manual-desk-placeholder`); ticket `manual-ticket` with
`manual-ticket-{symbol,side,qty,type,limit-price,confirm,submit}` +
`manual-ticket-{violation,override,override-confirm}`; blotter `manual-blotter`
with `manual-blotter-order-row` [`data-client-order-id`/`data-symbol`/`data-status`
/`data-filled-qty`] + per-row `manual-order-cancel`; positions `manual-positions`
with `manual-position-row` [`data-symbol`/`data-signed-qty`] + per-row
`manual-position-close`; close confirm `manual-close-confirm` with
`manual-close-confirm-{input,submit,cancel}`; account `manual-account` with
`manual-account-day-pnl` [`data-day-pnl-usd`]; trade-from-signal
`manual-trade-from-signal` [`data-symbol`/`data-side`]; live arming
`manual-mode-live` -> `manual-live-confirm` with
`manual-live-confirm-{phrase,submit,cancel}`; sync-from-broker `manual-sync`
[`data-last-synced`] with `manual-sync-button`, `manual-sync-last`,
`manual-sync-result` [`data-read-only`/`data-has-drift`/`data-reflected`],
`manual-sync-read-only`, `manual-sync-counts`
[`data-positions`/`data-orders`/`data-fills`], `manual-sync-error`. **SAFETY is paramount** — the desk
specs NEVER place against a live account (every order-placing case gates on
`manualDeskIsPaper()`), and the live guards are proven at the API boundary
(412/422 refusals) + the disabled-until-exact-phrase dialog without ever arming a
real order. All ground truth is queried independently from postgres (`lib/db`,
MANUAL-scoped `strategy_id = 'MANUAL'`) — no fabricated values.

### Backtests specs and build order

The Backtests workspace ships after the P1 Data workspace. Specs 07-09 (and the
new detail console-errors case) are **permanent** and assert the documented
contract (`docs/api.md` Backtests, conventional `data-testid`s mirroring the
Data workspace: `open-backtest-dialog` / `backtest-form` / `backtest-submit`,
the shared `job-progress` panel, `/backtests/{id}` `backtest-detail` with
`metric-*` cards, `equity-chart`, `trades-table`, `orders-table`). While the UI
is still the `coming-soon` placeholder, or the stack has no tradable bars / no
COMPLETE run, they **self-skip** cleanly so the gate stays green; once the UI is
wired the assertions bind and never weaken. Every rendered number is compared to
ground truth queried independently from postgres (`research.*`) and the API — no
fabricated values.

## Running

```bash
# one-time: download the chromium browser
make e2e-install            # or: npm --prefix e2e run install:browser

# run the suite (expects the stack already up on 13000/18080/55432)
make e2e

# full integration: compose up --wait + seed-if-empty + suite + compose down
make itest-full
```

The HTML report is written to `e2e/report/html/` (plus `report/results.json`);
open it with `npm --prefix e2e run report`.

## Configuration

Defaults target this project's reserved host ports (UI 13000, API 18080,
postgres 55432). Override any value via env vars — see `.env.example`. The
bearer token defaults to `local-e2e-token`; it must equal the API's
`TMS_API_TOKEN` and the UI proxy's token. The gate sets all three to the same
value.

## Determinism

The suite asserts UI numbers against postgres directly, so it needs *some* data.
`seed/seed.sql` plants a fixed, idempotent fixture (tickers CLEAN/GAPPY/DELIS,
daily bars with a deliberate gap on GAPPY, fundamentals, events, sync
watermarks + run history). It never seeds `tms.jobs` — the refresh/cancel specs
create jobs live so the job-count assertions stay honest. Specs that depend on
seed-only tickers self-skip when the stack carries real data instead.
