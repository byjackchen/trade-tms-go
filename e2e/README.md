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
                         nautilus-compat), job running -> succeeded via the UI,
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
    18-live-intents       /live cockpit streams signal intents over the WS; the
                          rendered (strategy_id, symbol) rows MATCH tms.signal_intents
                          (as_of NULL streaming rows); the live-intent count agrees
                          with the DB streaming truth and is append-only (DB-checked)
    19-live-health        /live session strip == GET /live/session (mode=signal,
                          status, connected); health strip renders the flat-book
                          informational NAV snapshot (signal mode: no halt; DB-gated)
    20-live-watchlist     /live watchlist rows == the DB tracked universe (distinct
                          intent symbols); never renders a fabricated/unemitted symbol
    21-live-killswitch    click halt -> confirmation -> command enqueued; tms.halts
                          gains an active row; cockpit reflects halted; NO NEW intents
                          after halt (streaming count freezes; DB-checked)
    22-live-mode-switch   mode-switch-to-paper opens a confirmation-PHRASE dialog whose
                          submit is disabled until the phrase is typed (guard only — no
                          actual switch); API rejects set_mode->paper w/o confirm_token
                          (412 confirmation_required); session mode unchanged (DB-checked)
    23-live-console       zero severe console errors on /live (placeholder OR real
                          cockpit; opening+dismissing the halt dialog stays clean)
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

### Live cockpit specs and build order

The /live cockpit ships in P5, after the P1 Data + P2 Backtests + P3
Strategies/Hyperopt workspaces. Specs 18-23 are **permanent** and assert the
documented contract (`docs/api.md` "Live (P5)"; `docs/spec/portfolio-risk.md`,
`docs/spec/ui-runner-modes-eod.md`). The gate runs `tms-live --mode signal`
against the in-repo **mock OpenD** (`TMS_MOOMOO_ADDR` → mock), which replays a
day of bars out of Postgres, so a signal session emits intents into
`tms.signal_intents` + the Redis streams the API bridges to the cockpit's WS.
The real-OpenD smoke is deferred to market hours (`docs/runbooks/live-smoke.md`).

Conventional `data-testid`s the cockpit must expose:
- cockpit root `live-page` (placeholder is `live-placeholder`, the ComingSoon
  testid the suite skips on);
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
