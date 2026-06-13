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
    api.ts               direct API client (auth-rejection test + health probe)
    format.ts            mirrors the UI's Intl integer formatting for DB-truth
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
    06-console-errors    zero severe console errors across every route
```

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
