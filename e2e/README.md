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
