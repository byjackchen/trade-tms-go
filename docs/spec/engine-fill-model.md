# SPEC: Engine Fill Model (`engine-fill-model`)

This repo's rule for **when** a market order fills and at **what price(s)** in
the backtest engine. This is the core of the P2 accuracy gate (locked
decision 3). Go citations are relative to this repo
(`github.com/byjackchen/trade-tms-go`).

Two fill profiles exist:
- the `close-fill` profile follows the rule below exactly;
- the production `realistic` profile deliberately differs (it is a more
 conservative, non-look-ahead model); the determinism golden never runs it.

The rule below is exercised by driving the engine over controlled bars and
observing every fill event. The probes are permanent and re-runnable:

- the canonical multi-symbol harness;
- the small-volume depth-walk probe (writes
 the golden table `internal/exec/testdata/depthwalk.json`).

---

## 1. Configuration under test

The wiring under test:

| Setting | Value |
|---|---|
| Venue | `SIM` |
| Account type | `AccountType.MARGIN` |
| OMS type | `OmsType.NETTING` |
| Base currency | `USD` |
| Instrument | `TestInstrumentProvider.equity(symbol, "SIM")` — `price_precision=2`, `price_increment=0.01`, `size_precision=0`, `size_increment=1`, zero-fee |
| Bar type | `{iid}-1-DAY-LAST-EXTERNAL` |
| Bars | `BarDataWrangler.process(df)` over raw Sharadar SEP OHLCV (rounded to 2 dp) |
| Fill model | engine default (`FillModel`), no custom slippage |
| Orders | `MARKET` / `GTC`, integer quantity, submitted in `on_bar` |

---

## 2. Fill TIMING — same bar, at the bar's timestamp

A market order submitted inside `on_bar(T)` fills **within bar T**: the
`OrderFilled` event carries `ts_event == bar.ts_event` (the same timestamp as
the bar that triggered the submission). The fill is settled before the next
data point is processed, so a strategy reading its net position later in the
same `on_bar` pass sees the updated book.

Observed (canonical harness, AAPL/KO/NVDA, 2021-01..2021-06): every one of the
10 fills carries the submitting bar's `ts_event` exactly (e.g. a BUY submitted
on the 2021-01-04 bar fills at `ts=2021-01-04T00:00:00+00:00`).

Go reproduction: `internal/engine/engine.go` `handleBar` runs strategy
callbacks, then immediately calls `executor.ProcessBar(bar)` for the
`FillThisBar` model; `internal/exec/executor.go` stamps each `Fill.TS` with the
bar's `TS`. The fill is settled synchronously via `fillSink` so same-bar
position reads are correct.

---

## 3. Fill PRICE — the bar-close depth walk

When the matching engine decomposes a daily bar into ticks for `LAST`-price
matching (`bar_execution`; `x`), it posts the bar's **CLOSE** price as
a single book level whose depth is `close_tick_vol` (§3.1). A marketable order:

- fills entirely at the **close** price while quantity remains ≤ `close_tick_vol`;
- any **residual** quantity beyond `close_tick_vol` fills at **one
 `price_increment` (0.01) adverse** to the order side — BUY at `close + 0.01`,
 SELL at `close - 0.01`. The synthetic L1 book has a single level, so the whole
 residual fills at exactly one increment away (no further walking).

```
legs(order, bar):
 ctv = close_tick_vol(bar.volume)
 if order.qty <= ctv:
 -> [ (order.qty, bar.close) ]
 else:
 residual = order.qty - ctv
 adverse = bar.close + 0.01 (BUY) | bar.close - 0.01 (SELL)
 -> [ (ctv, bar.close), (residual, adverse) ]
```

For the canonical real-data gate every order is far smaller than `close_tick_vol`
(Sharadar equity volumes are in the millions), so all 10 fills land exactly at
the bar close — e.g. `NVDA BUY 300 @ 13.11` (the 2 dp wrangle of the raw
`13.113` close). The depth-walk branch is exercised separately by §3.2.

### 3.1 `close_tick_vol` — `compute_bar_quarter_sizes`

The close-tick depth is `compute_bar_quarter_sizes(volume, min_size)`, with
`min_size = 1` share (equity `size_increment = 1`, `size_precision = 0`):

```
quarter = volume // 4
quarter = max(quarter, min_size) # floor to size_increment, then min
if 3*quarter >= volume: close = min_size # underflow guard
else: close = volume - 3*quarter (>= min_size)
```

Worked values (observed close depth from the probe, all confirmed):

| volume | quarter | close_tick_vol |
|---|---|---|
| 100 | 25 | **25** |
| 8 | 2 | **2** |
| 7 | 1 | **4** |
| 3 | 0→1 | **1** (underflow guard: `3*1 ≥ 3`) |
| 1 | 0→1 | **1** (underflow guard) |

> ⚠️ The naive `volume - 3*floor(volume/4)` is WRONG for small volumes (it
> yields 3 for `volume=3`, but the correct value is 1). The model implements the
> full `compute_bar_quarter_sizes` logic including the `max(quarter, 1)` floor
> and the `3*quarter ≥ volume` underflow guard.

Go reproduction: `internal/exec/fillmodel.go` `closeTickVolume`.

### 3.2 Depth-walk golden table

 drives a synthetic 1-share-increment instrument
over `volume ∈ {1,3,7,8,100}` × `side ∈ {BUY,SELL}` × `qty` straddling
`close_tick_vol`, capturing the exact legs the model produces. Representative
rows (bar close = 105.00):

```
vol= 8 ctv= 2 BUY qty=2 -> 2@105.00
vol= 8 ctv= 2 BUY qty=3 -> 2@105.00 | 1@105.01
vol= 8 ctv= 2 SELL qty=7 -> 2@105.00 | 5@104.99
vol=100 ctv=25 BUY qty=30 -> 25@105.00 | 5@105.01
vol= 7 ctv= 4 SELL qty=5 -> 4@105.00 | 1@104.99
vol= 3 ctv= 1 BUY qty=2 -> 1@105.00 | 1@105.01
vol= 1 ctv= 1 SELL qty=6 -> 1@105.00 | 5@104.99
```

The full table (40 cases) is committed at
`internal/exec/testdata/depthwalk.json` and asserted by the Go unit test
`internal/exec/fillmodel_depthwalk_test.go` (`TestCloseFillDepthWalkGolden`),
which runs in the default `go test ./...`. The golden table is checked in.

---

## 4. Commission — zero (zero-fee equity)

`TestInstrumentProvider.equity` has zero maker/taker fees, so every observed
fill reports `commission = "0.00 USD"`. The `close-fill` profile returns
zero commission unconditionally (`CloseFillModel.Commission`).

---

## 5. AccountState cadence

(The settled value is the invariant; the event cadence is informational.)

In some engines an `AccountState` event is emitted per fill plus one initial
state **per instrument** at run start (so a 3-instrument run emits 3–4 initial
states at the first bar's ts, all carrying the starting balance), and occasionally
a duplicate at the same ts/value during multi-fill bars. The balance carried is
**cash** (realized only): for the canonical run it steps
`100000 → 100473 → 101045 → 101264 → 101072.40 → 100872.40 → 100661.80`.

This engine emits one initial state and one per fill — a different *count*, but
the **settled balance at each unique timestamp is identical**. The comparator
therefore compares the last (settled) balance per unique ts, not the raw event
count.

---

## 6. NETTING realized-PnL attribution

Under `OmsType.NETTING` a single instrument keeps one reused `position_id`, but
each time the net quantity returns to zero or **flips through zero** the position
*instance* closes (locking its `realized_pnl`) and a new instance opens with
`realized_pnl` reset to 0. Consequences:

- A position **flip** (e.g. NVDA long 300 → SELL 500 → short 200) realizes the
 closing leg's PnL on the *closed* long instance, then opens a fresh short
 instance. `pos.realized_pnl` on the final cached snapshot reports only the
 LAST instance's realized — it does NOT carry the earlier closed instance.
- The **authentic cumulative** realized PnL for an instrument is
 `portfolio.realized_pnl(instrument_id)`, which sums across all instances.
- Per-fill realized PnL is quantized to **cents** (USD `Money` precision 2,
 `ROUND_HALF_EVEN`) on every closing fill.

Observed (canonical run): AAPL splits into two instances (`+473.00`, then
`-402.20`) summing to `+70.80`; NVDA splits across the flip (`+219.00`,
`-200.00`) summing to `+19.00`; KO `+572.00`. These cumulative-per-instrument
values, and the total PnL `+661.80`, are what the Go engine reports directly —
its `accounting.Position` tracks cumulative realized over the whole
(strategy, symbol) life, equal to `portfolio.realized_pnl`.

Go reproduction: `internal/accounting/position.go` (`closeLeg` quantizes
realized to cents per fill; flips close-then-open while accumulating realized).

> The golden `positions.json` therefore records `portfolio.realized_pnl(iid)`
> (the cumulative), NOT the final `pos.realized_pnl` snapshot, so comparisons are
> like-for-like.

---

## 7. Determinism gate summary

The golden regression pins the engine output with these tolerances:

| Field | Tolerance |
|---|---|
| Fill price (after fixed-point) | `|diff| <= 5e-5` |
| Fill qty / ts / side / ticker | exact |
| Position cumulative realized PnL | `|diff| <= 0.01 USD` |
| Account settled balance per ts | `|diff| <= 0.01 USD` |
| Per-bar total equity | `|diff| <= 0.01 USD` |
| Counts (fills, equity points) | exact |
| Ordering (fills, equity) | exact |

Every fill price/qty/ts, every per-ticker cumulative realized PnL, every settled
account balance, and all 124 equity-curve points match within tolerance (worst
equity `|diff| = 1.5e-11`).

The hermetic Go golden tests assert the committed golden dumps, so the engine,
fill model, accounting and artifact dumper are regression-guarded in the default
test run.
