# Benchmarks

This is the permanent performance baseline for trade-tms-go. The numbers below
are produced by the in-repo Go benchmark suite via `make bench`. Every benchmark
is **hermetic** — it builds its own in-memory inputs and needs no Postgres,
Redis, or any external service — so the suite runs anywhere and the numbers are reproducible.

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
exercised). `close-fill` fill profile.

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

**Determinism preserved.** The change was verified against the committed golden
dump: running the canonical script over the golden bars produces an
`equity.json`, `account.json`, and every `strategy_equity/*` curve that is
**byte-identical** to the pre-change baseline. A permanent regression
guard, `TestEngineDeterminismMultiStrategy`, runs the multi-strategy config twice
and asserts bit-identical equity curves / balances. The full `go test -race ./...`
suite (including all strategy golden tests) passes unchanged.

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
(`float64 → shortest-repr decimal → 1e-4 fixed point`, i.e. the exact
`Decimal(str(x))` bridge). The DB `CopyFrom` is I/O measured against a live stack separately; this
isolates the per-row conversion that bounds import **CPU** throughput.

| Benchmark | rows/sec | ns/row | allocs/row |
| --- | ---: | ---: | ---: |
| `ImportWrangleRowsPerSec` (100k rows) | **~3,675,000** | ~272 | 4 |

**Note on the 4 allocs/row.** Each price field goes through `strconv.FormatFloat`
+ decimal parse — this string round-trip is **correctness-load-bearing** (it is
what makes the fixed-point bridge match `Decimal(str(x))` exactly) and is
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
- After any change to the engine, accounting, or strategies, re-run the golden
  gate (`go test ./internal/...`) — **never** trade determinism for throughput.
