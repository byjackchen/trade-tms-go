# Spec: Trading Calendar & Universe Construction

This repo's definition of trading-calendar/session handling, the screener
protocol, SEPA screener ranking, market-cap top-N capping,
`TMS_LIVE_UNIVERSE_LIMIT` logic, and the exact ticker-exclusion sets. The rules
below are invariants of this system, including edge cases, NaN handling,
rounding, and ordering. Where a known weakness is called out, the better
behavior this repo adopts is documented alongside it.

---

## 1. Trading calendar & sessions

### 1.1 There is NO NYSE holiday-calendar dependency — by design

 This system deliberately does **not** depend on any
NYSE trading-calendar library (no `exchange_calendars`, no
`pandas_market_calendars`). Three independent mechanisms approximate the
calendar; each must be replicated as-is:

1. Weekday-only "trading days" for cache catch-up (§1.2).
2. A fixed RTH wall-clock window in `America/New_York` for live minute-bar
 aggregation (§1.3).
3. Date-rollover detection on incoming bars for pre-open scheduling (§1.4).

The decision is documented in:
US holidays produce zero-row API calls which are swallowed cleanly; "~9
holidays/yr x 2 calls" of waste was judged preferable to a NYSE-calendar
dependency.

 Original: holidays and half-days are not modeled anywhere (see
§1.3 for the explicit half-day gap). Proposed Go improvement: embed a static
NYSE holiday/early-close table (pure data, no heavy dependency) and use it to
(a) skip holiday catch-up calls, and (b) close half-day sessions at 13:00 ET.
If implemented, it must be behind a flag defaulting to reference-compatible
behavior, because half-day bars after 13:00 ET are *dropped* in the original
 and reproducing reference
outputs requires the same drop.

### 1.2 Catch-up "trading days" = pandas business days (Mon–Fri)

Source:; tests.

 `_trading_days(start, end)`:

- Returns every **weekday** (Mon–Fri) from `start` **inclusive** to `end`
 **inclusive** (`pd.bdate_range(start, end, freq="B")`,).
- If `end < start` → empty list.
- If `start` or `end` falls on a weekend, `bdate_range` simply excludes
 those days (verified by test: range Sat 2026-05-23 → Tue 2026-05-26 yields
 `[Mon 2026-05-25, Tue 2026-05-26]`,).
- US holidays are **included** (they are weekdays); downstream per-day fetches
 return zero rows and are treated as success, not error.

 Catch-up window derivation:

- Source of truth for "last synced" = `meta.last_sync["SEP"]` (a timestamp);
 if absent → skip entirely with `skipped_reason="not-bootstrapped"`.
- `today_utc = datetime.now(tz=UTC).date` unless injected. **Timezone: UTC**, not exchange-local.
- `target = today_utc - 1 day` (T-1).
- `start = last_sep.date` — i.e. the already-synced day is **re-fetched**
 (inclusive start). Idempotent: same-day re-writes add 0 rows.
- Empty day list → "cache fresh" no-op report.

 Per-day loop order and failure policy:
for each day, SEP first then SFP; each step wrapped in
try/except — a failure appends to `errors` and continues (warn-and-continue,
never aborts startup,); a day counts as `succeeded` only if
both SEP and SFP succeeded; meta is persisted after
every day; then one TICKERS refresh, then SF1 + EVENTS
keyed on the refreshed ticker list; empty ticker list
skips SF1/EVENTS with an error entry.

### 1.3 Live RTH session window (minute → daily aggregation)

Source:.

 Session definition:

| Item | Value | Source |
|---|---|---|
| Timezone | `America/New_York` (IANA, DST-aware) |
| RTH open | 09:30 ET, minute-of-day 570, **inclusive** |
| RTH close | 16:00 ET, minute-of-day 960, **inclusive** (the 16:00 minute bar is kept) |
| Membership test | `570 <= et.hour*60 + et.minute <= 960` on the bar's `ts_event` converted from ns-UTC to ET |

- Pre-market and post-market bars are silently dropped.
- **Half-days are NOT handled**: on 13:00-ET-close days, bars after 13:00 are
 still accepted until 16:00 — explicit out-of-scope note at. (See in §1.1.)
- **Holiday detection is NOT implemented**: a non-trading day simply produces
 zero RTH bars and no daily emit.

 Daily-bar assembly semantics:

- Minute pushes dedup by exact `ts_event` (nanoseconds): a dict keyed on
 `ts_event` keeps only the **latest** push per minute bucket, so
 intermediate intra-minute updates never double-count volume.
- Day-boundary trigger: when an RTH minute bar arrives whose ET **date**
 differs from the date being accumulated, the prior day's daily bar is
 emitted and state resets. First-ever bar
 just records the date. Consequence: strategies see day D's daily bar at
 the start of day D+1.
- Emitted bar: bars sorted by `ts_event` ascending; `open` = first bar's
 open, `high` = max of highs, `low` = min of lows, `close` = last bar's
 close, `volume` = integer sum of minute volumes,
 `ts_event = ts_init =` last minute bar's `ts_event`.
- If a day accumulated zero RTH bars, **no** emit on rollover.

### 1.4 Pre-open trigger (screener refresh scheduling)

Source:.

 Day-boundary detection in `SEPAUniverseRunner`:

- `_maybe_trigger_pre_open` runs on **every** bar **before** the bar is
 dispatched to signal generators.
- Boundary test uses `bar.ts.date` where `Bar.ts` is **tz-aware UTC**
 — i.e. the UTC calendar date of
 the bar timestamp, not the ET date.
- First bar ever: record the date, do **not** fire (day 0 has no screener
 data).
- Fire `on_pre_open(new_date)` only when `bar_date > last_processed_date`
 (strictly greater, not `!=` — out-of-order older dates never re-fire).
- `on_pre_open(target_date)`: ask screener `top_k(k=active_cap,
 as_of=target_date)`; rebuild `active_set` keeping only candidates whose
 symbol maps to a registered instrument; then enforce subscription cap. Defaults: `active_cap=20`,
 `subscription_cap=30`.

### 1.5 Market-session labels (quote stream)

Source:.

 moomoo `market_status` → session label mapping (uppercased
input, unknown → `"closed"`):

| moomoo status | session |
|---|---|
| `PRE_MARKET_BEGIN`, `PRE_MARKET` | `pre` |
| `MORNING`, `AFTERNOON`, `REGULAR`, `REST` | `regular` |
| `AFTER_HOURS_BEGIN`, `AFTER_HOURS` | `post` |
| `CLOSED` (and anything else) | `closed` |

---

## 2. Universe construction (data tier)

### 2.1 BarHistoryProvider protocol

Source:.

 Interface (Go: a small interface):

- `get_bars(ticker, start, end) -> DataFrame` — date-indexed, columns at
 minimum `open, high, low, close, volume` (lower-case).
- `list_tickers(as_of) -> []string` — active universe symbols on `as_of`.

### 2.2 Ticker-window filtering (survivor-bias-free)

Source:.

 `_filter_by_window(df, start, end)` keeps a TICKERS row iff:

- `firstpricedate` is missing/NaT **or** `firstpricedate <= end`, **and**
- `lastpricedate` is missing/NaT/empty-string **or** `lastpricedate >= start`
 (; empty string coerces to NaT via
 `pd.to_datetime(..., errors="coerce")`,, and counts as
 "still active").
- If a date column is entirely absent, no filtering on it (keep all rows).
- Comparisons are date-inclusive on both ends.

 `list_universe_for_window(start, end, table=...)`:

- Empty cache or missing `ticker` column → empty list.
- Optional `table` filter on the TICKERS `table` column: `"SF1"` = common
 stocks (the SEPA universe), `"SFP"` = ETFs/funds, `None` = both.
- Result: tickers as strings, **sorted ascending** (lexicographic,
 `sort_values`), duplicates not removed by this function.

 `list_active_tickers(as_of)` = same filter with
`start = end = as_of`.

 `get_bars(ticker, start, end)`: scans
both SEP (stocks) and SFP (ETF) yearly parquet partitions for
`start.year... end.year`, filters `start <= date <= end` inclusive, concats,
sorts by `date` ascending, resets index; unknown ticker → empty frame, never
an exception.

### 2.3 Market-cap lookup

Source: (used by both the
backtest assembly and `live_runner` via import at).

 `_market_cap_lookup_factory(cache)` returns a memoized
`lookup(ticker) -> float`:

- Per-process memoization dict; first call per ticker hits
 `cache.get_fundamentals(ticker)`.
- If SF1 frame is nil/empty or lacks a `marketcap` column → cache and return
 `0.0`.
- Otherwise take the row with the **latest `datekey`**
 (`sort_values("datekey").iloc[-1]`) — note: across **all** SF1 dimensions
 (ARQ/ART/MRQ/MRT/ARY/MRY), since `get_fundamentals` is called without a
 dimension filter.
- `marketcap` of `None`/NaN → `0.0`; else `float(marketcap)`. Units: USD (Sharadar SF1
 `marketcap` is USD).
- Semantics of `0.0`: "unknown ticker, treated as fail-rule-8 by the
 screener" and "sorts last" in the
 universe cap (§4).

---

## 3. Screener protocol & SEPA screener

### 3.1 Protocol

Source:.

 Two-method contract:

- `update(bar)` — per-bar rolling-state update; complexity contract is
 **O(1) per call** (fired ~3,500×/trading day; ~4.4M calls over a 5-year
 backtest,). No full recompute per bar.
- `top_k(k, as_of) -> []ScreenedCandidate` — once per trading day;
 O(N log N) acceptable.

 `ScreenedCandidate` value object:
`instrument_id string`, `score float64`, `metadata map[string]any`
(intentionally untyped bag for diagnostics).

### 3.2 SEPA screener configuration

Source:.

| Parameter | Default | Meaning | Source |
|---|---|---|---|
| `market_cap_lookup` | required | `func(ticker) -> USD market cap; 0.0 = unknown (fails rule 8)` |
| `history_max_bars` | **260** | rolling per-ticker bar-tail cap (≈1 trading year; MA200 + 252-bar 52w window need ≥252) |
| `market_cap_min_usd` | **500,000,000.0** | forwarded to trend-template rule 8 |
| `_BREAKOUT_BASE_LOOKBACK` | **60** (constant) | breakout-proximity high/low window |

### 3.3 Per-ticker rolling state and `update(bar)`

Source:.:

- State per ticker: deque of `(ts, open, high, low, close, volume)` with
 floats for OHLC and int volume; inputs are
 coerced `float(bar.open/high/low/close)`, `int(bar.volume)`. `Bar` fields arrive as `Decimal` + tz-aware
 UTC `ts`.
- On append: push right; pop left while `len > history_max_bars`.
- After every append, recompute over the trailing
 `min(len, 60)` bars: `last_60_high = max(high)`, `last_60_low = min(low)`
 (note: window is whatever is available when fewer than 60 bars exist). Also store `last_close`, `last_ts`.
- First bar for an unseen symbol creates state implicitly.

 `warmup(ticker, df)` (; tests):

- nil/empty frame → no-op, ticker not tracked.
- Keep only the **latest** `history_max_bars` rows (`df.tail(...)`),
 then append row-by-row with identical semantics to `update`
 (volume cast to int). Resulting `bars_seen == min(len(df),
 history_max_bars)`.
- Purpose: without warmup, every ticker has <200 bars on day 1 and the score
 collapses to breakout-proximity alone, favoring volatile micro-caps.

### 3.4 Breakout proximity

Source:; tests.

 `breakout_proximity(ticker)`:

```
raw = (last_close - low_60) / (high_60 - low_60)
result = clamp(raw, 0.0, 1.0)
```

- Unknown ticker → `0.0`.
- Degenerate/flat range (`high_60 <= low_60`) → `0.0`.
- Clamp below 0 → `0.0`; above 1 → `1.0`.
 Note: because `high_60`/`low_60` are updated by the same bar that supplies
 `last_close`, a gap-up close above the prior high yields exactly 1.0
 (test).

### 3.5 Trend Template count

Source:;.

 `trend_template_count(ticker)`:

- Unknown ticker or zero bars → `0`.
- Materialize deque → frame (`open/high/low/close/volume`, timestamp index)
 and call `trend_template.evaluate(df, market_cap_usd=lookup(ticker),
 market_cap_min_usd=cfg.market_cap_min_usd)`; return `passing_rules`
 (count of true rules, 0–8).
- Cost contract: only called from `top_k` (once per ticker per day), never
 from `update`.

 `evaluate` — the 8 Minervini rules:

Insufficient history, `n < 200` bars:
rules 1–7 all false; rule 8 = `market_cap_usd >= market_cap_min_usd` still
evaluated; diagnostics: `close` = last close (or 0.0 if n==0), MAs and 52w
levels 0.0, uptrend days 0. So a <200-bar ticker has count ∈ {0, 1}
(test).

With `n >= 200`:

| # | Rule | Exact formula | Comparison |
|---|---|---|---|
| 1 | Price > MA50 | `close > mean(close[-50:])` | strict `>` |
| 2 | Price > MA150 | `close > mean(close[-150:])` | strict `>` |
| 3 | Price > MA200 | `close > mean(close[-200:])` | strict `>` |
| 4 | MA50 > MA150 | strict `>` |
| 5 | MA150 > MA200 | strict `>` |
| 6 | Within 25% of 52w high | `close >= high_52w * (1.0 - 0.25)` | `>=` |
| 7 | ≥30% above 52w low | `low_52w > 0 ? close >= low_52w * (1.0 + 0.30): false` | `>=`, guarded |
| 8 | Market cap ≥ min | `market_cap_usd >= market_cap_min_usd` (default 5e8) | `>=` |

- MA = simple rolling mean of `close`, last value.
- 52-week window = **252 bars** on `high`/`low` columns respectively. Rolling
 `min_periods` defaults to the window, so with 200–251 bars the rolling
 value is NaN → fallback to **full-history** `max(high)` / `min(low)`.
- Constants: `_HIGH_TOLERANCE = 0.25`, `_LOW_PREMIUM = 0.30`,
 `DEFAULT_MARKET_CAP_MIN_USD = 500_000_000.0`.
- Diagnostic `ma200_uptrend_days`: 0 when
 `len < period + 2`; else count consecutive trailing positive first
 differences of the MA200 series (first diff is filled with 0 and therefore
 breaks the streak). Not used by any rule — diagnostic only.
- `passed` (all 8 true) exists but the screener uses only `passing_rules`.

**NaN handling note:** with `n >= 200` all three MAs are
well-defined (no NaN); rule comparisons therefore never see NaN. The only
NaN path is the 52w rolling fallback above. Go must reproduce: float64
comparisons, no epsilon.

### 3.6 Ranking — `top_k`

Source:; tests.:

- `k <= 0` or no tracked tickers → empty slice.
- For **every** tracked ticker: `tt = trend_template_count(t)`,
 `prox = breakout_proximity(t)`,
 `score = tt * 10.0 + prox * 5.0`,
 `cap = market_cap_lookup(t)`.
- Sort key: `(-score, -market_cap, ticker)` — i.e. **score DESC, then
 market cap DESC (decision U-D7), then ticker ASC** as the final
 deterministic tiebreak. Output is fully
 deterministic regardless of map-iteration order.
- Take first `k`; emit `ScreenedCandidate{instrument_id, score, metadata}`
 with metadata keys exactly: `trend_template_count` (int),
 `breakout_proximity` (float), `market_cap_usd` (float),
 `as_of` (ISO-8601 date string, `as_of.isoformat`).
- `as_of` is **informational only** — ranking uses whatever state has been
 accumulated; the caller is responsible for invoking `top_k` after the
 close of `as_of`.

 Original: `market_cap_lookup` is called twice per ticker per
`top_k` (once inside `trend_template_count`, once for the sort key) and the
scored tuple stores the cap twice. Memoization
in the factory makes this cheap, but Go may compute the lookup once per
ticker per ranking pass. Output is identical.

---

## 4. Live universe assembly & top-N cap

Source:.

### 4.1 Raw SEPA universe & exclusions

Source:.

 Sequence (live mode, warmup enabled):

1. `today = date.today` (process-local date);
 `warmup_start = today - 730 days` (`2 * 365`, no leap handling).
2. Raw SEPA universe =
 `cache.list_universe_for_window(warmup_start, today, table="SF1")`
 — i.e. common stocks tradable at any point in the 2-year window,
 survivor-bias-free, **sorted ascending** (§2.2).
3. **Exclusion set** `_non_stock = {"SPY"} ∪ sector_tickers`; the raw list is
 filtered to drop these, preserving order.

**Excluded tickers — exact list:**

- `SPY` (regime/heartbeat instrument;).
- The 11 Select Sector SPDR ETFs, from
 `strategies.sector_rotation.DEFAULT_UNIVERSE`, in source order:
 `XLK, XLF, XLE, XLV, XLY, XLP, XLU, XLB, XLI, XLRE, XLC`.

**Pair legs — exact list:** from
`strategies.pairs.DEFAULT_PAIRS`:
`("KO","PEP"), ("MA","V"), ("XOM","CVX")` → deduped + sorted leg set used for
subscriptions: `CVX, KO, MA, PEP, V, XOM`.
**Pair legs are NOT excluded from the SEPA universe** — only SPY + the 11
sector ETFs are. A pair leg that ranks in the SEPA
top-N appears in both lists; the per-ticker `bar_type_by` map dedups by key, but `build_all` receives the lists separately.

The exclusion happens **before** the market-cap cap is applied, so the limit
budget is spent entirely on stocks.

 Failure policy: each load step (SPY warmup, universe, SF1,
earnings) is independently try/except'd; any failure prints a WARN and
degrades (e.g. universe failure → empty SEPA universe), never aborts. The same applies to Sharadar auto-catchup
(; enabled by default, disabled by `--no-sync` or
env `TMS_AUTO_SYNC=0` — any other value, including unset, enables it,).

### 4.2 `TMS_LIVE_UNIVERSE_LIMIT` resolution

Source:; tests.

 `_resolve_universe_limit`:

| Env state | Result |
|---|---|
| unset, empty, or whitespace-only | default **85** |
| parseable integer (after `strip`) | that integer (negative/zero allowed; handled downstream) |
| non-integer | error: `TMS_LIVE_UNIVERSE_LIMIT must be an integer, got {raw!r}` — **fail fast at startup**, no silent fallback (, test `:93-98`) |

Rationale for 85: moomoo OpenD caps one account at
**100 simultaneous K-line subscriptions**; SPY + sector ETFs + pair legs take
~15, leaving 85 for SEPA stocks. (The comment says "sector ETFs (10)" but the
actual list has 11 — see Open questions.)

### 4.3 `_apply_universe_limit` — top-N by market cap

Source:; tests.

 `_apply_universe_limit(sepa_tickers, market_cap_lookup,
limit)`:

1. `limit <= 0` **or** empty input → empty tuple (;
 tests `:26-36`). Negative limits are valid input (defensive) and yield
 empty.
2. `len(input) <= limit` → return input **unchanged, original order
 preserved** — no reshuffling (; tests `:39-49`).
3. Otherwise: sort by `market_cap_lookup(t)` **descending** and take the
 first `limit`.

**Ordering details:**

- The sort is a **stable** descending sort.
 Ties in market cap (including all unknown tickers at `0.0`) retain their
 relative order from the input, which at this call site is lexicographic
 ascending (from §2.2). Go must use a stable sort (`sort.SliceStable` or
 equivalent) keyed on cap descending to reproduce identical output for
 tied caps.
- Unknown tickers (`lookup == 0.0`) therefore sort **last** and are excluded
 whenever at least `limit` tickers have positive caps
 (, test `:66-76`).
- The returned tuple is in **cap-descending order**, not re-sorted
 alphabetically (test `:52-63` asserts `("MSFT","AAPL","NVDA")`).

 Wiring: the limit is applied to
the post-exclusion stock list using
`_market_cap_lookup_factory(cache)` (§2.3); the log line reports the raw
count and post-limit count. Then `all_tickers = ("SPY",) + sepa_tickers +
sector_tickers + pair_tickers` defines the subscription set; each ticker maps
to bar type `"{SYMBOL}.{MOOMOO_VENUE}-1-DAY-LAST-EXTERNAL"`.

 Original: the cap is a blunt market-cap ranking ("ignore the
rest until live universe is refined offline — V3-D follow-up",); the screener's own quality score is ignored at
subscription time, and `len <= limit` pass-through means order semantics
differ between the two branches (input order vs cap-descending). Proposed Go
improvement: optionally rank by screener score once warm state exists, and/or
always emit cap-descending for a single deterministic ordering. Default must
remain reference behavior.

### 4.4 Downstream consumers of the capped universe

 The capped `sepa_tickers` drives (a) SF1 MRT bulk load —
pyarrow dataset filtered to `ticker ∈ list` AND `dimension == "MRT"`, columns
`ticker, datekey, dimension, marketcap`
(, called at); (b) earnings load — EVENTS rows whose pipe-split
`eventcodes` contains the literal code `"22"`, with column `date` renamed to
`report_date` (, called at); (c) the screener instance built with the same
memoized market-cap lookup.

 Backtest assembly: the backtest path uses the same exclusion rule
`_non_stock = {"SPY", *DEFAULT_UNIVERSE}` on the `table="SF1"` window universe,
but applies **no** top-N cap (no subscription quota in backtest). The backtest
path likewise must not cap.

### 4.5 Runner-side caps that interact with the universe

Source: (context for completeness; the runner
itself is another spec's scope).

- `active_cap = 20` (U-D2), `subscription_cap = 30` (U-D3).
- Cap enforcement (U-D12): while
 `|active_set ∪ holdings| > subscription_cap`, evict the holding (never an
 active_set member) with the **lowest** `trend_template_count`, tie-broken
 by symbol **ascending**, by submitting a market flatten order; if nothing
 is evictable, log a warning and stop.

---

## 5. Parameter summary

| Parameter | Default | Unit | Source |
|---|---|---|---|
| `TMS_LIVE_UNIVERSE_LIMIT` | 85 | tickers |
| moomoo subscription hard cap (external) | 100 | K-line subs/account |
| Warmup window | 730 (`2*365`) | calendar days |
| Universe table filter | `"SF1"` | — |
| Excluded: heartbeat | `SPY` | — |
| Excluded: sector ETFs (11) | `XLK XLF XLE XLV XLY XLP XLU XLB XLI XLRE XLC` | — |
| Pair legs (subscribed, NOT excluded) | `CVX KO MA PEP V XOM` | — |; |
| `history_max_bars` | 260 | bars |
| Breakout lookback | 60 | bars |
| 52w high/low window | 252 | bars |
| High tolerance (rule 6) | 0.25 | fraction |
| Low premium (rule 7) | 0.30 | fraction |
| Market-cap min (rule 8) | 500,000,000.0 | USD |
| Score weights | TT×10.0 + proximity×5.0 | — |
| `active_cap` / `subscription_cap` | 20 / 30 | tickers |
| RTH window | 09:30–16:00 inclusive | ET minutes-of-day 570–960 |
| Aggregation timezone | `America/New_York` | IANA |
| Catch-up target | T-1 | UTC date |
| `TMS_AUTO_SYNC` | `"1"` (on; `"0"` disables) | — |

---

## 6. Open questions

1. **Sector-ETF count discrepancy in the quota comment.** The 85 default is
 justified as "SPY + sector ETFs (10) + pair legs (~10) take ~15", but `DEFAULT_UNIVERSE` has **11** ETFs and there
 are exactly **6** unique pair legs, so the fixed instruments total 18
 (or fewer after `bar_type_by` dedup), leaving 82 < 85 headroom under the
 100-sub cap if no overlap occurs. Should Go keep 85 verbatim (recommended:
 yes — it's an operator-tunable default) or recompute headroom dynamically?
2. **Market-cap lookup ignores SF1 dimension.** `_market_cap_lookup_factory`
 takes the latest row by `datekey` across **all** dimensions, while `_load_sf1_mrt` filters to
 `MRT` only. If multiple dimensions
 share the max `datekey`, `sort_values` stability makes "which row is
 `iloc[-1]`" depend on the parquet row order. Is dimension-agnostic
 intentional, and should Go pin a deterministic secondary key (e.g.
 dimension) — or filter to MRT?
3. **Pair legs intentionally not excluded from SEPA?** `KO/PEP/MA/V/XOM/CVX`
 are large caps that will routinely rank inside the top-85, so SEPA can
 trade names that the pairs strategy is simultaneously long/short. Confirm this overlap is accepted (it also
 slightly relieves the subscription quota via dedup).
4. **Catch-up start day re-fetch.** `start = last_sep.date` re-fetches the
 already-synced day every startup. Idempotent but wastes
 two API calls; should Go start at `last_sep.date + 1`? (Behavior change —
 default should match reference.)
5. **Pre-open boundary uses the UTC date of bar timestamps**. For 1-DAY bars whose
 `ts_event` is 16:00 ET (20:00/21:00 UTC) this is safe, but a bar
 timestamped after 19:00/20:00 ET would roll the UTC date early. Confirm Go
 may keep UTC-date semantics (required for determinism) and document the
 assumption that daily bars are stamped at RTH close.
6. **`warmup_start` uses calendar days, not trading days** (`2*365`,) — yields ~500 trading days, comfortably above the
 252-bar requirement. Confirm no need to align with `history_max_bars`.
7. **Half-day sessions** (e.g. day after Thanksgiving, Christmas Eve): the
 aggregator treats 13:00-ET closes as ordinary days and waits for bars
 until 16:00; the daily bar is only emitted on the next day's first RTH bar. Acceptable for Go v1, or implement the
 early-close table from §1.1?
8. **`date.today` in live universe assembly** is the
 machine-local date, while catch-up freshness uses UTC. On a US-evening start these can differ. Confirm Go should preserve
 the inconsistency, or normalize both to one zone behind a flag.

---

## 7. Go implementation addendum (P1, internal/data/universe)

Decisions locked for the Go port; each either resolves an open question
above or documents a sanctioned deviation.

1. **Trading-date normalization (resolves Q8).** All "today"
 logic in universe assembly uses the **America/New_York calendar date**
 of the injected clock (`calendar.DateOf(now, NY)` via
 `internal/data/calendar`), not the machine-local date of
 and not the UTC date of. The
 reference's local/UTC mix is intentionally NOT preserved; on a US
 trading evening the NY date equals the local US date, so reference
 comparisons on historical fixtures are unaffected (the golden tests pin
 `as_of` explicitly).
2. **Market-cap datekey tie-break (resolves Q2).** The Go lookup is
 `ORDER BY datekey DESC, dimension DESC LIMIT 1` over all six SF1
 dimensions. SF1 parquet rows
 are sorted by `(ticker, datekey, dimension)`,
 so the stable `sort_values("datekey").iloc[-1]` selects the greatest
 dimension among max-datekey rows.
3. **Default limit stays 85 verbatim (resolves Q1, Q3).** No dynamic
 headroom computation; pair legs remain NOT excluded from the SEPA
 universe — `KO/PEP/MA/V/XOM/CVX` may rank inside the top-N (overlap
 accepted, dedup happens at subscription time).
4. **Data source.** The universe tier reads the P0-imported TimescaleDB
 tables (`tms.tickers`, `tms.bars_daily`, `tms.fundamentals_sf1`)
 instead of the parquet cache; the SF1-driven flow's filter/sort
 semantics (§2.2, §2.3) are replicated in SQL. Fixed-point 1e-4 price
 storage round-trips losslessly for all observed Sharadar OHLC values
 (golden-verified bit-identical scores through PG).
5. **Snapshots.** Every computed universe is persisted to
 `tms.universe_snapshots` (migration 000007 adds a ranked `members`
 JSONB array: rank, score, trend_template_count, breakout_proximity,
 market_cap_usd, passing-rule reasons) with reader APIs
 (`SnapshotByID`, `LatestSnapshot`, `SnapshotAsOf`). This is new in this repo.
6. **Float determinism notes.** The trend-template MAs use a rolling-mean
 kernel (Kahan add/remove compensation, same-value short circuit, sign-count
 clamps) and the screener score forces per-term IEEE rounding (no FMA
 contraction) — both required for bit-identical scores across platforms
 (arm64 vs x86). Verified by
 `internal/data/universe/testdata/universe_golden.json` over the 48-ticker
 P0 subset (as_of 2026-05-27).
7. **Error policy.** Infrastructure failures (DB/query errors) return
 errors to the caller; the warn-and-continue path (§4.1) is
 applied to per-ticker warmup failures only (logged, recorded in the
 build result, ticker skipped).
