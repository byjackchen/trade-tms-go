# Benchmarks

This is the permanent performance baseline for trade-tms-go. The numbers below
are produced by the in-repo Go benchmark suite via `make bench`. Every benchmark
is **hermetic** — it builds its own in-memory inputs and needs no Postgres,
Redis, or Python — so the suite runs anywhere and the numbers are reproducible.

## How to run

```sh
make bench                       # full suite, 1s/benchmark
make bench BENCHTIME=20x         # fixed iteration count (lower variance)
make bench BENCH=Engine          # only matching benchmarks
go test -run '^$' -bench . -benchmem ./internal/engine/...   # one package
```

`make bench` runs `go test -bench` with `-benchmem` over the engine, hyperopt,
livengine, data/universe, and api packages. Each benchmark reports a domain
metric (bars/sec, trials/sec, ns/bar, rows/sec, p50/p99 µs) in addition to the
standard `ns/op` and `allocs/op`.

The benchmarks live next to the code they measure:

| File | Deliverable |
| --- | --- |
| `internal/engine/bench_test.go` | (a) backtest engine throughput (bars/sec) |
| `internal/hyperopt/nsga2/bench_test.go` | (b) hyperopt trials/sec + parallel scaling |
| `internal/livengine/bench_test.go` | (c) live engine per-bar latency |
| `internal/data/universe/bench_test.go` | (d) data import rows/sec (price-bridge wrangle) |
| `internal/api/bench_test.go` | (e) API p50/p99 for the heavy endpoints |

## Machine spec (reference run)

| | |
| --- | --- |
| CPU | Apple M4 Max (16 cores: 12 performance + 4 efficiency) |
| RAM | 128 GB |
| OS | macOS 26.3.1 (darwin/arm64) |
| Go | go1.26.1 |
| Date | 2026-06-14 |

All numbers below are from this machine. They are **relative** baselines: absolute
values scale with the host, but the ratios (and especially `allocs/op`, which is
host-independent) are the signal to watch in regressions.

---

## (a) Backtest engine throughput

The deterministic event-loop engine (`internal/core` + `internal/exec` +
`internal/accounting`) running scripted strategies over a synthetic
weekday-dense feed (~252 bars/yr/instrument), one strategy per instrument,
trading every 5th bar (fills + accounting + equity sampling continuously
exercised). `nautilus-compat` fill profile.

| Benchmark | Scale | bars/sec | ns/op | allocs/op |
| --- | --- | ---: | ---: | ---: |
| `EngineThroughput_5y_5sym`   | 5 yr × 5 instr (~6.3k bars)   | **1,090,922** | 5.77 ms | 37,242 |
| `EngineThroughput_10y_20sym` | 10 yr × 20 instr (~50k bars)  | **641,714**   | 78.5 ms | 298,336 |

A real multi-year multi-strategy backtest therefore completes in **single-digit
to tens of milliseconds** of pure engine time; the wall clock of a production run
is dominated by DB load of the bars, not the loop.

### Hotspot fixed (this change)

A memory profile of the 10y×20sym run showed the equity-sampler allocation path
dominating: `Account.sortedKeys` was **27.7%** of all allocated bytes, called
fresh on every `EquitySampler.Sample` (once per trading day) via
`Account.Unrealized()`, plus a fresh sorted-key slice **per strategy** inside
`Sample`. These were pure per-bar garbage.

The fix (allocation-only, **zero semantic change**): the hot `Unrealized()` and
`Sample()` paths now sort the position keys / strategy ids **into reusable
scratch buffers** held on the `Account` / `EquitySampler` instead of allocating a
fresh slice each call. The deterministic iteration order is byte-for-byte
unchanged (the same `sortKeys` comparator drives both the allocating
`sortedKeys()` and the in-place `sortedKeysInto()`), so the summation order — and
any overflow short-circuit — is identical.

Result (before → after, same machine):

| Benchmark | bars/sec before | bars/sec after | allocs/op before | allocs/op after |
| --- | ---: | ---: | ---: | ---: |
| `5y_5sym`   | 655,865 | **1,090,922** (+66%) | 67,480  | 37,242 (−45%)  |
| `10y_20sym` | 407,508 | **641,714** (+57%)   | 510,019 | 298,336 (−42%) |

**Parity preserved.** The change was verified against the committed Nautilus
golden dump (`testdata/parity/`): running the canonical script over the golden
bars produces an `equity.json`, `account.json`, and every `strategy_equity/*`
curve that is **byte-identical** to the pre-change baseline and numerically
identical (to 1e-4) to the Nautilus reference. A permanent regression guard,
`TestEngineDeterminismMultiStrategy`, runs the multi-strategy config twice and
asserts bit-identical equity curves / balances. The full `go test -race ./...`
suite (including all strategy golden/parity tests) passes unchanged.

---

## (b) Hyperopt trials/sec + parallel scaling

Self-written deterministic NSGA-II (`internal/hyperopt/nsga2`).

**Optimizer ceiling** (near-free ZDT1 evaluator — isolates the ask/tell +
fast-non-dominated-sort + crowding overhead, 1000 trials/op):

| Benchmark | trials/sec | ns/op | allocs/op |
| --- | ---: | ---: | ---: |
| `OptimizerTrialsPerSec` | **~305,000** | 3.28 ms | 27,317 |

**Parallel scaling** (fixed CPU-bound per-trial cost ≈ a backtest evaluation;
400 trials/op, `Parallelism` = N worker goroutines per generation):

| Parallelism | trials/sec | speedup vs P1 |
| ---: | ---: | ---: |
| 1  | 7,803  | 1.00× |
| 2  | 13,709 | 1.76× |
| 4  | 25,323 | 3.25× |
| 8  | 44,290 | 5.68× |
| 16 | 64,461 | 8.26× |

Scaling is monotonic and near-linear through the 12 performance cores, then
tapers as the 4 efficiency cores join (P16 on a 12-P-core machine). Aggregation
is **completion-order-independent**: the optimizer rebuilds the population in id
order each generation, so adding workers never changes the result, only the wall
time.

**The "~170 trials/sec" production figure** quoted for the real study refers to
**full backtest-based** trials (each trial = a multi-year walk-forward backtest
of a real strategy, ~tens of ms each), not the synthetic micro-trials here. The
optimizer itself is nowhere near the bottleneck: at P16 it can dispatch ~64k
trials/sec, so end-to-end study throughput is bounded entirely by the per-trial
backtest cost (deliverable (a)), exactly as intended. Confirmed: optimizer
overhead is < 0.02 ms/trial, four orders of magnitude below the backtest cost.

---

## (c) Live engine per-bar latency

`internal/livengine` signal-mode session driving the deterministic `Replay`
path (identical `onBar` to the wall-clock streaming path, minus the clock wait).
Each strategy submits one order per bar and emits a `SignalIntent` per timestamp
(the full per-bar evaluation + emission path runs every bar).

| Benchmark | universe | ns/bar | allocs/bar¹ |
| --- | --- | ---: | ---: |
| `LiveBarLatency_1strat`  | 1 symbol, 1 strategy   | **~177 ns** | 5 |
| `LiveBarLatency_10strat` | 10 symbols, 10 strats  | **~379 ns** | 14 |

¹ allocs/op ÷ bars-per-op.

Per-bar intent-emission latency is **sub-microsecond**. A live node processing a
daily-bar universe of hundreds of symbols spends well under a millisecond per
timestamp in the engine — the live loop's latency is bounded by the data feed and
the broker round-trip, never by the strategy/emission path.

---

## (d) Data import rows/sec

The CPU-bound core of the Sharadar import: wrangling raw `float64` OHLCV rows
into `domain.Bar` via the exact price bridge
(`float64 → shortest-repr decimal → 1e-4 fixed point`, Python `Decimal(str(x))`
parity). The DB `CopyFrom` is I/O measured against a live stack separately; this
isolates the per-row conversion that bounds import **CPU** throughput.

| Benchmark | rows/sec | ns/row | allocs/row |
| --- | ---: | ---: | ---: |
| `ImportWrangleRowsPerSec` (100k rows) | **~3,675,000** | ~272 | 4 |

**Note on the 4 allocs/row.** Each price field goes through `strconv.FormatFloat`
+ decimal parse — this string round-trip is **parity-load-bearing** (it is what
makes the Go fixed-point bridge match Python's `Decimal(str(x))` exactly) and is
deliberately **not** optimized away. At 3.6M rows/sec the full Sharadar daily-bar
universe (tens of millions of rows) wrangles in seconds of CPU; import wall time
is dominated by parquet read + Postgres `COPY`, not the bridge.

---

## (e) API p50/p99 — heavy endpoints

End-to-end through the real chi router + middleware (bearer auth, CORS, request
logging) + handler + JSON serialization, over in-memory stub stores populated
with realistically large datasets. The DB round-trip is stubbed (the store
returns its pre-built slice), so these numbers isolate the **server's own
per-request CPU + serialization cost** — the part the application controls.
Percentiles are over 2,000 sequential requests.

| Endpoint | payload | p50 | p99 | mean | allocs/op |
| --- | --- | ---: | ---: | ---: | ---: |
| `GET /data/coverage`         | 5,000-ticker gap scan vs NYSE calendar | **~2.1 ms** | ~4.0 ms | ~2.2 ms | 20,573 |
| `GET /backtests/{id}`        | detail + 20 per-strategy metric blocks | **~22 µs**  | ~284 µs | ~41 µs  | 595 |
| `GET /backtests/{id}/equity` | 2,520-point (10y daily) equity curve   | **~443 µs** | ~2.9 ms | ~674 µs | 10,619 |

The coverage endpoint is the heaviest: it scans every ticker's bar-span against
the NYSE session count to compute the gap summary. At ~2 ms p50 for a
5,000-ticker universe it is comfortably interactive. Backtest detail is
microsecond-class; the equity curve scales with point count (serialization
bound).

> p99 tail on the otherwise-cheap endpoints (e.g. backtest detail's ~284 µs vs
> ~22 µs p50) is GC/scheduler jitter on a few of the 2,000 samples, not a
> structural cost — the mean stays close to p50.

---

## Interpreting regressions

- **`allocs/op` is the host-independent signal.** A jump there is a real
  regression (a new per-bar / per-row / per-request allocation) regardless of
  the machine. The engine hotspot fix above was found exactly this way.
- **bars/sec, trials/sec, rows/sec** scale with the host CPU; compare ratios,
  not absolutes, across machines.
- After any change to the engine, accounting, or strategies, re-run the parity
  gate (`go test ./internal/...` + the `parity-*` make targets where a stack is
  available) — **never** trade determinism/parity for throughput.

---

## Go vs Python — hyperopt

This section is an **apples-to-apples** comparison of one hyper-parameter
optimization **trial's cost** — and, by extension, a full study's wall clock —
between the two engines that run the same study:

- **Go** — the self-written deterministic NSGA-II (`internal/hyperopt/nsga2`)
  driving the deterministic event-loop backtest engine, evaluating every fold
  over a **shared, read-only, in-process** bar dataset (locked decision 5).
- **Python** (`trade-multi-strategies`) — Optuna's `NSGAIISampler` driving
  Nautilus Trader backtests, fanned out across a `ProcessPoolExecutor` (spawn).

The optimizer overhead is negligible on both sides (deliverable (b) above: the
Go optimizer dispatches ~64k trials/sec at P16; Optuna's ask/tell is likewise
sub-millisecond). **End-to-end study time is bounded entirely by the per-trial
objective cost — a walk-forward backtest — so that is what this section
measures.**

### Methodology (and why a full Python study is impractical)

One Python objective evaluation is **minutes-to-tens-of-minutes** of Nautilus
backtesting (see below), so running a real Python study to completion at the
same trial budget as Go is not feasible in a benchmark window. The honest design
is therefore:

1. **One identical study config**, run on the **same underlying bars**, on both
   sides (table below). The walk-forward splits depend only on
   `(start, end, folds, embargo)` and are **byte-identical** across Go and
   Python (already parity-verified): for this config both produce exactly
   `fold0 = 2022-09-06 .. 2023-05-04` and `fold1 = 2023-05-05 .. 2023-12-31`.
2. **Confirm same data.** Both sides read the **same Sharadar SFP (fund/ETF)
   parquet cache** for SPY + the 11 SPDR sector ETFs. The Go side normally reads
   bars from Postgres; the comparison harness loaded the identical SFP bars into
   the in-memory `engine.SliceFeed` through the exact same price bridge
   (`float64 → shortest-repr decimal → 1e-4 fixed point`) used in production, so
   no Postgres/compose was started.
3. **Per-trial cost** — wall-clock for **one** objective evaluation (= run the
   strategy over the **2 folds** for one fixed param set) on each side, repeated
   for a stable median.
4. **Go full study** — an actual `pop=8 × gen=3 = 24`-trial study end-to-end,
   plus a parallel-scaling sweep.
5. **Python throughput within a time budget** — measured the dominant Python
   per-fold cost directly (a full `run_backtest`) rather than letting an
   unbounded study run.
6. **Compute + extrapolate** a 200-trial (`pop=20 × gen=10`) full study on both
   sides, stating clearly which figures are measured and which extrapolated.

### Study configuration (identical both sides)

| Knob | Value |
| --- | --- |
| Strategy | `sector_rotation` (11 SPDR sector ETFs + SPY context) |
| Window | `2022-01-01 .. 2023-12-31` |
| Walk-forward | anchored expanding, **2 folds**, 5-day embargo (byte-identical splits) |
| Search space | `momentum_lookback ∈ [42,126]` (int), `top_k ∈ [2,5]` (int) |
| Objectives | `(sharpe, calmar)`, both **maximize** |
| Seed | 42 |
| Starting balance | $100,000 |
| Bars | Sharadar **SFP** daily, SPY + XLK/XLF/XLE/XLV/XLY/XLP/XLU/XLB/XLI/XLRE/XLC |

### Hardware

| | |
| --- | --- |
| CPU | Apple **M4 Max**, 16 cores (12 performance + 4 efficiency), `hw.ncpu=16` |
| RAM | 128 GB |
| OS | macOS (darwin/arm64) |
| Go | go1.26.1 |
| Python | 3.12, Optuna NSGA-II + Nautilus Trader + `ProcessPoolExecutor` (spawn) |

### Per-trial cost (the core apples-to-apples number)

A trial = run the strategy over **both folds** for one fixed param set. Median
of 11 reps, plus a sweep across the search space to show the cost is real
(non-trading vs trading trials), all in one process over the shared dataset.

| Side | per-trial (2 folds) | notes |
| --- | ---: | --- |
| **Go** | **≈ 2.4 ms** (median; range 1.7–3.6 ms) | trading genome `lb=126,topK=5` → 20 orders, sharpe 0.286 / calmar 0.320. No-trade genomes ≈ 1.7 ms; 24-order genomes ≈ 3.6 ms. |
| **Python** | **≈ 30–40+ min** (see derivation) | one fold of the **real** config did **not finish in 20.7 min** of wall clock (killed); per-fold cost confirmed below. |

> **Go is ~3 ms; Python is ~30+ minutes — a ~10⁵–10⁶× per-trial gap.** The two
> are NOT doing the same amount of *work*, and that asymmetry is itself the key
> finding (next section) — but this is the honest, measured cost of one
> `sector_rotation` hyperopt trial on each stack.

#### Why the Python per-trial is minutes, not milliseconds

Python's `run_backtest` (`scripts/multi_strategy_backtest.py`), which
`research/workers.run_trial_worker` calls **once per fold**, **always** loads the
entire survivor-bias-free SF1 stock universe and runs the **full multi-strategy
portfolio** (SEPA over ~5,500 stocks + SectorRotation + Pairs) through Nautilus —
even for a `sector_rotation`-only trial — because the portfolio gate that
produces the objective is defined over the whole book. Measured directly on this
machine:

| Python phase | measured |
| --- | ---: |
| SF1 universe size for the window | **5,547** tradable stocks |
| Universe **bar load** alone (one short window, 4,650 stocks) | **80 s** |
| Full `run_backtest` over a **2-week** window (4,650 stocks → Nautilus) | **261 s** (4.35 min) |
| Full `run_backtest` over one **real fold** (~5–8 months) | **> 20 min** (killed incomplete at 20.7 min) |

The Go `sector_rotation` study, by contrast, loads only the **12 instruments**
the strategy actually trades (SPY + 11 ETFs, ~850 bars each) into the shared
in-process dataset and replays them — hence single-digit milliseconds.

> **Objective parity holds despite the cost gap.** For an identical param set
> through the identical folds, the two engines produce the **same**
> `(sharpe, calmar)` to within **3.55e-6** (already proven by the parity gate).
> The per-trial *cost* differs by orders of magnitude; the per-trial *result*
> does not. (The baseline genome `lb=63,topK=3` genuinely makes **0 trades** on
> this 2022–23 window on **both** sides — a real, parity-faithful outcome — which
> is why the headline Go number uses a trading genome so the cost is not
> understated.)

### Go full study — measured

`pop=8 × gen=3 = 24` trials, real NSGA-II + per-trial backtest + artifact writes,
nil DB sink (artifact-only), over the shared dataset:

| Metric | Value |
| --- | ---: |
| Total wall clock | **≈ 0.17 s** (24 trials) |
| Throughput | **≈ 138 trials/sec** |
| Effective ms/trial (incl. optimizer + artifacts) | **≈ 7 ms** |

**Parallel scaling** (per-trial work ≈ 2.4 ms is tiny, so the
generation-boundary sync dominates and scaling is shallow — `pop=16 × gen=2`):

| Workers | trials/sec | speedup vs P1 |
| ---: | ---: | ---: |
| 1  | ~124 | 1.00× |
| 2  | ~141 | 1.14× |
| 4  | ~149 | 1.20× |
| 8  | ~156 | 1.26× |
| 16 | ~160 | 1.29× |

> This shallow scaling is **expected and honest**: when one trial is only ~2.4 ms
> of CPU, the optimizer's ask/aggregate-in-id-order per generation is a
> significant fraction of the wall time, so adding workers helps little. With a
> *large* per-trial cost the same code scales near-linearly through the 12
> performance cores — see deliverable (b) above, where a fixed CPU-bound trial
> reaches **8.3×** at P16. Python, conversely, has so much per-trial work
> (minutes) that its `ProcessPoolExecutor` scales ~linearly with worker count
> until cores saturate — when Python's per-trial cost is this high, the
> ProcessPool is exactly the right tool and gives it good *relative* scaling.

### Throughput + extrapolated full study (200 trials = `pop=20 × gen=10`)

| | Go | Python (`workers=1`) | Python (`workers=14`, 14/16 cores) |
| --- | ---: | ---: | ---: |
| per-trial (2 folds) | ~2.4 ms | ~30–40 min¹ | ~30–40 min (per worker) |
| trials/sec (effective) | ~138 (measured, 24-trial) | ~0.0005 | ~0.006 |
| **200-trial study** | **~1.5 s** (measured-rate) | **~110 h** (extrapolated) | **~8–10 h** (extrapolated) |

¹ Lower bound: one real fold did not finish in 20.7 min; a 2-fold trial is
therefore conservatively 30–40+ min. The 2-week-window datapoint (261 s/fold)
scales up with window length, consistent with the >20-min single-fold result.

> **Headline.** On the identical study, **Go runs a 200-trial `sector_rotation`
> hyperopt in ~1.5 seconds; the same study in Python is an ~8–10 hour job even
> with a 14-process pool** (and ~110 h single-process) — a roughly
> **10⁴–10⁵× wall-clock advantage** for the Go stack. A *real* Python full study
> at this budget is impractical, which is exactly why it is extrapolated from the
> measured per-fold cost.

### Caveats (read these)

- **The per-trial workloads are not identical, by Python's design.** Python's
  `run_backtest` runs the full ~5,500-stock multi-strategy book per fold even for
  a single-strategy trial; the Go `sector_rotation` study loads only the 12
  instruments the strategy trades. This is the dominant reason for the cost gap.
  It is a fair statement of *what a `sector_rotation` hyperopt trial costs on each
  stack as shipped*, not a claim that the two run the same number of bars. (A
  hypothetical Python build that loaded only the 12 ETFs would be far faster than
  the numbers here — but that is not how the Python pipeline is wired.)
- **In-process shared bars vs forked processes.** Go shares one immutable
  in-memory dataset across all trial goroutines (zero copy, zero per-trial DB
  hit); Python forks worker processes (spawn) that each re-load bars from the
  parquet cache. The fork + re-load overhead is part of Python's real per-trial
  cost and is included above.
- **Optuna's RNG differs**, so the **trial sequences** the two optimizers explore
  are different — you cannot diff trial *N* across the two. What *is* comparable
  is (a) the **per-trial objective cost** measured here and (b) the **objective
  values** for any given param set, which match to 3.55e-6.
- **Numbers scale with the host**; the *ratios* (and the order-of-magnitude
  conclusion) are the signal.

### Reproducing

Go side: a throwaway harness (`tmp/hyperbench/`, deleted after the run) built the
production `study.Evaluator` / `study.Coordinator` over an `engine.SliceFeed`
loaded from the Sharadar SFP parquet — no Postgres, no compose, no product-code
changes. Python side: timed `research.workers.run_trial_worker` /
`scripts.multi_strategy_backtest.run_backtest` directly against the read-only
parquet cache (dumps disabled; the Python repo was not modified). No state was
left running; no DB or compose stack was started.
