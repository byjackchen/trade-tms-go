# Portfolio & Risk Layer — Implementation Spec (extracted from Python reference)

Source repo: `trade-multi-strategies` (Python). All citations are `path:line` relative to that
repo's root. A Go engineer must be able to implement byte-equivalent semantics from this document
alone.

Tag legend:

- **[MUST-MATCH]** — Go must replicate exactly: formulas, comparison operators (strict vs
  inclusive), evaluation order, edge cases, defaults, string formats where consumed downstream.
- **[IMPROVE]** — Known weakness in the original. Both the original behavior AND the proposed Go
  improvement are described. Original behavior must still be available/derivable for parity tests
  unless noted.

Layer overview (`src/portfolio/__init__.py:1-10`): four modules + facade —
`allocator.py` (per-strategy capital budget), `risk_constraints.py` (account-wide hard rules),
`portfolio.py` (facade + health snapshot), `reconciliation.py` (EOD strategy-books vs broker-net
diff), plus `context_state.py` / `context_refresher.py` (shared slow-moving context) and three
context Actors + one health Actor.

---

## 1. Core value types (`src/portfolio/types.py`)

All four are immutable ("frozen dataclasses, zero Nautilus dependency",
`src/portfolio/types.py:1-9`). In Go: plain structs, treat as value types, never mutate after
construction.

### 1.1 SignalSide (`src/strategies/sepa/signal.py:62-67`)

[MUST-MATCH] String enum with exactly three values:

| Value | Wire string | Meaning |
|---|---|---|
| `LONG` | `"LONG"` | open/add long |
| `FLAT` | `"FLAT"` | close order — bypasses all risk gates |
| `SHORT` | `"SHORT"` | open/add short |

### 1.2 ProposedOrder (`src/portfolio/types.py:21-31`)

| Field | Type | Semantics |
|---|---|---|
| `strategy_id` | string | proposing strategy |
| `symbol` | string | bare ticker (e.g. `"AAPL"`) |
| `side` | SignalSide | direction; qty is unsigned |
| `qty` | int | **absolute magnitude (positive); side encodes direction** (`types.py:28`) |
| `price` | Decimal | estimated fill price used for sizing math (`types.py:29`) |
| `ts` | datetime (UTC) | bar timestamp, "for daily-loss halt windowing" (`types.py:30`) — note: **`ts` is never read by any rule in the current code** (see Open questions) |

### 1.3 RiskDecision (`src/portfolio/types.py:33-47`)

| Field | Type | Default |
|---|---|---|
| `approved` | bool | — |
| `rule_name` | string | `""` |
| `reason` | string | `""` |

[MUST-MATCH] Constructors: `approve()` → `{true, "", ""}`; `reject(rule, reason)` →
`{false, rule, reason}` (`types.py:41-47`, verified `tests/portfolio/test_types.py:12-23`).

### 1.4 AccountSnapshot (`src/portfolio/types.py:50-94`)

Read-only point-in-time account view. Conventions (`types.py:56-62`):

| Field | Type | Semantics |
|---|---|---|
| `nav` | Decimal | total account value = cash + market value of positions |
| `cash` | Decimal | (carried but never read by any rule) |
| `realized_pnl_today` | Decimal | day realized P&L |
| `unrealized_pnl_today` | Decimal | day unrealized P&L |
| `positions` | map[(strategy_id, symbol)]int | **signed** share count: + long, − short, 0/missing = flat. Default empty map |
| `last_close` | map[symbol]Decimal | last close price per symbol. Default empty map |

Helper methods — all [MUST-MATCH]:

- `total_pnl_today() = realized_pnl_today + unrealized_pnl_today` (`types.py:71-72`,
  test `test_types.py:26-33`).
- `strategy_position(sid, sym)` → `positions[(sid,sym)]`, **0 if missing** (`types.py:74-75`,
  test `test_types.py:36-46`).
- `net_position_across_strategies(sym)` → sum of signed qty over all keys whose symbol matches;
  0 if none (`types.py:77-79`, test `test_types.py:49-64`: `{SEPA:+100, Pairs:-40}` → 60).
- `gross_exposure_for_strategy(sid)` → `Σ |qty| * last_close[sym]` over this strategy's
  non-zero positions (`types.py:81-94`, test `test_types.py:67-91`: SEPA `100*150+50*400=35000`;
  Pairs with a short `100*60+80*180=20400`; unknown strategy → 0). Gross, not net — "gross is
  what consumes margin / capacity" (`types.py:84-87`).
  - [MUST-MATCH] Positions with `qty == 0` are skipped (`types.py:90-91`).
  - [IMPROVE] **Missing-price fallback**: a symbol absent from `last_close` contributes
    `|qty| * 0 = 0` (`types.py:92`), silently UNDER-counting exposure and making the budget
    check more permissive than reality. Original: silent zero. Go improvement: keep the zero
    fallback for parity, but emit a structured WARN log (`symbol`, `strategy_id`,
    `"missing last_close; exposure undercounted"`) whenever the fallback fires.

---

## 2. Allocator (`src/portfolio/allocator.py`)

### 2.1 StrategyAllocation (`allocator.py:18-23`)

| Field | Type | Constraint |
|---|---|---|
| `strategy_id` | string | unique within the table |
| `capital_pct` | float64 | in `(0, 1]` |

### 2.2 Constructor validation (`allocator.py:34-52`)

[MUST-MATCH] In this exact order, fail-fast with errors whose messages contain the quoted
substrings (tests match on them, `tests/portfolio/test_allocator.py:53-94`):

1. Empty list → error containing `"at least one"` (`allocator.py:35-36`).
2. Per entry, in input order: duplicate `strategy_id` → error containing
   `"duplicate strategy_id"` (`allocator.py:41-42`); then `capital_pct` outside `(0, 1]`
   (i.e. `pct <= 0 || pct > 1.0`) → error containing `"capital_pct"` (`allocator.py:43-46`).
   The duplicate check runs BEFORE the pct check for each entry.
3. After the loop: `Σ capital_pct > 1.0 + 1e-9` → error containing `"sum"`
   (`allocator.py:49-50`). The `1e-9` epsilon is float-arithmetic slack — [MUST-MATCH] the
   tolerance value. Sums `< 1.0` are intentional slack (cash buffer, `allocator.py:29-30`;
   test `test_allocator.py:85-94`).

### 2.3 Budget accessors

- `budget_pct(strategy_id)` → table value; **`0.0` for unregistered** (`allocator.py:54-56`,
  test `test_allocator.py:102-104`).
- `budget_dollars(strategy_id, account)` = `account.nav * Decimal(str(budget_pct))`
  (`allocator.py:58-60`).
  [MUST-MATCH] The float is converted via its **shortest decimal string repr** before Decimal
  multiplication (`Decimal(str(0.4))` == exact `0.4`, NOT the binary float expansion
  `0.4000000000000000222...`). In Go: store/convert `capital_pct` through
  `strconv.FormatFloat(p, 'g', -1, 64)` → decimal, or store the pct as a decimal from the start.
  Test fixture: nav 100000 × 0.4 → `40000.0`; nav 250000 × 0.4 → `100000.0`
  (`test_allocator.py:107-110`).

### 2.4 check_order_within_budget (`allocator.py:62-97`)

[MUST-MATCH] Exact decision sequence:

1. `side == FLAT` → **approve** (closing reduces exposure; `allocator.py:72-73`, test
   `test_allocator.py:164-171` — note the FLAT test order also has `qty=0`).
2. `qty <= 0` → approve (`allocator.py:74-75`).
3. `budget = budget_dollars(strategy_id)`; if `budget <= 0` → reject with
   `rule = "allocator.unregistered_strategy"`,
   `reason = "strategy_id '{sid}' has no allocation"` (`allocator.py:77-82`, test
   `test_allocator.py:118-126`).
4. `current_gross = account.gross_exposure_for_strategy(strategy_id)` (§1.4);
   `order_value = Decimal(qty) * price`; `new_gross = current_gross + order_value`.
5. Reject iff `new_gross > budget` (**strict**: exactly-at-budget passes) with
   `rule = "allocator.budget_exceeded"` and reason format
   `"{sid} gross exposure ${new_gross:,.2f} would exceed budget ${budget:,.2f} (current ${current_gross:,.2f}, order ${order_value:,.2f})"`
   (`allocator.py:88-96`) — `:,.2f` = thousands separators + 2 decimals. Otherwise approve.

[MUST-MATCH] Independence: one strategy's exposure never counts against another's budget
(test `test_allocator.py:174-193`). Existing exposure DOES count against own budget
(test `test_allocator.py:149-161`: held $30k of $40k + $15k order → reject).

[MUST-MATCH] Note the order value always ADDS to gross regardless of side — a SHORT open order
also increases gross (`order_value = qty*price`, qty positive). Reason strings are operator-facing
log text; Go must keep the rule names byte-identical (`allocator.unregistered_strategy`,
`allocator.budget_exceeded`); reason text should be semantically identical (same numbers, same
units) — exact byte equality of reasons is required only if parity tests compare them.

---

## 3. RiskConstraints (`src/portfolio/risk_constraints.py`)

Three account-wide hard rules. Doc comment (`risk_constraints.py:1-19`): max_position is
**GROSS per-strategy**; concentration is **NET cross-strategy** ("Pairs has high gross + zero
net, so it sails through concentration but is bounded by max_position").

### 3.1 RiskConstraintsConfig (`risk_constraints.py:30-44`)

| Param | Library default | **Production value** (`src/runner/strategy_assembly.py:248-252`) | Meaning |
|---|---|---|---|
| `max_single_name_pct` | 0.20 | **0.50** | one strategy's gross $ in one symbol ≤ pct·NAV |
| `concentration_pct` | 0.30 | **0.40** | cross-strategy NET $ in one symbol ≤ pct·NAV |
| `daily_loss_halt_pct` | 0.05 | **0.10** | halt all new orders when day P&L < −pct·NAV |

[MUST-MATCH] Validation: each value must be in `(0, 1]`; otherwise error message
`"{name} must be in (0, 1], got {v}"` (`risk_constraints.py:36-44`, tests match on the field
name, `tests/portfolio/test_risk_constraints.py:65-71`). `RiskConstraints` with nil config uses
the library defaults (`risk_constraints.py:56-57`).

### 3.2 check() — rule order, first-rejection-wins (`risk_constraints.py:59-81`)

[MUST-MATCH] Exact sequence:

0. `side == FLAT` **or** `qty <= 0` → approve. FLAT bypasses ALL rules **including the daily
   loss halt** — "even when daily loss is hit, we want closes to fire (so stops can work)"
   (`risk_constraints.py:48-54,62-64`; tests `test_risk_constraints.py:79-87`,
   `test_portfolio.py:116-128`).
1. **daily_loss_halt** (most restrictive — halts everything; supersedes other rules even for a
   1-share order, test `test_risk_constraints.py:202-212`).
2. **max_single_name**.
3. **concentration**.
4. Approve.

### 3.3 Rule 1 — daily_loss_halt (`risk_constraints.py:85-96`)

```
threshold = -nav * Decimal(str(daily_loss_halt_pct))     # negative number
pnl       = realized_pnl_today + unrealized_pnl_today
reject iff pnl < threshold                               # STRICT less-than
```

[MUST-MATCH] Strict `<`: P&L exactly AT −pct·NAV does **not** halt (boundary test
`tests/portfolio/test_portfolio_health_snapshot.py:73-79`: −5000 on 100k at 5% → not halted;
−6000 → halted, `test_risk_constraints.py:95-109`).
Rule name `"risk.daily_loss_halt"`, reason format
`"day P&L ${pnl:,.2f} is below halt threshold ${threshold:,.2f} ({pct:.1%} NAV)"`
(`:.1%` = percent with 1 decimal, e.g. `5.0%`).

### 3.4 Rule 2 — max_single_name (gross, per strategy) (`risk_constraints.py:98-118`)

```
held_qty   = |positions[(order.strategy_id, order.symbol)]|        # 0 if missing
held_price = last_close[order.symbol]  if present, else order.price   # FALLBACK TO ORDER PRICE
held_value = held_qty * held_price
new_value  = held_value + order.qty * order.price
cap        = nav * Decimal(str(max_single_name_pct))
reject iff new_value > cap                                          # STRICT
```

[MUST-MATCH] Notes:
- Only THIS strategy's position in THIS symbol counts (gross via `abs`).
- The held-value price fallback differs from the Allocator's (which falls back to 0, §1.4):
  here missing `last_close` falls back to `order.price` (`risk_constraints.py:103-105`).
- Side is irrelevant: a SHORT add also increases `new_value` (qty positive).
- Rule name `"risk.max_single_name"`, reason
  `"{sid} {sym} gross ${new_value:,.2f} would exceed single-name cap ${cap:,.2f} ({pct:.1%} NAV)"`.
- Tests: order alone over cap (`test_risk_constraints.py:127-135`), held + new over cap
  (`:138-151`: 100@150 held + 50@150 = 22.5k > 20k cap → reject).

### 3.5 Rule 3 — concentration (net, cross-strategy) (`risk_constraints.py:120-141`)

```
current_net = Σ over all strategies of signed qty in order.symbol
signed_qty  = +order.qty if side == LONG else -order.qty           # SHORT → negative
new_net     = current_net + signed_qty
new_net_value = |new_net| * order.price                            # ENTIRE net valued at ORDER price
cap         = nav * Decimal(str(concentration_pct))
reject iff new_net_value > cap                                      # STRICT
```

[MUST-MATCH] `signed_qty` branch is literally `qty if LONG else -qty`
(`risk_constraints.py:127`) — FLAT can never reach this rule (filtered in step 0), so the
`else` arm only ever means SHORT.
Rule name `"risk.concentration"`, reason
`"net {sym} across all strategies = {new_net} shares (${new_net_value:,.2f}) would exceed concentration cap ${cap:,.2f} ({pct:.1%} NAV)"`.
Tests: two strategies long same name → reject (`test_risk_constraints.py:159-173`: 130 held +
100 new = 230·150 = 34.5k > 30k); market-neutral short hedge passes (`:176-194`: 130 long +
100 short → net 30 → 4.5k).

[IMPROVE] **Valuation price**: the entire `new_net` (including the previously-held shares) is
valued at `order.price`, not at `last_close` — inconsistent with rule 2 which uses `last_close`
for held shares. Original: held shares re-priced at the order's estimated fill price. Go
improvement: keep original formula as the parity-default, and add an optional
(config-gated, default off) mode valuing held net at `last_close[sym]` (with order-price
fallback) + the order delta at `order.price`. Document divergence in the decision reason when
the improved mode is active.

---

## 4. Portfolio facade (`src/portfolio/portfolio.py`)

### 4.1 check() pipeline (`portfolio.py:42-61`)

[MUST-MATCH] Composition order (documented "for determinism in tests", `portfolio.py:45-50`):

1. `allocator.check_order_within_budget(order, account)` — first rejection wins.
2. `risk_constraints.check(order, account)` — first rejection wins.
3. Approve.

Effective full rule order seen by a caller therefore is:
`allocator.unregistered_strategy` / `allocator.budget_exceeded` → `risk.daily_loss_halt` →
`risk.max_single_name` → `risk.concentration`. Verified: order over BOTH budget and single-name
cap reports `allocator.budget_exceeded` (`tests/portfolio/test_portfolio.py:82-92`); within
budget but over single-name reports `risk.max_single_name` (`:95-105`).

### 4.2 PortfolioHealthSnapshot (`portfolio.py:22-30`)

Frozen value type — all fields Decimal except the bool:

| Field | Type | Definition |
|---|---|---|
| `day_pnl` | Decimal | `realized_pnl_today + unrealized_pnl_today` |
| `day_pnl_pct` | Decimal | `day_pnl / nav` if `nav > 0` else `0` |
| `daily_loss_halt` | bool | `day_pnl < -nav * halt_pct` (same strict `<` as §3.3) |
| `halt_headroom_pct` | Decimal | `0` if halted, else `(day_pnl - threshold) / nav` if `nav > 0` else `0` |
| `concentration_pct` | Decimal | largest `|net_qty| * last_close` / NAV across symbols (0 if `nav <= 0`) |

### 4.3 health_snapshot() algorithm (`portfolio.py:63-104`)

[MUST-MATCH] Pure function — mutates nothing (`portfolio.py:7-9,68-70`). Exact steps:

1. `day_pnl = account.total_pnl_today()`;
   `day_pnl_pct = day_pnl / nav` when `nav > 0`, else `Decimal(0)`.
2. `threshold = -nav * Decimal(str(config.daily_loss_halt_pct))`;
   `halted = day_pnl < threshold` (strict, mirrors §3.3).
3. `headroom`: if halted → `0` (clamped, never negative —
   `test_portfolio_health_snapshot.py:93-98`); else `(day_pnl - threshold) / nav` when
   `nav > 0` else `0`. Headroom is a positive fraction of NAV; e.g. pnl=+100, threshold=−5000,
   nav=100000 → `0.051` (`test_portfolio_health_snapshot.py:82-90` asserts `Decimal("0.0510")`
   — numeric equality, trailing zeros irrelevant).
4. `concentration_pct`: when `nav > 0`, over the set of distinct symbols appearing in
   `positions` keys: skip symbols with net 0; `value = |net| * last_close.get(sym, 0)`
   (missing price → 0 contribution); `pct = value / nav`; keep the max
   (`portfolio.py:84-96`). NET not gross: +100/−100 pairs-style → 0
   (`test_portfolio_health_snapshot.py:124-134`); example: AAPL net 150@200=30k,
   NVDA 100@500=50k, nav 200k → `0.25` (`:101-114`).

Decimal precision: Python `Decimal` default context (28 significant digits) governs the
divisions. In Go use a decimal library (e.g. shopspring/decimal) with ≥28-digit division
precision; comparisons in parity tests are numeric, not string-based.

---

## 5. PortfolioHealthActor (`src/portfolio/health_actor.py`)

Live-mode-only publisher of `PortfolioHealthUpdate` at **minute cadence** (cockpit's primary
risk signal; coarser cadence is explicitly wrong — `health_actor.py:42-45`).

### 5.1 Wiring (`src/runner/strategy_assembly.py:260-283`)

[MUST-MATCH]
- Registered only in live mode; backtest gets nothing (`strategy_assembly.py:274-275`).
- Subscribes to the SPY **1-MINUTE-LAST-EXTERNAL** bar derived from the daily SPY bar type's
  instrument id: `"{instrument_id}-1-MINUTE-LAST-EXTERNAL"` (`strategy_assembly.py:276-277`).
- The `Portfolio` reference is passed OUTSIDE the serializable config (the Python reason is a
  msgspec encoding limitation, `health_actor.py:6-9,46-49`; in Go this is just a constructor
  dependency).
- Production Portfolio it reads: allocations SEPA 0.40 / SectorRotation 0.30 / Pairs 0.20
  (0.10 cash slack) + risk config 0.50/0.40/0.10 (`strategy_assembly.py:230-257`); the same
  Portfolio instance is attached to every strategy runner via `set_portfolio_service`
  (`strategy_assembly.py:255-256`).

### 5.2 Per-bar algorithm (`health_actor.py:87-119`)

[MUST-MATCH] For each 1-MIN SPY bar:

1. `nav = _read_nav()` (one read per bar; tests inject a NAV sequence,
   `tests/portfolio/test_portfolio_health_actor.py:49-71`).
2. `bar_date = UTC calendar date of bar.ts_event` (ts_event is int nanoseconds since epoch;
   conversion is `ts_ns / 1e9 → UTC datetime → date`, `health_actor.py:89`). **Timezone: UTC**,
   so the trading "day" boundary is midnight UTC, not exchange local time (see Open questions).
3. Day-boundary reset: if no baseline yet OR `bar_date != date_at_open` →
   `nav_at_open = nav; date_at_open = bar_date` (`health_actor.py:92-94`). First bar of a new
   day therefore always publishes `day_pnl = 0` (`test_portfolio_health_actor.py:115-129`).
4. `baseline = nav_at_open or nav` (`health_actor.py:96`).
   [IMPROVE] Python `or` treats `Decimal(0)` as falsy: if the day opened with NAV exactly 0
   (signal mode), baseline silently becomes the CURRENT nav and day_pnl pins to 0 even after
   capital appears intraday. Original: falsy-zero fallback. Go improvement: use an explicit
   `if navAtOpen == nil` nil-check only (do NOT replicate the zero-is-falsy coercion), and log
   when `nav_at_open == 0`. This changes behavior only in the degenerate zero-NAV-open case.
5. `day_pnl = nav - baseline`.
6. Build an AccountSnapshot: `nav=nav, cash=nav, realized_pnl_today=day_pnl,
   unrealized_pnl_today=0, positions=_read_positions(), last_close=copy(_last_close)`
   (`health_actor.py:100-107`). `_last_close` starts empty and is never populated in this MVP →
   `concentration_pct` is always 0 from the actor; accepted tradeoff documented at
   `health_actor.py:74-78`. [IMPROVE] Original: concentration permanently 0 in the live feed.
   Go improvement: optionally feed `last_close` from the quote/bar stream so the published
   concentration is real; keep field semantics identical.
7. `health = portfolio.health_snapshot(snapshot)` (§4.3). Note `day_pnl_pct` therefore equals
   `day_pnl / CURRENT nav` (not baseline nav) — test asserts `500/100500`
   (`test_portfolio_health_actor.py:100-112`).
8. Publish `PortfolioHealthUpdate` with all Decimal fields converted to float64, and
   `ts_event = ts_init = bar.ts_event` (`health_actor.py:110-119`). Published **on every bar
   unconditionally** (no dedup/transition gating, unlike the context actors).

### 5.3 Engine reads (`health_actor.py:125-149`)

[MUST-MATCH]
- `_read_nav`: venue account's total balance in USD; **no account registered (signal mode) →
  NAV = 0** (`health_actor.py:130-137`).
- `_read_positions`: aggregate open positions by `(strategy_id, symbol)` summing signed qty,
  skipping zero (`health_actor.py:139-149`) — identical aggregation to
  `build_snapshot_from_nautilus` (§8.1).

### 5.4 PortfolioHealthUpdate payload (`src/data/custom_data.py:241-267`)

| Field | Type | Notes |
|---|---|---|
| `day_pnl` | float64 | signed dollars |
| `day_pnl_pct` | float64 | fraction (0.007 = 0.7%) |
| `daily_loss_halt` | bool | strict-`<` rule, §3.3 |
| `halt_headroom_pct` | float64 | fraction, clamped ≥ 0 |
| `concentration_pct` | float64 | fraction |
| `ts_event`, `ts_init` | int64 | nanoseconds since epoch (UTC) |

Primitive floats are a Python serializer constraint; "Decimal precision is restored at the API
boundary via str-encoded schema" (`custom_data.py:258-260`).

### 5.5 REST exposure (`src/api/server.py:469-518`, `src/api/schemas/…`)

`GET /api/live/portfolio-health` returns the latest stream entry of
`data.PortfolioHealthUpdate`:

```json
{
  "day_pnl": "…", "day_pnl_pct": "…",          // stringified decimals
  "daily_loss_halt": false,
  "halt_headroom_pct": "…", "concentration_pct": "…",
  "ts_event": 1714572600000000000,              // int ns
  "last_update_age_ms": 1234                    // max(0, (now_ns - ts_event) // 1e6)
}
```

[MUST-MATCH] 503 with detail `"PortfolioHealthUpdate stream is empty — no live producer
running"` on cold start; 502 on malformed payload (`server.py:488-518`).

---

## 6. Reconciliation (`src/portfolio/reconciliation.py`)

EOD check: `sum(strategy books) == broker net?` Pure data module — runner supplies both sides
(`reconciliation.py:1-10`).

### 6.1 Report shape

`Mismatch` (`reconciliation.py:18-29`):

| Field | Type | Definition |
|---|---|---|
| `symbol` | string | |
| `strategy_books_sum` | int | signed sum across strategies |
| `broker_net` | int | signed broker view |
| `diff` | int | **`broker_net - strategy_books_sum`** (sign matters; test `test_reconciliation.py:28-41`: books 100, broker 95 → diff **−5**) |
| `diff_shares` (derived) | int | `|diff|` |

`ReconciliationReport` (`reconciliation.py:32-69`):

| Field | Type | |
|---|---|---|
| `ts` | datetime (UTC) | |
| `matched` | []string | symbols with diff within tolerance |
| `mismatches` | []Mismatch | |
| `symbols_only_in_strategies` | []string | we claim, broker shows zero |
| `symbols_only_at_broker` | []string | broker shows, no strategy claims |
| `has_issues` (derived) | bool | any of the three lists non-empty (`reconciliation.py:40-46`) |

`summary()` text format [MUST-MATCH] (tests match substrings `"OK"`, `"Mismatches"`, `"diff"`,
`test_reconciliation.py:102-120`):

- Clean: `"Reconciliation OK ({N} symbols matched)"`.
- Issues: first line `"Reconciliation report @ {ts.isoformat()}"` (RFC3339-ish,
  e.g. `2024-06-28T21:00:00+00:00`); then, if any mismatches, a line
  `"  Mismatches ({N}):"` followed per mismatch by
  `"    {sym}: strategies sum {s:+d}, broker {b:+d}, diff {d:+d}"` (`%+d` = always-signed int);
  then, if any, `"  Strategies claim positions, broker shows zero: {comma-joined syms}"`;
  then, if any, `"  Broker shows positions, no strategy claims them: {comma-joined syms}"`.
  Lines joined with `\n`.

### 6.2 reconcile() algorithm (`reconciliation.py:72-131`)

Inputs: `ts`, `strategy_books map[(sid,sym)]int` (signed), `broker_positions map[sym]int`
(signed), `tolerance_shares int = 0`.

[MUST-MATCH] Exact steps:

1. Aggregate strategy books per symbol, summing signed shares, **skipping entries with
   `qty == 0`** ("a 0-share entry is not a claimed position", `reconciliation.py:89-94`, test
   `test_reconciliation.py:77-88`).
2. `all_symbols = keys(strategy_sums) ∪ keys(broker_positions)`, iterated in **sorted
   (lexicographic ascending) order** — output list ordering is deterministic
   (`reconciliation.py:101-102`).
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

### 6.3 Live moomoo bridge (`src/adapters/moomoo/reconciliation.py`)

[MUST-MATCH] (consume in the broker adapter spec; summarized here because the report shape is
shared):

- `strategy_books_from_cache`: open positions only; sum signed qty per `(strategy_id, symbol)`
  defensively; skip zero (`adapters/moomoo/reconciliation.py:41-59`).
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

### 7.1 SharedContextState (`src/portfolio/context_state.py:18-33`)

Single mutable in-process store consulted by every strategy runner:

| Field | Type | Default |
|---|---|---|
| `regime` | string | `"neutral"` |
| `market_cap` | map[string]Decimal | empty |
| `earnings_blackout` | map[string]bool | empty |

[MUST-MATCH] Module-level singleton; all consumers observe the same mutations
(`context_state.py:31-33`, tests `tests/portfolio/test_context_state.py:11-63` — defaults,
identity across imports, mutation propagation, wholesale dict replacement must all work).

[IMPROVE] **Not thread-safe** — explicitly "Not thread-safe. The Nautilus event loop is
single-threaded so this is fine" (`context_state.py:22-24`). Original: bare struct.
Go improvement: the Go system is concurrent — guard with `sync.RWMutex` (or expose an
atomic-snapshot accessor). Semantics (last-writer-wins per field, sole-writer-per-field
convention §7.4) unchanged.

Sole-writer convention [MUST-MATCH]: RegimeActor is the sole writer of `regime`
(`src/data/regime_actor.py:4-6`), FundamentalsActor of `market_cap`
(`src/data/fundamentals_actor.py:5`), EarningsActor of `earnings_blackout`
(`src/data/earnings_actor.py:5-6`).

### 7.2 compute_regime (`src/portfolio/context_refresher.py:50-103`)

Constants (`context_refresher.py:35-47`):

| Constant | Value |
|---|---|
| Labels | `"bull"`, `"bear"`, `"neutral"`, `"warning"` |
| `_REGIME_MIN_BARS` | 200 |
| `_REGIME_SLOPE_WINDOW` | 30 |
| `_REGIME_SLOPE_FLAT_PCT` | 0.0 |
| `EARNINGS_BLACKOUT_DAYS` | 5 |

[MUST-MATCH] Input: SPY daily history with a `close` column (float). Optional `as_of` date
filters to bars with date ≤ as_of BEFORE any computation (look-ahead prevention —
`context_refresher.py:63-74`; behavioral test
`tests/portfolio/test_context_refresher.py:101-133`: same frame, early as_of → bear, late
as_of → bull). Date source: DatetimeIndex if present, else a `date` column.

Classification, exact order:

1. nil frame OR `< 200` rows (after as_of filter) → `"neutral"` (`:75-76`, test `:67-69,97-98`).
2. `ma200 =` simple rolling mean of close, window 200, requiring 200 points (first 199 entries
   are NaN). `last_close = close[-1]`, `last_ma = ma200[-1]`.
3. `last_ma` is NaN → `"neutral"` (`:83-84`). (Unreachable when len ≥ 200 and closes are
   non-NaN, but [MUST-MATCH] as a NaN guard.)
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

### 7.3 load_sf1_market_caps (`context_refresher.py:106-147`)

Latest known market cap per ticker as of a date, from a SHARADAR/SF1-shaped table
(`ticker`, `datekey` = filing date, `marketcap`, optional `dimension`).

[MUST-MATCH] Steps:

1. nil/empty input → empty map (test `test_context_refresher.py:185-186`).
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

### 7.4 load_earnings_calendar (`context_refresher.py:150-183`)

Earnings blackout flag per ticker from a `(ticker, report_date)` table (SHARADAR/EVENTS rows
filtered to earnings, eventcode `"22"` in the pipe-separated `eventcodes` column — filtering is
the CALLER's job, `context_refresher.py:20-25,164-166`).

[MUST-MATCH]:

1. nil/empty frame → empty map. Missing `ticker` or the date column → empty map (silent;
   `:168-171`).
2. Date column name defaults to `"report_date"`; caller may override (e.g. `"date"` for raw
   EVENTS frames, test `test_context_refresher.py:286-295`).
3. Window: ticker is in blackout iff ANY of its earnings dates `d` satisfies
   `as_of − N ≤ d ≤ as_of + N` **calendar days, inclusive both ends**, with
   `N = blackout_days` (default 5; `:173-178`). Tests: 3 days past → in (`:237-241`);
   10 days past → out for N=5, in for N=14 (`:244-248,258-264`); 4 days future → in
   (`:251-255`).
4. Output contains ONLY `true` entries — tickers not in blackout are ABSENT, never `false`
   (`:180-183`; consumers interpret absence as false, §7.7).

### 7.5 RegimeActor (`src/data/regime_actor.py`)

Subscribes to **SPY 1-DAY bars**; sole writer of `shared_state.regime`; publishes
`RegimeUpdate` on transitions only.

Config (`regime_actor.py:36-47`): `spy_bar_type` (daily); `history_max_bars = 280`
(200 warmup + 30 slope + ~50 cushion; avoids the exactly-200 warning edge).

[MUST-MATCH] State: bounded FIFO buffer of (UTC timestamp, close float) with max
`history_max_bars` (oldest evicted); `last_published *string = nil`; stats counters
`publish_count=0, last_publish_ts=0, last_value *string = nil`.

`seed_history(frame)` (`regime_actor.py:85-113`): pre-fill the buffer from a frame with a
`date` column or DatetimeIndex + `close` column (else error); timestamps normalized to UTC;
order preserved; only the most recent `history_max_bars` retained. nil/empty frame → no-op.
Purpose: meaningful classification on bar 1 instead of ~200 days of `"neutral"`.

`on_bar(bar)` (`regime_actor.py:122-163`) — exact flow:

1. Append `(UTC ts of bar.ts_event, float(close))` to the buffer.
2. Inside a try block: if buffer length ≥ 200:
   a. Build the close series in buffer order; `regime = compute_regime(series, as_of=bar UTC date)`.
   b. **Always** write `shared_state.regime = regime` (every bar, even without transition).
   c. If `regime != last_published` (nil counts as different → first classification always
      publishes): publish `RegimeUpdate{value: regime, ts_event: bar.ts_event,
      ts_init: bar.ts_event}`; THEN set `last_published = regime` and increment stats
      (`publish_count += 1; last_publish_ts = bar.ts_event; last_value = regime`) — counters
      update only AFTER a successful publish (`regime_actor.py:148-153`).
3. Any error in step 2 → WARN log `"RegimeActor: primary publish failed; stats heartbeat will
   still fire"`; never crash the actor (`:154-158`).
4. `finally` (ALWAYS, every bar, warmup included): publish
   `ActorStatsUpdate{actor_name:"regime", publish_count, last_publish_ts,
   last_value_json: JSON(last_value), ts_event, ts_init}` — `last_value_json` is `"null"`
   before first publish, else e.g. `"\"bull\""` (`:159-175`; shapes documented at
   `custom_data.py:113-122`).

[MUST-MATCH] Under 200 buffered bars: NO regime computation, NO shared_state write, NO
RegimeUpdate — only the stats heartbeat ("don't spam transitions during warmup",
`regime_actor.py:58-60`).

### 7.6 FundamentalsActor (`src/data/fundamentals_actor.py`)

Daily heartbeat on a reference bar (SPY 1-DAY, reused); sole writer of
`shared_state.market_cap`; publishes `MarketCapUpdate` per ticker when a new filing comes into
scope.

Config (`fundamentals_actor.py:35-53`): `sf1_df` (pre-loaded SF1 history), `reference_bar_type`,
`tickers []string`, `dimension = "MRT"`.
State: `last_published map[string]Decimal` (per-ticker dedup **by value**, robust against frame
replacement, `fundamentals_actor.py:85-90`); stats counters; `last_value map[string]float64`.

`on_bar` (`fundamentals_actor.py:103-147`) — exact flow:

1. try: if `sf1_df` empty OR no tickers → return (stats heartbeat in `finally` STILL fires).
2. `as_of = UTC date of bar.ts_event`.
3. `caps = load_sf1_market_caps(sf1_df, as_of, dimension)` (§7.3); empty → return.
4. For each tracked ticker **in configured order**:
   - absent from `caps` → skip silently;
   - `caps[ticker] == last_published[ticker]` → skip (no duplicate bus traffic);
   - else: `shared_state.market_cap[ticker] = value` (written BEFORE the publish attempt —
     shared state updates even if the publish later fails, `fundamentals_actor.py:123`);
     then try to publish `MarketCapUpdate{ticker, value: float64(value), ts_event, ts_init}`;
     on success set `last_published[ticker] = value` and bump stats
     (`publish_count += 1; last_publish_ts = ts; last_value[ticker] = float64(value)`);
     on per-ticker publish failure → WARN
     `"FundamentalsActor: primary publish failed for {ticker}; continuing"` and continue with
     remaining tickers (`:137-142`).
5. `finally`: publish `ActorStatsUpdate{actor_name:"fundamentals", …,
   last_value_json: JSON object map ticker→float, "{}" before first publish}` every bar.

[IMPROVE] `float(value)` on the wire loses Decimal precision for very large caps
(`custom_data.py:55-59` acknowledges this). Original: float64 payload. Go improvement: keep
float64 in the bus payload for wire parity, but carry the decimal string alongside in any
Go-internal representation handed to the API layer (the API already re-stringifies).

### 7.7 EarningsActor (`src/data/earnings_actor.py`)

Daily heartbeat (same reference bar); sole writer of `shared_state.earnings_blackout`;
publishes `EarningsBlackoutUpdate` on transitions **plus once on first observation per ticker**.

Config (`earnings_actor.py:42-62`): `earnings_df` (`(ticker, report_date)` frame),
`reference_bar_type`, `tickers []string`, `blackout_days = 5` (`EARNINGS_BLACKOUT_DAYS`).
State: `last_published map[string]bool` where MISSING key means "never published" — forces the
first-observation publish even when the value is `false` (`earnings_actor.py:95-99`); stats
counters; `last_value map[string]bool`.

`on_bar` (`earnings_actor.py:112-158`) — exact flow:

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

### 7.8 Published Data types (`src/data/custom_data.py`)

All bus payloads carry `ts_event` and `ts_init` as **int64 nanoseconds since epoch (UTC)**;
all actors here set both equal to the triggering bar's `ts_event`.

| Type | Fields (besides ts_event/ts_init) | Publisher | Cadence |
|---|---|---|---|
| `RegimeUpdate` (`custom_data.py:27-42`) | `value string` ∈ {bull, bear, neutral, warning} | RegimeActor | on transition only |
| `MarketCapUpdate` (`:45-67`) | `ticker string`, `value float64` (USD) | FundamentalsActor | on per-ticker value change |
| `EarningsBlackoutUpdate` (`:70-95`) | `ticker string`, `value bool` | EarningsActor | per-ticker transition + first observation |
| `ActorStatsUpdate` (`:98-131`) | `actor_name string` ∈ {regime, fundamentals, earnings}, `publish_count int` (cumulative primary publishes since process start), `last_publish_ts int64 ns` (0 if never), `last_value_json string` (shape per actor: regime → JSON string or `null`; fundamentals → JSON object ticker→float; earnings → JSON object ticker→bool) | each context actor (sole writer of its own stats — no aggregator) | EVERY heartbeat bar, unconditionally |
| `PortfolioHealthUpdate` (`:241-267`) | §5.4 | PortfolioHealthActor | every 1-MIN bar |

[MUST-MATCH] Registration order: context actors are added to the engine BEFORE strategies
(`regime_actor.py:13-17`, `strategy_assembly.py:320-341`). Assembly conditions: RegimeActor
always (with optional warmup seed); FundamentalsActor only when SF1 data is non-empty;
EarningsActor only when the earnings frame is non-empty; tickers for both = the SEPA stock
universe (`strategy_assembly.py:121-147,319-326`).

---

## 8. Runner glue (`src/runner/portfolio_glue.py`) — the check pipeline's call contract

### 8.1 build_snapshot_from_nautilus (`portfolio_glue.py:31-67`)

[MUST-MATCH] Translates engine state into an AccountSnapshot before every gate call:
`nav` = venue account total balance in base currency (USD); positions = open positions
aggregated by `(strategy_id, symbol)` summing signed qty, skipping zero; `cash = nav`
(simplification — balance_total already nets margin, `:62`); `last_close` = copy of the
caller-maintained map; `realized_pnl_today` / `unrealized_pnl_today` default to **0**.

[IMPROVE] With both P&L inputs defaulting to 0, **the daily-loss halt is effectively dormant in
backtest** (`portfolio_glue.py:13-16`). Original: dormant. Go improvement: wire real day-P&L
into the snapshot in both modes (the live health actor already computes `nav − nav_at_open`);
keep the parameter defaults so parity tests can reproduce dormancy.

### 8.2 maybe_check_portfolio (`portfolio_glue.py:70-98`)

[MUST-MATCH] Gate wrapper used by every strategy runner: no Portfolio configured → proceed
(`true`); else `decision = portfolio.check(order, account)`; approved → `true`; rejected →
log WARN
`"[Portfolio] REJECTED {sid}/{sym} ({side} {qty}): {rule_name} — {reason}"`
and return `false` (order silently dropped — never raises).

---

## 9. Parameter summary (production wiring)

| Parameter | Value | Source |
|---|---|---|
| Allocation: SEPA | 0.40 | `strategy_assembly.py:242` |
| Allocation: SectorRotation | 0.30 | `strategy_assembly.py:243` |
| Allocation: Pairs | 0.20 | `strategy_assembly.py:244` |
| Cash slack | 0.10 (implicit; sum 0.90 ≤ 1.0) | `strategy_assembly.py:234` |
| `max_single_name_pct` | 0.50 | `strategy_assembly.py:249` |
| `concentration_pct` | 0.40 | `strategy_assembly.py:250` |
| `daily_loss_halt_pct` | 0.10 | `strategy_assembly.py:251` |
| Allocator sum epsilon | 1e-9 | `allocator.py:49` |
| Library defaults (single/conc/halt) | 0.20 / 0.30 / 0.05 | `risk_constraints.py:32-34` |
| Regime min bars / slope window / flat threshold | 200 / 30 / 0.0 | `context_refresher.py:43-47` |
| Regime buffer | 280 bars | `regime_actor.py:47` |
| Earnings blackout window | ±5 calendar days, inclusive | `context_refresher.py:40,173-178` |
| SF1 dimension default | `"MRT"` | `context_refresher.py:110` |
| Reconciliation tolerance default | 0 shares | `reconciliation.py:77` |
| Health actor bar | SPY 1-MINUTE-LAST-EXTERNAL, live only | `strategy_assembly.py:274-277` |
| Context actor heartbeat | SPY 1-DAY bar | `strategy_assembly.py:121-147` |
| All timestamps | int64 ns since epoch, UTC; day boundaries by UTC calendar date | `health_actor.py:89`, actors' `as_of` derivation |

---

## 10. Open questions

1. **`ProposedOrder.ts` is dead.** The field is documented "for daily-loss halt windowing"
   (`types.py:30`) but no rule reads it — the halt uses snapshot P&L only. Should Go keep the
   field (for forward-compat / parity of the type shape) or is it droppable? Spec assumption:
   keep the field, never read it.
2. **Day boundary in UTC.** PortfolioHealthActor resets its baseline at midnight UTC
   (`health_actor.py:89-94`), not at the US-equity session boundary. For a US market this means
   a "day" spans 19:00–19:00 ET (EDT). Intentional simplification or latent bug? Go must match
   UTC for parity; flag before "improving" to exchange-local days.
3. **Concentration valuation inconsistency** (§3.5): held net valued at `order.price` in
   the concentration rule but at `last_close` in the single-name rule. The asymmetry looks
   accidental; the proposed [IMPROVE] is config-gated, but confirm whether the parity suite
   should ever exercise the improved mode.
4. **`baseline = nav_at_open or nav`** falsy-zero quirk (§5.2 step 4): is the degenerate
   zero-NAV-open behavior relied upon anywhere (signal mode shows $0 P&L by a different path —
   `_read_nav` returning 0 every bar makes the quirk invisible)? Spec proposes NOT replicating
   the falsiness; confirm.
5. **Strategy IDs in the allocation table** are Nautilus runner ids
   (`str(sepa_runner.id)`, `strategy_assembly.py:242-244`) — the exact id format (e.g.
   `"SEPAUniverseRunner-000"`) is engine-assigned. The Go system needs a stable strategy-id
   convention; the allocation table must key on whatever id the Go runners stamp on
   ProposedOrder.strategy_id and on positions. Decide the canonical id scheme.
6. **`AccountSnapshot.cash` is never consumed** by any rule or snapshot computation; the glue
   sets `cash = nav`. Keep as carried-but-unused for parity, or drop? Spec assumption: keep.
7. **Reconciliation `matched` includes broker-explicit zeros** (s_sum 0, b_net 0 → matched,
   §6.2 step 3.3). The summary's "N symbols matched" can therefore count flat symbols the
   broker happened to report. Acceptable for parity; confirm whether Go should also count them
   in `matched`. Spec: yes, match exactly.
8. **ActorStatsUpdate `last_value_json` for fundamentals/earnings grows monotonically** (a map
   accumulating every ticker ever published). Unbounded only in ticker-universe size — fine —
   but confirm the JSON key order doesn't matter to consumers (Python dict order = insertion
   order; Go maps are unordered). Spec assumption: consumers parse JSON, order-insensitive.
9. **Error/`NaN` policy in `compute_regime` inputs**: Python silently propagates NaN closes
   into the rolling mean (NaN-poisoning the MA → step 3/6 guards fire → neutral/warning). Go
   must reproduce: NaN closes within the 200-window must yield the same guard outcomes; decide
   whether Go's data layer can ever supply NaN closes at all.
10. **Pandas frames at the Go boundary**: `sf1_df` / `earnings_df` / `spy_history` are
    DataFrames in Python. Go equivalents are typed slices ([]SF1Row{Ticker, DateKey, MarketCap,
    Dimension?}, []EarningsRow{Ticker, ReportDate}, []SPYBar{Date, Close}). The "stable sort by
    datekey, last per ticker" tie-break (§7.3 step 5) requires a STABLE sort over the original
    input order — preserve input order in the Go loaders.
