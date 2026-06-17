# Portfolio & Risk Layer — Implementation Spec

This repo's definition of the portfolio and risk layer. The rules below are
invariants of this system: formulas, comparison operators (strict vs inclusive),
evaluation order, edge cases, defaults, and string formats where consumed
downstream. Where a known weakness is called out, the better behavior this repo
adopts is described alongside it.

Layer overview: four modules + facade —
 (per-strategy capital budget), (account-wide hard rules),
 (facade + health snapshot), (EOD strategy-books vs broker-net
diff), plus / (shared slow-moving context) and three
context Actors + one health Actor.

---

## 1. Core value types

All four are immutable value types with zero trading-engine dependency: plain structs, never mutated after
construction.

### 1.1 SignalSide

 String enum with exactly three values:

| Value | Wire string | Meaning |
|---|---|---|
| `LONG` | `"LONG"` | open/add long |
| `FLAT` | `"FLAT"` | close order — bypasses all risk gates |
| `SHORT` | `"SHORT"` | open/add short |

### 1.2 ProposedOrder

| Field | Type | Semantics |
|---|---|---|
| `strategy_id` | string | proposing strategy |
| `symbol` | string | bare ticker (e.g. `"AAPL"`) |
| `side` | SignalSide | direction; qty is unsigned |
| `qty` | int | **absolute magnitude (positive); side encodes direction** |
| `price` | Decimal | estimated fill price used for sizing math |
| `ts` | datetime (UTC) | bar timestamp, "for daily-loss halt windowing" — note: **`ts` is never read by any rule in the current code** (see Open questions) |

### 1.3 RiskDecision

| Field | Type | Default |
|---|---|---|
| `approved` | bool | — |
| `rule_name` | string | `""` |
| `reason` | string | `""` |

 Constructors: `approve` → `{true, "", ""}`; `reject(rule, reason)` →
`{false, rule, reason}` (, verified).

### 1.4 AccountSnapshot

Read-only point-in-time account view. Conventions:

| Field | Type | Semantics |
|---|---|---|
| `nav` | Decimal | total account value = cash + market value of positions |
| `cash` | Decimal | (carried but never read by any rule) |
| `realized_pnl_today` | Decimal | day realized P&L |
| `unrealized_pnl_today` | Decimal | day unrealized P&L |
| `positions` | map[(strategy_id, symbol)]int | **signed** share count: + long, − short, 0/missing = flat. Default empty map |
| `last_close` | map[symbol]Decimal | last close price per symbol. Default empty map |

Helper methods — all:

- `total_pnl_today = realized_pnl_today + unrealized_pnl_today` (,
 test).
- `strategy_position(sid, sym)` → `positions[(sid,sym)]`, **0 if missing** (,
 test).
- `net_position_across_strategies(sym)` → sum of signed qty over all keys whose symbol matches;
 0 if none (, test: `{SEPA:+100, Pairs:-40}` → 60).
- `gross_exposure_for_strategy(sid)` → `Σ |qty| * last_close[sym]` over this strategy's
 non-zero positions (, test: SEPA `100*150+50*400=35000`;
 Pairs with a short `100*60+80*180=20400`; unknown strategy → 0). Gross, not net — "gross is
 what consumes margin / capacity".
 - Positions with `qty == 0` are skipped.
 - **Missing-price fallback**: a symbol absent from `last_close` contributes
 `|qty| * 0 = 0`, silently UNDER-counting exposure and making the budget
 check more permissive than reality. Original: silent zero. Go improvement: keep the zero
 fallback, but emit a structured WARN log (`symbol`, `strategy_id`,
 `"missing last_close; exposure undercounted"`) whenever the fallback fires.

---

## 2. Allocator

### 2.1 StrategyAllocation

| Field | Type | Constraint |
|---|---|---|
| `strategy_id` | string | unique within the table |
| `capital_pct` | float64 | in `(0, 1]` |

### 2.2 Constructor validation

 In this exact order, fail-fast with errors whose messages contain the quoted
substrings (tests match on them,):

1. Empty list → error containing `"at least one"`.
2. Per entry, in input order: duplicate `strategy_id` → error containing
 `"duplicate strategy_id"`; then `capital_pct` outside `(0, 1]`
 (i.e. `pct <= 0 || pct > 1.0`) → error containing `"capital_pct"`.
 The duplicate check runs BEFORE the pct check for each entry.
3. After the loop: `Σ capital_pct > 1.0 + 1e-9` → error containing `"sum"`. The `1e-9` epsilon is float-arithmetic slack — the
 tolerance value. Sums `< 1.0` are intentional slack (cash buffer,;
 test).

### 2.3 Budget accessors

- `budget_pct(strategy_id)` → table value; **`0.0` for unregistered** (,
 test).
- `budget_dollars(strategy_id, account)` = `account.nav * Decimal(str(budget_pct))`.
 The float is converted via its **shortest decimal string repr** before Decimal
 multiplication (`Decimal(str(0.4))` == exact `0.4`, NOT the binary float expansion
 `0.4000000000000000222...`). In Go: store/convert `capital_pct` through
 `strconv.FormatFloat(p, 'g', -1, 64)` → decimal, or store the pct as a decimal from the start.
 Test fixture: nav 100000 × 0.4 → `40000.0`; nav 250000 × 0.4 → `100000.0`.

### 2.4 check_order_within_budget

 Exact decision sequence:

1. `side == FLAT` → **approve** (closing reduces exposure;, test
 — note the FLAT test order also has `qty=0`).
2. `qty <= 0` → approve.
3. `budget = budget_dollars(strategy_id)`; if `budget <= 0` → reject with
 `rule = "allocator.unregistered_strategy"`,
 `reason = "strategy_id '{sid}' has no allocation"` (, test).
4. `current_gross = account.gross_exposure_for_strategy(strategy_id)` (§1.4);
 `order_value = Decimal(qty) * price`; `new_gross = current_gross + order_value`.
5. Reject iff `new_gross > budget` (**strict**: exactly-at-budget passes) with
 `rule = "allocator.budget_exceeded"` and reason format
 `"{sid} gross exposure ${new_gross:,.2f} would exceed budget ${budget:,.2f} (current ${current_gross:,.2f}, order ${order_value:,.2f})"`
 — `:,.2f` = thousands separators + 2 decimals. Otherwise approve.

 Independence: one strategy's exposure never counts against another's budget
(test). Existing exposure DOES count against own budget
(test: held $30k of $40k + $15k order → reject).

 Note the order value always ADDS to gross regardless of side — a SHORT open order
also increases gross (`order_value = qty*price`, qty positive). Reason strings are operator-facing
log text; Go must keep the rule names byte-identical (`allocator.unregistered_strategy`,
`allocator.budget_exceeded`); reason text should be semantically identical (same numbers, same
units) — exact byte equality of reasons matters only where tests compare them.

---

## 3. RiskConstraints

Three account-wide hard rules. Doc comment: max_position is
**GROSS per-strategy**; concentration is **NET cross-strategy** ("Pairs has high gross + zero
net, so it sails through concentration but is bounded by max_position").

### 3.1 RiskConstraintsConfig

| Param | Library default | **Production value** | Meaning |
|---|---|---|---|
| `max_single_name_pct` | 0.20 | **0.50** | one strategy's gross $ in one symbol ≤ pct·NAV |
| `concentration_pct` | 0.30 | **0.40** | cross-strategy NET $ in one symbol ≤ pct·NAV |
| `daily_loss_halt_pct` | 0.05 | **0.10** | halt all new orders when day P&L < −pct·NAV |

 Validation: each value must be in `(0, 1]`; otherwise error message
`"{name} must be in (0, 1], got {v}"` (, tests match on the field
name,). `RiskConstraints` with nil config uses
the library defaults.

### 3.2 check — rule order, first-rejection-wins

 Exact sequence:

0. `side == FLAT` **or** `qty <= 0` → approve. FLAT bypasses ALL rules **including the daily
 loss halt** — "even when daily loss is hit, we want closes to fire (so stops can work)"
 (; tests,).
1. **daily_loss_halt** (most restrictive — halts everything; supersedes other rules even for a
 1-share order, test).
2. **max_single_name**.
3. **concentration**.
4. Approve.

### 3.3 Rule 1 — daily_loss_halt

```
threshold = -nav * Decimal(str(daily_loss_halt_pct)) # negative number
pnl = realized_pnl_today + unrealized_pnl_today
reject iff pnl < threshold # STRICT less-than
```

 Strict `<`: P&L exactly AT −pct·NAV does **not** halt (boundary test: −5000 on 100k at 5% → not halted;
−6000 → halted,).
Rule name `"risk.daily_loss_halt"`, reason format
`"day P&L ${pnl:,.2f} is below halt threshold ${threshold:,.2f} ({pct:.1%} NAV)"`
(`:.1%` = percent with 1 decimal, e.g. `5.0%`).

### 3.4 Rule 2 — max_single_name (gross, per strategy)

```
held_qty = |positions[(order.strategy_id, order.symbol)]| # 0 if missing
held_price = last_close[order.symbol] if present, else order.price # FALLBACK TO ORDER PRICE
held_value = held_qty * held_price
new_value = held_value + order.qty * order.price
cap = nav * Decimal(str(max_single_name_pct))
reject iff new_value > cap # STRICT
```

 Notes:
- Only THIS strategy's position in THIS symbol counts (gross via `abs`).
- The held-value price fallback differs from the Allocator's (which falls back to 0, §1.4):
 here missing `last_close` falls back to `order.price`.
- Side is irrelevant: a SHORT add also increases `new_value` (qty positive).
- Rule name `"risk.max_single_name"`, reason
 `"{sid} {sym} gross ${new_value:,.2f} would exceed single-name cap ${cap:,.2f} ({pct:.1%} NAV)"`.
- Tests: order alone over cap, held + new over cap
 (`:138-151`: 100@150 held + 50@150 = 22.5k > 20k cap → reject).

### 3.5 Rule 3 — concentration (net, cross-strategy)

```
current_net = Σ over all strategies of signed qty in order.symbol
signed_qty = +order.qty if side == LONG else -order.qty # SHORT → negative
new_net = current_net + signed_qty
new_net_value = |new_net| * order.price # ENTIRE net valued at ORDER price
cap = nav * Decimal(str(concentration_pct))
reject iff new_net_value > cap # STRICT
```

 `signed_qty` branch is literally `qty if LONG else -qty`
 — FLAT can never reach this rule (filtered in step 0), so the
`else` arm only ever means SHORT.
Rule name `"risk.concentration"`, reason
`"net {sym} across all strategies = {new_net} shares (${new_net_value:,.2f}) would exceed concentration cap ${cap:,.2f} ({pct:.1%} NAV)"`.
Tests: two strategies long same name → reject (: 130 held +
100 new = 230·150 = 34.5k > 30k); market-neutral short hedge passes (`:176-194`: 130 long +
100 short → net 30 → 4.5k).

 **Valuation price**: the entire `new_net` (including the previously-held shares) is
valued at `order.price`, not at `last_close` — inconsistent with rule 2 which uses `last_close`
for held shares. Original: held shares re-priced at the order's estimated fill price. Go
improvement: keep the original formula as the default, and add an optional
(config-gated, default off) mode valuing held net at `last_close[sym]` (with order-price
fallback) + the order delta at `order.price`. Document divergence in the decision reason when
the improved mode is active.

---

## 4. Portfolio facade

### 4.1 check pipeline

 Composition order (documented "for determinism in tests",):

1. `allocator.check_order_within_budget(order, account)` — first rejection wins.
2. `risk_constraints.check(order, account)` — first rejection wins.
3. Approve.

Effective full rule order seen by a caller therefore is:
`allocator.unregistered_strategy` / `allocator.budget_exceeded` → `risk.daily_loss_halt` →
`risk.max_single_name` → `risk.concentration`. Verified: order over BOTH budget and single-name
cap reports `allocator.budget_exceeded`; within
budget but over single-name reports `risk.max_single_name` (`:95-105`).

### 4.2 PortfolioHealthSnapshot

Frozen value type — all fields Decimal except the bool:

| Field | Type | Definition |
|---|---|---|
| `day_pnl` | Decimal | `realized_pnl_today + unrealized_pnl_today` |
| `day_pnl_pct` | Decimal | `day_pnl / nav` if `nav > 0` else `0` |
| `daily_loss_halt` | bool | `day_pnl < -nav * halt_pct` (same strict `<` as §3.3) |
| `halt_headroom_pct` | Decimal | `0` if halted, else `(day_pnl - threshold) / nav` if `nav > 0` else `0` |
| `concentration_pct` | Decimal | largest `|net_qty| * last_close` / NAV across symbols (0 if `nav <= 0`) |

### 4.3 health_snapshot algorithm

 Pure function — mutates nothing. Exact steps:

1. `day_pnl = account.total_pnl_today`;
 `day_pnl_pct = day_pnl / nav` when `nav > 0`, else `Decimal(0)`.
2. `threshold = -nav * Decimal(str(config.daily_loss_halt_pct))`;
 `halted = day_pnl < threshold` (strict, mirrors §3.3).
3. `headroom`: if halted → `0` (clamped, never negative —); else `(day_pnl - threshold) / nav` when
 `nav > 0` else `0`. Headroom is a positive fraction of NAV; e.g. pnl=+100, threshold=−5000,
 nav=100000 → `0.051` (asserts `Decimal("0.0510")`
 — numeric equality, trailing zeros irrelevant).
4. `concentration_pct`: when `nav > 0`, over the set of distinct symbols appearing in
 `positions` keys: skip symbols with net 0; `value = |net| * last_close.get(sym, 0)`
 (missing price → 0 contribution); `pct = value / nav`; keep the max. NET not gross: +100/−100 pairs-style → 0; example: AAPL net 150@200=30k,
 NVDA 100@500=50k, nav 200k → `0.25` (`:101-114`).

Decimal precision: the default decimal context (28 significant digits) governs the
divisions. In Go use a decimal library (e.g. shopspring/decimal) with ≥28-digit division
precision; comparisons in tests are numeric, not string-based.

---

## 5. PortfolioHealthActor

Live-mode-only publisher of `PortfolioHealthUpdate` at **minute cadence** (cockpit's primary
risk signal; coarser cadence is explicitly wrong —).

### 5.1 Wiring


- Registered only in live mode; backtest gets nothing.
- Subscribes to the SPY **1-MINUTE-LAST-EXTERNAL** bar derived from the daily SPY bar type's
 instrument id: `"{instrument_id}-1-MINUTE-LAST-EXTERNAL"`.
- The `Portfolio` reference is passed OUTSIDE the serializable config (a
 msgspec encoding limitation,; in Go this is just a constructor
 dependency).
- Production Portfolio it reads: allocations SEPA 0.40 / SectorRotation 0.30 / Pairs 0.20
 (0.10 cash slack) + risk config 0.50/0.40/0.10; the same
 Portfolio instance is attached to every strategy runner via `set_portfolio_service`.

### 5.2 Per-bar algorithm

 For each 1-MIN SPY bar:

1. `nav = _read_nav` (one read per bar; tests inject a NAV sequence,).
2. `bar_date = UTC calendar date of bar.ts_event` (ts_event is int nanoseconds since epoch;
 conversion is `ts_ns / 1e9 → UTC datetime → date`,). **Timezone: UTC**,
 so the trading "day" boundary is midnight UTC, not exchange local time (see Open questions).
3. Day-boundary reset: if no baseline yet OR `bar_date != date_at_open` →
 `nav_at_open = nav; date_at_open = bar_date`. First bar of a new
 day therefore always publishes `day_pnl = 0`.
4. `baseline = nav_at_open or nav`.
 A falsy-`or` treats `Decimal(0)` as falsy: if the day opened with NAV exactly 0
 (signal mode), baseline silently becomes the CURRENT nav and day_pnl pins to 0 even after
 capital appears intraday. Original: falsy-zero fallback. Go improvement: use an explicit
 `if navAtOpen == nil` nil-check only (do NOT replicate the zero-is-falsy coercion), and log
 when `nav_at_open == 0`. This changes behavior only in the degenerate zero-NAV-open case.
5. `day_pnl = nav - baseline`.
6. Build an AccountSnapshot: `nav=nav, cash=nav, realized_pnl_today=day_pnl,
 unrealized_pnl_today=0, positions=_read_positions, last_close=copy(_last_close)`. `_last_close` starts empty and is never populated in this MVP →
 `concentration_pct` is always 0 from the actor; accepted tradeoff documented at. Original: concentration permanently 0 in the live feed.
 Go improvement: optionally feed `last_close` from the quote/bar stream so the published
 concentration is real; keep field semantics identical.
7. `health = portfolio.health_snapshot(snapshot)` (§4.3). Note `day_pnl_pct` therefore equals
 `day_pnl / CURRENT nav` (not baseline nav) — test asserts `500/100500`.
8. Publish `PortfolioHealthUpdate` with all Decimal fields converted to float64, and
 `ts_event = ts_init = bar.ts_event`. Published **on every bar
 unconditionally** (no dedup/transition gating, unlike the context actors).

### 5.3 Engine reads


- `_read_nav`: venue account's total balance in USD; **no account registered (signal mode) →
 NAV = 0**.
- `_read_positions`: aggregate open positions by `(strategy_id, symbol)` summing signed qty,
 skipping zero — identical aggregation to
 the snapshot builder (§8.1).

### 5.4 PortfolioHealthUpdate payload

| Field | Type | Notes |
|---|---|---|
| `day_pnl` | float64 | signed dollars |
| `day_pnl_pct` | float64 | fraction (0.007 = 0.7%) |
| `daily_loss_halt` | bool | strict-`<` rule, §3.3 |
| `halt_headroom_pct` | float64 | fraction, clamped ≥ 0 |
| `concentration_pct` | float64 | fraction |
| `ts_event`, `ts_init` | int64 | nanoseconds since epoch (UTC) |

Primitive floats are a wire-serializer constraint; "Decimal precision is restored at the API
boundary via str-encoded schema".

### 5.5 REST exposure (, `src/api/schemas/…`)

`GET /api/live/portfolio-health` returns the latest stream entry of
`data.PortfolioHealthUpdate`:

```json
{
 "day_pnl": "…", "day_pnl_pct": "…", // stringified decimals
 "daily_loss_halt": false,
 "halt_headroom_pct": "…", "concentration_pct": "…",
 "ts_event": 1714572600000000000, // int ns
 "last_update_age_ms": 1234 // max(0, (now_ns - ts_event) // 1e6)
}
```

 503 with detail `"PortfolioHealthUpdate stream is empty — no live producer
running"` on cold start; 502 on malformed payload.

---

## 6. Reconciliation

EOD check: `sum(strategy books) == broker net?` Pure data module — runner supplies both sides.

### 6.1 Report shape

`Mismatch`:

| Field | Type | Definition |
|---|---|---|
| `symbol` | string |
| `strategy_books_sum` | int | signed sum across strategies |
| `broker_net` | int | signed broker view |
| `diff` | int | **`broker_net - strategy_books_sum`** (sign matters; test: books 100, broker 95 → diff **−5**) |
| `diff_shares` (derived) | int | `|diff|` |

`ReconciliationReport`:

| Field | Type |
|---|---|---|
| `ts` | datetime (UTC) |
| `matched` | []string | symbols with diff within tolerance |
| `mismatches` | []Mismatch |
| `symbols_only_in_strategies` | []string | we claim, broker shows zero |
| `symbols_only_at_broker` | []string | broker shows, no strategy claims |
| `has_issues` (derived) | bool | any of the three lists non-empty |

`summary` text format (tests match substrings `"OK"`, `"Mismatches"`, `"diff"`,):

- Clean: `"Reconciliation OK ({N} symbols matched)"`.
- Issues: first line `"Reconciliation report @ {ts.isoformat}"` (RFC3339-ish,
 e.g. `2024-06-28T21:00:00+00:00`); then, if any mismatches, a line
 `" Mismatches ({N}):"` followed per mismatch by
 `" {sym}: strategies sum {s:+d}, broker {b:+d}, diff {d:+d}"` (`%+d` = always-signed int);
 then, if any, `" Strategies claim positions, broker shows zero: {comma-joined syms}"`;
 then, if any, `" Broker shows positions, no strategy claims them: {comma-joined syms}"`.
 Lines joined with `\n`.

### 6.2 reconcile algorithm

Inputs: `ts`, `strategy_books map[(sid,sym)]int` (signed), `broker_positions map[sym]int`
(signed), `tolerance_shares int = 0`.

 Exact steps:

1. Aggregate strategy books per symbol, summing signed shares, **skipping entries with
 `qty == 0`** ("a 0-share entry is not a claimed position",, test).
2. `all_symbols = keys(strategy_sums) ∪ keys(broker_positions)`, iterated in **sorted
 (lexicographic ascending) order** — output list ordering is deterministic.
3. Per symbol, with `s_sum = strategy_sums.get(sym, 0)`, `b_net = broker_positions.get(sym, 0)`,
 `diff = b_net - s_sum`, classify with this PRIORITY (first match wins):
 1. `s_sum != 0 && b_net == 0` → `symbols_only_in_strategies` (test `:57-64` — note a broker
 entry explicitly reporting 0 still counts as "broker zero").
 2. `s_sum == 0 && b_net != 0` → `symbols_only_at_broker` (test `:67-74`).
 3. `|diff| <= tolerance_shares` → `matched` (inclusive ≤; tolerance absorbs tiny diffs,
 test `:91-99`: books 100, broker 99, tol 2 → matched). A symbol the broker reports as 0
 with no strategy claim (s_sum 0, b_net 0) lands here → appears in `matched`
 (implied by test `:57-64` where `MSFT: 0` produces no issue).
 4. else → `mismatches` with the Mismatch fields above.
4. Signed sums work for pairs: `+100 KO` and `−40 KO` across two books vs broker `+60` → match
 (test `:43-54`).

### 6.3 Live moomoo bridge

 (consume in the broker adapter spec; summarized here because the report shape is
shared):

- `strategy_books_from_cache`: open positions only; sum signed qty per `(strategy_id, symbol)`
 defensively; skip zero.
- `broker_positions_from_moomoo(df)`: empty/None frame → `{}`; missing `position_side` column
 on a non-empty frame → **error** (never default-to-LONG — "defaulting on missing columns
 defeats" reconciliation, `:73-89`); per row: strip leading `"US."` from `code` to bare
 symbol; non-parsable or zero `qty` → skip row; `position_side == "SHORT"`
 (case-insensitive) → negate qty; sum per symbol (`:91-116`).
- `reconcile_with_broker(...)`: books from cache + positions from broker →
 `reconcile(ts=ts or now(UTC), …, tolerance_shares)` (`:119-145`). Caller handles
 `has_issues` (log / alert / halt) — the module never acts on it.

---

## 7. Shared context: state, refresher functions, three Actors

### 7.1 SharedContextState

Single mutable in-process store consulted by every strategy runner:

| Field | Type | Default |
|---|---|---|
| `regime` | string | `"neutral"` |
| `market_cap` | map[string]Decimal | empty |
| `earnings_blackout` | map[string]bool | empty |

 Module-level singleton; all consumers observe the same mutations
(, tests — defaults,
identity across imports, mutation propagation, wholesale dict replacement must all work).

 **Not thread-safe** — explicitly "Not thread-safe. The event loop is
single-threaded so this is fine". Original: bare struct.
Go improvement: the Go system is concurrent — guard with `sync.RWMutex` (or expose an
atomic-snapshot accessor). Semantics (last-writer-wins per field, sole-writer-per-field
convention §7.4) unchanged.

Sole-writer convention: RegimeActor is the sole writer of `regime`, FundamentalsActor of `market_cap`, EarningsActor of `earnings_blackout`.

### 7.2 compute_regime

Constants:

| Constant | Value |
|---|---|
| Labels | `"bull"`, `"bear"`, `"neutral"`, `"warning"` |
| `_REGIME_MIN_BARS` | 200 |
| `_REGIME_SLOPE_WINDOW` | 30 |
| `_REGIME_SLOPE_FLAT_PCT` | 0.0 |
| `EARNINGS_BLACKOUT_DAYS` | 5 |

 Input: SPY daily history with a `close` column (float). Optional `as_of` date
filters to bars with date ≤ as_of BEFORE any computation (look-ahead prevention —; behavioral test: same frame, early as_of → bear, late
as_of → bull). Date source: DatetimeIndex if present, else a `date` column.

Classification, exact order:

1. nil frame OR `< 200` rows (after as_of filter) → `"neutral"` (`:75-76`, test `:67-69,97-98`).
2. `ma200 =` simple rolling mean of close, window 200, requiring 200 points (first 199 entries
 are NaN). `last_close = close[-1]`, `last_ma = ma200[-1]`.
3. `last_ma` is NaN → `"neutral"` (`:83-84`). (Unreachable when len ≥ 200 and closes are
 non-NaN, but as a NaN guard.)
4. `last_close < last_ma` (**strict**) → `"bear"` (`:86-87`). Equality falls through to the
 bull/warning branch (close == MA is treated as "above", test `:84-94`).
5. `len(ma200) < 31` (i.e. `< _REGIME_SLOPE_WINDOW + 1` rows total) → `"warning"`
 (conservative, `:92-94`). Note: with exactly 200–230 bars, `ma_then` below is NaN → also
 warning; the "exactly-200 edge" is why RegimeActor buffers 280 bars (§7.5).
6. `ma_then = ma200[-31]` (the MA value 30 positions before last). NaN or 0 → `"warning"`
 (`:96-98`).
7. `slope_pct = (last_ma - ma_then) / ma_then`; `> 0.0` (strict) → `"bull"`, else
 `"warning"` (`:100-103`; flat plateau → slope 0 → warning, test `:84-94`).

Units: slope is a unit-free fraction over the 30-bar window (the `/30` per-day normalization
mentioned in the comment is mathematically absorbed; implement exactly the formula above).

### 7.3 load_sf1_market_caps

Latest known market cap per ticker as of a date, from a SHARADAR/SF1-shaped table
(`ticker`, `datekey` = filing date, `marketcap`, optional `dimension`).

 Steps:

1. nil/empty input → empty map (test).
2. If a `dimension` column exists, keep only rows with `dimension == param`
 (default `"MRT"` — most-recent trailing twelve months); empty after filter → empty map.
 No `dimension` column → no filter (caller pre-filtered, test `:203-211`).
3. Coerce `datekey` to a calendar date; keep rows with `datekey ≤ as_of` (inclusive; rows after
 as_of ignored, test `:156-169`).
4. Drop rows with null `marketcap`; drop rows with `marketcap <= 0` (test `:214-223`).
5. Empty → empty map. Else **stable-sort by datekey ascending** and take the LAST row per
 ticker (ties on datekey: last in original input order wins — stable sort + tail semantics).
6. Value conversion: `Decimal(str(marketcap_float))` — float → shortest-repr string → Decimal
 (e.g. `2.7e12` → `Decimal("2700000000000.0")`, test `:156-169`). In Go: parse
 `strconv.FormatFloat(v,'g',-1,64)` into the decimal type. Tickers with no qualifying rows
 are absent from the map (test `:172-182`).

### 7.4 load_earnings_calendar

Earnings blackout flag per ticker from a `(ticker, report_date)` table (SHARADAR/EVENTS rows
filtered to earnings, eventcode `"22"` in the pipe-separated `eventcodes` column — filtering is
the CALLER's job,).:

1. nil/empty frame → empty map. Missing `ticker` or the date column → empty map (silent;
 `:168-171`).
2. Date column name defaults to `"report_date"`; caller may override (e.g. `"date"` for raw
 EVENTS frames, test).
3. Window: ticker is in blackout iff ANY of its earnings dates `d` satisfies
 `as_of − N ≤ d ≤ as_of + N` **calendar days, inclusive both ends**, with
 `N = blackout_days` (default 5; `:173-178`). Tests: 3 days past → in (`:237-241`);
 10 days past → out for N=5, in for N=14 (`:244-248,258-264`); 4 days future → in
 (`:251-255`).
4. Output contains ONLY `true` entries — tickers not in blackout are ABSENT, never `false`
 (`:180-183`; consumers interpret absence as false, §7.7).

### 7.5 RegimeActor

Subscribes to **SPY 1-DAY bars**; sole writer of `shared_state.regime`; publishes
`RegimeUpdate` on transitions only.

Config: `spy_bar_type` (daily); `history_max_bars = 280`
(200 warmup + 30 slope + ~50 cushion; avoids the exactly-200 warning edge).

 State: bounded FIFO buffer of (UTC timestamp, close float) with max
`history_max_bars` (oldest evicted); `last_published *string = nil`; stats counters
`publish_count=0, last_publish_ts=0, last_value *string = nil`.

`seed_history(frame)`: pre-fill the buffer from a frame with a
`date` column or DatetimeIndex + `close` column (else error); timestamps normalized to UTC;
order preserved; only the most recent `history_max_bars` retained. nil/empty frame → no-op.
Purpose: meaningful classification on bar 1 instead of ~200 days of `"neutral"`.

`on_bar(bar)` — exact flow:

1. Append `(UTC ts of bar.ts_event, float(close))` to the buffer.
2. Inside a try block: if buffer length ≥ 200:
 a. Build the close series in buffer order; `regime = compute_regime(series, as_of=bar UTC date)`.
 b. **Always** write `shared_state.regime = regime` (every bar, even without transition).
 c. If `regime != last_published` (nil counts as different → first classification always
 publishes): publish `RegimeUpdate{value: regime, ts_event: bar.ts_event,
 ts_init: bar.ts_event}`; THEN set `last_published = regime` and increment stats
 (`publish_count += 1; last_publish_ts = bar.ts_event; last_value = regime`) — counters
 update only AFTER a successful publish.
3. Any error in step 2 → WARN log `"RegimeActor: primary publish failed; stats heartbeat will
 still fire"`; never crash the actor (`:154-158`).
4. `finally` (ALWAYS, every bar, warmup included): publish
 `ActorStatsUpdate{actor_name:"regime", publish_count, last_publish_ts,
 last_value_json: JSON(last_value), ts_event, ts_init}` — `last_value_json` is `"null"`
 before first publish, else e.g. `"\"bull\""` (`:159-175`; shapes documented at).

 Under 200 buffered bars: NO regime computation, NO shared_state write, NO
RegimeUpdate — only the stats heartbeat ("don't spam transitions during warmup",).

### 7.6 FundamentalsActor

Daily heartbeat on a reference bar (SPY 1-DAY, reused); sole writer of
`shared_state.market_cap`; publishes `MarketCapUpdate` per ticker when a new filing comes into
scope.

Config: `sf1_df` (pre-loaded SF1 history), `reference_bar_type`,
`tickers []string`, `dimension = "MRT"`.
State: `last_published map[string]Decimal` (per-ticker dedup **by value**, robust against frame
replacement,); stats counters; `last_value map[string]float64`.

`on_bar` — exact flow:

1. try: if `sf1_df` empty OR no tickers → return (stats heartbeat in `finally` STILL fires).
2. `as_of = UTC date of bar.ts_event`.
3. `caps = load_sf1_market_caps(sf1_df, as_of, dimension)` (§7.3); empty → return.
4. For each tracked ticker **in configured order**:
 - absent from `caps` → skip silently;
 - `caps[ticker] == last_published[ticker]` → skip (no duplicate bus traffic);
 - else: `shared_state.market_cap[ticker] = value` (written BEFORE the publish attempt —
 shared state updates even if the publish later fails,);
 then try to publish `MarketCapUpdate{ticker, value: float64(value), ts_event, ts_init}`;
 on success set `last_published[ticker] = value` and bump stats
 (`publish_count += 1; last_publish_ts = ts; last_value[ticker] = float64(value)`);
 on per-ticker publish failure → WARN
 `"FundamentalsActor: primary publish failed for {ticker}; continuing"` and continue with
 remaining tickers (`:137-142`).
5. `finally`: publish `ActorStatsUpdate{actor_name:"fundamentals", …,
 last_value_json: JSON object map ticker→float, "{}" before first publish}` every bar.

 `float(value)` on the wire loses Decimal precision for very large caps
(acknowledges this). Original: float64 payload. Go improvement: keep
float64 in the bus payload for wire stability, but carry the decimal string alongside in any
Go-internal representation handed to the API layer (the API already re-stringifies).

### 7.7 EarningsActor

Daily heartbeat (same reference bar); sole writer of `shared_state.earnings_blackout`;
publishes `EarningsBlackoutUpdate` on transitions **plus once on first observation per ticker**.

Config: `earnings_df` (`(ticker, report_date)` frame),
`reference_bar_type`, `tickers []string`, `blackout_days = 5` (`EARNINGS_BLACKOUT_DAYS`).
State: `last_published map[string]bool` where MISSING key means "never published" — forces the
first-observation publish even when the value is `false`; stats
counters; `last_value map[string]bool`.

`on_bar` — exact flow:

1. try: no tickers → return (stats heartbeat still fires).
2. `as_of = UTC date of bar.ts_event`; `current = load_earnings_calendar(earnings_df, as_of,
 blackout_days)` (§7.4). Absence in result == `false` (`:118-125`).
3. For each tracked ticker in configured order:
 - `value = current[ticker] (default false)`;
 - `shared_state.earnings_blackout[ticker] = value` **unconditionally every bar** (mirror;
 `:129` — note: differs from FundamentalsActor which writes shared state only on change);
 - publish `EarningsBlackoutUpdate{ticker, value, ts_event, ts_init}` iff no prior publish
 for this ticker OR `value != prior`; on success record `last_published[ticker] = value`
 and bump stats; per-ticker publish failure → WARN
 `"EarningsActor: primary publish failed for {ticker}; continuing"`, continue (`:131-153`).
4. `finally`: publish `ActorStatsUpdate{actor_name:"earnings", …, last_value_json: JSON map
 ticker→bool, "{}" before first publish}` every bar.

### 7.8 Published Data types

All bus payloads carry `ts_event` and `ts_init` as **int64 nanoseconds since epoch (UTC)**;
all actors here set both equal to the triggering bar's `ts_event`.

| Type | Fields (besides ts_event/ts_init) | Publisher | Cadence |
|---|---|---|---|
| `RegimeUpdate` | `value string` ∈ {bull, bear, neutral, warning} | RegimeActor | on transition only |
| `MarketCapUpdate` (`:45-67`) | `ticker string`, `value float64` (USD) | FundamentalsActor | on per-ticker value change |
| `EarningsBlackoutUpdate` (`:70-95`) | `ticker string`, `value bool` | EarningsActor | per-ticker transition + first observation |
| `ActorStatsUpdate` (`:98-131`) | `actor_name string` ∈ {regime, fundamentals, earnings}, `publish_count int` (cumulative primary publishes since process start), `last_publish_ts int64 ns` (0 if never), `last_value_json string` (shape per actor: regime → JSON string or `null`; fundamentals → JSON object ticker→float; earnings → JSON object ticker→bool) | each context actor (sole writer of its own stats — no aggregator) | EVERY heartbeat bar, unconditionally |
| `PortfolioHealthUpdate` (`:241-267`) | §5.4 | PortfolioHealthActor | every 1-MIN bar |

 Registration order: context actors are added to the engine BEFORE strategies. Assembly conditions: RegimeActor
always (with optional warmup seed); FundamentalsActor only when SF1 data is non-empty;
EarningsActor only when the earnings frame is non-empty; tickers for both = the SEPA stock
universe.

---

## 8. Runner glue — the check pipeline's call contract

### 8.1 Snapshot construction from engine state

 Translates engine state into an AccountSnapshot before every gate call:
`nav` = venue account total balance in base currency (USD); positions = open positions
aggregated by `(strategy_id, symbol)` summing signed qty, skipping zero; `cash = nav`
(simplification — balance_total already nets margin, `:62`); `last_close` = copy of the
caller-maintained map; `realized_pnl_today` / `unrealized_pnl_today` default to **0**.

 With both P&L inputs defaulting to 0, **the daily-loss halt is effectively dormant in
backtest**. Original: dormant. Go improvement: wire real day-P&L
into the snapshot in both modes (the live health actor already computes `nav − nav_at_open`);
keep the parameter defaults so tests can reproduce dormancy.

### 8.2 maybe_check_portfolio

 Gate wrapper used by every strategy runner: no Portfolio configured → proceed
(`true`); else `decision = portfolio.check(order, account)`; approved → `true`; rejected →
log WARN
`"[Portfolio] REJECTED {sid}/{sym} ({side} {qty}): {rule_name} — {reason}"`
and return `false` (order silently dropped — never raises).

---

## 9. Parameter summary (production wiring)

| Parameter | Value | Source |
|---|---|---|
| Allocation: SEPA | 0.40 |
| Allocation: SectorRotation | 0.30 |
| Allocation: Pairs | 0.20 |
| Cash slack | 0.10 (implicit; sum 0.90 ≤ 1.0) |
| `max_single_name_pct` | 0.50 |
| `concentration_pct` | 0.40 |
| `daily_loss_halt_pct` | 0.10 |
| Allocator sum epsilon | 1e-9 |
| Library defaults (single/conc/halt) | 0.20 / 0.30 / 0.05 |
| Regime min bars / slope window / flat threshold | 200 / 30 / 0.0 |
| Regime buffer | 280 bars |
| Earnings blackout window | ±5 calendar days, inclusive |
| SF1 dimension default | `"MRT"` |
| Reconciliation tolerance default | 0 shares |
| Health actor bar | SPY 1-MINUTE-LAST-EXTERNAL, live only |
| Context actor heartbeat | SPY 1-DAY bar |
| All timestamps | int64 ns since epoch, UTC; day boundaries by UTC calendar date |, actors' `as_of` derivation |

---

## 9a. Go single-strategy gate selection (`internal/engine/strategyassembly/assembly.go`)

The canonical multi-strategy gate runs strategies under ONE portfolio gate
(SEPA 40 / SectorRotation 30 / Pairs 20; single-name 50% / concentration 40% /
daily-loss 10%), used by both the multi-strategy backtest and the hyperopt
worker. The multi-strategy path has no "lone strategy" variant.

Go exposes single-strategy backtest / live / paper runs (e.g. the live profile
default `--strategy sector_rotation`, `compose.yaml`). For those the Go
assembler synthesizes a gate. The rule (`strategyGate`):

| Path | Allocator budget | Risk caps |
|---|---|---|
| `MultiStrategyGate=true` (hyperopt objective, all strategies) | canonical multi slice (SEPA 40 / Sector 30 / Pairs 20) | 50 / 40 / 10 |
| Lone SEPA / Pairs / ORB | 100% | default 20 / 30 / 5 |
| **Lone SectorRotation** | 100% | **canonical 50 / 40 / 10** (NOT the 20/30/5 default) |

**Why lone SectorRotation needs the canonical 50% single-name cap** (FIXER round 2,
finding 1): SectorRotation is a *concentrated* rotation —,
`intent.go:72` size each pick at `target_value = equity / top_k`, i.e. 1/topK of
the deployed book (33% per name at the baseline `top_k=3`,
`baseline/sector_rotation.json`). A 33% pick can **never** pass a 20% single-name
cap (the cap is on full NAV, independent of budget), so a lone SectorRotation
under the generic default gate would have **every** order rejected and trade
nothing — the out-of-box live/paper default would be inert. The 50/40/10 caps are
the canonical risk config for a topK rotation, and they admit a 3-ETF rotation
(33% < 50%). Using them for lone SectorRotation does not weaken any
determinism-critical gate: the hyperopt objective path always sets
`MultiStrategyGate=true` and already uses 50/40/10; only single-strategy
backtest/live is affected.

**Why the multi-strategy path is deliberately left unchanged (sector trades zero
there).** In the *shared-book* multi-strategy gate, SectorRotation sizes against
the full shared NAV (33% per pick) but its allocator budget is only 30% NAV, so
the allocator (`allocator.budget_exceeded`) rejects every pick (a 333-share XLK
pick at $33,300 vs a $30,000 budget is rejected). This is a latent sizing/budget
mismatch, kept deliberately. Size-scaling sector in the multi path would make it
trade where it currently does not, changing the hyperopt objective surface (which
optimizes over a *flat* sector subspace because sector never trades). The multi
path is left unchanged; only the lone path is fixed.

---

## 10. Open questions

1. **`ProposedOrder.ts` is dead.** The field is documented "for daily-loss halt windowing"
 but no rule reads it — the halt uses snapshot P&L only. Should Go keep the
 field (for forward-compat / type-shape stability) or is it droppable? Spec assumption:
 keep the field, never read it.
2. **Day boundary in UTC.** PortfolioHealthActor resets its baseline at midnight UTC, not at the US-equity session boundary. For a US market this means
 a "day" spans 19:00–19:00 ET (EDT). Intentional simplification or latent bug? This repo keeps
 UTC; flag before "improving" to exchange-local days.
3. **Concentration valuation inconsistency** (§3.5): held net valued at `order.price` in
 the concentration rule but at `last_close` in the single-name rule. The asymmetry looks
 accidental; the alternative is config-gated, but confirm whether the test suite
 should ever exercise the improved mode.
4. **`baseline = nav_at_open or nav`** falsy-zero quirk (§5.2 step 4): is the degenerate
 zero-NAV-open behavior relied upon anywhere (signal mode shows $0 P&L by a different path —
 `_read_nav` returning 0 every bar makes the quirk invisible)? Spec proposes NOT replicating
 the falsiness; confirm.
5. **Strategy IDs in the allocation table** are runner ids
 (`str(sepa_runner.id)`) — the exact id format (e.g.
 `"SEPAUniverseRunner-000"`) is engine-assigned. The system needs a stable strategy-id
 convention; the allocation table must key on whatever id the Go runners stamp on
 ProposedOrder.strategy_id and on positions. Decide the canonical id scheme.
6. **`AccountSnapshot.cash` is never consumed** by any rule or snapshot computation; the glue
 sets `cash = nav`. Keep as carried-but-unused, or drop? Spec assumption: keep.
7. **Reconciliation `matched` includes broker-explicit zeros** (s_sum 0, b_net 0 → matched,
 §6.2 step 3.3). The summary's "N symbols matched" can therefore count flat symbols the
 broker happened to report. Acceptable; confirm whether to count them
 in `matched`. Spec: yes.
8. **ActorStatsUpdate `last_value_json` for fundamentals/earnings grows monotonically** (a map
 accumulating every ticker ever published). Unbounded only in ticker-universe size — fine —
 but confirm the JSON key order doesn't matter to consumers (Go maps are
 unordered). Spec assumption: consumers parse JSON, order-insensitive.
9. **Error/`NaN` policy in `compute_regime` inputs**: NaN closes propagate into the
 rolling mean (NaN-poisoning the MA → step 3/6 guards fire → neutral/warning).
 NaN closes within the 200-window must yield the same guard outcomes; decide
 whether the data layer can ever supply NaN closes at all.
10. **Tabular inputs**: `sf1_df` / `earnings_df` / `spy_history` are modeled as
 typed slices ([]SF1Row{Ticker, DateKey, MarketCap,
 Dimension?}, []EarningsRow{Ticker, ReportDate}, []SPYBar{Date, Close}). The "stable sort by
 datekey, last per ticker" tie-break (§7.3 step 5) requires a STABLE sort over the original
 input order — preserve input order in the Go loaders.
