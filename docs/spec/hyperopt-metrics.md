# SPEC: Hyperopt Subsystem + Performance Metrics

This repo's definition of the hyperopt subsystem and the performance-metrics
math. It covers the metrics, the walk-forward splitter, the search-space
registry, the params loader / search JSON, the constraint expression evaluator,
the baseline search-space JSONs
(`internal/hyperopt/baseline/{sepa,sector_rotation,pairs}.json`), the
optimizer / coordinator, the trial worker, artifacts, and the top/Pareto export
helpers. It also covers the CLI, the backtest entry that produces metrics, the
API readers (study list/detail, staleness), and promotion to
`runs/active_params/`. See also `docs/adr/ADR-006-hyperopt-architecture.md`. The
rules below are invariants of this system (formulas, edge cases, NaN/zero
handling, rounding, ordering, file names, JSON shapes). Where a known weakness is
called out, the better behavior this repo adopts is described alongside it.

Conventions:

- All wall-clock timestamps are **UTC**, RFC 3339 / ISO-8601 with offset
 (`datetime.now(UTC).isoformat` → e.g. `2026-05-04T17:06:29.123456+00:00`,
 microsecond precision, `+00:00` suffix)..
- Study directory timestamps use UTC format `%Y-%m-%d_%H-%M-%S`
 (e.g. `2026-05-04_17-06-29`)..
- Calendar dates (`start`, `end`, fold boundaries) are timezone-less ISO dates
 `YYYY-MM-DD` treated as calendar days, not trading days.
- Money unit is USD throughout; percentages are expressed in percent units
 (e.g. `max_drawdown_pct = -10.0` means −10%).

---

## 1. Performance metrics

Pure functions over an equity curve `[]float64`. Mean, population standard
deviation, and `math.sqrt` over IEEE-754 doubles.

### 1.1 `BacktestMetrics` struct

 Exact field set and JSON key names (this dict is serialized
verbatim into `trial_*.json` `metrics` and per-fold entries):

| Field (JSON key) | Type | Meaning |
|---|---|---|
| `final_balance_usd` | float | account balance at end |
| `total_pnl_usd` | float | `final_balance - starting_balance` |
| `sharpe` | float | see 1.3 |
| `calmar` | float | see 1.5 |
| `max_drawdown_pct` | float | see 1.4; ≤ 0, percent units |
| `num_orders` | int | all orders submitted |
| `num_filled_orders` | int | orders with `is_closed` true |
| `num_rejected_orders` | int | orders with status REJECTED |
| `num_positions` | int | all positions opened |

 `to_objectives` returns the tuple `(sharpe, calmar)` — this is
the objective ordering reported to the optimizer (,
verified by).

### 1.2 Simple returns helper

 Per-period simple returns over consecutive pairs:

```
returns = []
for (prev, cur) in pairwise(curve):
 if prev == 0: continue # skip pair entirely; no return emitted
 returns.append((cur - prev) / prev)
```

Edge case: a zero previous value drops that pair (the count of returns
shrinks); it does NOT emit 0.

### 1.3 Sharpe

 Formula, defaults, and edge cases:

```
compute_sharpe(curve, periods_per_year=252):
 r = _returns(curve)
 if len(r) < 2: return 0.0
 vol = pstdev(r) # POPULATION std-dev (ddof = 0)
 if vol == 0: return 0.0
 return mean(r) / vol * sqrt(252)
```

- Annualization factor: `sqrt(periods_per_year)`, default `periods_per_year = 252`.
 No call site overrides the default.
- Risk-free rate: **none** (raw mean return).
- `pstdev` is population standard deviation (divide by N, not N−1). In Go:
 `sqrt(sum((x-mean)^2)/N)`.
- Flat curve → 0.0 (test).
- Fewer than 2 returns (curve shorter than 3 points after zero-skip) → 0.0.


 Determinism note: a naive running sum can differ in the last ulp across
platforms (arm64 vs x86). To get cross-platform bit-reproducible results,
mean/pstdev use exact rational arithmetic (`math/big`), then convert to float.
This is the source of the metrics goldens (see §13 verification).

### 1.4 Max drawdown percent



```
compute_max_drawdown_pct(curve):
 if len(curve) == 0: return 0.0
 peak = curve[0]; maxdd = 0.0
 for v in curve:
 peak = max(peak, v)
 if peak == 0: continue
 dd = (v - peak) / peak * 100.0 # ≤ 0, percent units
 maxdd = min(maxdd, dd)
 return maxdd
```

- Returns a **non-positive percent** (e.g. `[100k,110k,99k,120k]` → −10.0;
 test).
- Empty curve → 0.0; monotonic-up curve → 0.0 exactly.
- A zero peak is skipped (no division by zero).

### 1.5 Calmar

 Exact algorithm including all special cases:

```
compute_calmar(curve, periods_per_year=252):
 if len(curve) < 2 or curve[0] == 0: return 0.0
 total_return = curve[-1]/curve[0] - 1.0
 if total_return <= -1.0: return -1.0
 years = max((len(curve)-1)/252, 1.0/252)
 ann = (1.0 + total_return)^(1.0/years) - 1.0
 mdd = abs(compute_max_drawdown_pct(curve)) / 100.0
 if mdd == 0:
 if ann <= 0: return 0.0
 return ann / 0.01 # zero-drawdown floor: divide by 1%
 return ann / mdd
```

- The period count is `len(curve) - 1` samples at `periods_per_year = 252`
 (curve is EOD-sampled).
- Zero-drawdown positive-growth curve → `ann / 0.01` (synthetic 1% DD floor),
 test.
- Total wipeout (`total_return ≤ −1`) → exactly −1.0.
- Annualization is geometric: `(1+tr)^(1/years) − 1`, with `years` floored at
 one period (`1/252`).

### 1.6 Where the equity curve comes from

(Scope note: the backtest engine itself is covered by the backtest spec; the
following is the contract hyperopt depends on.)

- `run_backtest` signature relevant to hyperopt:
 keyword-only `start: str = "2024-01-02"`, `end: str = "2024-12-31"`
 (`YYYY-MM-DD`), `configs: StrategyConfigOverrides | None`,
 `runs_dir: Path | None`, `bypass_logging: bool = False`,
 `verbose: bool = False`, `starting_balance_usd: float = 100_000.0`,
 `dump: bool = True`. Returns `BacktestResult{metrics, dump_dir,
 pre_open_count, equity_curve []float64}` (`:171-179`).
- `StrategyConfigOverrides` has exactly three optional maps: `sepa`,
 `sector_rotation`, `pairs` (`:90-103`). Reserved override keys raise:
 `strategy_id`, `bar_types` for all three; additionally `pairs_spec` for
 pairs (`:106-126`).
- Equity curve construction (`:683-704`): per-strategy daily mark-to-market
 samples `{ts, balance_usd}`; per timestamp the per-strategy values are
 **summed** into `pnl_by_ts[ts]`, then
 `curve = [starting + pnl_by_ts[ts] for ts in sorted(pnl_by_ts)]`
 (lexicographic sort of ISO timestamp strings == chronological).
 If there are no samples, the degenerate curve `[starting, final_balance]`
 is used.
- Strategy warmup: the backtest loads 400 calendar days of history before
 `start` for ticker warmup (`:404-411`) and ~500 days for SPY regime warmup
 (`:336-358`) — independent of the walk-forward buffer (see §3).
 - **OUT-OF-BAND, ROLE-ASYMMETRIC (`internal/engine.WarmupConfig`)**: the
 engine is fed ONLY the run window `[start, end]`
 (`run_df = engine_df[idx >= run_start]`); the 400d warmup tail goes into
 `warmup_by_ticker` and is **never replayed through the engine**. It is
 injected out-of-band into the SEPA SignalGenerators ONLY
 (`SEPAUniverseRunner.warmup_ticker`,),
 plus ~500d SPY into the RegimeActor's seed history. **SectorRotation and Pairs get NO warmup**
 — their loaders pull run-window-only bars and build rolling state from
 in-window `on_bar` calls, so e.g. a Pairs `lookback=60` SG is intentionally
 NOT warm at `test_start`. This asymmetry is FAITHFUL and.
 Go replicates it exactly: the engine event loop replays only `[Start, End]`;
 `engine.WarmupConfig.Bars` primes only `WarmupConsumer` strategies (SEPA's
 adapter), Pairs/Sector adapters are NOT `WarmupConsumer`s and receive
 nothing, and the SPY regime warmup is carried by the `ContextProvider`'s own
 full SPY history (look-ahead-safe, date-keyed — independent of the loop). No
 orders or equity samples are emitted during priming. The hyperopt objective
 feeds the engine via `Dataset.WindowFeed` (run-window only) and supplies
 SEPA warmup via `Dataset.WarmupSlices` (the pre-window `[start-400d, start)`
 tail). This closes a latent P3 bug: previously the hyperopt path replayed the
 400d warmup through the loop, warming Pairs/Sector and emitting ~430 equity
 samples instead of ~30 over the test window.
- Counters: `num_orders = len(all orders)`, `num_filled_orders` counts
 `is_closed`, `num_rejected_orders` counts status REJECTED, `num_positions =
 len(all positions)` (`:712-715`).
 - **Go mapping (`internal/engine.Result.Counts`)**: the engine settles fills
 asynchronously and never mutates a submitted order's `Status` to `FILLED`,
 so `num_filled_orders` is derived from `res.Fills` (orders that produced ≥1
 fill = `is_closed`), NOT from `Order.Status == FILLED`.
 `num_rejected_orders` unions submitted orders left in a `REJECTED` status
 with the signal orders the portfolio gate blocked pre-submit
 (`res.RejectedOrders` — the engine's pre-trade
 REJECTED order). This is the SINGLE source of truth shared by the P2/P3
 backtest assembly and the P4 objective path.
 - **`num_rejected_orders` is INFORMATIONAL, not a hard target.** The
 optimizer objective vector is `(sharpe, calmar)` ONLY (`to_objectives`,
 §1.1); `num_rejected_orders` never enters the objective, the dominance
 relation, or `current_best`. Its DENOMINATOR differs by design: it counts
 only venue-level `OrderStatus.REJECTED`
 — allocator/budget drops are SILENT
 (skips the submit without a venue order) —
 whereas Go's `Result.Counts` counts the portfolio-gate (allocator-budget +
 risk) drops in `res.RejectedOrders`. So Go's `num_rejected_orders` will
 generally be LARGER for the same run. Because the objective is
 `(sharpe, calmar)`, this divergence does NOT affect any optimizer decision or
 Pareto front; the counter is retained purely for telemetry/UI. Do NOT treat
 it as a number. (`num_orders`, `num_filled_orders`,
 `num_positions` and the curve-derived metrics remain.)
- **Portfolio gate (objective consistency)**: `run_backtest` ALWAYS
 builds all three daily runners (SEPA + SectorRotation + Pairs) under
 `_build_portfolio`'s MULTI-strategy gate — Allocator `SEPA 40 / Sector 30 /
 Pairs 20` (10% cash) + RiskConstraints `single-name 50%, concentration 40%,
 daily-loss 10%` (`:_build_portfolio:230-257`) — EVEN when
 a hyperopt trial overrides only one sub-strategy's params (the other two run
 on their JSON defaults). The optimized strategy is therefore gated on its
 canonical multi-strategy capital slice + caps, which admits/rejects a
 DIFFERENT order set than a lone-strategy 100%-budget gate would. The Go P4
 objective path replicates this exactly via
 `strategyassembly.Input.MultiStrategyGate = true`; feeding a fixed param set
 through identical folds then yields identical per-fold counters / curves /
 objective vectors (locked decision 3). The default-`false` single-strategy
 gate (100% budget, default caps) remains the standalone single-strategy
 backtest path and is NOT used by the objective.

---

## 2. Search-space JSON format (+ baseline JSONs)

### 2.1 File schema

One JSON file per strategy at `<params_dir>/<strategy>.json`.
Top-level shape:

```json
{
 "strategy": "<name>", // REQUIRED; must equal the requested name
 "schema_version": 1, // REQUIRED; allowed set = {1}
 "display": {... }, // opaque, not validated by loader
 "allocation": {... }, // opaque, not validated by loader
 "metadata": {... }, // free-form dict, defaults to {}
 "parameters": {
 "<param>": {
 "default": <any>, // REQUIRED
 "type": "float"|"int"|"str"|"list", // REQUIRED, allowed set exact
 "search": {"low": <num>, "high": <num>} | null, // optional
 "description": "<str>" // optional
 },...
 },
 "constraints": [
 {"kind": "clamp_high"|"clamp_low", "param": "<name>", "expression": "<expr>"}
 ]
}
```

Validation rules, all errors are `ValueError` with the
messages shown:

| Rule | Error |
|---|---|
| `strategy` key missing | `missing required field: strategy` |
| `strategy` ≠ requested | `file declared strategy '<x>' but loader was asked for '<y>'` |
| `schema_version` ∉ {1} | `unsupported schema_version <v>` |
| param `type` ∉ {float,int,str,list} | `parameter '<n>': type '<t>' not in {...}` |
| `search` present on non-numeric type | `parameter '<n>': search not supported on type '<t>'` |
| constraint `kind` ∉ {clamp_high, clamp_low} | `constraint kind '<k>' not in {...}` |

`search.low/high` are read verbatim (KeyError if missing). Parameter
**insertion order of the JSON object is preserved** — it determines suggest
order (§2.3) and output file order (§8).

### 2.2 File resolution order

`load_strategy_params(strategy, params_dir=None)`:

1. If `params_dir` given explicitly: `<params_dir>/<strategy>.json`, error
 `FileNotFoundError("strategy params file not found: <path>")` if missing.
2. Else if env `TMS_STRATEGY_PARAMS_DIR` is set (via app config,
 — unset/empty → nil) **and**
 `<env_dir>/<strategy>.json` exists: use it.
3. Else: package baseline dir `internal/hyperopt/baseline/<strategy>.json`;
 if also missing → `FileNotFoundError("strategy params file not found in env
 dir nor baseline: <strategy>.json")`.

The per-strategy fallback (2→3) is deliberate: hyperopt outputs are partial
(a sepa-only study writes only `best_params/sepa.json`); pointing the env var
at such a dir must still resolve the other strategies from baseline.

Any error loading config in step 2 silently degrades to baseline. Original: bare `except Exception: return None`
hides config errors. Go improvement: log a structured warning when the env dir
is set but unusable, still fall back to baseline (same resolution result).

### 2.3 Suggestion semantics

`suggest_with(params, trial)`:

- Iterate `parameters` in file order. Skip params with `search == null`.
- The Optuna parameter name is the **prefixed** `"<strategy>.<param>"`
 (e.g. `sepa.risk_pct`); the returned map key is the **unprefixed** name.
- `type == "float"` → `trial.suggest_float(full_name, float(low), float(high))`
 (uniform, no log/step).
- `type == "int"` → `trial.suggest_int(full_name, int(low), int(high))`
 (uniform inclusive of both ends, step 1).
- Returns ONLY sampled keys; static defaults are NOT merged in
 (; verified: sepa yields
 exactly `{risk_pct, market_cap_min_usd, hard_stop_pct, pivot_buffer_pct,
 breakout_volume_multiple, vcp_lookback}` — `history_max_bars` and
 `timezone` excluded).
- After sampling, apply constraints **in file order**, each one:
 `bound = safe_eval(expression, scope=sampled_unprefixed_map)` then
 `clamp_high`: `sampled[param] = min(current, bound)`;
 `clamp_low`: `sampled[param] = max(current, bound)`.
 Constraint clamping mutates only the returned map — the Optuna-side recorded
 value for that parameter remains the raw suggested value.
 (Consequence: `trial.params["pairs.exit_z"]` may differ from the value the
 backtest actually used. See §8 and Open Question Q5.)

`defaults_dict(params)` returns `{name: default}` for every parameter.

### 2.4 Constraint expression language

 AST-whitelisted expression evaluator:

- Literals (numbers), variable names resolved against the sampled map
 (unknown → `NameError("undefined: <name>")`),
- binary `+ - * /` (true division), unary `- +`,
- calls to exactly `min`, `max`, `abs` with positional args only (keyword args
 → error `keyword arguments not supported`),
- anything else → `ValueError("unsupported...")`.
- Division by zero propagates as an error.

In Go: implement a tiny recursive-descent parser or shunting-yard for this
grammar; numeric results are float64 (integer literals may produce ints —
arithmetic semantics are equivalent at the float64 level for the ranges used).

The single constraint in the shipped data: pairs
`exit_z ← min(exit_z, min(1.0, entry_z - 0.1))` keeping exit below entry
(`baseline/pairs.json:54-60`; test).

### 2.5 Baseline search spaces (verbatim data)

`sepa.json` (allocation capital_pct 0.40, active true):

| param | type | default | search low | search high |
|---|---|---|---|---|
| risk_pct | float | 1.0 | 1.0 | 4.0 |
| market_cap_min_usd | float | 500000000.0 | 250000000.0 | 1000000000.0 |
| hard_stop_pct | float | 7.5 | 4.0 | 12.0 |
| pivot_buffer_pct | float | 1.5 | 0.5 | 3.0 |
| breakout_volume_multiple | float | 1.5 | 1.0 | 2.5 |
| vcp_lookback | int | 5 | 3 | 10 |
| history_max_bars | int | 1000 | — | — |
| timezone | str | "America/New_York" | — | — |

constraints: `[]`.

`sector_rotation.json` (capital_pct 0.30, active true):

| param | type | default | low | high |
|---|---|---|---|---|
| momentum_lookback | int | 63 | 42 | 126 |
| top_k | int | 3 | 2 | 5 |
| universe | list | the 11 SPDR ETFs `["XLK","XLF","XLE","XLV","XLY","XLP","XLU","XLB","XLI","XLRE","XLC"]` | — | — |
| timezone | str | "America/New_York" | — | — |

constraints: `[]`.

`pairs.json` (capital_pct 0.20, active true):

| param | type | default | low | high |
|---|---|---|---|---|
| lookback | int | 60 | 30 | 120 |
| entry_z | float | 2.0 | 1.5 | 3.0 |
| exit_z | float | 0.5 | 0.1 | 1.0 |
| capital_per_pair_pct | float | 0.30 | 0.10 | 0.45 |
| pairs | list | `[["KO","PEP"],["MA","V"],["XOM","CVX"]]` | — | — |
| timezone | str | "America/New_York" | — | — |

constraints: `[{"kind":"clamp_high","param":"exit_z","expression":"min(1.0, entry_z - 0.1)"}]`.

All three files carry `metadata = {"source":"baseline",
"created_at":"2026-05-06T00:00:00+00:00", "tuned_from_study":null,
"tuned_from_trial":null}`.

(`intraday_breakout.json` also exists in baseline but is NOT registered in
the hyperopt search-space registry — ADR-006 "Known limitations".)

### 2.6 Registry



- `SEARCH_SPACES` keys exactly `{"sepa", "sector_rotation", "pairs"}`
 (test).
- `suggest_params(strategy, trial)` → unknown name raises
 `ValueError("unknown strategy: <name>")`.
- `suggest_joint_params(trial)` returns the nested map
 `{"sepa": {...}, "sector_rotation": {...}, "pairs": {...}}`, sampling all
 three spaces from one trial **in that fixed order** (sepa, sector_rotation,
 pairs — affects the RNG consumption order, hence determinism).

---

## 3. Walk-forward split algorithm

Strategies are rule-based — there is **no train period**. The splitter
produces only evaluation windows (`EvalSegment{test_start, test_end}`, both
inclusive calendar dates). Strategy warmup is loaded independently by
`run_backtest` (400 calendar days before each segment start; §1.6).

### 3.1 `expanding_anchored(start, end, n_folds=5, embargo_days=5)`

Validation (in this order, all `ValueError`):

| Condition | Message |
|---|---|
| `end <= start` | `end must be after start` |
| `n_folds < 1` | `n_folds must be >= 1` |
| `embargo_days < 0` | `embargo_days must be >= 0` |
| `remaining_days < n_folds` or `segment_days < 1` | `date range too short for requested folds` |

Algorithm (integer day arithmetic; all divisions are floor):

```
total_days = (end - start).days + 1 // inclusive day count
buffer_days = max(total_days / 3, 1) // integer floor div
remaining_days = total_days - buffer_days - embargo_days
if remaining_days < n_folds: error
segment_days = remaining_days / n_folds // floor
if segment_days < 1: error
for idx in 0..n_folds-1:
 prev_end = start + (buffer_days + idx*segment_days - 1) days
 test_start = prev_end + (embargo_days + 1) days
 test_end = test_start + (segment_days - 1) days
 if idx == n_folds-1: test_end = end // last fold absorbs remainder
```

Properties guaranteed (encoded in):

- Segments are strictly ordered, non-overlapping, fully inside `(start, end]`
 with `test_start > start`.
- The final fold ends exactly at `end`.
- **Embargo quirk**: because the embargo offset is constant
 across folds, consecutive segments are exactly **adjacent**
 (`later.test_start == earlier.test_end + 1 day`) for all but the last fold
 (last fold remains adjacent too; only its end is extended). The embargo
 effectively shifts ALL segments later by `embargo_days` and shrinks the
 usable range once — it is NOT a gap between consecutive test windows. The
 docstring claims otherwise; the test comment
 explicitly calls it "vestigial". Go must reproduce this arithmetic exactly
 (fold boundaries feed `run_backtest` and therefore all metrics).
- Worked example (from the test): `start=2022-01-01, end=2024-12-31,
 n_folds=3, embargo=5` → total=1096, buffer=365, remaining=726,
 segment=242 → folds `[2022-06-06..2023-02-02], [2023-02-03..2023-10-03],
 [2023-10-04..2024-12-31]`. Use as a golden vector.

 Original is calendar-day based and the first-third buffer is a
heuristic; embargo is dead weight. Go may additionally expose a true
inter-fold embargo mode, but the default behavior invoked by the optimizer
must be the algorithm above (anything else breaks metric equivalence).

### 3.2 Fold construction in the coordinator

 Folds are computed **once** in the coordinator from
`date.fromisoformat(start/end)` and passed identically to every trial:
`walk_forward=false` → `folds=nil` (single full-window backtest per trial).
`walk_forward=true` → `expanding_anchored(start, end, n_folds=folds,
embargo_days=embargo_days)`.

---

## 4. Per-fold metric aggregation

### 4.1 Equity-curve stitching

Each fold's backtest starts fresh from the same starting balance. Aggregation
concatenates per-period **returns** (not balances) into one continuous curve:

```
stitched = [starting_balance]
for curve in fold_curves (fold order):
 if len(curve) < 2: continue // degenerate folds contribute nothing
 for (prev, cur) in pairwise(curve):
 if prev == 0: continue
 ret = (cur - prev)/prev
 stitched.append(stitched[-1] * (1 + ret))
```

The result always has ≥ 1 point. Note that the first point of each fold curve
participates only as the denominator of that fold's first return — fold
boundaries do not introduce artificial returns.

### 4.2 Recompute, don't average

```
starting_balance = fold0.metrics.final_balance_usd - fold0.metrics.total_pnl_usd
equity = stitch(fold curves, starting_balance)
metrics = BacktestMetrics{
 final_balance_usd: equity[-1],
 total_pnl_usd: equity[-1] - starting_balance,
 sharpe: compute_sharpe(equity), // default 252
 calmar: compute_calmar(equity),
 max_drawdown_pct: compute_max_drawdown_pct(equity),
 num_orders / num_filled_orders / num_rejected_orders / num_positions:
 SUM over folds,
}
```

Rationale (docstring): the previous design mixed
`mean(sharpe)` with `min(calmar)` which could produce sign-contradictory
objectives; concat-and-recompute guarantees Sharpe and Calmar describe the
same return sequence. Regression tests:
 (two positive folds ⇒ both > 0, final ≈
starting·1.02·1.03, maxDD == 0.0) and `:154-189` (two losing folds ⇒ sharpe,
calmar, maxDD all < 0).

### 4.3 Per-fold payloads

For each fold `idx`, the worker records
`{"fold": idx, **fold.metrics.to_dict}` — i.e. the fold's OWN metrics
(computed by `run_backtest` from that fold's curve), with the `fold` index
key first. These go into `trial_*.json` `folds` array in
fold order. In single-window mode `folds = []`.

---

## 5. Trial worker

### 5.1 `WorkerConfig`

`{strategy, start, end, study_dir, dump_trials, folds (nil or []EvalSegment),
trial_timeout_sec (nil or int seconds)}`. Must be serializable to worker
processes (serialized across the spawn boundary).

### 5.2 Override mapping

- Strip keys starting with `__` (the coordinator injects
 `__artifact_number`).
- `sepa` / `sector_rotation` / `pairs` → put cleaned flat params under that
 one field of `StrategyConfigOverrides`.
- `joint` → nested: `sepa=params["sepa"]`, `sector_rotation=...`,
 `pairs=...` (each already a map from `suggest_joint_params`).
- Unknown strategy → `ValueError("unknown strategy: <s>")`.

### 5.3 Execution paths

- **Walk-forward** (`config.folds` non-empty): call `run_backtest` once per
 fold, in order, with `start=fold.test_start.isoformat`,
 `end=fold.test_end.isoformat`, `dump=False` (never dump fold runs),
 `runs_dir=study_dir/"run_dumps"`, `verbose=False`, `bypass_logging=True`.
 Aggregate per §4. `run_dump_ts = nil`.
- **Single window** (`folds` nil/empty): one `run_backtest` over
 `[config.start, config.end]` with `dump=config.dump_trials`; metrics are
 the backtest's own; `folds = []`;
 `run_dump_ts = basename(result.dump_dir)` if a dump dir was produced, else
 nil.
- `bypass_logging=True` always (the engine's logging can only init once per
 process — ADR-006).

### 5.4 Result envelope

`WorkerResult{state, metrics map, folds []map, run_dump_ts, error, duration_sec}`.

- Success: `state="COMPLETE"`, `metrics = BacktestMetrics.to_dict`,
 `error=nil`.
- Any exception: `state="FAIL"`, `metrics={}`, `folds=[]`,
 `run_dump_ts=nil`, `error=str(exc)`.
- Timeout: same as FAIL but `error = "timeout: trial timeout after <N>s"`
 (note the doubled word — outer wrapper prepends `timeout: ` to the
 exception text `trial timeout after <N>s`;).
- `duration_sec` = monotonic elapsed seconds, measured from worker entry to
 return, including failures (`>= 0`; test).

### 5.5 Per-trial timeout

Original [behavior to replicate semantically]: SIGALRM watchdog inside the
worker process; integer seconds; `nil` disables; non-Unix → no-op. Timeout
aborts the running backtest mid-flight and yields FAIL ~at the deadline
(test: 1s timeout fires well before a 5s
sleep completes; `duration_sec >= 0.9`).

 Original weaknesses: SIGALRM only works in the process main thread;
in sequential (workers=1) mode the alarm runs inside the coordinator; the
interrupted backtest may leak partially-initialized engine state into the
worker which then serves the next trial. Go improvement: run each trial under
`context.WithTimeout`; since the Go backtest is in-process and cooperative,
check `ctx.Err` at bar boundaries; on timeout return the same FAIL result
shape (`error` prefixed `timeout:`) and guarantee resource cleanup. The
observable contract (FAIL state, error prefix, prompt abort, duration
recorded) must be preserved.

---

## 6. Optimizer / study coordinator

### 6.1 `run_study` parameters and validation

| Param | Default | Validation |
|---|---|---|
| `strategy` | — | must be in `{"sepa","sector_rotation","pairs","joint"}` else `ValueError("unknown strategy: <s>")` |
| `n_trials` | — | `>= 1` else `ValueError("n_trials must be >= 1")` |
| `start`, `end` | — | ISO dates (strings, stored verbatim) |
| `runs_dir` | `runs/hyperopt` |
| `workers` | 1 | `>= 1` else `ValueError("workers must be >= 1")` |
| `seed` | 42 |
| `walk_forward` | true |
| `folds` | 5 |
| `embargo_days` | 5 |
| `dump_trials` | true |
| `resume` | nil | study_ts string to resume |
| `trial_timeout_sec` | 600 | nil disables |

### 6.2 Study identity

- `study_ts = resume ?? now-UTC "%Y-%m-%d_%H-%M-%S"`;
 `study_dir = runs_dir/study_ts` (mkdir -p).
- `study_name = existing study.json's study_name ??
 "hyperopt-<strategy>-<study_ts>"`.
- `created_at` preserved from existing study.json on resume; else now.
- `started_at` preserved from existing progress.json on resume; else now.

### 6.3 Resume mismatch guard

When `resume` is set and a `study.json` exists, compare: `strategy`, `start`,
`end`, and `walk_forward` (deep-equal against
`{"enabled": walk_forward, "folds": folds, "embargo_days": embargo_days}`).
Any mismatch → `ValueError("resume mismatch for study <ts>: <field>:
existing=<a> new=<b>[;...]")` listing all mismatched fields joined by `"; "`.
`seed`, `n_trials`, `workers`, `trial_timeout_sec` are NOT validated (a
resume may change them; study.json is overwritten with the new values).

### 6.4 Optuna study configuration

- Multi-objective, `directions = ["maximize", "maximize"]` over objectives
 `("sharpe", "calmar")` in that order.
- Sampler: **NSGA-II** seeded with `seed` (`NSGAIISampler(seed=seed)`),
 with standard NSGA-II defaults (match at the algorithm/semantics level — see
 Open Question Q1 for numeric stream determinism):
 - `population_size = 50` (generation-based).
 - Generation 0 (first 50 asked trials): each parameter sampled
 independently, uniformly within its range (random sampler).
 - Elite selection for the next generation: fast non-dominated sort, fill by
 rank; the boundary front truncated by crowding-distance (standard NSGA-II).
 - Child generation: with
 probability `crossover_prob = 0.9` perform **uniform crossover** of two
 parents (per-parameter swap with `swapping_prob = 0.5`,; retried until the child lies inside the
 search space), otherwise clone one uniformly-chosen parent. Parent
 selection: two candidates uniformly at random, the dominant one wins
 (binary tournament on Pareto dominance only — no crowding tiebreak;); the second parent is drawn from the population
 excluding the first.
 - Mutation: per-parameter with probability `mutation_prob = 1/max(1, n_params)`
 the parameter is **dropped** from the child and re-sampled independently
 (uniform in range).
 - FAILed trials do not join the population (they have no values).
- Storage: Optuna `JournalStorage` over append-only file
 `study_dir/optuna_journal.log`; `load_if_exists=True` so resume reloads
 sampler history. The implementation must persist optimizer state sufficient to
 resume the NSGA-II population from
 disk; see Q2 for format.]
- Objective reporting: COMPLETE →
 `tell(trial, (float(metrics["sharpe"]), float(metrics["calmar"])))`; any
 other state → `tell(trial, state=FAIL)`.

### 6.5 Trial numbering and resume skip

- **Artifact numbers** are `0..n_trials-1` and name the files
 `trials/trial_%04d.json`.
- On startup, scan `trials/trial_*.json`; numbers whose JSON parses and has
 `state == "COMPLETE"` and integer `number` are skipped;
 `artifact_numbers = [n for n in 0..n_trials-1 if n not completed]` in
 ascending order. FAILed/corrupt/missing artifacts are **re-run** and their
 files overwritten.
- The artifact number is injected into the suggested params map as
 `"__artifact_number"` before dispatch (workers receive it; it is stripped
 before writing the artifact and before building overrides). (Worker stubs in
 tests read it.)
- The optimizer-internal trial number (Optuna's `trial.number`, which grows
 monotonically across resumes including FAILs) is decoupled from the
 artifact number. `best_params` provenance uses the **Optuna** trial number
 (§8); everything else uses artifact numbers. On a fresh non-resumed study
 with no failures they coincide.

### 6.6 Sequential mode (`workers == 1`;)

For each artifact number in order: `ask` → suggest (§2.3/§2.6; `joint` uses
`suggest_joint_params`) → inject `__artifact_number` → record `started_at` →
call worker fn inline → record `finished_at` → `tell` → write trial artifact
→ write progress (status RUNNING, `running_trials=0`, no last_error update).
Exceptions from the worker fn propagate (caught by the outer
INTERRUPTED handler §6.9) — sequential mode has no per-trial try/except in
the coordinator; the standard worker fn (§5.4) catches its own exceptions, so
only KeyboardInterrupt/SystemExit (or bugs) escape.

### 6.7 Parallel mode — ProcessPool layout

- Executor: pool of `workers` OS processes, spawn context (fresh process, no
 fork) — Go: a worker-goroutine pool is acceptable ONLY if the backtest is
 thread-safe; otherwise mirror with subprocesses. Each worker is long-lived
 and serves many trials (cache warm amortization).
- **Streaming submit with in-flight cap** (test): submit at most `workers` tasks up front;
 on each completion, submit exactly one replacement while `pending`
 non-empty. Never bulk-submit all trials (ask-time param suggestion must see
 completed results). Pending order: ascending artifact number
 (`pending.pop(0)`).
- After the initial submits, write progress
 (RUNNING, `running_trials=len(in_flight)`).
- Completion loop: wait FIRST_COMPLETED over in-flight futures; for each done
 future (iteration order over the done set is unspecified): pop bookkeeping
 `(trial, artifact_number, params, started_at)`; `future.result`;
 if the future itself raised (worker crash / pickling failure) synthesize
 `WorkerResult{state:"FAIL", metrics:{}, folds:[], run_dump_ts:nil,
 error:str(exc), duration_sec:0.0}` (the `0.0` is significant); `finished_at=now`;
 `tell`; write trial artifact; **submit next pending**; write progress with
 `running_trials=len(in_flight)` and `last_error=result.error`.
- `last_error` is overwritten on EVERY completion — a success
 (error=nil) clears a previous failure's message. Original: last completion
 wins. Go improvement: keep "last error of the most recent FAILed trial"
 in addition to faithful `last_error` semantics, or preserve original
 behavior exactly; if improving, document the field as
 `last_error = error of most recently finished trial` is the original
 reference semantic.
- Synthesized FAIL on future exception records
 `duration_sec = 0.0`, losing the actual elapsed time. Go may record
 `finished_at - started_at` seconds instead; original writes exactly `0.0`.

### 6.8 Progress writes

Every `_write_progress` call **re-scans all trial artifacts** to count
`completed_trials` (state == COMPLETE) and `failed_trials` (state == FAIL)
and to compute `current_best`:

- `current_best` = over all COMPLETE trial artifacts with numeric sharpe and
 calmar, maximize `score = sharpe + calmar` (strict `>` ⇒ first-seen wins
 ties; scan order is sorted filename order); result
 `{"trial": <artifact number>, "sharpe": <float>, "calmar": <float>}` or
 null when none. (The API and source-picker labels read this.)
 The sum is an arbitrary scalarization of a Pareto pair; Go may
 ADDITIONALLY expose the Pareto front, but must keep this field byte-same.
- `updated_at` and `last_heartbeat_at` both set to now on every full write.
- `coordinator_pid` = PID captured at module import (the process that started
 the study) (value is process-specific).
- O(trials²) file re-reads over a study; Go may cache counters in
 memory (output must remain identical).

Write points: (1) RUNNING before any trial dispatch — deliberately before
data prefetch so an early interrupt still leaves a flippable progress file; (2) per-trial/per-completion as above;
(3) terminal states (§6.9).

### 6.9 Lifecycle states and interruption

- `KeyboardInterrupt`/`SystemExit` (Go: SIGINT/SIGTERM-triggered
 cancellation): write progress `status="INTERRUPTED"` (no last_error),
 re-raise (process exits non-zero).
- Any other exception: write `status="INTERRUPTED"` with
 `last_error=str(exc)`, re-raise (test).
- Normal completion: write `status="COMPLETE"` then write best_params (§8)
 then return `StudyResult{study_name, study_dir}`.
- The heartbeat thread is cancelled in a `finally` (§6.10).
- Status vocabulary: `RUNNING | INTERRUPTED | COMPLETE` written by the
 coordinator; `UNKNOWN` synthesized by the API reader when progress.json is
 absent (§9.2).

### 6.10 Heartbeat

- Daemon background ticker, interval **20 s**, started right after the
 initial RUNNING write, cancelled on exit.
- Each tick: read `progress.json`; if missing or unparseable or the write
 fails → silently no-op (file preserved verbatim on corrupt JSON — test); else set ONLY `last_heartbeat_at` and
 `updated_at` to now and atomically rewrite (all other fields preserved
 byte-for-byte at the JSON value level).
- Concurrency: heartbeat and trial-boundary writes race benignly —
 atomic tmp+rename means last-write-wins, never torn. In Go guard with a
 mutex around progress writes (improvement allowed; observable files
 unchanged).
- Purpose: the API staleness check (§9.2) treats heartbeat age > 60 s +
 dead PID as INTERRUPTED; PID checks are unreliable across Docker PID
 namespaces, so heartbeat freshness is the de-facto liveness signal.

---

## 7. Artifact files

### 7.1 Atomic write

`atomic_write_json(path, payload)`: mkdir -p parent; write
`json.dumps(payload, indent=2, default=str)` to `<path>.tmp` (note: suffix
appended to the existing suffix → `progress.json.tmp`); rename over `path`.
JSON: 2-space indent, **insertion-order keys** (struct field order below),
non-ASCII unescaped is irrelevant (all ASCII), unknown types stringified
(`default=str` — e.g. a stray Path or date becomes its string form). No
trailing newline. No fsync before rename — a power loss can leave
an empty file; Go should fsync file and directory, content unchanged.

### 7.2 `study.json` — exact schema (field order = write order)

```json
{
 "version": 1,
 "study_name": "hyperopt-sepa-2026-05-04_17-06-29",
 "strategy": "sepa", // or sector_rotation | pairs | joint
 "start": "2023-01-01",
 "end": "2024-12-31",
 "directions": ["maximize", "maximize"],
 "objectives": ["sharpe", "calmar"],
 "seed": 42,
 "n_trials": 200,
 "workers": 8,
 "walk_forward": {"enabled": true, "folds": 5, "embargo_days": 5},
 "created_at": "2026-05-04T17:06:29.123456+00:00",
 "updated_at": "2026-05-04T17:06:29.123456+00:00"
}
```

Rewritten at every `run_study` start (including resume; `created_at`
preserved, `updated_at` refreshed, other fields take the new invocation's
values).

### 7.3 `progress.json` — exact schema

```json
{
 "status": "RUNNING", // RUNNING | INTERRUPTED | COMPLETE
 "completed_trials": 37,
 "failed_trials": 2,
 "running_trials": 8,
 "total_trials": 200,
 "workers": 8,
 "started_at": "...", // ISO UTC; preserved across resume
 "updated_at": "...",
 "last_heartbeat_at": "...", // nullable
 "coordinator_pid": 12345, // nullable
 "current_best": {"trial": 22, "sharpe": 1.8, "calmar": 2.4}, // nullable
 "last_error": null // nullable string
}
```

### 7.4 `trials/trial_%04d.json` — exact schema

File name: `trial_` + zero-padded 4-digit artifact number + `.json`
(`trial_0007.json`; numbers ≥ 10000 print wider, no truncation).

```json
{
 "number": 7,
 "strategy": "sepa",
 "params": {"risk_pct": 2.1,...}, // __-prefixed keys stripped;
 // joint: nested {"sepa":{...},...}
 "metrics": { BacktestMetrics.to_dict }, // {} when FAIL
 "folds": [{"fold": 0,...metrics...},...], // [] when no walk-forward or FAIL
 "state": "COMPLETE", // COMPLETE | FAIL
 "started_at": "...",
 "finished_at": "...", // nullable in schema; always set by coordinator
 "duration_sec": 9.2,
 "run_dump_ts": "2026-05-04_17-07-09", // nullable; only single-window dumps
 "error": null // nullable string
}
```

### 7.5 Directory layout (ADR-006)

```
runs/hyperopt/{study_ts}/
├── study.json
├── progress.json
├── optuna_journal.log # optimizer state journal (Go: equivalent state file, Q2)
├── trials/trial_0000.json...
├── run_dumps/{run_ts}/... # only when dump_trials && !walk_forward
└── best_params/<strategy>.json # written on COMPLETE with ≥1 completed trial
```

---

## 8. Best-params snapshot & promotion flow

### 8.1 `best_params/` writing

After COMPLETE only (never after INTERRUPTED):

1. `candidates = study.best_trials` — the **Pareto-optimal COMPLETE trials**
 (maximize both objectives; standard dominance over (sharpe, calmar)).
 Errors fetching / empty → silently skip (no `best_params/` dir; test).
2. `best_trial = argmax over candidates of values[0]` (highest **sharpe**;
 missing values treated as −inf; `max` keeps the FIRST maximal
 element on ties — candidate order is Optuna trial-number ascending).
3. Strategy list: `joint` → `["sepa","sector_rotation","pairs"]`, else
 `[strategy]`.
4. For each strategy `strat`: filter `best_trial.params` (the OPTUNA param
 map, prefixed keys, **pre-constraint-clamp values** — see §2.3 note and
 Q5) to keys starting `"<strat>."`, strip the prefix; skip strat if empty;
 call `write_tuned_params`.

### 8.2 `write_tuned_params`

- Read baseline `base_dir/<strat>.json` (the **package baseline**, not the
 env dir —); missing → FileNotFoundError.
- For each tuned key: must exist in `parameters`, else
 `KeyError("strategy '<s>' has no param '<n>'; cannot tune")`; replace ONLY
 its `default` value. `search`, `constraints`, ordering, all other fields
 preserved verbatim.
- Replace `metadata` with
 `{**old_metadata, "source": "tuned", "created_at": <now UTC ISO>,
 "tuned_from_study": <study_name>, "tuned_from_trial": <optuna trial number>}`.
- Write to `out_dir/<strat>.json` (`out_dir = study_dir/best_params`),
 `json.dumps(raw, indent=2, sort_keys=False)` (insertion order preserved),
 plain write (not atomic [IMPROVE: Go may write atomically; content
 identical]).

### 8.3 Promotion path A — env var (CLI)

The `--promote-best` flag only **prints** (never edits files):

```
To promote, set in your.env:
 TMS_STRATEGY_PARAMS_DIR=<best_params dir, cwd-relative if possible>
```

The loader resolution (§2.2) then picks tuned files per-strategy with
baseline fallback. Promotion to committed defaults remains a manual git
change (PLAN-HYPEROPT boundary rule 2; ADR-006 Consequences).

### 8.4 Promotion path B — `runs/active_params/` (API SourceManager)

Deployment wires `TMS_STRATEGY_PARAMS_DIR=runs/active_params`
(`docker-compose.yml:42`, `.env.example:46`); `SourceManager` manages that
directory (: `active_dir = TMS_RUNS_DIR(default
"runs")/active_params`, hyperopt dir `runs/hyperopt`).:

- `source_id` grammar: `"baseline"` | `"hyperopt:<study_ts>"`; empty ts or
 any other shape → `ValueError("invalid source_id...")` (HTTP 422 at the
 API). Reader-side third value `"external"` (never settable).
- `set_active(strategy, source_id)`:
 - baseline → delete `active_params/<strategy>.json` if present
 (idempotent).
 - hyperopt → source file
 `runs/hyperopt/<ts>/best_params/<strategy>.json`; missing →
 FileNotFoundError (HTTP 404). Copy with metadata → `.tmp` suffix in
 active dir → atomic rename onto `active_params/<strategy>.json`; on
 failure remove the tmp, pre-existing target untouched.
- `get_active(strategy)`: no active file → `baseline`; unparseable JSON →
 `external`; `metadata.tuned_from_study` absent/non-string → `external`;
 else extract ts via regex
 `^hyperopt-([^-]+(?:_[^-]+)*)-(?P<ts>\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2})$`
 (matches study names for strategies containing underscores; no hyphens in
 strategy names) — no match → `external`; ts known (in options list, or dir
 `runs/hyperopt/<ts>` exists) → `hyperopt:<ts>`, else `external`.
- `list_options(strategy)`: always `baseline` first; then every study
 (newest-first per reader order, §9.2) that has a parseable
 `best_params/<strategy>.json`; label `"<ts>"` or
 `"<ts> (Sharpe %.2f, Calmar %.2f)"` from `progress.current_best`
 (each part included only if numeric; joined `", "`).
- API endpoints:
 `GET /api/strategies/registered/{name}/available-sources` →
 `{strategy_name, active, options[]}`;
 `PUT /api/strategies/registered/{name}/source` body `{"source": "<id>"}`
 (404 unknown strategy/missing best_params; 422 malformed id).
- Effect is **next-run-only**: live processes read params at startup; nothing
 hot-reloads (design intent, header comment).

---

## 9. Read-side API contract

### 9.1 Endpoints

- `GET /api/hyperopt/studies` → list of study.json contents + injected
 `"ts": <dirname>`; sorted by directory name **descending** (lexicographic
 == newest first); directories without parseable study.json skipped.
- `GET /api/hyperopt/studies/{ts}` → study.json + `ts` + `progress` (object;
 synthesized when missing: `{status:"UNKNOWN", completed_trials:0,
 failed_trials:0, running_trials:0, total_trials:study.n_trials|0,
 workers:study.workers|0, started_at:null, updated_at:null,
 last_heartbeat_at:null, coordinator_pid:null, current_best:null,
 last_error:null}`) + `trials` (all parseable `trials/trial_*.json` in
 sorted filename order). 404 when study.json missing.
- `GET /api/hyperopt/studies/{ts}/best-params` → `{strategy: parsed JSON}`
 for every `best_params/*.json` (sorted, stem as key); 404 when the dir is
 missing or yields nothing.

### 9.2 Staleness override

On `get_study`, if progress `status == "RUNNING"`:
take `last_heartbeat_at ?? updated_at`; unparseable/missing → not stale;
naive timestamps assumed UTC; if `now − ts > 60 s` AND
`coordinator_pid` is nil or not alive (`kill(pid, 0)` semantics) → present
`status = "INTERRUPTED"` to the caller (the file on disk is NOT modified).
 PID liveness is meaningless across container namespaces (false
"alive" never happens, false "dead" can); with the 20 s heartbeat the 60 s
threshold dominates. Go: same rule; may use `process.Signal(syscall.Signal(0))`.

---

## 10. Export helpers

Operate on trial-artifact maps (as read from disk).

- `_metric(trial, name)`: `trial.metrics[name]` if int/float (bool is a
 bool is an int subtype — a `true` would coerce to 1.0; irrelevant in practice)
 else nil.
- Only `state == "COMPLETE"` trials considered, both helpers.
- `top_trials(trials, metric, limit=10)`: filter metric-present trials, sort
 descending by metric, **stable** (ties keep input order), truncate to
 `limit`.
 Sort key is `value or -inf`: a metric equal to **0.0** is falsy
 sorts as −inf (below any negative value). Original behavior
 exactly that; Go improvement: use the value directly (0.0 sorts between
 −ε and +ε). Pick one and document; default to replicating the original
 for byte-equivalence of API outputs.
- `pareto_trials(trials, x_metric="sharpe", y_metric="calmar")`: candidate
 is dominated iff some other trial has `x' ≥ x ∧ y' ≥ y ∧ (x' > x ∨ y' > y)`
 (weak dominance with strict improvement in at least one; O(n²) scan;
 duplicates of the same point are all kept — neither dominates the other).
 Result sorted by `(x, y)` tuple descending. Same `or 0.0` falsy quirk
 applies to comparison values of exactly-0.0 metrics (, same
 treatment as above).

---

## 11. CLI and Make integration (, `Makefile:56-57`)

 (Go: same flags on the equivalent cobra/flag command)

| Flag | Type | Default | Notes |
|---|---|---|---|
| positional `strategy` | enum | — | `sepa\|sector_rotation\|pairs\|joint` |
| `--n-trials` | int | 20 |
| `--workers` | int | 1 |
| `--start` | str | `2023-01-01` |
| `--end` | str | `2024-12-31` |
| `--runs-dir` | path | `runs/hyperopt` |
| `--seed` | int | 42 |
| `--walk-forward` / `--no-walk-forward` | bool | true |
| `--folds` | int | 5 |
| `--embargo-days` | int | 5 |
| `--no-dump-trials` | flag | off | inverts `dump_trials` |
| `--resume` | str | nil | study_ts |
| `--trial-timeout-sec` | int | 600 | **0 → disabled (nil)** |
| `--promote-best` | flag | off | print-only (§8.3) |

Output on success: `Study complete: <study_name>` and
`Artifacts: <study_dir>` lines. Make target:
`make hyperopt STRATEGY=sepa N_TRIALS=20 WORKERS=1 [START= END=
TRIAL_TIMEOUT_SEC= FOLDS= WALK_FORWARD=true|false]`.

---

## 12. Defaults & constants summary

| Constant | Value | Source |
|---|---|---|
| Objectives / directions | (sharpe, calmar) / (max, max) |
| Sampler | NSGA-II, seed=42 default | CLI default |
| NSGA-II population_size | 50 | optuna 4.8.0 default |
| NSGA-II crossover | uniform, crossover_prob 0.9, swapping_prob 0.5 | optuna defaults |
| NSGA-II mutation_prob | 1/max(1, n_params) | optuna default |
| periods_per_year | 252 |
| Calmar zero-DD divisor | 0.01 |
| Walk-forward defaults | 5 folds, 5 embargo days |
| Buffer | total_days // 3, min 1 |
| Trial timeout default | 600 s |
| Heartbeat interval | 20 s |
| Stale heartbeat threshold | 60 s |
| Starting balance | 100,000 USD |
| Strategy warmup | 400 calendar days (SPY: 500) |
| Study ts format | UTC `%Y-%m-%d_%H-%M-%S` |
| Trial file pattern | `trial_%04d.json` |
| current_best score | sharpe + calmar, strict-> first wins |
| Runs dir default | `runs/hyperopt`; active dir `runs/active_params` |

---

## 13. Verification plan for the Go port

1. **Metric golden vectors**: compute the metrics over a fixed
 set of curves (flat, monotonic, the test vectors in §1, randomized 252/504
 point curves with seeds) and assert Go outputs match to ≤1e-12 relative
 (exact equality expected for the rational-mean caveat cases; see §1.3
 IMPROVE).
2. **Walk-forward**: enumerate (start, end, folds, embargo) grids
 `expanding_anchored` and diff against Go (exact date equality).
3. **Stitching**: feed identical fold curves to `_concat_fold_metrics` and
 the Go aggregator; diff full metric dicts.
4. **Artifacts**: byte-compare JSON output for fixed inputs (same key order,
 indent=2; timestamps injected).
5. **Suggest/constraints**: drive `suggest_with` with a deterministic fake
 trial (mid-point sampling like) in both
 languages; diff param maps including clamped pairs `exit_z`.
6. **Optimizer**: stub the worker fn and compare the
 full artifact tree (minus timestamps and sampler-dependent param values;
 see Q1) for sequential and parallel runs, fresh and resumed.

---

## Open questions

1. **Q1 — NSGA-II suggestion-stream determinism.** The NSGA-II sampler is
 seeded with `seed` and must be deterministic per seed and self-consistent
 across resume. Reproducibility is asserted at the metrics/artifact-schema
 level (same algorithm, same hyper-defaults). The exact per-seed suggestion
 stream is an implementation detail, not a fixed contract.
2. **Q2 — Journal file format.** Any durable state file at the journal path
 whose resume restores asked/told trials and population is acceptable.
 Nothing else in the repo reads the journal file.
3. **Q3 — ask/tell trial-number drift on resume.** After a crash, the journal
 may contain asked-but-never-told trials (left RUNNING/stale), so sampler
 trial numbers drift further from artifact numbers and `tuned_from_trial`
 (§8.1) can refer to a number that has no `trial_NNNN.json` counterpart.
 Recommendation: define trial identity as artifact-number-only (affects only
 the provenance integer).
4. **Q4 — `finished_at` nullability.** `TrialArtifact.finished_at` is
 declared nullable but every coordinator path sets it. Is a nil value ever
 expected by consumers (UI), or can Go make it required?
5. **Q5 — Constraint-clamped values in best_params.** `write_tuned_params`
 receives `best_trial.params` from Optuna — the **pre-clamp** suggested
 values, not the clamped values the backtest actually used (§2.3). For
 pairs, a promoted `exit_z` default can violate `exit_z < entry_z - 0.1`
 until the runtime loader's constraint pass — but `defaults_dict` does NOT
 apply constraints, so a promoted file may run with an exit_z the study
 never actually evaluated. Replicate (bug-compatible) or fix (store
 clamped values)? Original: pre-clamp values written.
6. **Q6 — `default=str` JSON fallback.** `atomic_write_json` stringifies
 non-JSON types silently. The only observed case is none in normal flow;
 should Go hard-error on non-serializable payloads instead?
7. **Q7 — Trial artifact `params` for joint studies.** Joint trials store
 nested per-strategy maps; the UI/export helpers treat `params` as opaque.
 Confirm no consumer requires flat prefixed keys for joint studies.
8. **Q8 — Sequential-mode progress `last_error`.** In workers=1 mode,
 per-trial FAIL errors are written to the trial artifact but never to
 `progress.last_error` (only parallel mode and terminal-exception paths
 set it). Intentional? Original behavior as described.
9. **Q9 — Prefetch step.** There is no prefetch step today (each worker hits the
 parquet cache directly). Do not add one unless the data-layer spec requires it.
