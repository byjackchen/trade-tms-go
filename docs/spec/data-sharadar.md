# Spec: Sharadar Data Layer (SEP / SFP / SF1 / EVENTS / TICKERS)

This repo's definition of the Sharadar data layer (SEP / SFP / SF1 / EVENTS /
TICKERS): cache layout/meta/writers/reader/catchup, the Nasdaq Data Link client
and backtest history provider, and the column contracts consumers pin against.
Implemented in Go (`github.com/byjackchen/trade-tms-go`). The rules below are
invariants of this system (edge cases, NaN handling, ordering, return shapes).
Where a known weakness is called out, the better behavior this repo adopts is
documented alongside it.

---

## 1. Configuration and environment

| Env var | Default | Meaning | Source |
|---|---|---|---|
| `NASDAQ_DATA_LINK_API_KEY` | *(none — required for any API call)* | Nasdaq Data Link API key |; |
| `TMS_SHARADAR_CACHE_DIR` | `""` → `<repo_root>/cache/sharadar` | Cache root override; `~` expanded |; |
| `TMS_AUTO_SYNC` | `"1"` | `"0"` disables live-node auto-catchup (also `--no-sync` CLI flag) |
| `TMS_LIVE_UNIVERSE_LIMIT` | `85` | Live SEPA universe cap (top-N by market cap) |

- Cache root resolution: if `TMS_SHARADAR_CACHE_DIR` is non-empty after `strip`, use it (with `~` expansion). Otherwise walk up from the package file's directory until a directory containing `pyproject.toml` is found; cache root = `<that dir>/cache/sharadar`. If no marker file found, raise an error telling the operator to set `TMS_SHARADAR_CACHE_DIR`. There is **no home-dir fallback**. (; test.) For Go: the repo-root marker should be `go.mod` *or* a configured marker — see Open questions Q1.
- Missing API key must raise a configuration error naming the key (`nasdaq_data_link_api_key`) with a hint pointing to `https://data.nasdaq.com/account/profile` — fail loud at client construction, not first call. (; test.) An explicitly passed key overrides config.

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

- Adjusted-vs-raw semantics: every strategy consumer reads the **split-adjusted `open/high/low/close`** columns directly; `closeadj` and `closeunadj` are stored but never consumed anywhere in the codebase (verified by grep over `src/`). Bars handed to the engine are `df[["open","high","low","close","volume"]]`, and EOD refresh builds `Bar` from `row["open"|"high"|"low"|"close"|"volume"]` plus `row["date"]`. The Go port must keep `closeadj`/`closeunadj` in the cache (COMPLETE-ness) even though nothing reads them yet.
- `volume` must tolerate NaN (it is float). Conversion to int happens only at consumer boundaries: `int(row["volume"])`. NaN-containing tickers are *dropped by the backtest consumer*, not cleaned in the cache: `_has_nan_bars` checks NaN in any of `open,high,low,close,volume` and the ticker is skipped with its name collected into `skipped_nan`.
- Price-cap filter (consumer-side, not cache-side): tickers whose any-of-`open,high,low,close` max exceeds the engine price cap `_NAUTILUS_PRICE_MAX = 17_014_118_346_046.0` are skipped. Rationale: 1:100/1:1000 reverse splits balloon back-adjusted prices.
- Relational-store price representation (Go-only deviation, sanctioned here). The parquet cache stores `open/high/low/close/closeadj/closeunadj/dividends` as raw `double` (float64); the parquet layer of the Go port keeps that exactly, and the float64 round-trip gate of §12 applies to it unchanged. The **PostgreSQL store** (`migrations/000002_marketdata.up.sql` `tms.bars_daily`, `internal/data/sharadar/convert.go`) persists those columns as BIGINT 1e-4 fixed point per the project Money model (`docs/spec/domain-types-money.md` §1.2, `Decimal(str(x)).quantize(0.0001, ROUND_HALF_EVEN)`), with unrepresentable values (±Inf, or |x| beyond int64 at 1e-4 scale ≈ 9.22e14 USD) stored as NULL and counted in `ImportStats.FieldsNulled`. Empirical evidence against the 2026-05-28 production cache that this loses nothing consumer-observable: (a) zero prices fall below 0.00005, so nothing quantizes to 0; (b) only ~0.005–0.02% of cells per year-partition are not exactly 1e-4-representable, all at 1e7–1e18 magnitudes where the 4-decimal quantization error is at or below the float64 ULP; (c) exactly 3,479 cells — a single ticker, BINI, peak 1.4065e18 — overflow int64 at 1e-4 scale and are stored NULL with FieldsNulled accounting, and BINI still exceeds `_NAUTILUS_PRICE_MAX` via its surviving sub-overflow rows, so the consumer-side `skipped_overflow` classification above is preserved exactly. The parquet cache stays the source of truth for round-trip determinism (§12); the DB is a derived store.

### 2.2 SFP — daily ETF/fund bars (`SHARADAR/SFP`)

- Identical column schema to SEP (verified against `cache/sharadar/SFP/year=2024/part-0.parquet`; test fixture comment). Same writers' semantics, separate partition tree (§4). ~6,500 tickers vs SEP's ~3,500.

### 2.3 SF1 — quarterly fundamentals (`SHARADAR/SF1`)

On-disk schema (observed at `cache/sharadar/SF1/ticker=AAPL.parquet`): 111 columns.
Key/index columns:

| Column | Arrow type | Notes |
|---|---|---|
| `ticker` | `string` |
| `dimension` | `string` | one of `ARQ ART MRQ MRT ARY MRY` — **all cached** |
| `calendardate` | `timestamp[ns]` | normalized quarter-end |
| `datekey` | `timestamp[ns]` | **filing date** — the point-in-time key; tz-naive normalized |
| `reportperiod` | `timestamp[ns]` | fiscal period end |
| `fiscalperiod` | `string` | e.g. `Q3` |
| `lastupdated` | `timestamp[ns]` |

Remaining 104 metric columns are all `double` (full list, alphabetical, as observed on disk):
`accoci assets assetsavg assetsc assetsnc assetturnover bvps capex cashneq cashnequsd cor consolinc currentratio de debt debtc debtnc debtusd deferredrev depamor deposits divyield dps ebit ebitda ebitdamargin ebitdausd ebitusd ebt eps epsdil epsusd equity equityavg equityusd ev evebit evebitda fcf fcfps fxusd gp grossmargin intangibles intexp invcap invcapavg inventory investments investmentsc investmentsnc liabilities liabilitiesc liabilitiesnc marketcap ncf ncfbus ncfcommon ncfdebt ncfdiv ncff ncfi ncfinv ncfo ncfx netinc netinccmn netinccmnusd netincdis netincnci netmargin opex opinc payables payoutratio pb pe pe1 ppnenet prefdivis price ps ps1 receivables retearn revenue revenueusd rnd roa roe roic ros sbcomp sgna sharefactor sharesbas shareswa shareswadil sps tangibles taxassets taxexp taxliabilities tbvps workingcapital`

- SF1 columns *actually consumed* by code: `ticker`, `datekey`, `dimension`, `marketcap` and `revenue` only in test fixtures. All other columns must still be cached verbatim (no pruning) — `bootstrap_sf1` stores the API frame as-is (; test asserts the API call has **no** dimension filter).
- All six dimensions coexist per `(ticker, datekey)`; dedup key is `(ticker, datekey, dimension)`. `marketcap` unit: USD (Sharadar reports raw USD, e.g. `3.4e12` for AAPL); consumers compare `marketcap > 0` and convert via `Decimal(str(value))`.

### 2.4 EVENTS — corporate events (`SHARADAR/EVENTS`)

On-disk schema (observed):

| Column | Arrow type | Notes |
|---|---|---|
| `ticker` | `string` |
| `date` | `timestamp[ns]` | event date, tz-naive normalized |
| `eventcodes` | `string` | **pipe-separated numeric codes**, e.g. `"13"`, `"22|71"` |

- Earnings filter: a row is an earnings event iff `"22"` is an exact member of `eventcodes.split("|")` — NOT substring match (`"122"` or `"221"` must not match). (.) After filtering, the `date` column is renamed `report_date` for the earnings consumers.
- Dedup key is `(ticker, date, eventcodes)` — multiple same-day events with different code strings coexist (and module docstring lines 1-9).

### 2.5 TICKERS — universe master (`SHARADAR/TICKERS`)

Unlike the others, TICKERS **is filtered and column-pruned** at write time.

Keep-columns, in this exact order:
`ticker, name, exchange, isdelisted, category, sector, industry, table, firstpricedate, lastpricedate, delistedate` — intersected with whatever columns the API actually returned.

On-disk observed schema (real cache): `ticker name exchange isdelisted category sector industry table` as `string`, `firstpricedate lastpricedate` as `timestamp[ns]`. **`delistedate` is absent on disk** — the live API does not return a column by that name, so the keep-list intersection drops it silently. See Open questions Q2.

- Row filter (survivorship-bias policy),:
 - SF1 stocks: keep iff `table == "SF1"` AND `category.fillna("").startswith("Domestic Common Stock")` — keeps **both active and delisted** (catches `"Domestic Common Stock"` and `"Domestic Common Stock Primary Class"`; drops preferred etc.).
 - SFP funds: keep iff `table == "SFP"` AND `isdelisted == "N"` — **active only** (delisted ETFs dropped).
 - NaN `category` is treated as `""` (fillna before startswith) → dropped for SF1.
 - Test oracle: (7-row fixture → exactly `AAPL, ACWX, DEAD, MSFT, SPY` survive).
- Output is sorted by `ticker` ascending, index dropped, full overwrite of `TICKERS.parquet` (no merge) (; overwrite test).
- API call shape: `get_table("SHARADAR/TICKERS", paginate=True)` with **no other filters** (; asserted).
- Return value: number of rows written. Log line includes SF1 active / SF1 delisted / SFP counts.
- `isdelisted` values are the strings `"N"` / `"Y"`. `lastpricedate` may be an empty string `""` in API output (tests use `""`), which the reader coerces to NaT = "still active" (§7.2). On the real disk cache it is `timestamp[ns]` with NaT for active tickers. Reader code must accept **both** representations.

### 2.6 Timezone conventions (global)

- All date-like columns persisted in the cache are **tz-naive, normalized to 00:00:00** (`pd.to_datetime(col).dt.tz_localize(None).dt.normalize`); semantics: if the input was tz-aware, drop the tz designator keeping the wall-clock value, then truncate to midnight. Verified: pandas `tz_localize(None)` is a no-op on naive input. (,, (on `datekey`),.)
- `.meta.json` timestamps are ISO 8601 **UTC** with offset (e.g. `2026-05-28T16:04:12.539258+00:00`) — `astimezone(UTC).isoformat` on save.
- Consumers re-attach UTC at the engine boundary: backtest sets `index = pd.to_datetime(df["date"]).tz_localize("UTC")`; EOD refresh localizes a naive `date` to UTC before building a `Bar`. i.e. cache layer = naive dates; engine layer = UTC midnight.
- "Today" for catchup is `datetime.now(UTC).date`; for the `update` CLI subcommand it is local `date.today`. See Open questions Q3.

---

## 3. Nasdaq Data Link client (`SharadarClient`)

Source:. Tests:.

### 3.1 `get_table(dataset, *, paginate=True, **filters) -> DataFrame`

- Delegates to the Nasdaq Data Link "datatables" API (`get_table(dataset, api_key=..., paginate=..., **filters)`). Filters used in this codebase:
 - `date={"gte": "YYYY-MM-DD", "lte": "YYYY-MM-DD"}` (SEP/SFP) — REST form: `date.gte=...&date.lte=...`
 - `ticker=[...list of symbols...]` (SF1/EVENTS/smoke-test SEP/SFP) — REST form: comma-joined `ticker=`
 - `paginate=True` means: follow `cursor_id` pages until exhausted; the SDK caps total at ~1M rows per call. Go must implement cursor pagination against `https://data.nasdaq.com/api/v3/datatables/<dataset>.json`.
- Retry policy:
 - Max **4 attempts** total (`_MAX_ATTEMPTS = 4`).
 - Backoff before retry `attempt n` (1-based): `2**n` seconds → **2, 4, 8** s waits between the 4 attempts; the test for "gives up" asserts exactly 4 underlying calls. (Docstring says "2/4/8/16" but the 16 s wait is computed then the loop exits — only 3 sleeps ever happen.)
 - Retryable iff the stringified error (`f"{type(e).__name__}: {e}"`) contains `"429"` or any of `"500" "501" "502" "503" "504"` as a **substring**. Everything else propagates immediately, no retry (; tests at: `401/403/404` → False).
 - After exhausting attempts on retryable errors: raise `RuntimeError("get_table {dataset} failed after 4 retries: {last_err}")` — match message shape `failed after` (; test regex `"failed after"`).
- Counters: each *successful* call increments `fetch_count` and `cache_miss_count` and sets `last_fetch_ts` (ns since epoch; 0 = never). Failed calls increment nothing. `record_cache_hit` increments `cache_hit_count` only.
- `state_summary` JSON shape (; tests 233-262):
 `{"source": "sharadar", "fetch_count": int, "cache_hit_count": int, "cache_miss_count": int, "last_fetch_ts": ISO-8601-UTC-string|null, "last_fetch_ts_ns": int, "quota_used_today": null}` — `quota_used_today` is always `null` (the SDK never exposes quota).
- Substring-based HTTP-status classification (`"500" in err_text`) can false-positive (e.g. a ticker `X500` or row-count `500` embedded in an error message triggers retry). Original: substring scan over the formatted error string. Improvement for Go: classify on the actual HTTP status code from the response (`resp.StatusCode == 429 || >= 500`), plus context-cancellation awareness; keep the 4-attempt / 2-4-8s schedule identical. Also honor `Retry-After` if present (additive, never weaker than the original).
- `time.sleep` is uninterruptible. Original: blocking sleep between retries. Improvement: `select { case <-ctx.Done:...; case <-timer.C: }` so shutdown cancels in-flight backoff (required by PRODUCTION-GRADE bar).

### 3.2 `export_table(dataset, *, target_dir, **filters) -> Path`

- Bulk async export: creates `target_dir` (mkdir -p), filename `<target_dir>/<dataset with "/"→"_">.zip`, delegates to SDK `export_table` (triggers Sharadar's async export job, polls until ready, downloads zip), returns the resulting path (; test). Currently unused by any sync path (bootstrap goes through paginated `get_table`) but must exist (COMPLETE).

---

## 4. Parquet cache layout (`CacheLayout`)

Source:. Tests:.

- Path map (all relative to `root`):

| Artifact | Path |
|---|---|
| meta | `.meta.json` (atomic temp: `.meta.json.tmp` — note `with_suffix` on `.meta.json` yields `.meta.json.tmp`) |
| tickers | `TICKERS.parquet` |
| SEP partition | `SEP/year=<YYYY>/part-0.parquet` |
| SFP partition | `SFP/year=<YYYY>/part-0.parquet` |
| SF1 per ticker | `SF1/ticker=<TICKER>.parquet` |
| EVENTS per ticker | `EVENTS/ticker=<TICKER>.parquet` |

(; exact-path tests.)

- `ensure_dirs` creates `root, SEP/, SFP/, SF1/, EVENTS/` with parents, idempotent.
- One file per year partition (`part-0.parquet` literally; `stats` globs `part-*.parquet` so multi-part is tolerated on read — §7.6). Parquet written **without** the pandas index (`index=False` everywhere).
- Ticker symbols are embedded raw in filenames (`ticker=BRK.A.parquet` etc.). Sharadar symbols may contain `.` and `-` (safe on POSIX), but defensive sanitization/escaping of path separators would be more robust. Original: raw f-string interpolation. Improvement: reject/escape tickers containing `/` or NUL before path construction; log and skip such rows.

---

## 5. Cache metadata (`CacheMeta` / `.meta.json`)

Source:. Tests:.

- JSON schema:

```json
{
 "schema_version": 1,
 "last_sync": { "<DATASET>": "ISO 8601 UTC string" },
 "row_counts": { "<DATASET>": int }
}
```

Dataset keys observed in production: `TICKERS, SEP, SFP, SF1, EVENTS`. `CURRENT_SCHEMA_VERSION = 1`.

- `load`: missing file → empty meta (version 1, empty maps). ISO strings parsed with full offset support (`datetime.fromisoformat`) (; test).
- `save`: ensure dirs; serialize with `indent=2, sort_keys=True`; timestamps via `astimezone(UTC).isoformat` (microsecond precision, `+00:00` suffix); write to `.meta.json.tmp` then atomic `os.replace` rename; no tmp file remains after success (; test).
- `record_sync(dataset, ts, row_count)` is a pure functional update returning a new value; only the named dataset's entries change (; test).
- Semantics of `row_counts`: for `bootstrap` it is rows written in that run; for `update`/catchup it is **previous count + newly added**. For TICKERS it is always the full rewrite count. It is *not* re-derived from disk.
- Semantics of `last_sync`: wall-clock time the sync ran, **not** the data as-of date. Catchup explicitly trusts a same-day timestamp ("If `make sync-universe update` ran earlier today, we trust it" — test).

---

## 6. Writers — merge keys, idempotency, chunking

Common merge algorithm (used by every incremental path) —:

1. `_normalize(new)`: coerce key date column (`date` for SEP/SFP/EVENTS, `datekey` for SF1) via `pd.to_datetime(...).tz_localize(None).normalize`; sort by the dedup keys ascending; reset index.
2. If target file exists: read it, `_normalize(existing)` (SEP/SFP/SF1/EVENTS all re-normalize existing too), `concat([existing, new])`, `drop_duplicates(subset=dedup_keys, keep="last")` — **new rows win** on key collision (a revised bar/filing replaces the old row), then sort by dedup keys, reset index.
3. `added = len(merged) - len(existing)` — counts only *net new keys*; a re-run with identical data returns 0 (idempotency oracle:,,,).
4. Overwrite the whole target parquet (`to_parquet(path, index=False)`).

Note on ordering: sort is by the dedup-key tuple only — e.g. SEP rows ordered by `(ticker, date)` (ticker-major), SF1 by `(ticker, datekey, dimension)` with dimension order = lexicographic (`ARQ < ART < ARY < MRQ < MRT < MRY`). — readers and goldens depend on row order.

- Updated rows (same key, changed values — e.g. Sharadar restates a bar; `lastupdated` advances) are silently applied but **not counted** in the return value (`added` counts only net-new keys). Original: return value undercounts revisions. Improvement: optionally also report `revised` count; the on-disk result and the `added` number must remain identical.
- Whole-partition rewrite is not crash-atomic (a kill mid-`to_parquet` corrupts the year file; meta may then point at a corrupt partition). Original: direct overwrite. Improvement: write to `part-0.parquet.tmp` + `os.Rename`, mirroring `.meta.json`'s pattern.

### 6.1 Date chunking — `_date_chunks(start, end, months=3)`

(; oracle.)

- `months` must divide 12 (`{1,2,4,3,6,12}`), else `ValueError("months must divide 12 evenly; got {months}")`.
- Chunks are **calendar-aligned** windows of width `months`, clamped to `[start, end]`, inclusive on both ends. Algorithm: for cursor `cur`, `chunk_end_month = ((cur.month-1)//months)*months + months` (capped at 12); window end = last day of that month (Dec → Dec 31; else first day of next month minus 1 day); `chunk_end = min(window_end, end)`; yield `(cur, chunk_end)`; `cur = chunk_end + 1 day`.
- With `months=3` over a full year this yields exactly the four quarters `[01-01..03-31] [04-01..06-30] [07-01..09-30] [10-01..12-31]` → exactly 4 API calls (asserted with literal filter dicts in). A `start` mid-quarter yields a partial first chunk ending at that quarter's boundary.
- Rationale (keep in Go doc comment): quarterly ≈ 220k rows/call (~3,500 stocks × 63 trading days), safely under the per-call ~1M row cap; half-year empirically hit the cap in 2021-H2.

### 6.2 `bootstrap_sep(client, layout, *, start, end) -> int` / `bootstrap_sfp`

(,.)

- `ensure_dirs`; for each quarterly chunk call `get_table("SHARADAR/SEP"|"SHARADAR/SFP", paginate=True, date={"gte": chunk_start.isoformat, "lte": chunk_end.isoformat})`; empty result → log info, continue. Normalize; `groupby(df["date"].dt.year)`; per year: merge into the year partition with the common algorithm (bootstrap **also merges** — re-running bootstrap on an overlapping range is idempotent, asserted by the writer docstring and dedup logic). Return total net-new rows across all chunks/years.
- A chunk spanning rows of multiple years (possible when API returns straddling data) is split per-year into separate partitions (writes 2023 and 2024 partitions from one response).

### 6.3 `update_sep(client, layout, *, asof) -> int` / `update_sfp`

(,.)

- Single-day pull: `date={"gte": asof.isoformat, "lte": asof.isoformat}`. Empty → return 0 **without** touching any partition. Otherwise normalize, group by year (handles year-boundary days —), merge, return net-new row count (same-day re-run → 0, existing rows preserved;).

### 6.4 `bootstrap_sf1 / update_sf1(client, layout, *, tickers) -> int`

(.)

- Empty ticker list → return 0, no API calls. Tickers batched in groups of **500** (`_TICKER_BATCH_SIZE`,), call `get_table("SHARADAR/SF1", paginate=True, ticker=batch)` — **no dimension filter, no date filter** (full history, all 6 dimensions; asserted).
- `bootstrap_sf1` **overwrites** each per-ticker file with the grouped slice (no merge with pre-existing files); `update_sf1` merges per ticker on `(ticker, datekey, dimension)` with the common algorithm and returns net-new rows. Ticker keys from `groupby("ticker")` are stringified for the filename.

### 6.5 `bootstrap_events / update_events(client, layout, *, tickers) -> int`

(.)

- Same shape as SF1: batches of 500, `get_table("SHARADAR/EVENTS", paginate=True, ticker=batch)` (no date filter — full history); bootstrap overwrites per-ticker files; update merges on `(ticker, date, eventcodes)`; `date` normalized.

### 6.6 `write_tickers(client, layout) -> int`

See §2.5. Always a full fetch + filter + overwrite, used identically by bootstrap, update and catchup.

- `update_sf1` / `update_events` re-fetch the **entire history for every ticker on every daily update** (~3,000 rows/ticker × 21k tickers; production meta shows SF1 ≈ 2.96M rows). Original: full-table refresh per update (correct but wasteful of API quota and time). Improvement: filter by `lastupdated={"gte": last_sync_date}` on the API call while keeping the identical on-disk merge; fall back to full fetch on bootstrap. Functional results must be identical.

---

## 7. Read API — `SharadarUniverseCache`

Source:. Tests:.
Read-only; constructed with a `CacheLayout`. All methods return an
**empty frame for unknown tickers / missing files — never an error**.

Go return-shape note: "DataFrame" maps to an ordered columnar/record representation; what is
contractual is the **column set, dtypes, row order, and emptiness semantics** below.

### 7.1 `get_tickers -> DataFrame`

- Missing `TICKERS.parquet` → empty frame; otherwise full file contents, order as written (sorted by ticker).

### 7.2 `_filter_by_window(df, start, end)` — tradability predicate

(.) Underlies both list methods.

- Keep a row iff:
 - `firstpricedate` missing/NaT **or** `firstpricedate <= end` (listed on/before window end), AND
 - `lastpricedate` missing/NaT/empty-string **or** `lastpricedate >= start` (still trading at/after window start).
 - Date coercion via `pd.to_datetime(value, errors="coerce")` — empty string and invalid values become NaT ⇒ "keep". If a column is entirely absent, no filtering on it (keep all).
 - Comparison at day precision against `pd.Timestamp(start|end)` (midnight); since stored values are normalized dates, this is a date comparison.
- Test oracle: (ALIVE kept, DELISTED_IN_WINDOW kept, DELISTED_BEFORE dropped, TOO_NEW dropped).

### 7.3 `list_active_tickers(as_of: date) -> list[str]`

- `= _filter_by_window(tickers, as_of, as_of)["ticker"].astype(str).sort_values.tolist` — point-in-time tradability; empty cache or missing `ticker` column → `[]`; result **sorted ascending**, plain strings (; tests). Sort is pandas lexicographic on str (byte-wise for ASCII tickers).

### 7.4 `list_universe_for_window(start, end, *, table=None) -> list[str]`

- Optional pre-filter `df[df["table"] == table]` (`"SF1"` = common stocks, `"SFP"` = ETFs/funds, `None` = both); then window filter; sorted ticker list (; tests). This is the survivor-bias-free backtest universe API — includes mid-window delistings.

### 7.5 `get_bars(ticker, *, start, end) -> DataFrame`

(.)

- Scan years `start.year..= end.year`; for each year check **both** `SEP/year=Y/part-0.parquet` and `SFP/year=Y/part-0.parquet` (SEP first); skip missing files; read with predicate pushdown `filters=[("ticker","==",ticker)]`; mask `start <= date <= end` **inclusive both ends**; concat all non-empty frames; if none → empty frame; else sort by `date` ascending, reset index.
- A ticker lives in exactly one of SEP/SFP, so the union is single-source per ticker — but the implementation does NOT assume it: if (pathologically) the same ticker existed in both, rows would be concatenated, **not deduplicated**, and the sort is by `date` only (stable concat order: SEP rows before SFP rows within a date). Replicate as-is.
- Returned columns = full SEP/SFP schema (§2.1) in file order; `date` is tz-naive `timestamp[ns]`. Tests: cross-year boundary, unknown ticker empty (`190-193`), ETF served from SFP transparently (`213-238`).

### 7.6 `get_fundamentals(ticker, *, dimension=None) -> DataFrame`

- Missing per-ticker file → empty. `dimension=None` → all cached dimensions. Non-None and frame non-empty and `dimension` column present → `df[df["dimension"] == dimension]`, index reset. Valid lenses: `ARQ/ART/MRQ/MRT/ARY/MRY` — not validated, unknown values just return empty selection (; tests).

### 7.7 `get_events(ticker) -> DataFrame`

- Missing file → empty; else full per-ticker file.

### 7.8 `stats -> dict`

- Keys `TICKERS, SEP, SFP, SF1, EVENTS`; each value = total row count: TICKERS = `len(read_parquet(tickers_file))` or 0 if missing; SEP/SFP = sum over glob `year=*/part-*.parquet` (reading only the `ticker` column); SF1/EVENTS = sum over glob `ticker=*.parquet` (; tests).

### 7.9 `SharadarHistoryProvider` (backtest `BarHistoryProvider`)

Source:; protocol; tests.

- Depends on a narrow `SharadarCacheReader` interface: `read_bars(ticker) -> DataFrame` (full history, **DatetimeIndex-indexed**, columns at minimum lower-case `open high low close volume`) and `list_active_tickers(as_of) -> list[str]`.
- `get_bars(ticker, start, end)`:
 - `start > end` → `ValueError` with message matching `start ({start}) after end ({end})` (test regex `r"start.*after.*end"`).
 - empty source → return it as-is.
 - else inclusive index mask `start <= idx <= end`, return a **copy**.
- `list_tickers(*, as_of)` = `list(cache.list_active_tickers(as_of))` — order preserved from the reader (which sorts).
- **Gap to note**: `SharadarUniverseCache` does **not** implement `read_bars`; in the current codebase nothing wires `SharadarHistoryProvider` to the real cache (backtests use `SharadarUniverseCache.get_bars` directly via). Go must keep both surfaces (COMPLETE); see Open questions Q4.

---

## 8. `ensure_cache_fresh` — startup auto-catchup

Source:. Tests:. Call site:.

### 8.1 Signature and report

```
ensure_cache_fresh(client, layout, *, today: date | None = None) -> CatchupReport
CatchupReport{ skipped_reason: str|None, days_attempted: int, days_succeeded: int,
 rows_added: map[string]int, errors: []string }
CatchupReport.did_work = days_attempted > 0 || len(rows_added) > 0
```

(.)

### 8.2 Flow — in this exact order

1. `ensure_dirs`; load meta.
2. `meta.last_sync["SEP"]` absent → log warning, return `CatchupReport(skipped_reason="not-bootstrapped")` (never auto-bootstraps — full backfill takes hours; test).
3. `today` defaults to `datetime.now(UTC).date`. `target = today - 1 day` (T-1). `start = last_sync["SEP"].date` (the **wall-clock** date of the previous sync — see §5 and below).
4. `days = _trading_days(start, target)`: **weekdays only** (Mon–Fri, `pd.bdate_range`), inclusive both ends; `end < start` → `[]`. US holidays are *not* excluded — holiday calls return 0 rows and are harmless (; tests). Empty `days` → log "cache fresh", return empty report (`did_work == false`).
5. Per day `d` in order — **SEP first, then SFP** (quota exhaustion mid-loop should leave the most-used dataset most complete):
 - `update_sep(client, layout, asof=d)`; on success `rows_added["SEP"] += n` and `meta = meta.record_sync("SEP", ts=now, row_count=old+ n)`; on exception: append `"SEP {d}: {exc}"` to errors, log warning, **continue** (warn-and-continue).
 - same for SFP with key `"SFP"`.
 - `days_succeeded` increments only when **both** SEP and SFP succeeded for that day.
 - **`meta.save(layout)` after every day** so a crash preserves progress (test).
6. Once, after the day loop: `write_tickers` → `rows_added["TICKERS"] = n` (absolute count), `record_sync("TICKERS", row_count=n)`, save meta; failure → error `"TICKERS: {exc}"`, continue.
7. Reload ticker list from disk (`sorted(get_tickers["ticker"].astype(str))`,). Empty → append error `"TICKERS list empty — skipping SF1 / EVENTS"`, skip step 8.
8. `update_sf1(tickers)` then `update_events(tickers)`; each: `rows_added[key] = n` (absolute, not +=), `record_sync(key, row_count=old + n)`, save meta; failure → `"SF1: {exc}"` / `"EVENTS: {exc}"`, continue. **TICKERS failure does not prevent SF1/EVENTS** (they use the previous on-disk ticker list; test).
9. Return report with `days_attempted = len(days)`, accumulated `rows_added` (always containing all five keys, zero-initialized), `errors`.

Oracle for the full ordering + per-day arguments: (3 weekday gap → exactly 3 `update_sep` + 3 `update_sfp` interleaved per-day, then exactly one `write_tickers`, `update_sf1`, `update_events`, with `rows_added == {"SEP":6,"SFP":3,"SF1":50,"EVENTS":20,"TICKERS":100}`).

- Nothing in catchup ever raises to the caller for per-step failures; only programming errors escape. The live runner additionally wraps the whole call in try/except WARN.
- `start = last_sync["SEP"].date` re-pulls the day of the previous sync (overlap by design — idempotent merges make it safe but it costs one redundant API day per run) and uses wall-clock sync date rather than max bar date on disk; if a sync ran at 00:30 UTC the prior trading day could in principle be skipped when bars publish late. Original: as described. Improvement: track an explicit `last_asof` per dataset in meta (additive field, schema_version bump) and catch up from `last_asof + 1`; keep the overlap-by-one as a safety default.
- `_trading_days` is weekday-based, knowingly wasting ~9 holiday calls/yr × 2 datasets. Original: `bdate_range` Mon–Fri. Improvement allowed: embed a NYSE holiday table to skip them — but zero-row days must remain harmless either way.

---

## 9. CLI `sync-universe` (bootstrap | update | stats)

Required operationally (Makefile target `sync-universe`).

- `bootstrap --start YYYY-MM-DD --end YYYY-MM-DD [--ticker T...]`:
 order TICKERS → SEP → SFP → SF1 → EVENTS. After TICKERS, the ticker list drives SF1/EVENTS (and, with `--ticker`, also SEP/SFP fetched per-ticker instead of date-only — smoke-test path writes year partitions directly **without merge**,). Abort if the ticker list is empty. Each step records `record_sync(dataset, ts=now, row_count=n)`; **meta saved once at the end** for bootstrap.
- `update [--asof YYYY-MM-DD]` (default local today): TICKERS → SEP(asof) → SFP(asof) → SF1(full) → EVENTS(full); row counts accumulate (`cur + n`); meta saved at end (`:140-180`).
- `stats`: prints cache root, then a table `Dataset | Rows | Last sync` for `TICKERS, SEP, SF1, EVENTS` (note: **SFP is computed by `stats` but not printed** — preserve or fix, see Open questions Q5) with `(never)` for missing timestamps (`:183-199`).
- Bootstrap saves meta only at the very end — a crash mid-bootstrap loses all `last_sync` records even for completed datasets (the parquet data itself survives). Improvement: save meta after each step, like catchup does.

---

## 10. Downstream column contracts (what the cache must guarantee)

These consumers define what "byte-equivalent" means in practice:

| Consumer | Reads | Contract | Source |
|---|---|---|---|
| `_bars_to_engine_format` | `date, open, high, low, close, volume` | index = `to_datetime(date).tz_localize("UTC")`; column subset in that order |
| EOD refresh `_bar_from_row` | same + `date` | `Decimal(str(x))` for OHLC, `int(volume)`, naive ts → UTC |
| `_market_cap_lookup_factory` | SF1 `datekey, marketcap` | latest row by `datekey` (sort ascending, take last); `0.0` for unknown/NaN/missing column; memoized |
| `_load_sf1_mrt` | SF1 `ticker, datekey, dimension, marketcap` | pyarrow dataset scan with pushdown `ticker ∈ set && dimension == "MRT"` |
| `_load_earnings` | EVENTS `ticker, date, eventcodes` | exact `"22" ∈ split(eventcodes,"|")`; rename `date → report_date` |
| `load_sf1_market_caps` | SF1 `ticker, datekey, dimension, marketcap` | filter `dimension == "MRT"` (only if column exists) → `datekey.date <= as_of` → drop NaN marketcap → `marketcap > 0` → sort by datekey-date → last per ticker → `Decimal(str(v))` |
| `load_earnings_calendar` | `ticker, report_date` | blackout = any earnings date within `as_of ± 5` calendar days (`EARNINGS_BLACKOUT_DAYS = 5`); returns only `True` entries |
| live/EOD universe | TICKERS via `list_universe_for_window(warmup_start, today, table="SF1")` | warmup = `today − 2*365 days`; exclude `{"SPY"} ∪ sector ETFs (∪ pair legs in EOD)`; cap top-N (default 85) by market cap desc, unknown caps (0.0) sort last; ≤ limit → pass through unchanged |; |
| SPY warmup | `get_bars("SPY", warmup_start, today)` | served from SFP partitions |; (500-day SPY warmup) |

All unless the owning spec for those modules says otherwise; they are listed here because they constrain the cache's column names, dtypes, and ordering.

Note on `load_sf1_market_caps` tie-handling —: sort key is the *date-truncated* `datekey` and pandas sort is stable, so among multiple filings on the same date the **last row in input order** wins.

---

## 11. Production-cache reference figures (sanity targets, not contracts)

From `cache/sharadar/.meta.json` as of 2026-05-28: TICKERS 21,362 rows; SEP 8,742,128; SFP 6,362,149; SF1 2,958,955; EVENTS 2,116,119. SEP/SFP partitions span `year=2020.. year=2026`. Use for smoke-validation of a Go bootstrap (row counts will drift daily).

---

## 12. Go implementation notes (non-normative)

- Parquet writing must produce the canonical column types: dates as `timestamp[ns]` (tz-naive), prices/volume as `float64`, strings as UTF-8. Round-trip fidelity is a gate criterion: a write-then-read of the cache must agree. This gate is scoped to the **parquet cache layer**; the derived PostgreSQL store deliberately uses BIGINT 1e-4 fixed point for price columns under the note in §2.1 (overflow → NULL + `FieldsNulled` accounting), which is not a round-trip surface.
- "Empty DataFrame" maps to an empty typed slice + present-but-empty schema; the distinction "empty with columns" vs "completely empty" is never observed by callers (only `.empty`/`len` and column membership checks).
- Error policy: reader never errors on missing data; writers propagate API errors; catchup converts everything to warn-and-continue.
- PRODUCTION-GRADE additions (context cancellation through client retries and per-day catchup loop, structured logging mirroring the `log.info/warning` messages above, graceful shutdown that finishes the in-flight `meta.save`) are required by the project bar and are all tagged where they alter observable timing, never data.

---

## 13. Open questions

1. **Repo-root discovery** — the default cache dir is anchored at the directory containing `go.mod`? Or require `TMS_SHARADAR_CACHE_DIR` to be always set in the containerized deployment (ports 18080/13000 stack) and make the walk-up a dev-only fallback? Needs a decision; affects only path resolution, not data.
2. **`delistedate` keep-column** — never present in real API output (absent from the production TICKERS.parquet; Sharadar's actual TICKERS table has no such field — closest is `lastpricedate`/`isdelisted`). Likely a misspelling of an intended `delistdate`. Keep the dead keep-list entry, or drop it? (No observable difference either way given current API output.)
3. **Local-vs-UTC "today" inconsistency** — catchup uses UTC date, the `update` CLI uses local `date.today`, live_runner's warmup window uses local `date.today`. On a host west of UTC around midnight these disagree by one day. Replicate exactly, or normalize all to UTC (candidate)? Currently spec'd as replicate-exactly.
4. **`SharadarHistoryProvider.read_bars` wiring gap** — the Protocol expects a `read_bars(ticker)` returning a DatetimeIndex-indexed frame, but `SharadarUniverseCache` exposes only `get_bars(ticker, start, end)` with a `date` column; no adapter exists in the repo. Should Go implement a thin adapter (full-history read + set index) to make the provider actually usable, or port it as the same dormant seam?
5. **`stats` CLI omits SFP** — `SharadarUniverseCache.stats` computes `SFP` but the CLI prints only `TICKERS, SEP, SF1, EVENTS`, and `last_sync["SFP"]` is never shown. Bug or intentional? Spec'd as replicate; flag for product decision.
6. **Concurrent writers** — nothing guards two simultaneous syncs (CLI update + live-node catchup both rewrite year partitions and `.meta.json`). Today this relies on operator discipline. Should we add a lock file under the cache root, or preserve the unguarded behavior?
7. **SEP `dividends` column** — Sharadar's current SEP table documentation also lists a `dividends` column; the production cache (last full bootstrap) does not contain it. If a fresh bootstrap returns extra columns, the store-verbatim rule (§2) admits them automatically, but cross-language schema goldens should pin the observed 10-column set or tolerate supersets — which?
8. **Per-call row cap** — chunk sizing assumes the data provider's ~900k–1M row response cap on `paginate=True` (which stops at 1M rows with a warning). A native Go pagination loop has no such cap. Keep quarterly chunks for quota friendliness (spec'd), but confirm whether the client should also emit a warning when a single logical call exceeds 1M rows.

---

## 14. P1 addendum — API sync implementation decisions (Go, `internal/data/sharadar`)

The P1 builder implemented the Nasdaq Data Link client (`client.go`/`wire.go`)
and the API → PostgreSQL incremental sync (`sync.go`, pure layers in
`syncplan.go`/`syncconvert.go`). This addendum records the locked decisions
that resolve the open questions above and the sanctioned deviations taken.

### 14.1 Resolved open questions

- **Q1 (cache/data dir discovery)** — *locked*: explicit env wins
 (`TMS_SHARADAR_CACHE_DIR` etc.); the repo-root walk-up
 (`go.mod`/`pyproject.toml`) remains a dev-only fallback in
 `ResolveCacheDir`. Containerized deployments must set the env var.
- **Q2 (`delistedate` keep-column)** — *locked, deviation*: the source repo's
 `delistedate` keep-list entry is a typo for a column
 the live API never returns; the dead spelling is **dropped**. The Go API sync
 reads `delistdate` (`syncconvert.go convertTickerAPIRow`) into
 `tms.tickers.delist_date`; if the API does not return it either, the column
 is NULL — observably identical to the original on current API output.
- **Q3 (local-vs-UTC "today")** — *locked, deviation*: all
 "today"/trading-date logic in the Go sync is normalized to the
 **America/New_York trading date** via `internal/data/calendar`
 (`catchupWindow` in `syncplan.go`), replacing the original's mix of UTC
 and host-local dates. The watermark start date is likewise interpreted as
 the NY date of the previous sync; combined with the overlap-by-one-day
 repull and idempotent merges, no data can be skipped or duplicated.
- **Q8 (per-call row cap)** — the Go client paginates natively without a cap
 and logs a warning when one logical call exceeds 1,000,000 rows; quarterly
 chunking for SEP/SFP bootstrap is kept (quota friendliness).

### 14.2 Sanctioned deviations in the Go API sync

- **Retry schedule** (spec §3.1): 4 attempts on HTTP 429/5xx with 2/4/8 s
 waits between attempts, classified on the real status code
 (`resp.StatusCode == 429 || >= 500`), `Retry-After` honored when longer
 than the backoff, context-aware sleeps. The original's final 16 s sleep
 computed before giving up is dropped (no observable data effect). Terminal
 error message keeps the `failed after 4 retries` shape.
- **Trading-day enumeration** (spec §8.2 step 4): NYSE holidays are skipped
 via `internal/data/calendar` instead of issuing zero-row weekday calls
 (explicitly allowed by §8's final); outside the calendar's
 covered year range the Mon–Fri rule is the fallback.
- **SF1/EVENTS incremental refresh** (spec §6.6): catchup adds
 `lastupdated.gte=<NY date of previous sync>` to the 500-ticker batch calls;
 on-disk/merge results are identical to a full refetch.
 `WithFullRefetch` restores the original full-history behavior.
- **Net-new counting** (spec §6 step 3): `added` = net-new keys, computed
 relationally via `INSERT... ON CONFLICT... RETURNING (xmax = 0)`; revised
 rows are applied but not counted, exactly like
 `len(merged) - len(existing)`.
- **Streaming ingestion**: rows stream from the API through bounded staging
 batches; an API failure mid-dataset can leave earlier batches committed
 (the original wrote nothing on failure). The merge is idempotent, so the
 next run converges; the watermark only advances on full success.
- **Audit trail** (additive): every bootstrap/catchup run inserts one row per
 dataset into `tms.dataset_sync_runs`
 (started/finished/rows_added/status/error, migration
 `000009_dataset_sync_runs`). `tms.dataset_sync` remains the
 CacheMeta watermark.
- **API key env name**: canonical Go-side name is
 `TMS_NASDAQ_DATA_LINK_API_KEY`; the bare
 `NASDAQ_DATA_LINK_API_KEY` is accepted as a fallback. Missing key fails
 loud at client construction with the account-profile hint (spec §1).

### 14.3 Production entry points for the API sync engine

The `Syncer` (EnsureFresh/Bootstrap) is reachable two ways in production;
both build it from `config.NasdaqDataLinkAPIKey`, the NYSE calendar and the
live `pgStore`, and both honor context cancellation (cooperative cancel /
graceful drain):

- **Worker job `data.refresh source=api`** → `EnsureFresh` (catchup).
 `cmd/tms/worker.go` constructs a `*sharadar.Syncer`, wraps it in
 `handlers.SharadarAPISyncer` (the `handlers.APISyncer` implementation) and
 injects it into `NewDataRefresh`. When `TMS_NASDAQ_DATA_LINK_API_KEY` is
 unset the worker still starts (parquet refreshes and every other job kind
 unaffected) and `source=api` jobs fail fast with a "key not set" message —
 the same best-effort-degradation pattern as the Redis notifier. Because
 catchup is whole-universe + watermark-driven, a `source=api` job carrying
 `tables`/`tickers`/`since` is rejected with a pointer to
 `tms sync bootstrap` rather than silently ignoring the scope.
- **CLI `tms sync`** → `bootstrap` (bounded backfill, `--start/--end/
 --ticker`) and `catchup` (the same `EnsureFresh`). The operator twin of
 the worker path and the relational counterpart of `make sync-universe`
 (spec §9). The API key is required up front (fail-loud before any work).

Both paths write `tms.dataset_sync_runs` audit rows and advance
`tms.dataset_sync`.
