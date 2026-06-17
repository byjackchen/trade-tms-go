# SPEC: Domain Value Objects & Numeric Model (`domain-types-money`)

This repo's definition of the domain value objects and numeric model. The
rules below are invariants of this system, including edge cases,
truncation/rounding mode, ordering, and defaults. Where a known weakness is
called out, the better behavior this repo adopts is described alongside it.

---

## 1. Numeric model conventions (project-wide)

| Quantity kind | Type | Where converted | Rule |
|---|---|---|---|
| Prices (open/high/low/close, stop, pivot, entry, last_close) | `decimal.Decimal` | at the market-data boundary | Constructed via `Decimal(str(x))` from the source `Price` object, never from raw float —. Because the source `Price` has `precision=2` for equities (see §7.2), `str(bar.open)` is a 2-decimal string; the resulting `Decimal` is exact to 2 dp. |
| Share counts (qty, target_qty, position) | `int` | everywhere | Always whole, signed integers. Positive = long, negative = short, 0 = flat. Order quantities are `Quantity.from_int(...)`. |
| Volume | `int` | `int(bar.volume)` at translation | truncating cast from the source `Quantity`. |
| Timestamps | tz-aware UTC `datetime` | from `ts_event` (int ns since epoch) | See §1.1. |
| Equity / NAV | `Decimal` | `Decimal(str(account.balance_total(USD).as_decimal))` | pulled live via a no-arg `equity_provider` closure at *sizing time*, never cached. |
| Strategy-internal indicator math (SEPA klines, sizing, OLS, z-score) | `float` (float64) | SGs cast `Decimal -> float` on entry into math | all SEPA/Pairs/Rotation indicator and sizing arithmetic is IEEE-754 float64. Go must use `float64` for these paths to produce values that are bit-reproducible across platforms (arm64 vs x86). Do NOT "upgrade" them to decimal — that changes entry/exit decisions. |
| JSON serialization of Decimal/datetime | string | `json.dumps(..., default=str)` | Decimals serialize as their exact decimal string (e.g. `"123.45"`), datetimes as ISO-8601 with offset (e.g. `"2024-01-02 00:00:00+00:00"` — the string form uses a space separator, not `T`). |

### 1.1 Timestamp conventions

- All `Bar.ts` / `Signal.ts` / intent `updated_at` values are **tz-aware UTC**
 (docstring, construction at).
- Conversion from integer nanoseconds:
 - `_to_my_bar`: `datetime.fromtimestamp(bar.ts_event / 1e9, tz=UTC)` —.
 - `_publish_intent`: `datetime.fromtimestamp(ts_event / 1_000_000_000, tz=UTC)` —.
 - dumper: `datetime.fromtimestamp(evt.ts_event / 1e9, tz=UTC).isoformat` —.
- Dividing int-ns by float `1e9`, which loses
 sub-microsecond precision for ns values > 2^53 (timestamps after ~1970+104
 days at full ns resolution; in practice any modern epoch-ns value cannot be
 represented exactly in float64). For daily bars (midnight-aligned) the loss
 is zero in practice, so behavior is indistinguishable. This repo uses exact integer arithmetic `time.Unix(ns/1e9, ns%1e9).UTC`, which is
 byte-identical for all bar timestamps that are whole microseconds.
- Strategy-local session logic (ORB) converts UTC to exchange-local via IANA
 zone `America/New_York`.
 session date = the *local* calendar date after conversion.

### 1.2 Float→Decimal and Decimal→float bridges

 The exact bridging idioms (these decide tie-breaking digits):

- `Decimal(str(float_value))` — e.g. stop price persisted after float math:
 `self._stop_price = Decimal(str(stop_f))`.
 Go equivalent: format the float with Go's shortest-round-trip formatting
 (`strconv.FormatFloat(x, 'g', -1, 64)` is the shortest-round-trip form for all
 finite values) then parse as decimal.
- `float(decimal_value)` — e.g. `close_f = float(bar.close)`.
- `Decimal(str(config_float))` when a config float enters Decimal math:
 `Decimal(str(self.config.hard_stop_pct)) / Decimal(100)`,
 `Decimal(str(self.budget_pct(...)))`,
 `Decimal(str(self.config.daily_loss_halt_pct))`.

### 1.3 Rounding/truncation semantics

- `round(x, 4)` is **banker's rounding (half-to-even) on the binary float**,
 used for SEPA stop computation.
 Go's `math.Round` is half-away-from-zero — do not use it.
 Use a correctly-rounded half-even round at the decimal digit, implemented over the binary double; the practical equivalent is
 `strconv.FormatFloat(x, 'f', 4, 64)` is NOT equivalent (it rounds
 half-to-even on decimal digits of the shortest repr — verify with golden vectors). Provide a `round4(x float64) float64` helper validated by a golden-file test.
- `int(x)` / `int(a // b)` truncates toward zero (a//b on positive floats is
 floor; all uses here have positive operands so floor == truncate).
 Used in every share-sizing path.
 Go: `int(math.Floor(a / b))` for positive operands.
- `int(tt.passing_rules / 8 * 100)` — truncation, not rounding.
- `statistics.fmean` = float64 sum / n; `statistics.pstdev` = **population**
 standard deviation (divide by N, not N−1).
 — using sample stddev shifts every z-score.

---

## 2. Core value objects

All are immutable value types unless noted. Field order matters for
JSON round-trips (declaration order is preserved).

### 2.1 `SignalSide` —

String enum (StrEnum: the value *is* the string):

| Member | Value | Notes |
|---|---|---|
| `LONG` | `"LONG"` | maps to broker BUY |
| `FLAT` | `"FLAT"` | close-everything |
| `SHORT` | `"SHORT"` | only used by Pairs; requires MARGIN account |

 This single enum is shared by all four strategies and by
`ProposedOrder`.

### 2.2 `Bar` —

Frozen. The project-wide bar contract (reused by Pairs, SectorRotation,
IntradayBreakout —,,).

| Field | Type | Invariant |
|---|---|---|
| `symbol` | `str` | plain ticker, e.g. `"AAPL"` (no venue suffix) |
| `ts` | `datetime` | tz-aware UTC, |
| `open` | `Decimal` | 2-dp exact (from the source Price str) |
| `high` | `Decimal` | 〃 |
| `low` | `Decimal` | 〃 |
| `close` | `Decimal` | 〃 |
| `volume` | `int` | truncated from the source Quantity |

Translation from a source bar:
`symbol = str(bar.bar_type.instrument_id.symbol)`;
`ts = fromtimestamp(ts_event/1e9, UTC)`; OHLC via `Decimal(str(...))`;
`volume = int(bar.volume)`. symbol comes from the *bar type's*
instrument id (works for multi-instrument runners).

### 2.3 `Signal` —

Frozen. Target-position-style signal emitted by every SignalGenerator.

| Field | Type | Default | Semantics |
|---|---|---|---|
| `symbol` | `str` | — |
| `ts` | `datetime` | — | the originating bar's ts (UTC) |
| `side` | `SignalSide` | — |
| `target_qty` | `int` | — | signed by convention (positive long, negative short, 0 flat). In practice all emitters pass positive magnitudes for LONG/SHORT and 0 for FLAT — except ORB FLAT, see below. |
| `reason` | `str` | — | human-readable; exact format strings documented in §2.3.1 |
| `confidence` | `float` | `1.0` | currently never set ≠ 1.0 |
| `grade` | `Grade \| None` | `None` | only SEPA sets it |
| `stop_price` | `Decimal \| None` | `None` | SEPA entry/exit and ORB entry set it |

 Edge case: ORB's `_make_flat_signal` emits
`side=FLAT, target_qty=self._position_qty` (the *held* qty, not 0) —. SEPA/Pairs/Rotation FLAT
signals carry `target_qty=0`. Runners ignore `target_qty` on FLAT (they close
the live net position instead, §8.4), so behavior is identical, but the field
value differs and is visible in logs/serialized intents.

#### 2.3.1 Reason string formats (exact, used in logs and run dumps)

- SEPA entry —:
 `"SEPA {grade}:: stage=2, TT pass, VCP {n} contractions (last {pct}%), pivot ${pivot:.2f} -> close ${close:.2f}, stop ${stop:.2f}"`
- SEPA stop exit —:
 `"SEPA stop hit at ${stop:.2f}:: close ${close:.2f}"`
- Pairs open —:
 `"Pairs {long}/{short} LONG_SPREAD:: z={z:+.2f}, β={beta:.3f}"` (or `SHORT_SPREAD`)
- Pairs close —:
 `"Pairs {long}/{short} close ({reason}):: z={z:+.2f}"` where `reason` ∈
 {`"mean reversion"`, `"spread diverged"`}
- Rotation close —:
 `"Sector Rotation rebalance:: closing {sym} (was {qty} sh, no longer in top-{k})"`
- Rotation entry —:
 `"Sector Rotation rebalance:: top-{k} entry, {lookback}-bar return {pct:+.2f}%"`
- ORB entry —:
 `"ORB breakout: close {entry} > range_high {high}, vol {vol} > avg {avg:.0f} * {mult}:: stop {stop}, target {target}"`
 (Decimal values render via `str(Decimal)`)
- ORB exits —:
 `"EOD exit at {HH:MM}"`, `"stop hit at {stop}"`, `"target hit at {target}"`,
 `"session boundary"`.

### 2.4 `Grade` and `SetupInputs` —

- `Grade = Literal["A+", "B", "skip"]` (string union,).
- `SetupInputs` (frozen): `trend_template_pass: bool`, `earnings_pass: bool`,
 `stage: str`, `catalyst: bool`, `vcp_contraction_count: int`, `regime: str`.
- `grade_setup` rules, in order:
 1. `regime == "bear"` or `stage != "2"` → `"skip"`
 2. not (`trend_template_pass` and `earnings_pass`) → `"skip"`
 3. `vcp_contraction_count < 2` → `"skip"`
 4. `catalyst` and `vcp_contraction_count >= 3` and `regime == "bull"` → `"A+"`
 5. else → `"B"`

### 2.5 `VCPSnapshot` —

Frozen; referenced by Signal reason text and intents. Fields:
`code: str`, `contractions: list[float]` (depths %, oldest→newest),
`last_contraction_pct: float`, `pivot_price: float`,
`base_length_days: int`, `volume_dryup: bool`, `quality_score: float`,
`vol_dryup_ratio: float`, `final_contraction_duration_days: int`.
(Detection algorithm itself is in the strategies spec; the *type* lives here.)

### 2.6 SignalIntent family

Shared state machine enum, replicated identically in all four intent modules:

`SignalState` StrEnum: `NO_SETUP="no_setup"`, `FORMING="forming"`, `BUY="buy"`,
`HOLD="hold"`, `EXIT="exit"`, `STOP_HIT="stop_hit"`.

Strategy ID constants: `"sepa"`,
`"pairs"`, `"sector_rotation"`, `"intraday_breakout"`. Note these are the *logical* strategy IDs
inside intents — distinct from the runner StrategyId (§8.6).

#### 2.6.1 `SEPASignalIntent` —

| Field | Type | Default |
|---|---|---|
| `symbol` | `str` | — |
| `state` | `SignalState` | — |
| `strength` | `float` | — (0..100, == grade) |
| `proximity_to_trigger_pct` | `float \| None` | — |
| `updated_at` | `datetime` (UTC) | — |
| `generation` | `int` | — |
| `strategy_id` | `str` | `"sepa"` |
| `grade` | `int` | `0` |
| `trend_template_pass` | `bool` | `False` |
| `base_age_days` | `int \| None` | `None` |
| `base_depth_pct` | `float \| None` | `None` |
| `volume_dryup` | `bool \| None` | `None` |
| `pivot_price` | `Decimal \| None` | `None` |
| `stop_price` | `Decimal \| None` | `None` |
| `rs_rank` | `int \| None` | `None` (reserved, never set) |

Evaluation logic:
- `generation` is a per-SG monotonically increasing counter incremented on
 *every* `evaluate_intent` call; it is intentionally
 mutating state inside an otherwise "pure read" (it is excluded from
 `state_dict` persistence — restarts reset it to 0).
- `pivot_price`/`stop_price` included only when `> 0`, else `None`.
- `< 50` bars of history → `NO_SETUP`, strength 0, grade 0.
- held and `last_close < stop` → `STOP_HIT` (strict `<`;).
- held otherwise → `HOLD`, strength `50.0`, `trend_template_pass=True`, grade 0.
- flat: `tt_grade = int(passing_rules / 8 * 100)`; VCP recomputed on the FULL
 history *including the current bar* (unlike entry logic, which excludes it;
 vs); TT fail → `NO_SETUP` with
 strength=grade=tt_grade; pivot>0 and `last_close >= pivot` → `BUY` with
 `proximity = (last_close - pivot)/pivot*100` (float of a Decimal expression);
 else `FORMING` with proximity only when pivot>0.

#### 2.6.2 `PairsSignalIntent` —

Fields after the 6 shared ones: `strategy_id="pairs"`, `pair_id: str = ""`
(format `"{long_leg}/{short_leg}"`), `leg_role: Literal["long","short"]="long"`,
`z_score: float = 0.0`, `z_entry_threshold: float = 2.0`,
`z_exit_threshold: float = 0.5`, `hedge_ratio: float = 1.0`,
`half_life_days: float = 0.0` (reserved, always 0.0).

`strength_from_z(z_abs) = min(100.0, abs(z_abs)/3.0*100.0)`.

Evaluation:
2N intents per call (one per leg per pair, ordered long leg then short leg, in
config pair order). Warmup defaults z=0.0, β=1.0, state FLAT (`:356-358`).
State mapping: FLAT & `|z| >= entry_z` → BUY, proximity
`(|z|-entry_z)/entry_z*100`; FLAT & `|z| >= 0.7*entry_z` → FORMING (same
proximity formula, negative); else NO_SETUP, proximity None. In-position:
`|z| <= exit_z` → EXIT with proximity `(|z|-exit_z)/max(exit_z,0.1)*100`,
else HOLD with None (`:361-383`).

#### 2.6.3 `SectorRotationIntent` —

Extra fields: `strategy_id="sector_rotation"`, `momentum_score: float = 0.0`,
`rank: int = 0` (1=best; 0=unranked/warming up), `target_weight: float = 0.0`,
`current_weight: float = 0.0`.

`strength_from_rank(rank, total)`:
`total<=1 or rank<=1` → `100.0` if rank==1 else `0.0`; `rank>=total` → `0.0`;
else `max(0.0, 100.0 - (rank-1)/(total-1)*100.0)`.

Evaluation:
if ANY universe symbol lacks `lookback+1` closes, ALL intents are NO_SETUP with
rank 0 (`:255-269`). Otherwise: returns from history first vs last close
(`old<=0` → 0.0); ranking `sorted(universe, key=returns, reverse=True)` —
**The sort is stable**: ties preserve universe declaration order
(`:280-281`). States: in-top & held → HOLD; in-top & !held → BUY;
!in-top & held → EXIT; `rank <= top_k+2` → FORMING; else NO_SETUP.
`proximity = (top_k - rank)/max(n,1)*100`; `target_weight = 1/top_k` when in
top else 0; `current_weight = qty*last_close/equity` (0 if equity<=0).

#### 2.6.4 `IntradayBreakoutIntent` —

Extra fields: `strategy_id="intraday_breakout"`, `orb_high: Decimal|None`,
`orb_low: Decimal|None`, `atr_at_open: Decimal|None` (reserved, always None),
`entry_window_end: datetime|None` (UTC).

Evaluation: `entry_window_end` = session date + `eod_exit_time` in local
tz, converted to UTC. Range not locked → NO_SETUP; held → HOLD strength 100;
flat past EOD → NO_SETUP; flat with no last close → FORMING strength 50;
`last > orb_high` → BUY strength 100, proximity `(last-orb_high)/orb_high*100`;
else FORMING with strength = position-in-range
`clamp((last-orb_low)/(orb_high-orb_low)*100, 0, 100)` (50.0 if zero-width)
and the same (negative) proximity formula.

### 2.7 `ProposedOrder` —

Frozen. The pre-risk-gate order intent.

| Field | Type | Semantics |
|---|---|---|
| `strategy_id` | `str` | runner strategy id `str(self.id)` (§8.6), NOT the logical id |
| `symbol` | `str` |
| `side` | `SignalSide` |
| `qty` | `int` | absolute magnitude (positive); side encodes direction |
| `price` | `Decimal` | estimated fill price for sizing = `_last_close.get(symbol, Decimal(0))` falls back to 0 when no close seen |
| `ts` | `datetime` | bar timestamp (daily-loss halt windowing) |

### 2.8 `RiskDecision` —

Frozen: `approved: bool`, `rule_name: str = ""`, `reason: str = ""`.
Constructors `approve` and `reject(rule=, reason=)`. Known rule names
 (they appear in logs/tests,):
`"allocator.unregistered_strategy"`, `"allocator.budget_exceeded"`, `"risk.daily_loss_halt"`,
`"risk.max_single_name"`, `"risk.concentration"`.

### 2.9 `AccountSnapshot` —

Frozen. Read-only account view consumed by the risk pipeline.

| Field | Type | Default | Semantics |
|---|---|---|---|
| `nav` | `Decimal` | — | total account value |
| `cash` | `Decimal` | — | set equal to `nav` by the glue (see below) |
| `realized_pnl_today` | `Decimal` | — | default `Decimal(0)` from glue |
| `unrealized_pnl_today` | `Decimal` | — | default `Decimal(0)` from glue |
| `positions` | `dict[(strategy_id, symbol)] -> int` | `{}` | signed shares; 0/missing = flat |
| `last_close` | `dict[symbol] -> Decimal` | `{}` |

Derived methods:
- `total_pnl_today = realized + unrealized`.
- `strategy_position(sid, sym)` → `positions.get((sid,sym), 0)`.
- `net_position_across_strategies(sym)` → sum of qty over all keys with that
 symbol.
- `gross_exposure_for_strategy(sid)` → `Σ |Decimal(qty)| * last_close.get(sym, Decimal(0))`
 over that strategy's non-zero positions. Missing
 last_close prices contribute 0 — (positions with unknown
 price are invisible to the budget).

Construction from engine state:
- `nav = Decimal(str(portfolio.account(venue).balance_total(base_currency).as_decimal))`.
- positions from `cache.positions_open`: key `(str(pos.strategy_id),
 str(pos.instrument_id.symbol))`, value `int(pos.signed_qty)` accumulated with
 `+=`; entries with `signed == 0` skipped (`:50-58`,
 tests).
- `cash = nav` ("balance_total already accounts for margin", `:62`).
 original conflates cash and NAV; Go may track true cash
 separately but the `AccountSnapshot.cash` value fed to the risk pipeline must
 remain `nav` (no rule reads `cash` today).
- `realized_pnl_today = unrealized_pnl_today = Decimal(0)` by default —
 **the daily-loss-halt rule is dormant in backtest**. Go may wire
 real intraday P&L (the rule then activates).
- `last_close` is a **copy** (`dict(last_close)`, `:66`).

### 2.10 `PortfolioHealthSnapshot` —

Frozen: `day_pnl: Decimal`, `day_pnl_pct: Decimal`, `daily_loss_halt: bool`,
`halt_headroom_pct: Decimal`, `concentration_pct: Decimal`.

Computation `Portfolio.health_snapshot`:
`day_pnl_pct = day_pnl/nav` (0 when nav<=0); `threshold = -nav *
Decimal(str(daily_loss_halt_pct))`; `halted = day_pnl < threshold` (strict);
headroom = 0 when halted else `(day_pnl - threshold)/nav` (0 when nav<=0);
concentration = max over symbols of `|Decimal(net)| * last_close.get(sym,
Decimal(0)) / nav`, zero-net symbols skipped, 0 when nav<=0.

### 2.11 Reconciliation types —

- `Mismatch` (frozen): `symbol: str`, `strategy_books_sum: int`,
 `broker_net: int`, `diff: int` (= broker_net − strategy_books_sum);
 property `diff_shares = abs(diff)`.
- `ReconciliationReport` (frozen): `ts: datetime`, `matched: list[str]`,
 `mismatches: list[Mismatch]`, `symbols_only_in_strategies: list[str]`,
 `symbols_only_at_broker: list[str]`; `has_issues` property; `summary`
 format strings at `:48-69`.
- `reconcile(...)` (`:72-131`): aggregates strategy books per
 symbol skipping zero entries; iterates `sorted(all_symbols)`;
 classification ORDER matters — `s_sum != 0 and b_net == 0` → only_in_strategies;
 `s_sum == 0 and b_net != 0` → only_at_broker; `|diff| <= tolerance_shares`
 (default 0) → matched; else mismatch.

### 2.12 `SharedContextState` —

Mutable singleton (not thread-safe; single event loop):
`regime: str = "neutral"`, `market_cap: dict[str, Decimal] = {}`,
`earnings_blackout: dict[str, bool] = {}`.
 Module-level mutable singleton; Go should make this an injected
struct guarded for concurrency (single-thread
assumption). Behavior to preserve: FundamentalsActor is the sole writer of
`market_cap`.

### 2.13 Fundamentals model

There is **no standalone `Fundamentals` type**; fundamentals = the
SF1 market-cap pipeline:

1. `load_sf1_market_caps(sf1_df, as_of, dimension="MRT") -> dict[str, Decimal]`:
 filter `dimension == "MRT"` if the column exists; coerce `datekey` to date;
 keep rows with `datekey <= as_of`, non-null `marketcap > 0`; sort by
 datekey; take the **last** row per ticker; value = `Decimal(str(marketcap))`.
2. `FundamentalsActor`
 publishes `MarketCapUpdate(ticker, float(value))` on a SPY
 daily heartbeat only when the value differs from the last published one
 (per-ticker dedup by value, `:119-121`); writes
 `shared_state.market_cap[ticker] = value` (Decimal) before publishing;
 stats counters increment only after a successful publish (`:133-136`);
 `ActorStatsUpdate` is published unconditionally per bar in a `finally`
 (`:143-147`).
3. SG side: `set_market_cap(market_cap_usd: float)` stores plain float; runner dispatch filters per
 ticker.
4. Cold-start default `market_cap_usd = 0.0` is **conservatively blocking**
 (TT rule 8 fails until first publish,).

### 2.14 Order/Fill adapter state —

`OrderTracker` (mutable, single-loop):
- Bidirectional `ClientOrderId ↔ venue order_id` map; `link` evicts stale
 pairs on BOTH sides and clears fill history when a venue id is reused
 (`:52-74`). `unlink` clears both directions plus cumulative fill state
 (`:112-137`).
- Cumulative fill state: `_fill_cum_qty: int`, `_fill_cum_notional: float`
 (notional stored, not avg price, to avoid repeated rounding — `:44-50`).
- `prior_fill` defaults `(0, 0.0)` (`:101-110`).

Cumulative→delta fill conversion: broker pushes cumulative `dealt_qty`/`dealt_avg_price`; per
Engine execution:
`delta_qty = cum_qty - prior_qty`;
`cum_notional = cum_qty * cum_avg_price`;
`delta_notional = cum_notional - prior_notional`;
`last_px = delta_notional / delta_qty` formatted `f"{last_px:.4f}"`;
emit only when `delta_qty > 0` (duplicate/regressed pushes are logged and
dropped, `:464-473`); `trade_id = f"{venue_order_id}-{ts_ns}"`;
`commission = Money(0, USD)` (TODO in source); `venue_position_id=None`
(NETTING — engine resolves). On `FILLED_ALL` the mapping is unlinked (`:474-475`).
`FILL_CANCELLED` is logged as an error but **not** reversed in the cache
(`:478-491`) — Go may implement automatic reversal, but must
still surface the loud operator-visible error and keep reconciliation
authoritative.

### 2.15 Custom Data payloads —

All carry `ts_event`/`ts_init` int-ns. Primitive-only fields (custom-data
constraint). field sets:

| Type | Fields | Cite |
|---|---|---|
| `RegimeUpdate` | `value: str` ∈ {"bull","bear","neutral","warning"} |
| `MarketCapUpdate` | `ticker: str`, `value: float` (USD) | `:45-67` |
| `EarningsBlackoutUpdate` | `ticker: str`, `value: bool`; published on transitions plus once per ticker on first observation | `:70-95` |
| `ActorStatsUpdate` | `actor_name: str`, `publish_count: int`, `last_publish_ts: int` (ns, 0=never), `last_value_json: str` | `:98-131` |
| `DataIngestionUpdate` | `source, fetch_count, cache_hit_count, cache_miss_count, last_fetch_ts` | `:134-151` |
| `BrokerConnectionUpdate` | `connected, last_ping_ts, quote_context_alive, trade_context_alive` | `:154-168` |
| `StrategyStateUpdate` | `strategy_id: str`, `state_json: str` (JSON of `sg.state_summary`) | `:171-192` |
| `SignalIntentUpdate` | `strategy_id: str`, `symbol: str`, `intent_json: str` | `:195-212` |
| `QuoteUpdate` | `symbol, last, bid, ask` (str-encoded decimals), `volume: int`, `change_pct: float`, `prev_close: str`, `market_session: str` ∈ {"pre","regular","post","closed"}, `generation: int` | `:215-238` |
| `PortfolioHealthUpdate` | `day_pnl, day_pnl_pct, daily_loss_halt, halt_headroom_pct, concentration_pct` (floats/bool) | `:241-267` |

Publication pipeline:
per bar, after signal submission: (1) `StrategyStateUpdate` with
`state_json = json.dumps(sg.state_summary, default=str)`, `ts_init = ts_event`;
(2) one `SignalIntentUpdate` per intent from `sg.evaluate_intent(as_of)`
(single object or list), `intent_json` = the intent serialized to JSON (its
fields as a map, with non-JSON-native values stringified), with
`strategy_id`/`symbol` lifted from the payload dict
(`payload.get(...)`, defaulting `""`). All exceptions in this observability
path are swallowed after logging — **strategy execution must never fail due to
observability** (`:159,196-198`).

### 2.16 Run-dump value objects —

- `StrategySummarySample`: `ts: str` (ISO 8601 UTC), `summary: dict` (`:31-43`).
- `RunDump` (`:45-75`): `start_date, end_date: str`,
 `starting_balance_usd, final_balance_usd, total_pnl_usd: float`,
 `strategies: list[str]` (actual runner ids), `kind: str = "multi-strategy"`,
 `orders/positions/account_history: list[dict]`,
 `regime_samples: dict[str,int]`, `strategy_summaries`,
 `per_strategy_equity: dict[str, list[{ts, balance_usd}]]`.
- `account_history_from_cache` (`:78-101`): iterate
 `account.events` (AccountState list, §8.5); for each, take the first balance
 whose `str(bal.currency) == "USD"`, value `float(bal.total.as_decimal)`;
 skip events without USD; `ts` = ISO 8601 from `evt.ts_event/1e9` UTC.
- File layout (`:104-169`): `runs/{YYYY-MM-DD_HH-MM-SS}/` (UTC) containing
 `meta.json` (with `version: 1` — `:28`), `orders.json`, `positions.json`,
 `account.json`, `regime_samples.json`, `strategy_summaries/{safe_id}.json`,
 `strategy_equity/{safe_id}.json`; `safe_id` replaces `:` and `/` with `_`
 (`:149`). JSON written with `indent=2, default=str`.

---

## 3. SignalGenerator config objects (validation invariants)

These are domain value objects with constructor-time invariants. Validation
errors are raised eagerly (TypeError/ValueError) — Go constructors must return
errors for the same conditions.

### 3.1 `SEPASignalGeneratorConfig` —

| Field | Type | Baseline default (`internal/hyperopt/baseline/sepa.json`) |
|---|---|---|
| `symbol` | str | — |
| `equity_provider` | `func -> Decimal` | required, must be callable (validated in SG `__post_init__`, NOT called at construction) |
| `risk_pct` | float | 1.0 — must be in (0, 100] |
| `market_cap_min_usd` | float | 500_000_000.0 |
| `hard_stop_pct` | float | 7.5 — must be > 0 |
| `pivot_buffer_pct` | float | 1.5 |
| `breakout_volume_multiple` | float | 1.5 |
| `vcp_lookback` | int | 5 |
| `history_max_bars` | int | 1000 |
| `timezone` | str | "America/New_York" |

### 3.2 `PairsSignalGeneratorConfig` —

| Field | Baseline (`baseline/pairs.json`) | Invariant |
|---|---|---|
| `equity_provider` | required | callable |
| `pairs` | `(("KO","PEP"),("MA","V"),("XOM","CVX"))` (also `DEFAULT_PAIRS`,) | non-empty |
| `lookback` | 60 | `>= 5` |
| `entry_z` | 2.0 | `> 0` |
| `exit_z` | 0.5 | `>= 0` and `< entry_z` |
| `capital_per_pair_pct` | 0.3 | in (0, 1] |
| `timezone` | "America/New_York" |

### 3.3 `SectorRotationSignalGeneratorConfig` —

| Field | Baseline (`baseline/sector_rotation.json`) | Invariant |
|---|---|---|
| `equity_provider` | required | callable |
| `universe` | 11 SPDR ETFs (XLK XLF XLE XLV XLY XLP XLU XLB XLI XLRE XLC;) | non-empty |
| `momentum_lookback` | 63 | `>= 2` |
| `top_k` | 3 | `1 <= top_k <= len(universe)` |
| `timezone` | "America/New_York" |

### 3.4 `IntradayBreakoutSignalGeneratorConfig` —

| Field | Default (in-code = baseline JSON) | Invariant |
|---|---|---|
| `symbol` | — |
| `equity_provider` | required | callable |
| `risk_pct` | 1.0 | (0, 100] |
| `range_minutes` | 30 | `>= 1` |
| `vol_multiple` | 1.5 | `> 0` |
| `profit_target_r` | 2.0 | `> 0` |
| `hard_stop_pct` | 1.0 | (0, 50] |
| `eod_exit_time` | "15:55" | HH:MM, 0<=h<=23, 0<=m<=59 |
| `timezone` | "America/New_York" | must resolve as IANA zone |

Session open hard-coded 09:30 local.

---

## 4. Sizing formulas (money math) — all float64

| Strategy | Formula | Cite |
|---|---|---|
| SEPA | `hard_stop = round(entry_f*(1-hard_stop_pct/100), 4)`; `pivot_stop = round(pivot_f*(1-pivot_buffer_pct/100), 4)`; `stop = max(hard_stop, pivot_stop)`; `tranches = 3 if grade=="A+" else 2`; `risk$ = float(equity)*(risk_pct/100)`; `full = int(risk$ // (entry−stop))`; `shares = full // max(1, tranches)`; emit only if `shares > 0`; `stop_distance <= 0` → 0 shares |
| Pairs | per-leg `target$ = float(equity)*capital_per_pair_pct/2`; `qty = int(target$ // float(last_close))`; both legs must be > 0 or no entry; prices `<= 0` → (0,0) |
| SectorRotation | `target$ = float(equity)/top_k`; `shares = int(target$ // float(last_close))`; skip symbol when price<=0 or shares<=0 |
| ORB | stop = `max(range_low, entry*(1−Decimal(str(hard_stop_pct))/100))` (Decimal math; max = *tighter* stop); reject if `stop >= entry`; `target = entry + (entry−stop)*Decimal(str(profit_target_r))`; `risk$ = float(equity)*(risk_pct/100)`; `shares = int(risk$ // float(entry−stop))`; require `shares >= 1` |

Equity is always pulled via `equity_provider` **at sizing time** and cast
`float(...)`.

SEPA breakout-volume check: `base_lookback = 60` hard-coded; require
`len(klines) >= 61`; denominator = mean of `volume[-61:-1]` (60 bars,
EXCLUDES today); `avg <= 0` → fail; pass iff
`today_vol > breakout_volume_multiple * avg` (strict >).

---

## 5. Risk-pipeline formulas (Decimal math)

Pipeline order: Allocator → RiskConstraints; first rejection wins.

- Allocator:
 - constructor: non-empty allocations; duplicate strategy_id → error;
 each `capital_pct` in (0,1]; `Σ <= 1.0 + 1e-9`.
 - FLAT or `qty <= 0` → approve. `budget = nav * Decimal(str(pct))`;
 `budget <= 0` → reject `allocator.unregistered_strategy`.
 `new_gross = gross_exposure_for_strategy + Decimal(qty)*price`;
 `new_gross > budget` (strict) → reject `allocator.budget_exceeded`.
- RiskConstraints:
 - `RiskConstraintsConfig` defaults: `max_single_name_pct=0.20`,
 `concentration_pct=0.30`, `daily_loss_halt_pct=0.05`; each must be in (0,1].
 - FLAT or `qty <= 0` → approve (closes always pass, including during halt).
 - Rule order: daily_loss_halt → max_single_name → concentration.
 - daily_loss_halt: `pnl < -nav*Decimal(str(pct))` (strict) → reject.
 - max_single_name: `held_value = |strategy_position| *
 last_close.get(symbol, order.price)`; `held_value + qty*price > nav*pct` → reject.
 - concentration: `signed_qty = +qty if LONG else −qty`;
 `new_net = net_across_strategies + signed_qty`;
 `|new_net| * order.price > nav*pct` → reject. Note: uses `order.price`
 for the whole net, not per-position closes.
- Production wiring:
 allocations SEPA 0.40 / SectorRotation 0.30 / Pairs 0.20 keyed by
 `str(runner.id)`; risk config 0.50 / 0.40 / 0.10.
- Gate glue `maybe_check_portfolio`:
 no portfolio configured → pass; rejection log format
 `"[Portfolio] REJECTED {sid}/{sym} ({side} {qty}): {rule} — {reason}"`.

---

## 6. Per-bar orchestration contract

`BaseSignalRunner.on_bar` template,
strictly in this order:
1. translate bar (§2.2);
2. `_last_close[symbol] = close` (BEFORE signal generation — gate prices see
 the current bar's close);
3. `for signal in sg.on_bar(bar): _submit_for_signal(signal)` — submission is
 interleaved per signal, in list order;
4. `_publish_state_summary(ts_event)`;
5. `_publish_intent(ts_event)`.

The SEPA universe runner overrides on_bar (multi-SG routing) but preserves the
same ordering per instrument and omits `_publish_intent` from the base path.

SG-side per-strategy on_bar invariants:
- SEPA: ignore foreign symbols; append bar first; flat → entry path, held →
 exit path, never both in one bar.
 Entry requires `len >= 200` bars; VCP runs on history EXCLUDING current bar
 and requires `len(prior) >= 30` (`:199-229`); exit on `close <= stop`
 (`<=`, not `<` — `:333`).
- Pairs: evaluate only when BOTH legs have a bar dated TODAY
 (`_pair_in_sync`,); per-symbol
 deques `maxlen = lookback+1`; β from OLS of long on short (None when x
 variance 0 or n<2, `:505-521`); `std == 0` → no signal (`:202`);
 state machine per module docstring `:8-15` (no auto-flip on divergence).
- SectorRotation: month rollover detected as
 `bar_date.month != last_universe_date.month` (year is ignored — fine for
 daily data,); rebalance
 computed BEFORE ingesting the triggering bar (prior-month-end snapshot,
 `:138-151`); requires full warmup `len >= lookback+1` for ALL symbols;
 FLAT signals (sorted symbols) precede LONG signals (sorted) (`:191,211`).
- ORB: session reset on local-date change; positions surviving a session
 boundary are flattened defensively before reset;
 bars with `local_ts < session_open + range_minutes` accumulate the range
 (strict `<`, boundary bar excluded — `:146-157`); range locks once,
 idempotently; joined-mid-session (0 range bars) → no trading that session
 (`:161-164`); exits precedence: EOD ≥ stop (`bar.low <= stop`) ≥ target
 (`bar.high >= target`) (`:254-264`); no new entries at/after EOD time.

---

## 7. Engine semantics

### 7.1 Venue/engine configuration —

| Setting | Value | Cite |
|---|---|---|
| `account_type` | `AccountType.MARGIN` (default; required for Pairs SHORT) |, test |
| `oms_type` | `OmsType.NETTING` |
| `base_currency` | currency of `starting_balance` (USD in all scripts; `Money(100_000, USD)` default in) |
| `starting_balances` | `[starting_balance]` |
| trader id | `"MULTI-STRAT-001"` default |
| venue | `Venue("SIM")` in scripts |
| default leverage | **10** for MARGIN when not specified |
| fill model | `FillModel(prob_fill_on_limit=1.0, prob_slippage=0.0)` — no slippage |
| fee model | default maker/taker fee model |
| latency model | None → orders processed immediately (same engine timestamp) |
| `bar_execution` | True (venue default) |
| `bar_adaptive_high_low_ordering` | False (venue default) |

 With zero fees on the equity instrument (below), commissions
are 0 in backtest. With leverage 10 and margin_init/maint 0, margin never
blocks an order in practice for the tested sizes.

### 7.2 Instrument definition — `TestInstrumentProvider.equity`: USD equity with
`price_precision=2`, `price_increment=0.01`, `lot_size=100`,
`size_precision=0`, `size_increment=1`, `multiplier=1`,
`maker_fee=0`, `taker_fee=0`, `margin_init=0`, `margin_maint=0`
(verified by running the venv). price precision 2 is what
makes `Decimal(str(price))` 2-dp exact (§1).

Bar type string: `f"{instrument_id}-1-DAY-LAST-EXTERNAL"`
 — EXTERNAL aggregation is required;
INTERNAL bars are NOT processed by the matching engine.

### 7.3 Bar-driven fill model

Engine main loop ordering for each data point at timestamp T:
1. advance clocks/timers to T;
2. `exchange.process_bar(bar)` — the simulated exchange ingests the bar FIRST;
3. `data_engine.process(bar)` — strategies' `on_bar` fire;
4. `_process_and_settle_venues(T)` — orders submitted during `on_bar` are
 processed (zero latency).

`process_bar` decomposes a LAST-price bar into up to 4 trade ticks applied in
order **Open → High → Low → Close** (default; with `bar_adaptive_high_low_ordering=False`; High always precedes Low); each tick updates the L1 book and runs
`iterate` (matches resting orders such as future limit/stop orders;
the current system uses market orders only). Ticks are skipped when the price
equals the current last (`_process_trade_bar_open/high/low/close`;). Volume is split: each of O/H/L ticks gets
`volume/4`; the close tick gets the remainder (`compute_bar_quarter_sizes`;).

Consequences this engine relies on:
- After step 2, the book's last/bid/ask rest at the bar's **close** price.
- A **market order submitted inside `on_bar(T)` fills at bar T's close price,
 at timestamp T** (same bar, no next-bar-open delay), with zero slippage and
 zero commission. `fill_market_order` fills marketable orders against the
 book as TAKER.
- Same-bar-close fills are optimistic (signal computed on the
 close is filled at that same close). A Go improvement would be next-bar-open
 fills; same-bar-close is the default and any alternative is gated behind config.

### 7.4 Position aggregation

- NETTING OMS assigns the venue position id
 `PositionId(f"{instrument_id}-{strategy_id}")`: exactly **one open Position
 per (instrument, strategy)**, netted across that strategy's fills. Two
 strategies trading the same instrument hold two separate Position objects.
- `Position.signed_qty`: float; positive long / negative short. The platform
 truncates to `int`.
- `cache.positions_open` returns only open positions; the glue additionally
 skips `signed == 0` and **sums** duplicates per (strategy_id, symbol) key.
- `portfolio.net_position(instrument_id)` nets across **ALL strategies** (and
 accounts) for that instrument. Runners use it to
 size FLAT close orders.
 (and caveat): a FLAT from strategy A flattens the
 cross-strategy net, which could close strategy B's exposure if universes
 overlapped. Current universes are disjoint, so behavior is identical — the
 Go engine must reproduce the cross-strategy netting semantics of
 `net_position`, not per-strategy netting. optionally add a
 per-strategy close mode behind config; default must match.
- FLAT translation: `qty = int(abs(net_qty))`; side = SELL if net>0 else BUY;
 no order when net == 0 (all four runners, cites above).

### 7.5 AccountState event semantics

- The venue account is materialized during `engine.run` (first event at run
 start carrying the starting balances) —.
- `account.balance_total(USD)` returns a `Money`; starting balance flows
 through unchanged before any fills;
 equity provider reads it as the live equity (§1).
- `account.events` is the ordered list of `AccountState` events; each carries
 `balances` (per-currency `AccountBalance` with `.total/.locked/.free`) and
 `ts_event` int-ns. The dumper builds the equity curve from `total` of the
 first USD balance per event. For a MARGIN
 account, a new AccountState is emitted whenever balances change (fills,
 PnL settlement) — the equity curve granularity equals "one point per
 balance-changing event", not per bar.
- The Go engine must emit equivalent account-state events: initial state with
 starting balance, then one per balance mutation, each timestamped with the
 causal event's ts.

### 7.6 Order semantics

All orders in the system are `MarketOrder` with `TimeInForce.GTC`, quantity
`Quantity.from_int(...)`. No limit, stop, or bracket
orders exist; stops are *strategy-evaluated* on bar closes (SEPA) or bar
extremes (ORB), and exits are market orders on the following evaluation.
SHORT = plain SELL on the MARGIN account.

Submission gating: every signal passes `_gate` (build ProposedOrder +
AccountSnapshot, run Portfolio.check) before any order is created; on rejection the signal is dropped
(SG internal state has already been mutated by the SG — this
divergence-by-design: e.g. SEPA thinks it is long even if the runner dropped
the order; the source accepts this asymmetry).

Log line formats on submission (used in tests/ops): `"[SEPA] LONG {qty} {iid}:: {reason}"`, `"[SEPA] FLAT (close {net}) {iid}:: {reason}"`,
`"[Pairs] {label} {qty} {iid}:: {reason}"`, `"[SectorRot]..."`,
`"[SEPA-Universe]..."` (cites above).

### 7.7 Strategy identity

`str(self.id)` — the runner's auto-assigned StrategyId
(`"{ClassName}-{order_id_tag}"`, e.g. `"SEPARunner-000"`) — is the canonical
key used for: ProposedOrder.strategy_id,
AccountSnapshot positions (via Position.strategy_id), Allocator table, and StrategyStateUpdate. The logical ids (`"sepa"` etc.) appear ONLY inside
SignalIntent payloads (§2.6). The Go engine must keep these two id spaces
distinct and consistent.

---

## 8. State persistence round-trips

Every SG implements `state_dict -> dict` / `load_state(dict)` for crash
recovery. Encodings:
- Decimals → `str(...)`; dates → `isoformat`; deques → list of str.
- SEPA: full klines via `reset_index.to_dict(orient="list")`; restore parses
 `index` column with `pd.to_datetime`.
 `equity_at_snapshot` is informational only — never restored.
- Pairs: `pair_state` keyed `f"{a}|{b}"`, split on first `"|"` at load; history deque maxlen
 re-derived from config.
- SectorRotation:; missing
 universe symbols re-seeded with empty deques / 0 positions.
- ORB:; `range_high=None`
 encoded as `None`, restored via `Decimal(x) if x else None`.
- `state_summary` (UI-facing, JSON-primitive only) shapes:
 SEPA (note `entry_price`/`stop_price`
 are `None` when flat; `vcp_detected = (not flat) and pivot > 0`);
 Pairs `:410-439` (z/β `None` until first computation);
 Rotation `:329-350` (only positive-qty holdings);
 ORB `:401-424`.
- `_intent_generation` and Pairs `_latest_z/_latest_beta` are NOT persisted
 (recomputed;).

---

## 9. Parameter quick-reference (baseline JSON, schema_version 1)

 allows schema_version {1}; param types
{float,int,str,list}; search ranges only on numeric params. Resolution order:
`TMS_STRATEGY_PARAMS_DIR` env (if file exists) else package baseline.

| Strategy | Param | Default |
|---|---|---|
| sepa | risk_pct | 1.0 |
| sepa | market_cap_min_usd | 500000000.0 |
| sepa | hard_stop_pct | 7.5 |
| sepa | pivot_buffer_pct | 1.5 |
| sepa | breakout_volume_multiple | 1.5 |
| sepa | vcp_lookback | 5 |
| sepa | history_max_bars | 1000 |
| sepa | timezone | America/New_York |
| pairs | lookback | 60 |
| pairs | entry_z | 2.0 |
| pairs | exit_z | 0.5 |
| pairs | capital_per_pair_pct | 0.3 |
| pairs | pairs | [["KO","PEP"],["MA","V"],["XOM","CVX"]] |
| sector_rotation | momentum_lookback | 63 |
| sector_rotation | top_k | 3 |
| sector_rotation | universe | 11 SPDR ETFs (§3.3) |
| intraday_breakout | risk_pct | 1.0 |
| intraday_breakout | range_minutes | 30 |
| intraday_breakout | vol_multiple | 1.5 |
| intraday_breakout | profit_target_r | 2.0 |
| intraday_breakout | hard_stop_pct | 1.0 |
| intraday_breakout | eod_exit_time | 15:55 |
| all | timezone | America/New_York |

Portfolio wiring (production): allocations 0.40/0.30/0.20 (10% cash slack);
risk caps 0.50/0.40/0.10.
Backtest starting balance default $100,000 USD.

---

## 10. Open questions

1. **`round(x, 4)` determinism.** Rounding is half-to-even on the *decimal*
 representation using correctly-rounded double-to-string conversion. The
 `round4` helper is validated with golden vectors, especially near ties
 (e.g. entry*0.925 products ending in...5). Recommendation: derive vectors
 from the actual Sharadar price universe used in the determinism golden.
2. **`str(float)` shortest-repr.** `Decimal(str(stop_f))` depends on the
 shortest-round-trip float formatting. Go's `strconv.FormatFloat(x,'g',-1,64)`
 produces the same shortest round-trip digits but may differ in exponent
 formatting for very small/large values (e.g. `1e-05` vs `0.00001`). Prices
 never hit those ranges in practice — confirm with golden tests or normalize.
3. **AccountState emission cadence.** The engine emits AccountState on every
 balance mutation; the dumper's equity curve granularity therefore depends
 on settlement batching. Is point-per-fill sufficient for `account.json`, or
 do we need a specific event count (e.g. one event per order with multiple
 fills)? Lock the expected sequence with a golden dump.
4. **`ts_init` vs `ts_event` for bar ordering.** The engine loop sorts by
 `ts_init`; `BarDataWrangler` sets
 `ts_init == ts_event` for daily bars with default `ts_init_delta=0`.
 The platform code reads only `ts_event`. Confirm the Go data wrangler also
 sets both equal — otherwise multi-symbol same-day ordering could differ.
5. **Same-timestamp multi-symbol bar ordering.** When several daily bars share
 one timestamp, the engine processes them in data-stream order (stable by
 insertion: per-ticker registration order). Pairs' `_pair_in_sync` and
 Rotation's rebalance-before-ingest depend on this ordering. The engine
 defines a deterministic tie-break (preserve instrument registration order),
 validated by golden tests.
6. **`Pairs._latest_z` recorded before vs after entry decision** — telemetry
 only, but `evaluate_intent` reads it; the recorded z is the value used for
 the *current* bar's decision. Confirmed from
 source; flagged here because the docstring ("recomputed each bar") could
 mislead implementers into recomputing inside evaluate_intent. Resolved —
 do NOT recompute.
7. **SEPA `evaluate_intent` VCP window asymmetry** (full history including
 current bar) vs entry path (excludes current bar) — intentional per source
 (vs `:219-227`) but unusual; confirm with author before
 "fixing" in Go. Spec'd as.
8. **Float `signed_qty` truncation.** `int(pos.signed_qty)` truncates toward
 zero; for equities signed_qty is always integral so no difference — but if
 fractional shares ever appear (broker corporate actions), truncation vs
 rounding diverges. Go should truncate to match.
9. **`net_position` return type.** `net_position` returns a decimal; runners
 compare `net_qty == 0` and call `int(abs(net_qty))`. Net position is modeled
 as an exact integer-share decimal to avoid float drift in long fill chains.
