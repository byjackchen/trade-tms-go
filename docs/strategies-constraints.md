# Strategy Research Constraints

> **Purpose:** A reference for *strategy research*. Before investing time in a strategy idea,
> check it against the hard limits below. If an idea depends on something marked ❌, it
> **cannot be implemented or backtested** on this platform without first building new
> infrastructure — don't research it as if it were ready to go.
>
> Scope: covers the **data layer**, the **backtest/execution engine**, and the **live/paper
> data + order interface (moomoo OpenD)**. All claims are grounded in source; file:line
> references are given so you can re-verify. Last verified against commit `9cefd76`
> (2026-06-18).

---

## 0. TL;DR — Strategy ideas that are OUT OF SCOPE today

Don't research these without scoping new infra first; the platform has no path to test them:

| Idea / requirement | Why it's blocked | Layer |
|---|---|---|
| **Anything intra-bar / tick / quote-driven** | Engine is bar-closed only; no tick or quote stream | Engine + Live |
| **HFT / sub-minute** | Smallest bar is 1-minute (live) / 1-minute (intraday DB) | Data + Live |
| **Order-book / microstructure / spread capture** | No bid/ask, no Level-2, no VWAP field | Data + Live |
| **Limit / stop / bracket / trailing-stop order logic** | Engine + broker submit **MARKET orders only** | Engine + Live |
| **Options / futures / crypto / FX / non-US** | Data is US equities + ETFs only; live trades US equities only | Data + Live |
| **Strategies needing realistic slippage/impact by default** | Slippage & commission default to **0**; no market-impact or ADV model | Engine |
| **Large-size / illiquid-name execution realism** | Fills assume **unlimited liquidity**; no partial-fill/lot/ADV caps | Engine |
| **Margin / leverage-dependent sizing** | Account is **zero-margin** (Free == Total); no margin model | Engine |
| **Short-locate / borrow-cost-aware shorting** | Shorting allowed mechanically, but no locate/borrow-fee model | Engine |
| **Weekly / monthly / quarterly bars** | No higher-timeframe aggregation; daily is the coarsest bar | Data |
| **Dividend-reinvestment / dividend-capture** | `dividends` column stored but **not consumed** anywhere | Data |
| **Win-rate / profit-factor / turnover-optimized research** | Engine reports Sharpe/Calmar/MaxDD only; no win-rate/turnover | Engine |
| **Cron/calendar/intraday-scheduled rebalancing** | Engine is bar-driven only; scheduling lives in a separate daemon | Engine |

What you CAN research freely: **daily (and 1-min intraday) long/short equity & ETF strategies,
bar-close signals, market-order entries/exits, strategy-evaluated stops, portfolio-level
risk gating, regime/market-cap/earnings-aware logic.**

---

## 1. Data layer constraints

**Sources:** Parquet cache (`internal/data/sharadar/parquet.go`), TimescaleDB
(`tms.bars_daily`, `tms.bars_intraday`, `tms.fundamentals_sf1`, `tms.events`), bootstrapped
from Nasdaq Data Link / SHARADAR.

### Bar contract — OHLCV only
`Bar` (`internal/domain/bar.go:21-29`) is **strictly** `Symbol, TS, Open, High, Low, Close, Volume`.
- Prices are `Price` = 1e-4 fixed-point int64 (source is 2-decimal). Volume is `int64`.
- `TS` is **enforced UTC** (`bar.go:40-41` rejects non-UTC).
- ❌ No bid/ask, no VWAP, no spread, no trade count, no per-bar fundamentals on the bar.
  Anything beyond OHLCV must come through the context channel (§1.4) or be computed by the strategy.

### Frequencies
- ✅ Daily (`tms.bars_daily`) — primary cadence.
- ✅ Intraday DB table (`tms.bars_intraday`) stores 1/3/5/15/30/60-min widths.
- ⚠️ **Live feed** maps only **daily, 1, 5, 15, 30, 60-min** (`internal/broker/moomoo/convert.go:79-96`,
  `KLTypeForSeconds`). **3-minute is NOT available live** even though the DB table can hold it.
- ❌ No tick. ❌ No seconds bars. ❌ No weekly/monthly/quarterly aggregation.

### Asset universe
- ✅ US common stock (SHARADAR SEP, ~3,500 tickers) + US ETFs/funds (SFP, ~6,500).
- Universe is **survivorship-bias-free** via `ListUniverseForWindow(...)`
  (`internal/data/universe/store.go:55-77`) — keyed on first/last price date.
- ❌ No options, futures, crypto, or FX anywhere in the data layer.
- ⚠️ Live runs cap the universe (`TMS_LIVE_UNIVERSE_LIMIT`, default ~85 by market cap);
  backtest/hyperopt/EOD are uncapped (`internal/runner/assembly.go`).

### History depth
- Backtest: no hard cap; bounded by what's cached.
- SEPA screener uses a rolling buffer of `history_max_bars = 260` per ticker (not tunable).
- Warmup depth: SEPA 400 calendar days; Sector ≈ `lookback + 63` td; Pairs `lookback + 20` td.

### Fundamentals / corporate data (via context, not bars)
- ✅ Market cap — `tms.fundamentals_sf1`, quarterly, default dimension `MRT`
  (`MarketCaps(...)`, `internal/data/universe/store.go`).
- ✅ Earnings dates — `tms.events`, used for an earnings **blackout** flag (fixed ±5 calendar days).
- ✅ Regime — derived from SPY 200-day MA + slope (`internal/riskgate/context_*`), values like
  `bull`/`bear`/`neutral`/`warning`.
- ✅ Splits — already baked into adjusted OHLC.
- ❌ Dividends — column exists but is **never consumed**; no dividend-aware logic possible.
- ❌ No sector/industry classification surfaced to strategies, no analyst/estimate/sentiment data.

### How non-OHLCV data reaches a strategy
Only via `ContextConsumer.InjectContext(StrategyContext)` (`internal/engine/strategy.go`):
```
StrategyContext{ Regime string; AsOf time.Time;
                 MarketCapUSD map[string]float64; EarningsBlackout map[string]bool }
```
That is the **entire** out-of-band data surface. If your idea needs a field not in `Bar` or this
struct, it's not available without new plumbing.

### Look-ahead & missing data
- Look-ahead protection is real but **context-level**: the `ContextProvider` only reads
  `date <= ts` (`internal/riskgate/context_provider.go`), and regime recomputes on the SPY
  heartbeat bar. There is **no automatic** look-ahead guard inside indicator math — the strategy
  must not peek forward itself.
- NaN/missing prices flow through as NaN/NULL; **no auto-fill/interpolation**. Strategies decide
  how to skip/handle. Unknown tickers return empty (no error).

---

## 2. Backtest / execution engine constraints

**Sources:** `internal/core/loop.go`, `internal/engine/engine.go`, `internal/exec/*`,
`internal/accounting/*`, `internal/riskgate/*`, `internal/metrics/metrics.go`.

### Event model — bar-closed, single goroutine
- Single-goroutine deterministic loop (`internal/core/loop.go`); events are `KindBar`,
  `KindFill`, `KindSample`, ordered by `(timestamp, kind-priority, seq)`.
- `OnBar` fires **once per closed bar**. ❌ No reaction to intra-bar price movement, no
  intra-bar events, no depth changes. You only ever see O/H/L/C of a completed bar.

### Order types — MARKET only
- The engine and `SimExecutor` accept **MARKET orders exclusively**; non-market is rejected
  (`internal/exec/executor.go:94-96`). `OrderSubmitter` exposes only `SubmitMarket` /
  `SubmitMarketSignal` (`internal/engine/strategy.go`).
- `Limit`/`StopMarket`/`StopLimit` enum values exist as **placeholders only** — not executable.
- All orders are **GTC**; no DAY/IOC/FOK.
- ❌ No order amend/cancel, no bracket/OCO, no trailing stop. Stops are **strategy-evaluated**
  (you compute the stop and emit a market exit on the bar it triggers).

### Fill model — deterministic, zero-cost by default
Two profiles (`internal/exec/fillmodel.go`):
- **Close-fill** (`ProfileCloseFill`): same-bar fill at close; deterministic depth-walk; **0 slippage, 0 commission**. Used for golden/unit determinism.
- **Realistic** (`ProfileRealistic`): **next-bar open** fill, with *configurable* slippage (bps)
  and commission (per-share or notional bps) — **but both default to 0**.
- ❌ No volume/ADV cap (orders always fully fill), ❌ no market-impact model, ❌ no partial-fill
  by quantity, ❌ no lot-size/min-size check. **Implication:** backtests assume unlimited
  liquidity and will *overstate* fills for large size or illiquid names unless you explicitly
  configure slippage/commission — and even then there's no size-dependent impact.

### Position & account model
- Long and short both supported via signed quantity; one netting position per `(strategy, symbol)`.
- ❌ **Zero-margin**: `Free == Total`, `Locked == 0` (`internal/accounting/account.go`). No margin
  requirement, no buying-power constraint, no leverage model — sizing can use 100% of cash.
- ❌ No short locate / borrow-fee / short-utilization model.
- No first-class tranche/scale-in primitive; you emit multiple intents and the netting book
  averages cost.

### Risk gate (what protects you pre-trade)
Pre-trade gate, first rejection wins (`internal/riskgate/risk_constraints.go`, `allocator.go`):
- **Per-strategy capital budget** (default 40% SEPA / 30% Sector / 20% Pairs / 10% cash slack).
- **Daily loss halt** (default 5% of NAV).
- **Max single-name gross** (default 20% of NAV).
- **Cross-strategy net concentration** (default 30% of NAV).
- FLAT / `qty<=0` bypasses gates (closing always allowed).
- ❌ No per-order max-size cap, no daily order-count cap, no sector/industry concentration limit,
  no graded warnings (hard reject only), no hot-reload of constraints.

### Performance metrics produced
`internal/metrics/metrics.go` outputs: `final_balance`, `total_pnl`, **Sharpe** (252-annualized,
rf=0), **Calmar**, **max_drawdown_pct**, order counts, plus a daily total-equity curve and
per-strategy curves (`internal/accounting/sampler.go`).
- ❌ No win rate, profit factor, turnover, longest-losing-streak, Sortino, monthly/weekly
  breakdowns, per-symbol alpha attribution, or cost (slippage-vs-commission) decomposition.
  **Implication:** any research that ranks/optimizes strategies on those metrics needs new
  reporting code first.

### Scheduling / rebalancing
- Engine is **bar-driven only**. "Monthly rebalance" etc. is implemented *inside* a strategy by
  detecting a month change in `OnBar` — there is no engine-level scheduler.
- ❌ No cron/calendar/intraday scheduling in the engine (a separate scheduler daemon exists but is
  not the backtest path). The backtest's end-of-run liquidation is a *terminal* close-out, not a
  recurring intraday EOD-flat behavior.

### Multi-strategy
- All strategies share **one account book** and **one cash balance** (cross-strategy netting).
  One strategy's losses shrink others' budgets. ❌ No sub-account isolation, ❌ no dynamic budget
  reallocation, ❌ no cross-strategy priority/hedging.

---

## 3. Live / paper data + order interface (moomoo OpenD)

**Sources:** `internal/runner/live.go`, `internal/runner/feed.go`,
`internal/broker/moomoo/{client,convert,trade}.go`, `internal/exec/moomoo/executor.go`,
`internal/livengine/*`.

### Realtime data
- Source is moomoo OpenD `Qot_*` push (`Qot_UpdateKL`). ✅ K-line bars only.
- ❌ No tick push, ❌ no standalone quote push, ❌ no Level-2 depth (the protobuf defines Level-2
  but it's unused).
- Live bar widths: daily, 1, 5, 15, 30, 60-min (`convert.go:79-96`). **No 3-min, no seconds.**
- Intraday bars are aggregated **broker-side**; the client only does close-detection on the
  forming bar (`internal/runner/feed.go`) and emits the completed bar.

### Order interface
- Same as the engine: **MARKET orders only**, **GTC** (`internal/exec/moomoo/executor.go`,
  `internal/broker/moomoo/trade.go`).
- Idempotent submit keyed on `ClientOrderID` (reconnect won't double-submit). Broker rejects
  surface as `ErrOrderRejected`.
- ❌ No automatic retry of rejected orders, ❌ no auto-flatten on broker reject (manual).

### Run modes (`internal/runner/live.go`)
| Mode | Executor | Account | Behavior |
|---|---|---|---|
| `signal` | NoopExecutor | none/paper | Records intent only; **never places orders**; net position always 0 |
| `paper` | MoomooExecutor (TrdEnvSimulate) | simulated | Real order plumbing, sim account |
| `live` | MoomooExecutor (TrdEnvReal) | real | Real money; behind a 4-gate guard |
- **Live 4-gate** (all required, not bypassable): real `BrokerAccID`, exact confirmation phrase,
  matching `TraderID`, successful broker `UnlockTrade`.
- ⚠️ Mode change triggers a graceful session restart (not a hot swap); enabling live is decided at
  startup gating.

### Markets
- ✅ **US equities only** (`USMarket = QotMarket_US_Security`, `convert.go:42-53`).
- ❌ HK / CN / futures / options are defined in the protobuf but **not wired** — cannot trade live.

### Account / order feedback
- ✅ Real-time position, funds, order list, fill list queries; order/fill status pushed
  (`Trd_UpdateOrder` / `Trd_UpdateOrderFill`) into the same accounting book as backtest.
- Crash recovery is cold-start: restore positions from broker (`RestoreFromBroker`); no in-process
  position persistence across crash.

### Operational hard limits
- OpenD must be **local/mock** (`TMS_MOOMOO_ADDR`, default `127.0.0.1:11111`); no remote
  commercial OpenD wiring.
- **One bound account per node**; ≤ ~100 subscriptions per connection (with a safety margin),
  which is what drives the live universe cap.
- Backtest ↔ live **consistency is a tested invariant** (`internal/livengine/consistency_test.go`):
  identical strategy/portfolio/context/warmup code on both paths; only executor + sink differ.

---

## 4. Pre-research checklist

Before researching a strategy, confirm every box — any ❌ means scope new infra first:

- [ ] Signals computable from **bar-close OHLCV** (+ regime / market-cap / earnings-blackout context)?
- [ ] Timeframe is **daily or ≥1-min** (and ≥5-min if it must run live)?
- [ ] Instruments are **US equities / ETFs**?
- [ ] Entries/exits expressible as **market orders** (stops computed by the strategy)?
- [ ] Sizing does **not** rely on margin/leverage or short borrow costs?
- [ ] Backtest realism doesn't hinge on **liquidity/impact/partial fills** (or you'll configure
      slippage/commission and accept no size-impact model)?
- [ ] Success metrics are **Sharpe / Calmar / MaxDD / equity curve** (not win-rate/turnover/etc.)?
- [ ] Rebalance cadence is expressible as **bar-driven** logic inside `OnBar`?
- [ ] No dependency on **dividends, options, order-book, VWAP, or sub-minute** data?

---

*Re-verify these limits against current source before relying on them; the engine has explicit
"extension point" hooks (limit/stop orders, fill cost models) that may be implemented later.*
