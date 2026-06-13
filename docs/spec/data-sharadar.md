# Spec: Sharadar Data Layer (SEP / SFP / SF1 / EVENTS / TICKERS)

Extracted from the Python reference repo `trade-multi-strategies` (read-only).
Target: byte-equivalent Go implementation in `github.com/byjackchen/trade-tms-go`.

Source roots:

- `src/data/sharadar_cache/` — layout, meta, writers, reader, catchup
- `src/adapters/sharadar/` — Nasdaq Data Link client, backtest history provider
- Consumers that pin column contracts: `scripts/multi_strategy_backtest.py`,
  `src/runner/live_runner.py`, `src/runner/eod/refresh.py`,
  `src/portfolio/context_refresher.py`, `src/cli/sync_sharadar_universe.py`
- Tests (encode exact semantics): `tests/data/sharadar_cache/*`,
  `tests/adapters/test_sharadar_client.py`,
  `tests/adapters/sharadar/test_history_provider.py`

All `path:line` citations are relative to
`/Users/byjackchen/codespace/trade-multi-strategies/`.

Tagging convention:

- **[MUST-MATCH]** — Go must replicate exactly (edge cases, NaN handling, ordering, return shapes).
- **[IMPROVE]** — known weakness in the original; Go may improve. Both the original behavior AND the proposed improvement are described; if improved, the deviation must be documented and the original behavior must remain reproducible where it affects data equality.

---

## 1. Configuration and environment

| Env var | Default | Meaning | Source |
|---|---|---|---|
| `NASDAQ_DATA_LINK_API_KEY` | *(none — required for any API call)* | Nasdaq Data Link API key | `src/config.py:41,68`; `src/adapters/sharadar/client.py:41-44` |
| `TMS_SHARADAR_CACHE_DIR` | `""` → `<repo_root>/cache/sharadar` | Cache root override; `~` expanded | `src/config.py:53-54,74`; `src/data/sharadar_cache/layout.py:39-48` |
| `TMS_AUTO_SYNC` | `"1"` | `"0"` disables live-node auto-catchup (also `--no-sync` CLI flag) | `src/runner/live_runner.py:233-236` |
| `TMS_LIVE_UNIVERSE_LIMIT` | `85` | Live SEPA universe cap (top-N by market cap) | `src/runner/live_runner.py:50-61` |

- **[MUST-MATCH]** Cache root resolution: if `TMS_SHARADAR_CACHE_DIR` is non-empty after `strip()`, use it (with `~` expansion). Otherwise walk up from the package file's directory until a directory containing `pyproject.toml` is found; cache root = `<that dir>/cache/sharadar`. If no marker file found, raise an error telling the operator to set `TMS_SHARADAR_CACHE_DIR`. There is **no home-dir fallback**. (`src/data/sharadar_cache/layout.py:14-48`; test `tests/data/sharadar_cache/test_layout.py:42-58`.) For Go: the repo-root marker should be `go.mod` *or* a configured marker — see Open questions Q1.
- **[MUST-MATCH]** Missing API key must raise a configuration error naming the key (`nasdaq_data_link_api_key`) with a hint pointing to `https://data.nasdaq.com/account/profile` — fail loud at client construction, not first call. (`src/adapters/sharadar/client.py:41-44`; test `tests/adapters/test_sharadar_client.py:48-55`.) An explicitly passed key overrides config (`tests/adapters/test_sharadar_client.py:43-45`).

---

## 2. Dataset schemas

The cache stores **whatever columns the Nasdaq Data Link API returns** — writers never prune
columns except TICKERS (§2.5). Schemas below were dumped from the real production cache
(`cache/sharadar/` parquet footers, pandas 2.3.3 / pyarrow defaults).

### 2.1 SEP — daily stock bars (`SHARADAR/SEP`)

On-disk parquet schema (observed at `cache/sharadar/SEP/year=2024/part-0.parquet`):

| Column | Arrow type | Notes |
|---|---|---|
| `ticker` | `string` | Sharadar ticker symbol |
| `date` | `timestamp[ns]` | **tz-naive**, normalized to midnight (date-only) |
| `open` | `double` | split-adjusted (Sharadar SEP convention) |
| `high` | `double` | split-adjusted |
| `low` | `double` | split-adjusted |
| `close` | `double` | split-adjusted |
| `volume` | `double` | **float, not int** — NaN volume rows exist upstream |
| `closeadj` | `double` | fully adjusted close (splits + dividends) |
| `closeunadj` | `double` | raw unadjusted close |
| `lastupdated` | `timestamp[ns]` | Sharadar row revision date |

- **[MUST-MATCH]** Adjusted-vs-raw semantics: every strategy consumer reads the **split-adjusted `open/high/low/close`** columns directly; `closeadj` and `closeunadj` are stored but never consumed anywhere in the codebase (verified by grep over `src/`). Bars handed to the engine are `df[["open","high","low","close","volume"]]` (`scripts/multi_strategy_backtest.py:206-221`), and EOD refresh builds `Bar` from `row["open"|"high"|"low"|"close"|"volume"]` plus `row["date"]` (`src/runner/eod/refresh.py:90-105`). The Go port must keep `closeadj`/`closeunadj` in the cache (COMPLETE-ness) even though nothing reads them yet.
- **[MUST-MATCH]** `volume` must tolerate NaN (it is float). Conversion to int happens only at consumer boundaries: `int(row["volume"])` (`src/runner/eod/refresh.py:102`). NaN-containing tickers are *dropped by the backtest consumer*, not cleaned in the cache: `_has_nan_bars` checks NaN in any of `open,high,low,close,volume` and the ticker is skipped with its name collected into `skipped_nan` (`scripts/multi_strategy_backtest.py:139-150,427-431`).
- **[MUST-MATCH]** Price-cap filter (consumer-side, not cache-side): tickers whose any-of-`open,high,low,close` max exceeds `_NAUTILUS_PRICE_MAX = 17_014_118_346_046.0` are skipped (`scripts/multi_strategy_backtest.py:87,129-136,424-426`). Rationale: 1:100/1:1000 reverse splits balloon back-adjusted prices.
- **[IMPROVE]** Relational-store price representation (Go-only deviation, sanctioned here). *Original:* the parquet cache stores `open/high/low/close/closeadj/closeunadj/dividends` as raw `double` (float64); the parquet layer of the Go port keeps that exactly, and the float64 round-trip gate of §12 applies to it unchanged. *Improvement:* the **PostgreSQL store** (`migrations/000002_marketdata.up.sql` `tms.bars_daily`, `internal/data/sharadar/convert.go`) persists those columns as BIGINT 1e-4 fixed point per the project Money model (`docs/spec/domain-types-money.md` §1.2, `Decimal(str(x)).quantize(0.0001, ROUND_HALF_EVEN)`), with unrepresentable values (±Inf, or |x| beyond int64 at 1e-4 scale ≈ 9.22e14 USD) stored as NULL and counted in `ImportStats.FieldsNulled`. Empirical evidence against the 2026-05-28 production cache that this loses nothing consumer-observable: (a) zero prices fall below 0.00005, so nothing quantizes to 0; (b) only ~0.005–0.02% of cells per year-partition are not exactly 1e-4-representable, all at 1e7–1e18 magnitudes where the 4-decimal quantization error is at or below the float64 ULP; (c) exactly 3,479 cells — a single ticker, BINI, peak 1.4065e18 — overflow int64 at 1e-4 scale and are stored NULL with FieldsNulled accounting, and BINI still exceeds `_NAUTILUS_PRICE_MAX` via its surviving sub-overflow rows, so the consumer-side `skipped_overflow` classification above is reproduced bit-for-bit. The original float64 behavior remains reproducible: the parquet cache stays the source of truth for cross-language round-trip parity (§12); the DB is a derived store.

### 2.2 SFP — daily ETF/fund bars (`SHARADAR/SFP`)

- **[MUST-MATCH]** Identical column schema to SEP (verified against `cache/sharadar/SFP/year=2024/part-0.parquet`; test fixture comment `tests/data/sharadar_cache/test_writer_sfp.py:18-22`). Same writers' semantics, separate partition tree (§4). ~6,500 tickers vs SEP's ~3,500 (`src/data/sharadar_cache/writer_sfp.py:1-9`).

### 2.3 SF1 — quarterly fundamentals (`SHARADAR/SF1`)

On-disk schema (observed at `cache/sharadar/SF1/ticker=AAPL.parquet`): 111 columns.
Key/index columns:

| Column | Arrow type | Notes |
|---|---|---|
| `ticker` | `string` | |
| `dimension` | `string` | one of `ARQ ART MRQ MRT ARY MRY` — **all cached** |
| `calendardate` | `timestamp[ns]` | normalized quarter-end |
| `datekey` | `timestamp[ns]` | **filing date** — the point-in-time key; tz-naive normalized |
| `reportperiod` | `timestamp[ns]` | fiscal period end |
| `fiscalperiod` | `string` | e.g. `Q3` |
| `lastupdated` | `timestamp[ns]` | |

Remaining 104 metric columns are all `double` (full list, alphabetical, as observed on disk):
`accoci assets assetsavg assetsc assetsnc assetturnover bvps capex cashneq cashnequsd cor consolinc currentratio de debt debtc debtnc debtusd deferredrev depamor deposits divyield dps ebit ebitda ebitdamargin ebitdausd ebitusd ebt eps epsdil epsusd equity equityavg equityusd ev evebit evebitda fcf fcfps fxusd gp grossmargin intangibles intexp invcap invcapavg inventory investments investmentsc investmentsnc liabilities liabilitiesc liabilitiesnc marketcap ncf ncfbus ncfcommon ncfdebt ncfdiv ncff ncfi ncfinv ncfo ncfx netinc netinccmn netinccmnusd netincdis netincnci netmargin opex opinc payables payoutratio pb pe pe1 ppnenet prefdivis price ps ps1 receivables retearn revenue revenueusd rnd roa roe roic ros sbcomp sgna sharefactor sharesbas shareswa shareswadil sps tangibles taxassets taxexp taxliabilities tbvps workingcapital`

- **[MUST-MATCH]** SF1 columns *actually consumed* by code: `ticker`, `datekey`, `dimension`, `marketcap` (`src/portfolio/context_refresher.py:108-147`; `scripts/multi_strategy_backtest.py:182-204,224-246`) and `revenue` only in test fixtures. All other columns must still be cached verbatim (no pruning) — `bootstrap_sf1` stores the API frame as-is (`src/data/sharadar_cache/writer_sf1.py:44-74`; test `tests/data/sharadar_cache/test_writer_sf1.py:46-60` asserts the API call has **no** dimension filter).
- **[MUST-MATCH]** All six dimensions coexist per `(ticker, datekey)`; dedup key is `(ticker, datekey, dimension)` (`src/data/sharadar_cache/writer_sf1.py:24`). `marketcap` unit: USD (Sharadar reports raw USD, e.g. `3.4e12` for AAPL); consumers compare `marketcap > 0` and convert via `Decimal(str(value))` (`src/portfolio/context_refresher.py:136-146`).

### 2.4 EVENTS — corporate events (`SHARADAR/EVENTS`)

On-disk schema (observed):

| Column | Arrow type | Notes |
|---|---|---|
| `ticker` | `string` | |
| `date` | `timestamp[ns]` | event date, tz-naive normalized |
| `eventcodes` | `string` | **pipe-separated numeric codes**, e.g. `"13"`, `"22|71"` |

- **[MUST-MATCH]** Earnings filter: a row is an earnings event iff `"22"` is an exact member of `eventcodes.split("|")` — NOT substring match (`"122"` or `"221"` must not match). (`scripts/multi_strategy_backtest.py:256-267`.) After filtering, the `date` column is renamed `report_date` for the earnings consumers.
- **[MUST-MATCH]** Dedup key is `(ticker, date, eventcodes)` — multiple same-day events with different code strings coexist (`src/data/sharadar_cache/writer_events.py:23` and module docstring lines 1-9).

### 2.5 TICKERS — universe master (`SHARADAR/TICKERS`)

Unlike the others, TICKERS **is filtered and column-pruned** at write time.

Keep-columns, in this exact order (`src/data/sharadar_cache/writer_tickers.py:34-48`):
`ticker, name, exchange, isdelisted, category, sector, industry, table, firstpricedate, lastpricedate, delistedate` — intersected with whatever columns the API actually returned (`writer_tickers.py:77`).

On-disk observed schema (real cache): `ticker name exchange isdelisted category sector industry table` as `string`, `firstpricedate lastpricedate` as `timestamp[ns]`. **`delistedate` is absent on disk** — the live API does not return a column by that name, so the keep-list intersection drops it silently. See Open questions Q2.

- **[MUST-MATCH]** Row filter (survivorship-bias policy), `src/data/sharadar_cache/writer_tickers.py:62-72`:
  - SF1 stocks: keep iff `table == "SF1"` AND `category.fillna("").startswith("Domestic Common Stock")` — keeps **both active and delisted** (catches `"Domestic Common Stock"` and `"Domestic Common Stock Primary Class"`; drops preferred etc.).
  - SFP funds: keep iff `table == "SFP"` AND `isdelisted == "N"` — **active only** (delisted ETFs dropped).
  - NaN `category` is treated as `""` (fillna before startswith) → dropped for SF1.
  - Test oracle: `tests/data/sharadar_cache/test_writer_tickers.py:20-71` (7-row fixture → exactly `AAPL, ACWX, DEAD, MSFT, SPY` survive).
- **[MUST-MATCH]** Output is sorted by `ticker` ascending, index dropped, full overwrite of `TICKERS.parquet` (no merge) (`writer_tickers.py:78-80`; overwrite test `test_writer_tickers.py:91-110`).
- **[MUST-MATCH]** API call shape: `get_table("SHARADAR/TICKERS", paginate=True)` with **no other filters** (`writer_tickers.py:60`; asserted `test_writer_tickers.py:74-88`).
- **[MUST-MATCH]** Return value: number of rows written. Log line includes SF1 active / SF1 delisted / SFP counts (`writer_tickers.py:81-88`).
- **[MUST-MATCH]** `isdelisted` values are the strings `"N"` / `"Y"`. `lastpricedate` may be an empty string `""` in API output (tests use `""`), which the reader coerces to NaT = "still active" (§7.2). On the real disk cache it is `timestamp[ns]` with NaT for active tickers. Reader code must accept **both** representations (`src/data/sharadar_cache/reader.py:18-20,39-44`).

### 2.6 Timezone conventions (global)

- **[MUST-MATCH]** All date-like columns persisted in the cache are **tz-naive, normalized to 00:00:00** (`pd.to_datetime(col).dt.tz_localize(None).dt.normalize()`); semantics: if the input was tz-aware, drop the tz designator keeping the wall-clock value, then truncate to midnight. Verified: pandas `tz_localize(None)` is a no-op on naive input. (`writer_sep.py:61-65`, `writer_sfp.py:26-30`, `writer_sf1.py:28-31` (on `datekey`), `writer_events.py:32-35`.)
- **[MUST-MATCH]** `.meta.json` timestamps are ISO 8601 **UTC** with offset (e.g. `2026-05-28T16:04:12.539258+00:00`) — `astimezone(UTC).isoformat()` on save (`src/data/sharadar_cache/meta.py:50-52`).
- **[MUST-MATCH]** Consumers re-attach UTC at the engine boundary: backtest sets `index = pd.to_datetime(df["date"]).tz_localize("UTC")` (`scripts/multi_strategy_backtest.py:206-221`); EOD refresh localizes a naive `date` to UTC before building a `Bar` (`src/runner/eod/refresh.py:92-95`). i.e. cache layer = naive dates; engine layer = UTC midnight.
- **[MUST-MATCH]** "Today" for catchup is `datetime.now(UTC).date()` (`src/data/sharadar_cache/catchup.py:68-69,109`); for the `update` CLI subcommand it is local `date.today()` (`src/cli/sync_sharadar_universe.py:147`). See Open questions Q3.

---

## 3. Nasdaq Data Link client (`SharadarClient`)

Source: `src/adapters/sharadar/client.py`. Tests: `tests/adapters/test_sharadar_client.py`.

### 3.1 `get_table(dataset, *, paginate=True, **filters) -> DataFrame`

- **[MUST-MATCH]** Delegates to the Nasdaq Data Link "datatables" API (Python SDK `nasdaqdatalink.get_table(dataset, api_key=..., paginate=..., **filters)`). Filters used in this codebase:
  - `date={"gte": "YYYY-MM-DD", "lte": "YYYY-MM-DD"}` (SEP/SFP) — REST form: `date.gte=...&date.lte=...`
  - `ticker=[...list of symbols...]` (SF1/EVENTS/smoke-test SEP/SFP) — REST form: comma-joined `ticker=`
  - `paginate=True` means: follow `cursor_id` pages until exhausted; the SDK caps total at ~1M rows per call. Go must implement cursor pagination against `https://data.nasdaq.com/api/v3/datatables/<dataset>.json`.
- **[MUST-MATCH]** Retry policy (`client.py:27,58-100,179-192`):
  - Max **4 attempts** total (`_MAX_ATTEMPTS = 4`).
  - Backoff before retry `attempt n` (1-based): `2**n` seconds → **2, 4, 8** s waits between the 4 attempts; the test for "gives up" asserts exactly 4 underlying calls (`test_sharadar_client.py:124-133`). (Docstring says "2/4/8/16" but the 16 s wait is computed then the loop exits — only 3 sleeps ever happen.)
  - Retryable iff the stringified error (`f"{type(e).__name__}: {e}"`) contains `"429"` or any of `"500" "501" "502" "503" "504"` as a **substring**. Everything else propagates immediately, no retry (`client.py:179-192`; tests at `test_sharadar_client.py:157-170`: `401/403/404` → False).
  - After exhausting attempts on retryable errors: raise `RuntimeError("get_table {dataset} failed after 4 retries: {last_err}")` — match message shape `failed after` (`client.py:98-100`; test regex `"failed after"`).
- **[MUST-MATCH]** Counters (`client.py:46-56,81-86,106-117`): each *successful* call increments `fetch_count` and `cache_miss_count` and sets `last_fetch_ts` (ns since epoch; 0 = never). Failed calls increment nothing (`test_sharadar_client.py:217-227`). `record_cache_hit()` increments `cache_hit_count` only.
- **[MUST-MATCH]** `state_summary()` JSON shape (`client.py:119-150`; tests 233-262):
  `{"source": "sharadar", "fetch_count": int, "cache_hit_count": int, "cache_miss_count": int, "last_fetch_ts": ISO-8601-UTC-string|null, "last_fetch_ts_ns": int, "quota_used_today": null}` — `quota_used_today` is always `null` (the SDK never exposes quota).
- **[IMPROVE]** Substring-based HTTP-status classification (`"500" in err_text`) can false-positive (e.g. a ticker `X500` or row-count `500` embedded in an error message triggers retry). Original: substring scan over the formatted error string. Improvement for Go: classify on the actual HTTP status code from the response (`resp.StatusCode == 429 || >= 500`), plus context-cancellation awareness; keep the 4-attempt / 2-4-8s schedule identical. Also honor `Retry-After` if present (additive, never weaker than the original).
- **[IMPROVE]** `time.sleep` is uninterruptible. Original: blocking sleep between retries. Improvement: `select { case <-ctx.Done(): ...; case <-timer.C: }` so shutdown cancels in-flight backoff (required by PRODUCTION-GRADE bar).

### 3.2 `export_table(dataset, *, target_dir, **filters) -> Path`

- **[MUST-MATCH]** Bulk async export: creates `target_dir` (mkdir -p), filename `<target_dir>/<dataset with "/"→"_">.zip`, delegates to SDK `export_table` (triggers Sharadar's async export job, polls until ready, downloads zip), returns the resulting path (`client.py:153-176`; test `test_sharadar_client.py:139-149`). Currently unused by any sync path (bootstrap goes through paginated `get_table`) but must exist (COMPLETE).

---

## 4. Parquet cache layout (`CacheLayout`)

Source: `src/data/sharadar_cache/layout.py`. Tests: `tests/data/sharadar_cache/test_layout.py`.

- **[MUST-MATCH]** Path map (all relative to `root`):

| Artifact | Path |
|---|---|
| meta | `.meta.json` (atomic temp: `.meta.json.tmp` — note `with_suffix` on `.meta.json` yields `.meta.json.tmp`) |
| tickers | `TICKERS.parquet` |
| SEP partition | `SEP/year=<YYYY>/part-0.parquet` |
| SFP partition | `SFP/year=<YYYY>/part-0.parquet` |
| SF1 per ticker | `SF1/ticker=<TICKER>.parquet` |
| EVENTS per ticker | `EVENTS/ticker=<TICKER>.parquet` |

(`layout.py:51-84`; exact-path tests `test_layout.py:26-38`.)

- **[MUST-MATCH]** `ensure_dirs()` creates `root, SEP/, SFP/, SF1/, EVENTS/` with parents, idempotent (`layout.py:86-89`).
- **[MUST-MATCH]** One file per year partition (`part-0.parquet` literally; `stats()` globs `part-*.parquet` so multi-part is tolerated on read — §7.6). Parquet written **without** the pandas index (`index=False` everywhere).
- **[IMPROVE]** Ticker symbols are embedded raw in filenames (`ticker=BRK.A.parquet` etc.). Sharadar symbols may contain `.` and `-` (safe on POSIX), but defensive sanitization/escaping of path separators would be more robust. Original: raw f-string interpolation. Improvement: reject/escape tickers containing `/` or NUL before path construction; log and skip such rows.

---

## 5. Cache metadata (`CacheMeta` / `.meta.json`)

Source: `src/data/sharadar_cache/meta.py`. Tests: `tests/data/sharadar_cache/test_meta.py`.

- **[MUST-MATCH]** JSON schema (`meta.py:1-10`):

```json
{
  "schema_version": 1,
  "last_sync":  { "<DATASET>": "ISO 8601 UTC string" },
  "row_counts": { "<DATASET>": int }
}
```

Dataset keys observed in production: `TICKERS, SEP, SFP, SF1, EVENTS`. `CURRENT_SCHEMA_VERSION = 1` (`meta.py:21`).

- **[MUST-MATCH]** `load`: missing file → empty meta (version 1, empty maps). ISO strings parsed with full offset support (`datetime.fromisoformat`) (`meta.py:31-44`; test `test_meta.py:62-75`).
- **[MUST-MATCH]** `save`: ensure dirs; serialize with `indent=2, sort_keys=True`; timestamps via `astimezone(UTC).isoformat()` (microsecond precision, `+00:00` suffix); write to `.meta.json.tmp` then atomic `os.replace` rename; no tmp file remains after success (`meta.py:46-58`; test `test_meta.py:37-47`).
- **[MUST-MATCH]** `record_sync(dataset, ts, row_count)` is a pure functional update returning a new value; only the named dataset's entries change (`meta.py:60-63`; test `test_meta.py:50-59`).
- **[MUST-MATCH]** Semantics of `row_counts`: for `bootstrap` it is rows written in that run; for `update`/catchup it is **previous count + newly added** (`src/cli/sync_sharadar_universe.py:158-176`; `catchup.py:131,140,160,170,181`). For TICKERS it is always the full rewrite count. It is *not* re-derived from disk.
- **[MUST-MATCH]** Semantics of `last_sync`: wall-clock time the sync ran, **not** the data as-of date. Catchup explicitly trusts a same-day timestamp ("If `make sync-universe update` ran earlier today, we trust it" — test `test_catchup.py:76-99`).

---

## 6. Writers — merge keys, idempotency, chunking

Common merge algorithm (used by every incremental path) — **[MUST-MATCH]**:

1. `_normalize(new)`: coerce key date column (`date` for SEP/SFP/EVENTS, `datekey` for SF1) via `pd.to_datetime(...).tz_localize(None).normalize()`; sort by the dedup keys ascending; reset index.
2. If target file exists: read it, `_normalize(existing)` (SEP/SFP/SF1/EVENTS all re-normalize existing too), `concat([existing, new])`, `drop_duplicates(subset=dedup_keys, keep="last")` — **new rows win** on key collision (a revised bar/filing replaces the old row), then sort by dedup keys, reset index.
3. `added = len(merged) - len(existing)` — counts only *net new keys*; a re-run with identical data returns 0 (idempotency oracle: `test_writer_sep.py:80-103`, `test_writer_sfp.py:80-93`, `test_writer_sf1.py:84-96`, `test_writer_events.py:44-52`).
4. Overwrite the whole target parquet (`to_parquet(path, index=False)`).

Note on ordering: sort is by the dedup-key tuple only — e.g. SEP rows ordered by `(ticker, date)` (ticker-major), SF1 by `(ticker, datekey, dimension)` with dimension order = lexicographic (`ARQ < ART < ARY < MRQ < MRT < MRY`). **[MUST-MATCH]** — readers and goldens depend on row order.

- **[IMPROVE]** Updated rows (same key, changed values — e.g. Sharadar restates a bar; `lastupdated` advances) are silently applied but **not counted** in the return value (`added` counts only net-new keys). Original: return value undercounts revisions. Improvement: optionally also report `revised` count; the on-disk result and the `added` number must remain identical.
- **[IMPROVE]** Whole-partition rewrite is not crash-atomic (a kill mid-`to_parquet` corrupts the year file; meta may then point at a corrupt partition). Original: direct overwrite. Improvement: write to `part-0.parquet.tmp` + `os.Rename`, mirroring `.meta.json`'s pattern.

### 6.1 Date chunking — `_date_chunks(start, end, months=3)`

(`src/data/sharadar_cache/writer_sep.py:33-58`; oracle `test_writer_sep.py:57-77`.)

- **[MUST-MATCH]** `months` must divide 12 (`{1,2,4,3,6,12}`), else `ValueError("months must divide 12 evenly; got {months}")`.
- **[MUST-MATCH]** Chunks are **calendar-aligned** windows of width `months`, clamped to `[start, end]`, inclusive on both ends. Algorithm: for cursor `cur`, `chunk_end_month = ((cur.month-1)//months)*months + months` (capped at 12); window end = last day of that month (Dec → Dec 31; else first day of next month minus 1 day); `chunk_end = min(window_end, end)`; yield `(cur, chunk_end)`; `cur = chunk_end + 1 day`.
- **[MUST-MATCH]** With `months=3` over a full year this yields exactly the four quarters `[01-01..03-31] [04-01..06-30] [07-01..09-30] [10-01..12-31]` → exactly 4 API calls (asserted with literal filter dicts in `test_writer_sep.py:64-77`). A `start` mid-quarter yields a partial first chunk ending at that quarter's boundary.
- Rationale (keep in Go doc comment): quarterly ≈ 220k rows/call (~3,500 stocks × 63 trading days), safely under the per-call ~1M row cap; half-year empirically hit the cap in 2021-H2 (`writer_sep.py:1-15`).

### 6.2 `bootstrap_sep(client, layout, *, start, end) -> int` / `bootstrap_sfp`

(`writer_sep.py:81-125`, `writer_sfp.py:46-88`.)

- **[MUST-MATCH]** `ensure_dirs()`; for each quarterly chunk call `get_table("SHARADAR/SEP"|"SHARADAR/SFP", paginate=True, date={"gte": chunk_start.isoformat(), "lte": chunk_end.isoformat()})`; empty result → log info, continue. Normalize; `groupby(df["date"].dt.year)`; per year: merge into the year partition with the common algorithm (bootstrap **also merges** — re-running bootstrap on an overlapping range is idempotent, asserted by the writer docstring and dedup logic). Return total net-new rows across all chunks/years.
- **[MUST-MATCH]** A chunk spanning rows of multiple years (possible when API returns straddling data) is split per-year into separate partitions (`test_writer_sep.py:33-54` writes 2023 and 2024 partitions from one response).

### 6.3 `update_sep(client, layout, *, asof) -> int` / `update_sfp`

(`writer_sep.py:127-156`, `writer_sfp.py:90-119`.)

- **[MUST-MATCH]** Single-day pull: `date={"gte": asof.isoformat(), "lte": asof.isoformat()}`. Empty → return 0 **without** touching any partition. Otherwise normalize, group by year (handles year-boundary days — `test_writer_sep.py:106-117`), merge, return net-new row count (same-day re-run → 0, existing rows preserved; `test_writer_sep.py:80-103`).

### 6.4 `bootstrap_sf1 / update_sf1(client, layout, *, tickers) -> int`

(`writer_sf1.py:44-107`.)

- **[MUST-MATCH]** Empty ticker list → return 0, no API calls. Tickers batched in groups of **500** (`_TICKER_BATCH_SIZE`, `writer_sf1.py:25`), call `get_table("SHARADAR/SF1", paginate=True, ticker=batch)` — **no dimension filter, no date filter** (full history, all 6 dimensions; asserted `test_writer_sf1.py:46-60`).
- **[MUST-MATCH]** `bootstrap_sf1` **overwrites** each per-ticker file with the grouped slice (no merge with pre-existing files); `update_sf1` merges per ticker on `(ticker, datekey, dimension)` with the common algorithm and returns net-new rows. Ticker keys from `groupby("ticker")` are stringified for the filename.

### 6.5 `bootstrap_events / update_events(client, layout, *, tickers) -> int`

(`writer_events.py:43-93`.)

- **[MUST-MATCH]** Same shape as SF1: batches of 500, `get_table("SHARADAR/EVENTS", paginate=True, ticker=batch)` (no date filter — full history); bootstrap overwrites per-ticker files; update merges on `(ticker, date, eventcodes)`; `date` normalized.

### 6.6 `write_tickers(client, layout) -> int`

See §2.5. Always a full fetch + filter + overwrite, used identically by bootstrap, update and catchup.

- **[IMPROVE]** `update_sf1` / `update_events` re-fetch the **entire history for every ticker on every daily update** (~3,000 rows/ticker × 21k tickers; production meta shows SF1 ≈ 2.96M rows). Original: full-table refresh per update (correct but wasteful of API quota and time). Improvement: filter by `lastupdated={"gte": last_sync_date}` on the API call while keeping the identical on-disk merge; fall back to full fetch on bootstrap. Functional results must be identical.

---

## 7. Read API — `SharadarUniverseCache`

Source: `src/data/sharadar_cache/reader.py`. Tests: `tests/data/sharadar_cache/test_reader.py`.
Read-only; constructed with a `CacheLayout` (`reader.py:48-49`). All methods return an
**empty frame for unknown tickers / missing files — never an error** (`reader.py:1-6`).

Go return-shape note: "DataFrame" maps to an ordered columnar/record representation; what is
contractual is the **column set, dtypes, row order, and emptiness semantics** below.

### 7.1 `get_tickers() -> DataFrame`

- **[MUST-MATCH]** Missing `TICKERS.parquet` → empty frame; otherwise full file contents, order as written (sorted by ticker) (`reader.py:51-55`).

### 7.2 `_filter_by_window(df, start, end)` — tradability predicate

(`reader.py:23-45`.) Underlies both list methods.

- **[MUST-MATCH]** Keep a row iff:
  - `firstpricedate` missing/NaT **or** `firstpricedate <= end` (listed on/before window end), AND
  - `lastpricedate` missing/NaT/empty-string **or** `lastpricedate >= start` (still trading at/after window start).
  - Date coercion via `pd.to_datetime(value, errors="coerce")` — empty string and invalid values become NaT ⇒ "keep" (`reader.py:18-20`). If a column is entirely absent, no filtering on it (keep all).
  - Comparison at day precision against `pd.Timestamp(start|end)` (midnight); since stored values are normalized dates, this is a date comparison.
- Test oracle: `test_reader.py:104-129` (ALIVE kept, DELISTED_IN_WINDOW kept, DELISTED_BEFORE dropped, TOO_NEW dropped).

### 7.3 `list_active_tickers(as_of: date) -> list[str]`

- **[MUST-MATCH]** `= _filter_by_window(tickers, as_of, as_of)["ticker"].astype(str).sort_values().tolist()` — point-in-time tradability; empty cache or missing `ticker` column → `[]`; result **sorted ascending**, plain strings (`reader.py:57-71`; tests `test_reader.py:93-101,157-173`). Sort is pandas lexicographic on str (byte-wise for ASCII tickers).

### 7.4 `list_universe_for_window(start, end, *, table=None) -> list[str]`

- **[MUST-MATCH]** Optional pre-filter `df[df["table"] == table]` (`"SF1"` = common stocks, `"SFP"` = ETFs/funds, `None` = both); then window filter; sorted ticker list (`reader.py:73-97`; tests `test_reader.py:132-154`). This is the survivor-bias-free backtest universe API — includes mid-window delistings.

### 7.5 `get_bars(ticker, *, start, end) -> DataFrame`

(`reader.py:99-127`.)

- **[MUST-MATCH]** Scan years `start.year ..= end.year`; for each year check **both** `SEP/year=Y/part-0.parquet` and `SFP/year=Y/part-0.parquet` (SEP first); skip missing files; read with predicate pushdown `filters=[("ticker","==",ticker)]`; mask `start <= date <= end` **inclusive both ends**; concat all non-empty frames; if none → empty frame; else sort by `date` ascending, reset index.
- **[MUST-MATCH]** A ticker lives in exactly one of SEP/SFP, so the union is single-source per ticker — but the implementation does NOT assume it: if (pathologically) the same ticker existed in both, rows would be concatenated, **not deduplicated**, and the sort is by `date` only (stable concat order: SEP rows before SFP rows within a date). Replicate as-is.
- **[MUST-MATCH]** Returned columns = full SEP/SFP schema (§2.1) in file order; `date` is tz-naive `timestamp[ns]`. Tests: cross-year boundary (`test_reader.py:183-187`), unknown ticker empty (`190-193`), ETF served from SFP transparently (`213-238`).

### 7.6 `get_fundamentals(ticker, *, dimension=None) -> DataFrame`

- **[MUST-MATCH]** Missing per-ticker file → empty. `dimension=None` → all cached dimensions. Non-None and frame non-empty and `dimension` column present → `df[df["dimension"] == dimension]`, index reset. Valid lenses: `ARQ/ART/MRQ/MRT/ARY/MRY` — not validated, unknown values just return empty selection (`reader.py:129-147`; tests `test_reader.py:196-209,255-275`).

### 7.7 `get_events(ticker) -> DataFrame`

- **[MUST-MATCH]** Missing file → empty; else full per-ticker file (`reader.py:149-153`).

### 7.8 `stats() -> dict`

- **[MUST-MATCH]** Keys `TICKERS, SEP, SFP, SF1, EVENTS`; each value = total row count: TICKERS = `len(read_parquet(tickers_file))` or 0 if missing; SEP/SFP = sum over glob `year=*/part-*.parquet` (reading only the `ticker` column); SF1/EVENTS = sum over glob `ticker=*.parquet` (`reader.py:155-188`; tests `test_reader.py:241-252,278-285`).

### 7.9 `SharadarHistoryProvider` (backtest `BarHistoryProvider`)

Source: `src/adapters/sharadar/history_provider.py`; protocol `src/universe/history_provider.py:24-29`; tests `tests/adapters/sharadar/test_history_provider.py`.

- **[MUST-MATCH]** Depends on a narrow `SharadarCacheReader` interface: `read_bars(ticker) -> DataFrame` (full history, **DatetimeIndex-indexed**, columns at minimum lower-case `open high low close volume`) and `list_active_tickers(as_of) -> list[str]`.
- **[MUST-MATCH]** `get_bars(ticker, start, end)`:
  - `start > end` → `ValueError` with message matching `start ({start}) after end ({end})` (test regex `r"start.*after.*end"`).
  - empty source → return it as-is.
  - else inclusive index mask `start <= idx <= end`, return a **copy**.
- **[MUST-MATCH]** `list_tickers(*, as_of)` = `list(cache.list_active_tickers(as_of))` — order preserved from the reader (which sorts).
- **Gap to note**: `SharadarUniverseCache` does **not** implement `read_bars`; in the current codebase nothing wires `SharadarHistoryProvider` to the real cache (backtests use `SharadarUniverseCache.get_bars` directly via `scripts/multi_strategy_backtest.py`). Go must keep both surfaces (COMPLETE); see Open questions Q4.

---

## 8. `ensure_cache_fresh` — startup auto-catchup

Source: `src/data/sharadar_cache/catchup.py`. Tests: `tests/data/sharadar_cache/test_catchup.py`. Call site: `src/runner/live_runner.py:226-256`.

### 8.1 Signature and report

```
ensure_cache_fresh(client, layout, *, today: date | None = None) -> CatchupReport
CatchupReport{ skipped_reason: str|None, days_attempted: int, days_succeeded: int,
               rows_added: map[string]int, errors: []string }
CatchupReport.did_work = days_attempted > 0 || len(rows_added) > 0
```

(`catchup.py:38-49,82-216`.)

### 8.2 Flow — **[MUST-MATCH]** in this exact order

1. `ensure_dirs`; load meta.
2. `meta.last_sync["SEP"]` absent → log warning, return `CatchupReport(skipped_reason="not-bootstrapped")` (never auto-bootstraps — full backfill takes hours; test `test_catchup.py:69-73`).
3. `today` defaults to `datetime.now(UTC).date()`. `target = today - 1 day` (T-1). `start = last_sync["SEP"].date()` (the **wall-clock** date of the previous sync — see §5 and [IMPROVE] below).
4. `days = _trading_days(start, target)`: **weekdays only** (Mon–Fri, `pd.bdate_range`), inclusive both ends; `end < start` → `[]`. US holidays are *not* excluded — holiday calls return 0 rows and are harmless (`catchup.py:52-65`; tests `test_catchup.py:57-64`). Empty `days` → log "cache fresh", return empty report (`did_work == false`).
5. Per day `d` in order — **SEP first, then SFP** (quota exhaustion mid-loop should leave the most-used dataset most complete):
   - `update_sep(client, layout, asof=d)`; on success `rows_added["SEP"] += n` and `meta = meta.record_sync("SEP", ts=now, row_count=old+ n)`; on exception: append `"SEP {d}: {exc}"` to errors, log warning, **continue** (warn-and-continue).
   - same for SFP with key `"SFP"`.
   - `days_succeeded` increments only when **both** SEP and SFP succeeded for that day.
   - **`meta.save(layout)` after every day** so a crash preserves progress (test `test_catchup.py:198-221`).
6. Once, after the day loop: `write_tickers` → `rows_added["TICKERS"] = n` (absolute count), `record_sync("TICKERS", row_count=n)`, save meta; failure → error `"TICKERS: {exc}"`, continue.
7. Reload ticker list from disk (`sorted(get_tickers()["ticker"].astype(str))`, `catchup.py:72-79`). Empty → append error `"TICKERS list empty — skipping SF1 / EVENTS"`, skip step 8.
8. `update_sf1(tickers)` then `update_events(tickers)`; each: `rows_added[key] = n` (absolute, not +=), `record_sync(key, row_count=old + n)`, save meta; failure → `"SF1: {exc}"` / `"EVENTS: {exc}"`, continue. **TICKERS failure does not prevent SF1/EVENTS** (they use the previous on-disk ticker list; test `test_catchup.py:170-195`).
9. Return report with `days_attempted = len(days)`, accumulated `rows_added` (always containing all five keys, zero-initialized), `errors`.

Oracle for the full ordering + per-day arguments: `test_catchup.py:102-141` (3 weekday gap → exactly 3 `update_sep` + 3 `update_sfp` interleaved per-day, then exactly one `write_tickers`, `update_sf1`, `update_events`, with `rows_added == {"SEP":6,"SFP":3,"SF1":50,"EVENTS":20,"TICKERS":100}`).

- **[MUST-MATCH]** Nothing in catchup ever raises to the caller for per-step failures; only programming errors escape. The live runner additionally wraps the whole call in try/except WARN (`live_runner.py:240-256`).
- **[IMPROVE]** `start = last_sync["SEP"].date()` re-pulls the day of the previous sync (overlap by design — idempotent merges make it safe but it costs one redundant API day per run) and uses wall-clock sync date rather than max bar date on disk; if a sync ran at 00:30 UTC the prior trading day could in principle be skipped when bars publish late. Original: as described. Improvement: track an explicit `last_asof` per dataset in meta (additive field, schema_version bump) and catch up from `last_asof + 1`; keep the overlap-by-one as a safety default.
- **[IMPROVE]** `_trading_days` is weekday-based, knowingly wasting ~9 holiday calls/yr × 2 datasets. Original: `bdate_range` Mon–Fri. Improvement allowed: embed a NYSE holiday table to skip them — but zero-row days must remain harmless either way.

---

## 9. CLI `sync-universe` (bootstrap | update | stats)

Source: `src/cli/sync_sharadar_universe.py`. Required for operational parity (Makefile target `sync-universe`).

- **[MUST-MATCH]** `bootstrap --start YYYY-MM-DD --end YYYY-MM-DD [--ticker T ...]`:
  order TICKERS → SEP → SFP → SF1 → EVENTS. After TICKERS, the ticker list drives SF1/EVENTS (and, with `--ticker`, also SEP/SFP fetched per-ticker instead of date-only — smoke-test path writes year partitions directly **without merge**, `sync_sharadar_universe.py:80-96,103-119`). Abort if the ticker list is empty. Each step records `record_sync(dataset, ts=now, row_count=n)`; **meta saved once at the end** for bootstrap.
- **[MUST-MATCH]** `update [--asof YYYY-MM-DD]` (default local today): TICKERS → SEP(asof) → SFP(asof) → SF1(full) → EVENTS(full); row counts accumulate (`cur + n`); meta saved at end (`:140-180`).
- **[MUST-MATCH]** `stats`: prints cache root, then a table `Dataset | Rows | Last sync` for `TICKERS, SEP, SF1, EVENTS` (note: **SFP is computed by `stats()` but not printed** — preserve or fix, see Open questions Q5) with `(never)` for missing timestamps (`:183-199`).
- **[IMPROVE]** Bootstrap saves meta only at the very end — a crash mid-bootstrap loses all `last_sync` records even for completed datasets (the parquet data itself survives). Improvement: save meta after each step, like catchup does.

---

## 10. Downstream column contracts (what the cache must guarantee)

These consumers define what "byte-equivalent" means in practice:

| Consumer | Reads | Contract | Source |
|---|---|---|---|
| `_bars_to_engine_format` | `date, open, high, low, close, volume` | index = `to_datetime(date).tz_localize("UTC")`; column subset in that order | `scripts/multi_strategy_backtest.py:206-221` |
| EOD refresh `_bar_from_row` | same + `date` | `Decimal(str(x))` for OHLC, `int(volume)`, naive ts → UTC | `src/runner/eod/refresh.py:90-105` |
| `_market_cap_lookup_factory` | SF1 `datekey, marketcap` | latest row by `datekey` (sort ascending, take last); `0.0` for unknown/NaN/missing column; memoized | `scripts/multi_strategy_backtest.py:182-204` |
| `_load_sf1_mrt` | SF1 `ticker, datekey, dimension, marketcap` | pyarrow dataset scan with pushdown `ticker ∈ set && dimension == "MRT"` | `scripts/multi_strategy_backtest.py:224-246` |
| `_load_earnings` | EVENTS `ticker, date, eventcodes` | exact `"22" ∈ split(eventcodes,"|")`; rename `date → report_date` | `scripts/multi_strategy_backtest.py:248-267` |
| `load_sf1_market_caps` | SF1 `ticker, datekey, dimension, marketcap` | filter `dimension == "MRT"` (only if column exists) → `datekey.date() <= as_of` → drop NaN marketcap → `marketcap > 0` → sort by datekey-date → last per ticker → `Decimal(str(v))` | `src/portfolio/context_refresher.py:108-147` |
| `load_earnings_calendar` | `ticker, report_date` | blackout = any earnings date within `as_of ± 5` calendar days (`EARNINGS_BLACKOUT_DAYS = 5`); returns only `True` entries | `src/portfolio/context_refresher.py:150-188` |
| live/EOD universe | TICKERS via `list_universe_for_window(warmup_start, today, table="SF1")` | warmup = `today − 2*365 days`; exclude `{"SPY"} ∪ sector ETFs (∪ pair legs in EOD)`; cap top-N (default 85) by market cap desc, unknown caps (0.0) sort last; ≤ limit → pass through unchanged | `src/runner/live_runner.py:63-87,270-300`; `src/runner/eod/refresh.py:67-68,113-140` |
| SPY warmup | `get_bars("SPY", warmup_start, today)` | served from SFP partitions | `src/runner/live_runner.py:272`; `scripts/multi_strategy_backtest.py:335-359` (500-day SPY warmup) |

All **[MUST-MATCH]** unless the owning spec for those modules says otherwise; they are listed here because they constrain the cache's column names, dtypes, and ordering.

Note on `load_sf1_market_caps` tie-handling — **[MUST-MATCH]**: sort key is the *date-truncated* `datekey` and pandas sort is stable, so among multiple filings on the same date the **last row in input order** wins (`context_refresher.py:141-143`).

---

## 11. Production-cache reference figures (sanity targets, not contracts)

From `cache/sharadar/.meta.json` as of 2026-05-28: TICKERS 21,362 rows; SEP 8,742,128; SFP 6,362,149; SF1 2,958,955; EVENTS 2,116,119. SEP/SFP partitions span `year=2020 .. year=2026`. Use for smoke-validation of a Go bootstrap (row counts will drift daily).

---

## 12. Go implementation notes (non-normative)

- Parquet writing must produce pandas-compatible types: dates as `timestamp[ns]` (tz-naive), prices/volume as `float64`, strings as UTF-8. Round-trip fidelity with the Python cache is a gate criterion: a Go-read of a Python-written cache and vice versa must agree. This gate is scoped to the **parquet cache layer**; the derived PostgreSQL store deliberately uses BIGINT 1e-4 fixed point for price columns under the [IMPROVE] note in §2.1 (overflow → NULL + `FieldsNulled` accounting), which is not a round-trip surface.
- "Empty DataFrame" maps to an empty typed slice + present-but-empty schema; the distinction "empty with columns" vs "completely empty" is never observed by callers (only `.empty`/`len` and column membership checks).
- Error policy parity: reader never errors on missing data; writers propagate API errors; catchup converts everything to warn-and-continue.
- PRODUCTION-GRADE additions (context cancellation through client retries and per-day catchup loop, structured logging mirroring the `log.info/warning` messages above, graceful shutdown that finishes the in-flight `meta.save`) are required by the project bar and are all tagged [IMPROVE] where they alter observable timing, never data.

---

## 13. Open questions

1. **Repo-root discovery in Go** — Python anchors the default cache dir at the directory containing `pyproject.toml` (`layout.py:14-27`). Go equivalent: `go.mod`? Or require `TMS_SHARADAR_CACHE_DIR` to be always set in the containerized deployment (ports 18080/13000 stack) and make the walk-up a dev-only fallback? Needs a decision; affects only path resolution, not data.
2. **`delistedate` keep-column** (`writer_tickers.py:47`) — never present in real API output (absent from the production TICKERS.parquet; Sharadar's actual TICKERS table has no such field — closest is `lastpricedate`/`isdelisted`). Likely a misspelling of an intended `delistdate`. Keep the dead keep-list entry for byte-parity, or drop it? (No observable difference either way given current API output.)
3. **Local-vs-UTC "today" inconsistency** — catchup uses UTC date (`catchup.py:109`), the `update` CLI uses local `date.today()` (`sync_sharadar_universe.py:147`), live_runner's warmup window uses local `date.today()` (`live_runner.py:258`). On a host west of UTC around midnight these disagree by one day. Replicate exactly, or normalize all to UTC ([IMPROVE] candidate)? Currently spec'd as replicate-exactly.
4. **`SharadarHistoryProvider.read_bars` wiring gap** — the Protocol expects a `read_bars(ticker)` returning a DatetimeIndex-indexed frame, but `SharadarUniverseCache` exposes only `get_bars(ticker, start, end)` with a `date` column; no adapter exists in the repo. Should Go implement a thin adapter (full-history read + set index) to make the provider actually usable, or port it as the same dormant seam?
5. **`stats` CLI omits SFP** — `SharadarUniverseCache.stats()` computes `SFP` but the CLI prints only `TICKERS, SEP, SF1, EVENTS` (`sync_sharadar_universe.py:194`), and `last_sync["SFP"]` is never shown. Bug or intentional? Spec'd as replicate; flag for product decision.
6. **Concurrent writers** — nothing guards two simultaneous syncs (CLI update + live-node catchup both rewrite year partitions and `.meta.json`). Python relies on operator discipline. Should Go add a lock file under the cache root ([IMPROVE]), or preserve the unguarded behavior?
7. **SEP `dividends` column** — Sharadar's current SEP table documentation also lists a `dividends` column; the production cache (last full bootstrap) does not contain it. If a fresh bootstrap returns extra columns, the store-verbatim rule (§2) admits them automatically, but cross-language schema goldens should pin the observed 10-column set or tolerate supersets — which?
8. **Per-call row cap** — chunk sizing assumes a ~900k–1M row response cap of the *Python SDK's* `paginate=True` (which stops at 1M rows with a warning). A native Go pagination loop has no such cap. Keep quarterly chunks for parity and quota friendliness (spec'd), but confirm whether the Go client should also emit a warning when a single logical call exceeds 1M rows.
