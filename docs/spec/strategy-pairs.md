# Pairs Strategy — Implementation-Grade Specification

This repo's definition of the Pairs strategy. It covers the pure signal
generator (the core), the per-leg signal-intent type + helpers, the engine
wrapper (Signal → Order), the shared runner template (`on_bar`, publishing), the
parameter defaults + hyperopt ranges
(`internal/hyperopt/baseline/pairs.json`), param loading / constraint
clamping, and wiring. The rules below are invariants of this system, including
edge cases, precision, and ordering. Where a known weakness is called out, the
better behavior this repo adopts is described alongside it.

---

## 1. Strategy overview

Statistical mean-reversion on the price spread of two co-moving equities. Per pair, per synchronized daily bar:

1. Both legs must have a bar at the same calendar date before evaluation (vintage
 consistency) —.
2. Hedge ratio β = OLS slope of `P_long` regressed on `P_short` over a rolling
 `lookback` window —.
3. `spread_t = P_long_t − β · P_short_t` —.
4. `z = (spread_t − mean(spread)) / pstdev(spread)` over the same window —.
5. State machine (per pair): FLAT / LONG_SPREAD / SHORT_SPREAD with entry at
 `|z| > entry_z`, exit at `|z| < exit_z`, and divergence-stop at re-crossing
 `entry_z` on the adverse side —.

Architectural contract ("Eng-D2"): the signal generator (SG) is engine-agnostic pure
code with the single hot-path API `on_bar(Bar) -> []Signal`; a thin runner translates
signals to broker orders. Multi-leg
simultaneity: one pair entry atomically returns 2 signals (one LONG + one SHORT)
from a single `on_bar` call.

This is the only strategy in the platform that emits `SignalSide.SHORT`.

---

## 2. Shared platform types (contract boundary)

Reused from (the platform-wide contract,):

### 2.1 `SignalSide` —

String enum: `LONG = "LONG"`, `FLAT = "FLAT"`, `SHORT = "SHORT"`.

### 2.2 `Bar` —

Immutable. Fields:

| Field | Type | Notes |
|---|---|---|
| `symbol` | string | ticker, e.g. `"KO"` |
| `ts` | datetime | **tz-aware UTC** |
| `open`, `high`, `low`, `close` | Decimal | exact decimal price |
| `volume` | int |

In Go: use a fixed-point/decimal type for OHLC (e.g. `shopspring/decimal` or scaled
ints); `time.Time` in UTC for `ts`. The Pairs SG only reads `symbol`, `ts`, `close`; the other fields must still exist on the type (COMPLETE).

### 2.3 `Signal` —

| Field | Type | Pairs usage |
|---|---|---|
| `symbol` | string | leg ticker |
| `ts` | datetime | the triggering bar's `ts`, passed through unchanged |
| `side` | SignalSide | LONG / SHORT / FLAT |
| `target_qty` | int | **always non-negative magnitude for entries** (side encodes direction,); `0` for FLAT |
| `reason` | string | exact format in §7.4 |
| `confidence` | float, default `1.0` | Pairs never sets it (stays 1.0) |
| `grade` | optional, default nil | Pairs never sets it |
| `stop_price` | optional Decimal, default nil | Pairs never sets it |

---

## 3. Pair definition and default universe

### 3.1 `Pair` —

Frozen value type with `long_leg string`, `short_leg string`. The labels are
arbitrary direction anchors, not a directional bet: the strategy trades the spread
both ways. Convention (doc only): `long_leg` = larger-cap / more
liquid name.

`Pair.key` = ordered tuple `(long_leg, short_leg)` — the map key
for all per-pair state. Key equality is positional and case-sensitive.

### 3.2 `DEFAULT_PAIRS` —

```
("KO", "PEP") # consumer staples — beverage duopoly
("MA", "V") # payment networks
("XOM", "CVX") # integrated oil majors
```

Exactly 3 pairs; order as listed. Doc note: empirically
chosen, not cointegration-vetted; a cointegration pre-filter is a flagged future
enhancement — see I-1 in §13.

The same triple is duplicated as the `pairs` parameter default in
`params/baseline/pairs.json` ("pairs" → `[["KO","PEP"],["MA","V"],["XOM","CVX"]]`)
and is the live-runner source of leg subscriptions.

---

## 4. Configuration

### 4.1 `PairsSignalGeneratorConfig` —

Immutable (frozen). Fields (all required, no defaults in the struct itself —
defaults come from the JSON param file, §5):

| Field | Type | Meaning |
|---|---|---|
| `equity_provider` | func → Decimal | no-arg callable returning **live** account equity in USD; called at sizing time, never cached |
| `pairs` | tuple of Pair | the pair universe |
| `lookback` | int | rolling window for β AND spread mean/std (bars) |
| `entry_z` | float | z-score entry threshold (absolute) |
| `exit_z` | float | z-score exit threshold (absolute) |
| `capital_per_pair_pct` | float | fraction of equity allocated per pair, `(0,1]` |
| `timezone` | string | IANA tz string; **declared/persisted only, never used in signal math** (see §11) |

### 4.2 Construction-time validation —

Checks run in this exact order with these exact error semantics (tests pin the
messages:):

1. `equity_provider` not callable → **TypeError** `"equity_provider must be a callable returning Decimal"`. The provider is **not invoked** during validation — its closure may not be ready yet.
2. `pairs` empty → ValueError `"pairs must not be empty"`.
3. `lookback < 5` → ValueError `"lookback must be >= 5"`.
4. `entry_z <= 0` **or** `exit_z < 0` → ValueError `"entry_z must be > 0 and exit_z must be >= 0"`. Note: `exit_z == 0` is legal.
5. `exit_z >= entry_z` → ValueError `"exit_z must be < entry_z (else no entry/exit gap)"`.
6. `capital_per_pair_pct` outside `(0, 1]` → ValueError `"capital_per_pair_pct must be in (0, 1]"`.

In Go, return errors (no panics) from a constructor; preserve the distinction
between the type error (1) and value errors (2–6), and keep messages
substring-compatible (tests match on `"pairs must not be empty"`, `"lookback"`,
`"entry_z"`, `"exit_z must be < entry_z"`, `"equity_provider"`,
`"capital_per_pair_pct"`).

### 4.3 Generator state initialization —

On construction, for every pair and each of its two legs:

- `_history[sym]` = ring buffer of Decimal closes with **capacity `lookback + 1`**. The `+1` is load-bearing for `state_dict` round-trips: the
 serialized history may contain up to `lookback+1` closes even though evaluation
 only ever uses the last `lookback` (§7.1). A Go ring buffer must evict oldest
 beyond `lookback+1` and serialize all retained values.
- `_leg_position[sym] = 0`.
- `_pair_state[pair.key] = "FLAT"`.

Initialization uses set-if-absent semantics (`setdefault`), so a symbol shared by
two pairs gets ONE shared history buffer and ONE shared leg-position slot
 — see I-2.

Other state: `_last_close map[sym]Decimal`, `_last_bar_date map[sym]date` start
empty; `_latest_z`, `_latest_beta` (per-pair telemetry maps) start empty;
`_intent_generation = 0`.

---

## 5. Parameters: defaults, hyperopt search ranges, constraints

Source of truth: `internal/hyperopt/baseline/pairs.json` (schema_version 1).
Loaded via (env dir `TMS_STRATEGY_PARAMS_DIR` overrides
per-strategy file, falling back to the baked-in baseline;).

### 5.1 Parameter table

| Name | Type | Default | Hyperopt range (inclusive) | Description |
|---|---|---|---|---|
| `lookback` | int | **60** | 30 – 120 | Rolling β + spread mean/std window (bars) |
| `entry_z` | float | **2.0** | 1.5 – 3.0 | Spread z-score threshold to open a pair |
| `exit_z` | float | **0.5** | 0.1 – 1.0 | Spread z-score threshold to close a pair |
| `capital_per_pair_pct` | float | **0.30** | 0.10 – 0.45 | Account % allocated per active pair |
| `pairs` | list | `[["KO","PEP"],["MA","V"],["XOM","CVX"]]` | — (not searched) | Pair tickers |
| `timezone` | str | `"America/New_York"` | — (not searched) | Declared market tz (metadata only) |

Strategy-level allocation block in the same JSON: `allocation.capital_pct = 0.20`,
`allocation.active = true` — the Pairs strategy as a whole receives 20% of total
account equity in the multi-strategy Allocator (also hard-wired in: SEPA 0.40 / SectorRotation 0.30 / Pairs
0.20 / 10% cash; risk constraints `max_single_name_pct=0.50`,
`concentration_pct=0.40`, `daily_loss_halt_pct=0.10`,).

### 5.2 Hyperopt mechanics —,

- Only params with a non-null `search` block are sampled. Trial param names are
 namespaced `"<strategy>.<name>"`: `pairs.lookback`, `pairs.entry_z`,
 `pairs.exit_z`, `pairs.capital_per_pair_pct`.
- `int` params via `suggest_int(low, high)` (both inclusive); `float` via
 `suggest_float(low, high)`.
- `suggest_with` returns ONLY the sampled keys; callers merge over
 `defaults_dict`.
- **Constraints applied after sampling, in file order**.
 Pairs has exactly one:

 ```json
 { "kind": "clamp_high", "param": "exit_z", "expression": "min(1.0, entry_z - 0.1)" }
 ```

 Semantics: `exit_z = min(sampled_exit_z, min(1.0, sampled_entry_z - 0.1))`.
 The expression is evaluated by a restricted evaluator over the sampled dict
 (variables = sampled sibling params; allowed: numeric literals, `+ - * /`,
 unary ±, `min`/`max`/`abs`; anything else errors —).
 This guarantees `exit_z <= entry_z - 0.1 < entry_z`, so config validation
 rule 5 (§4.2) can never reject a hyperopt sample.

### 5.3 Tuned-params write-back —

`write_tuned_params` copies the baseline JSON, replaces only `parameters.<k>.default`
for tuned keys (unknown key → error), preserves search ranges/constraints verbatim,
and stamps `metadata = {source:"tuned", created_at: now-UTC ISO,
tuned_from_study, tuned_from_trial}`. Output JSON `indent=2`, key order preserved.

---

## 6. Multi-symbol bar synchronization

This is the look-ahead guard at the heart of the strategy.

### 6.1 Per-bar bookkeeping —

`on_bar(bar)`:

1. If `bar.symbol` is not a leg of any configured pair → return empty slice
 immediately, no state mutated.
2. Append `bar.close` (Decimal) to `_history[symbol]` (ring, cap `lookback+1`).
3. `_last_close[symbol] = bar.close`.
4. `_last_bar_date[symbol] = bar.ts.date` — **the calendar date of the UTC
 timestamp**. No timezone conversion is applied; with
 daily bars stamped near US-market close in UTC this is the trading date.

These three updates happen **unconditionally and before** any sync check, so a
symbol's history advances even when its sibling leg is absent that day.

### 6.2 Sync rule —

After bookkeeping, iterate `config.pairs` **in configured order**; for each pair
containing this symbol:

- Evaluate the pair **only if BOTH legs' `_last_bar_date` equal the current bar's
 date** (`_pair_in_sync`,).

Consequences (all pinned by tests):

- A single leg streaming alone never triggers evaluation, no matter how many bars
 arrive.
- With both legs delivered per day, evaluation fires exactly once per pair per day:
 on arrival of the **later** leg's bar for that date (the earlier leg sees the
 sibling still stamped with yesterday's date).
- If a symbol receives two bars on the same date, the second triggers a re-evaluation
 with the duplicate close appended to history — history is NOT deduplicated.
 See I-3.
- Signals from one `on_bar` call may cover multiple pairs if the symbol is shared;
 results are concatenated in pair-config order.
- Independent pairs never cross-contaminate.

### 6.3 Replay ordering (EOD refresh path) —

When replaying historical bars into a fresh SG (the EOD intent-refresh job), bars
for all legs are merged into ONE chronological stream **sorted by `(date, symbol
ascending)`** and fed through the same `on_bar`. Ties on the
same date break by symbol lexicographic order. Per-symbol sequential streaming is
explicitly forbidden because it would let one symbol race ahead. Go's replay driver must use the same merge + tie-break to
get identical signal sequences.

---

## 7. Per-pair evaluation: OLS, z-score, state machine

All in `_evaluate_pair`, invoked only when in-sync (§6.2).

### 7.1 Warmup gate —

Snapshot each leg's history. If either leg has **fewer than `lookback`** closes →
return no signals (warmup;). Evaluation uses exactly the
**last `lookback`** closes of each leg, converted Decimal → float64. Conversion is exact-value `float(Decimal)` (round-half-even
to nearest double); Go: `decimal.Float64` equivalent.

### 7.2 OLS hedge ratio β —

- Regression: `y = a + b·x` with **x = short-leg prices, y = long-leg prices**;
 β is the slope `b`. Window = the same `lookback`
 closes; **refit on every evaluation** (every in-sync bar) — there is no separate
 refit cadence parameter.
- Formula, in float64:

 ```
 n = len(x); must equal len(y) and be >= 2, else nil
 mean_x = fmean(x); mean_y = fmean(y)
 num = Σ (x_i − mean_x)(y_i − mean_y)
 den = Σ (x_i − mean_x)²
 β = num / den; nil if den == 0 (degenerate x)
 ```

- `fmean` = arithmetic mean computed in float (sum of float64s / n). Go: plain
 `sum/float64(n)` accumulation in float64; for bit-reproducibility on
 adversarial inputs note that compensated summation uses
 `math.fsum`-style accuracy — see Open Question Q1.
- `β == nil` (degenerate) → `_evaluate_pair` returns no signals AND does not touch
 telemetry. Pinned: constant x with sloped y →
 nil; perfect line y=2x → β≈2 within 1e-9.
- No NaN can enter via the normal path (prices come from finite Decimals); there is
 **no explicit NaN/Inf check**: rely on the
 `den == 0` and `std == 0` guards only.

### 7.3 Spread and z-score —

```
spread_i = long_p_i − β · short_p_i for i in the lookback window (strict zip,
 lengths already equal)
mean = fmean(spread)
std = pstdev(spread) ← POPULATION std-dev: sqrt(Σ(s−mean)²/N)
if std == 0 → no signals (return empty; telemetry NOT updated)
z = (spread[-1] − mean) / std; spread[-1] = today's spread
```

Critical details:

- **Population** standard deviation (divide by N, not N−1) — `statistics.pstdev`. Using sample std-dev shifts every z and changes trade timing.
- The current bar's close **is included** in the window for β, mean, std, and is the
 spread whose z is measured. This is intentional ("signal at the close of bar t");
 it is not look-ahead because execution happens after the close (§10).
- `len(spread) < 2 → no signals` — unreachable in practice
 given `lookback >= 5`, but replicate the guard.
- Telemetry `_latest_z[pair.key] = z`, `_latest_beta[pair.key] = β` is recorded
 **only after all numeric guards pass**, so the telemetry maps never hold NaN/nil. Telemetry is read-side only (UI), never persisted, never
 feeds back into signal logic.

### 7.4 State machine —

States per pair: `"FLAT"`, `"LONG_SPREAD"`, `"SHORT_SPREAD"` (string constants,
persisted verbatim;).

| Current state | Condition (strict inequalities) | Action → next state | Signals emitted |
|---|---|---|---|
| FLAT | `z > entry_z` | open SHORT_SPREAD | SHORT long_leg, LONG short_leg |
| FLAT | `z < −entry_z` | open LONG_SPREAD | LONG long_leg, SHORT short_leg |
| FLAT | otherwise | stay FLAT | none |
| LONG_SPREAD | `|z| < exit_z` | close → FLAT, reason `"mean reversion"` | FLAT both legs |
| LONG_SPREAD | `z > entry_z` | close → FLAT, reason `"spread diverged"` | FLAT both legs |
| LONG_SPREAD | otherwise | hold | none |
| SHORT_SPREAD | `|z| < exit_z` | close → FLAT, reason `"mean reversion"` | FLAT both legs |
| SHORT_SPREAD | `z < −entry_z` | close → FLAT, reason `"spread diverged"` | FLAT both legs |
| SHORT_SPREAD | otherwise | hold | none |

Rules and edge cases:

- All comparisons are **strict** (`>`, `<`); `z == entry_z` exactly does not enter,
 `|z| == exit_z` exactly does not exit.
- Divergence close is a **loss cap, not a flip**: after closing, state is FLAT; a
 re-entry in the opposite direction may happen only on a **subsequent** bar. There is no auto-flip within one bar.
- When both close conditions could be evaluated, the reason string is chosen by
 re-testing `|z| < exit_z` first: `"mean reversion"` if true else `"spread
 diverged"`. (The two conditions are mutually exclusive
 given `exit_z < entry_z`, but replicate the selection expression.)
- There is **no max-holding-period, no time stop, no hard dollar stop** in the
 reference. See I-4.
- While a position is open and z stays in `[exit_z, entry_z]` (band), **no signals
 of any kind** are emitted.
- `STOP_HIT` exists in the intent enum but is never emitted by Pairs (§9.1).

### 7.5 Opening a position —

`_open_long_spread` (z < −entry_z): LONG the long_leg, SHORT the short_leg.
`_open_short_spread` (z > entry_z): SHORT the long_leg, LONG the short_leg.

Common mechanics:

1. Compute leg quantities (§8). If **either** `long_qty <= 0` or `short_qty <= 0`:
 abort the entry — return no signals AND leave state FLAT and leg positions
 untouched. (Entry retries naturally on later bars.)
2. Set `_pair_state[pair.key]` to the new state.
3. Record signed leg positions: LONG_SPREAD → `_leg_position[long_leg] = +long_qty`,
 `_leg_position[short_leg] = −short_qty`; SHORT_SPREAD → mirror signs.
4. Emit exactly 2 signals, **long_leg's signal first, short_leg's second**, both with the triggering bar's `ts`,
 `target_qty` = positive magnitude, shared `reason` string:

 - LONG_SPREAD entry: `"Pairs {long}/{short} LONG_SPREAD:: z={z:+.2f}, β={beta:.3f}"`
 - SHORT_SPREAD entry: `"Pairs {long}/{short} SHORT_SPREAD:: z={z:+.2f}, β={beta:.3f}"`

 Formatting: z with explicit sign, 2 decimals, round-half-even
 (format spec `+.2f`); β with 3 decimals (`.3f`); the literal Greek `β`
 (U+03B2) and `::` separator. Example: `z=+2.31, β=0.987`.

### 7.6 Closing a position —

`_close_pair`:

- Reason: `"Pairs {long}/{short} close ({reason}):: z={z:+.2f}"` where `{reason}`
 is `mean reversion` or `spread diverged`. No β in close
 reasons.
- Iterate legs **long_leg first, then short_leg**; for each leg with a non-zero
 `_leg_position`, set it to 0 and emit
 `Signal{side: FLAT, target_qty: 0, ts: bar ts, reason: full_reason}`. Legs already at 0 are skipped silently.
- `_pair_state → "FLAT"` **unconditionally**, even if zero signals were emitted.
- Normal close therefore emits exactly 2 FLAT signals, one per leg.

---

## 8. Position sizing —

Equal-dollar-weighted legs; β is deliberately NOT used in sizing — it lives only in
the spread definition.

```
long_price = float(_last_close[long_leg]); 0.0 if missing
short_price = float(_last_close[short_leg]); 0.0 if missing
if long_price <= 0 or short_price <= 0 → (0, 0) // aborts entry per §7.5
equity = float(equity_provider) // live pull, every time
target_value_per_leg = equity * capital_per_pair_pct / 2
long_qty = int(target_value_per_leg // long_price) // float floor-div, then int
short_qty = int(target_value_per_leg // short_price)
```

- `//` is float floor division; for the positive operands here `int(a // p)` ==
 `floor(a/p)` computed in float64. Example pinned by test: equity 100 000, pct
 0.30 → 15 000 per leg; price 97.5 → 153 shares; price 120 → 125 shares. Go: `int(math.Floor(a / p))` on float64 reproduces
 this including the float-rounding quirks (e.g. `15000/97.5` cases).
- Equity is fetched from the provider **at sizing time on every entry** — no
 caching anywhere in the SG; doubling equity between entries doubles share counts.
- The price used is `_last_close`, i.e. the leg's most recent close (today's,
 because the pair is in sync).
- Fractional shares are floored away; legs are therefore only approximately
 dollar-equal. See I-5.

Equity provider in production: a closure over the
engine portfolio returning `Decimal(str(balance_total(USD)))` for the **venue of the
first configured bar type** (all legs assumed same venue,).
In the EOD refresh path it is a constant captured at job start. (Provider
injection pattern; see I-6 for the single-venue assumption.)

---

## 9. Observability surfaces (read-side; must exist, never affect trading)

### 9.1 Per-leg intents: `evaluate_intent(as_of) → []PairsSignalIntent` —,

Called by the runner after every bar and by the
EOD refresh job. Pure read of telemetry + state; emits
**exactly 2·N intents for N configured pairs** in pair order, long leg then short
leg.

Inputs per pair: `z = _latest_z[key]` (default **0.0** if warmup),
`β = _latest_beta[key]` (default **1.0**), `pair_state` (default `"FLAT"`). `abs_z = |z|`.

State mapping (note: thresholds here use `>=`/`<=`, unlike the strict trading
comparisons — this is intentional and pinned by tests):

| pair_state | Condition | Intent state | proximity_to_trigger_pct |
|---|---|---|---|
| FLAT | `abs_z >= entry_z` | `buy` | `(abs_z − entry_z)/entry_z · 100` (nil if `entry_z <= 0`, unreachable) |
| FLAT | `abs_z >= 0.7·entry_z` | `forming` | same formula (negative value) |
| FLAT | else | `no_setup` | nil |
| LONG/SHORT_SPREAD | `abs_z <= exit_z` | `exit` | `(abs_z − exit_z)/max(exit_z, 0.1) · 100` |
| LONG/SHORT_SPREAD | else | `hold` | nil |

(;.)

`strength = strength_from_z(abs_z) = min(100, |z|/3 · 100)` — |z|=3 maps to 100,
clamped.

`_intent_generation` increments by 1 at the **top** of every `evaluate_intent`
call (before building the list) and is stamped on every intent; strictly
monotonic per SG instance, starts at 1 on first call. Not persisted in `state_dict` (resets on restart).

`PairsSignalIntent` fields — immutable value type, all fields
serialized by the runner as JSON:

| Field | Type | Value |
|---|---|---|
| `symbol` | string | leg ticker |
| `state` | enum string | `no_setup` / `forming` / `buy` / `hold` / `exit` / `stop_hit` (`stop_hit` never produced by Pairs) |
| `strength` | float | §above |
| `proximity_to_trigger_pct` | float or nil | §above |
| `updated_at` | datetime | the `as_of` argument |
| `generation` | int | monotonic counter |
| `strategy_id` | string | constant `"pairs"` |
| `pair_id` | string | `"{long_leg}/{short_leg}"` |
| `leg_role` | string | `"long"` or `"short"` |
| `z_score` | float | signed z (0.0 in warmup) |
| `z_entry_threshold` | float | config entry_z |
| `z_exit_threshold` | float | config exit_z |
| `hedge_ratio` | float | β (1.0 in warmup) |
| `half_life_days` | float | **always 0.0** — reserved, not computed. See I-7. |

### 9.2 `state_summary → map` —

JSON-serializable summary published every bar as a `StrategyStateUpdate`. Exact shape (key set pinned by):

```json
{ "pairs": [ {
 "long_leg": "KO",
 "short_leg": "PEP",
 "state": "FLAT" | "LONG_SPREAD" | "SHORT_SPREAD",
 "current_z": float | null, // null until first successful evaluation
 "current_beta": float | null, // null until first successful evaluation
 "long_leg_qty": int, // signed; negative when shorted
 "short_leg_qty": int
},... ] }
```

One entry per configured pair, in config order. Must round-trip through standard
JSON encoding.

### 9.3 Runner publishing (I-8)

Template `on_bar` (subclasses must not override): translate engine bar → platform
Bar (ts = engine `ts_event` ns → UTC,), update runner-level
`_last_close`, call `sg.on_bar`, submit each returned signal, then publish
state-summary and intents. **All publishing failures are swallowed after logging —
observability must never break trading**. Intent payloads are
JSON-serialized with stringification fallback for Decimals/datetimes.

---

## 10. Signal → order translation (runner) —

`PairsRunnerConfig`: `bar_types` (engine
subscriptions; runner-only), `pairs_spec tuple[tuple[str,str],...]`
(serialization-friendly raw form), plus `lookback`, `entry_z`, `exit_z`,
`capital_per_pair_pct`, `timezone`. `to_sg_config(equity_provider)` converts
`pairs_spec` → `[]Pair` preserving order and passes everything through verbatim
(; pinned by). There is
deliberately **no `account_size`** field anywhere (live equity only,,).

Lifecycle: `on_start` subscribes to every bar type;
`on_stop` closes all positions for every instrument.

Per signal (`_submit_for_signal`,):

1. Resolve instrument by symbol; unknown symbol → silently drop (`:124-126`).
2. Portfolio gate per **leg** (atomicity is at the SG layer only — one leg may be
 gated while the other proceeds;,).
 Gate input price = runner's `_last_close` for the symbol or 0. See I-9.
3. `LONG` → market BUY of `target_qty`; `SHORT` → market SELL (requires a MARGIN
 account; `:131-137`); `FLAT` → read the **net engine position** for the
 instrument; if 0 do nothing, else market order of `|net|` on the offsetting side
 (`:139-145`). FLAT sizes from the broker's actual net position, NOT from the
 SG's `_leg_position` — survives partial fills/manual intervention.
4. All orders: market, `TimeInForce = GTC`, integer quantity; `qty <= 0` → no order
 (`:147-163`). Log line format: `"[Pairs] {label} {qty} {instrument_id}:: {reason}"`
 with label `LONG` / `SHORT` / `FLAT (close {net_qty})` (`:164`).

Assembly: the runner subscribes to the deduplicated
set of all pair legs; live strategy id `"Pairs-001"`,
backtest id `Pairs-002` observed in run artifacts.

---

## 11. Timezone & units

- Bar `ts` is tz-aware **UTC** everywhere. Sync dates are UTC dates of those timestamps (§6.1).
- `config.timezone` default `"America/New_York"` is declared, validated by nothing,
 persisted in `state_dict`, surfaced in tests — but
 **never used in any computation**. Carry the field for config/state
 stability. (Flagged I-10.)
- Prices: USD. Equity: USD Decimal. Quantities: whole shares (int). z, β: unitless
 float64. `capital_per_pair_pct`: fraction of account equity.
- Bars are daily (`1-DAY` bar types,); the SG itself
 is cadence-agnostic — it only cares about same-date sync.

---

## 12. State persistence —

### 12.1 `state_dict` exact shape

```json
{
 "config": {
 "pairs": [["KO","PEP"],...], // list-of-2-lists, config order
 "lookback": 60,
 "entry_z": 2.0,
 "exit_z": 0.5,
 "equity_at_snapshot": 100000.0, // float(equity_provider) AT SAVE TIME
 "capital_per_pair_pct": 0.3,
 "timezone": "America/New_York"
 },
 "history": { "KO": ["98.5", "99.1",...],... }, // str(Decimal), oldest→newest, ≤ lookback+1 entries
 "last_close": { "KO": "99.1",... }, // str(Decimal)
 "last_bar_date": { "KO": "2026-06-11",... }, // ISO date
 "pair_state": { "KO|PEP": "LONG_SPREAD",... }, // key = long + "|" + short
 "leg_position": { "KO": 153, "PEP": -125,... } // signed ints
}
```

(;.) Notes:

- Decimal closes serialize via `str` — Go must emit the same canonical decimal
 string it parsed/constructed (e.g. `"100.0"` not `"100"` if the input had a
 fractional part; preserve scale).
- `equity_at_snapshot` is informational; **`load_state` never reads it** and the
 provider is never serialized. The legacy
 `account_size` key must NOT exist.
- `_latest_z` / `_latest_beta` / `_intent_generation` are intentionally NOT
 persisted — recomputed from history on the next bar.

### 12.2 `load_state(d)` —

- Rebuild `history` as ring buffers with capacity `config.lookback + 1` from the
 **current** config (if lookback shrank, oldest entries are evicted on load —
 deque semantics). Then seed empty buffers for any configured leg missing from the
 snapshot (`:469-476`).
- `last_close`: parse Decimals; `last_bar_date`: parse ISO dates (`:478-484`).
- `pair_state`: split key on the **first** `"|"` into the (long, short) tuple; then
 seed `"FLAT"` for any configured pair missing (`:485-490`).
- `leg_position`: ints; seed 0 for any configured leg missing (`:492-497`).
- Unknown extra symbols/pairs in the snapshot are kept in the maps (harmless: they
 fail the universe check at `on_bar`). The `"config"` block is entirely ignored.
- Round-trip invariant pinned by: pair_state,
 leg_position, last_close, and history lists identical after save → fresh SG →
 load.

---

## 13. register (original behavior vs proposed Go improvement)

Each item below describes the exact original behavior (which the Go port must be
able to reproduce in compatibility mode) and the sanctioned improvement.

- **I-1 — No cointegration vetting of pairs**. Original: the
 pair list is static config; no statistical test gates trading. Improvement: keep
 the static list as default; optionally compute/log an ADF or half-life diagnostic
 as telemetry only. Trading semantics unchanged unless explicitly enabled.
- **I-2 — Shared per-symbol state across pairs**. Original: `_history`, `_last_close`, `_leg_position` are keyed by
 symbol only. If one symbol appears in two configured pairs, they share one
 history (fine) but also ONE leg-position slot: a second pair's entry overwrites
 the first's recorded quantity, and a close of either pair zeroes the shared slot
 and emits one FLAT that flattens the whole net broker position. DEFAULT_PAIRS has
 disjoint symbols so this never fires in production. Improvement: Go should key
 `legPosition` by (pairKey, symbol) — or reject configs with overlapping symbols
 at validation time — while keeping symbol-keyed behavior available for
 byte-equivalence testing. The `state_dict` wire format stays symbol-keyed for
 compatibility.
- **I-3 — Duplicate same-date bars re-evaluate and pollute history** (§6.2;). Original: no dedupe; a second bar for the same (symbol,
 date) appends another close and can trigger a second evaluation. Improvement:
 optional same-date replace-instead-append guard, default off in
 compatibility mode.
- **I-4 — No time stop / hard stop** (§7.4). Original: positions are held
 indefinitely until `|z| < exit_z` or adverse re-cross of `entry_z`; a β-regime
 break that keeps z in the band parks capital forever. Improvement: optional
 `max_holding_days` (emit close with reason `"time stop"`) — must default to
 disabled.
- **I-5 — Floor sizing drift** (§8). Original: `int(value // price)` floors each
 leg independently; legs can be up to one share-price apart in notional.
 Improvement allowed: none to semantics — flooring must match. Only better
 diagnostics (log the residual cash) are sanctioned.
- **I-6 — Single-venue equity assumption**. Original:
 equity read from the venue of the first bar type. Improvement: Go runner may sum
 across venues or make the venue explicit config; default must reproduce
 first-bar-type-venue behavior.
- **I-7 — `half_life_days` always 0.0**.
 Original: reserved field, never computed. Improvement: Go may compute OU
 half-life from the spread series for the intent payload only; default 0.0.
- **I-8 — Silent observability degradation**. Original:
 publish errors are logged-and-swallowed per bar with no aggregation. Improvement:
 Go should add a counter/metric for dropped publishes (structured logging +
 Prometheus-style counter), behavior otherwise identical (never fail the trading
 path).
- **I-9 — Per-leg portfolio gating can break pair atomicity**. Original: each leg passes through
 `Portfolio.check` independently; one leg can be rejected leaving a naked
 single-leg position (the SG still believes the pair is open). Improvement: Go may
 pre-check both legs and submit only if both pass (all-or-nothing), behind a flag;
 default mode keeps per-leg gating for deterministic backtests.
- **I-10 — Unused `timezone` parameter** (§11). Original: dead config carried
 through persistence. Improvement: document as metadata-only; optionally validate
 it is a parseable IANA zone at config load. Must remain in config/state shape.

Everything not listed here is.

---

## 14. Acceptance fixtures the Go port must reproduce

Golden cases:

1. Constant-x OLS → no β → no signal.
2. y=2x → β = 2 ± 1e-9.
3. 15 bars < lookback 20 → zero signals regardless of divergence.
4. 30 single-leg bars → zero signals.
5. Warmup co-movement `100 + 0.1·i` for 25 days, then day-25 long=80/short=102.5,
 lookback 20, entry 2.0 → exactly 1 LONG(A) + 1 SHORT(B) on day 25, both
 qty > 0.
6. Mirror with long=120/short=97.5 → SHORT(A) + LONG(B).
7. Post-entry drift within band → zero additional signals.
8. Lookback 10, 15-day warmup, 1 outlier, 15 recovery days → ≥ 2 FLATs covering
 both legs once the outlier ages out.
9. Sizing: equity 100 000, pct 0.30 → LONG B qty = floor(15000/97.5) = 153,
 SHORT A qty = floor(15000/120) = 125.
10. Two pairs, only one diverges → exactly 2 signals, all on the diverging pair.
11. state_dict round-trip equality of pair_state / leg_position / last_close /
 history; `equity_at_snapshot` present,
 `account_size` absent.
12. state_summary key set and null-before-first-eval semantics; post-entry SHORT_SPREAD summary with
 long_leg_qty < 0 < short_leg_qty and current_z > 2.0.
13. Equity 4× → leg quantities 4× within floor tolerance.
14. Intent: 2 intents per pair with correct roles/pair_id/strategy_id; state mapping for z ∈ {0.3, 1.6, 2.1} FLAT and
 z ∈ {−1.5, 0.3} in-position; strength mapping
 {0→0, 1.5→50, 3→100, 5→100}; generation strictly
 increasing.
15. Runner config: pairs_spec → Pair conversion order-preserving; all scalar knobs
 pass through; no runner-only fields leak into SG config.

A golden-output harness may be produced by replaying fixture bar
streams through `PairsSignalGenerator` to dump `(signals, state_dict,
state_summary, intents)` JSON for diffing against the Go implementation.

---

## 15. Open questions

1. **mean/stddev accumulation accuracy.** Compensated summation can
 higher-accuracy summation than a naive float64 loop. For typical equity prices
 the difference is far below signal thresholds, but byte-equivalence of z at the
 ~1e-15 level near a strict threshold (`z > entry_z`) could in principle flip a
 trade. Decision needed: is "same trades on the golden datasets" the
 acceptance bar (recommended), or bit-identical floats (would require Neumaier/
 Kahan summation for full precision)?
2. **Pair with identical legs** (`Pair("A","A")`) is not rejected by validation; it
 would regress A on itself (β=1, spread≡0, std=0 → permanently no signals).
 Should it be rejected at config time, or silently no-op?
3. **`bar.ts.date` uses the UTC date.** For daily bars stamped at or after
 00:00 UTC of the *next* calendar day (some vendors stamp midnight-exclusive),
 two legs from different vendors could land on different UTC dates and never
 sync. The design assumes a single consistent bar source. Confirm the data
 layer guarantees one timestamp convention for all legs.
4. **Duplicate-bar semantics** (I-3): is replay of a corrected/amended EOD bar for
 the same date a real scenario in the Go data pipeline? If yes, decide between
 replicate (append + re-evaluate) and the improved replace-guard as default.
5. **`pair_state` key collision in `state_dict`**: keys are `"{long}|{short}"`
 split on the FIRST `|`; a ticker containing `|` would corrupt the round-trip.
 Tickers are not validated today. Should we validate tickers (`^[A-Z0-9.\-]+$`)?
6. **`load_state` with shrunken lookback** silently truncates history to the new
 `lookback+1` (deque semantics). Confirm this is the desired Go behavior versus
 rejecting a state file whose config block disagrees with the live config (the
 the embedded config is ignored entirely).
7. **Strategy instance IDs**: live uses `"Pairs-001"`,
 recorded backtests show `Pairs-002`. The ID feeds Allocator keys. Confirm the Go ID scheme (engine-assigned vs
 config-assigned) before wiring the portfolio gate.
8. **Equity provider failure mode**: if the provider errors/returns non-finite at
 sizing time, this would raise inside `on_bar` (no guard). A production-grade
 bar) should presumably catch, log, and skip the entry — but that is a semantic
 deviation under. Decide: replicate raise-through, or tag as an
 approved IMPROVE with skip-entry semantics.
