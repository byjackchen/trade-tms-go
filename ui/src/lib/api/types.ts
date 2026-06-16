/**
 * Wire types for the TMS control-plane API.
 *
 * Hand-authored from the authoritative contract in `docs/api.md` (the Go
 * `internal/api` handlers). Kept narrow and explicit so the UI fails loud at
 * the type level when the contract drifts. All timestamps are RFC3339 UTC
 * strings; all dates are `YYYY-MM-DD` America/New_York trading dates.
 */

export type ErrorEnvelope = {
  error: { code: string; message: string };
};

// ---- /api/v1/data/coverage (summary) ----

export type Freshness = {
  latest_session: string;
  lag_sessions: number;
};

export type GapWorstEntry = {
  ticker: string;
  first: string;
  last: string;
  bars: number;
  expected_sessions: number;
  missing_days: number;
};

export type GapSummary = {
  tickers_scanned: number;
  tickers_with_gaps: number;
  missing_days_total: number;
  worst: GapWorstEntry[];
};

export type CoverageTable = {
  table: string;
  rows: number;
  tickers: number;
  min_date?: string;
  max_date?: string;
  freshness?: Freshness;
  gaps?: GapSummary;
};

export type CoverageResponse = {
  latest_session: string;
  generated_at: string;
  tables: CoverageTable[];
};

// ---- /api/v1/data/coverage?ticker= (drill-down) ----

export type TickerGapDetail = {
  ticker: string;
  first: string;
  last: string;
  bars: number;
  expected_sessions: number;
  missing_days: number;
  missing: string[];
  missing_truncated: boolean;
};

// ---- /api/v1/data/tickers ----

export type TickerSearchResult = {
  ticker: string;
  name: string;
  exchange: string;
  is_delisted: boolean;
  category: string;
  sector: string;
  industry: string;
  table: string;
  first_price_date: string;
  last_price_date: string;
  delist_date: string;
};

export type TickerSearchResponse = {
  query: string;
  results: TickerSearchResult[];
};

// ---- /api/v1/data/sync-runs ----

export type DatasetWatermark = {
  dataset: string;
  last_sync: string | null;
  row_count: number;
  schema_version: number;
  updated_at: string;
};

export type SyncRun = {
  id: number;
  dataset: string;
  kind: string;
  started_at: string;
  finished_at: string | null;
  rows_added: number;
  status: string;
  error?: string;
};

export type SyncRunsResponse = {
  datasets: DatasetWatermark[];
  runs: SyncRun[];
};

// ---- Jobs ----

export type JobStatus =
  | "queued"
  | "running"
  | "succeeded"
  | "failed"
  | "canceled";

export type JobProgress = {
  stage?: string;
  pct?: number;
  [k: string]: unknown;
};

export type Job = {
  id: number;
  kind: string;
  status: JobStatus;
  payload: Record<string, unknown>;
  priority: number;
  run_at: string;
  attempts: number;
  max_attempts: number;
  dedupe_key: string | null;
  claimed_by: string | null;
  claimed_at: string | null;
  heartbeat_at: string | null;
  started_at: string | null;
  finished_at: string | null;
  last_error: string | null;
  progress?: JobProgress;
  cancel_requested: boolean;
  result?: Record<string, unknown>;
  created_at: string;
  updated_at: string;
};

export type JobsResponse = { jobs: Job[] };
export type JobResponse = { job: Job };

export type RefreshSource = "parquet" | "api";

export type DataRefreshRequest = {
  source: RefreshSource;
  tables?: string[];
  tickers?: string[];
  since?: string;
  actor?: string;
  max_attempts?: number;
};

export type EnqueueResponse = { job: Job; deduped: boolean };

export type CancelResponse = {
  outcome: "canceled" | "cancel_requested" | "already_terminal";
  job: Job;
};

// ---- Universe ----

export type UniverseKind = "live" | "eod" | "backtest" | "manual";

export type UniverseMember = {
  ticker: string;
  rank: number;
  score: number;
  trend_template_count: number;
  breakout_proximity: number;
  market_cap_usd: number;
  reasons: string[];
};

export type UniverseSnapshot = {
  id: number;
  as_of: string;
  kind: UniverseKind;
  table_filter?: string;
  window_start?: string;
  window_end?: string;
  limit_n: number;
  tickers: string[];
  excluded: string[];
  params: Record<string, unknown>;
  members: UniverseMember[];
  created_at: string;
};

export type UniverseLatestResponse = { snapshot: UniverseSnapshot };

export type UniverseRebuildRequest = {
  kind?: UniverseKind;
  limit?: number | null;
  uncapped?: boolean;
  top_k?: number;
  actor?: string;
};

// ---- Backtests ----

export type BacktestStatus = "RUNNING" | "COMPLETE" | "INTERRUPTED" | "FAIL";

export type FillProfile = "nautilus-compat" | "realistic";

/** A run summary, as returned in the list and the detail `backtest` field. */
export type BacktestSummary = {
  id: number;
  run_ts: string;
  kind: string;
  status: BacktestStatus;
  start_date: string;
  end_date: string;
  starting_balance_usd: number;
  final_balance_usd: number;
  total_pnl_usd: number;
  strategies: string[];
  created_at: string;
  updated_at: string;
};

export type BacktestsResponse = { backtests: BacktestSummary[] };

/** Portfolio / per-strategy metrics block. */
export type BacktestMetrics = {
  final_balance_usd: number;
  total_pnl_usd: number;
  sharpe: number;
  calmar: number;
  max_drawdown_pct: number;
  num_orders: number;
  num_filled_orders: number;
  num_rejected_orders: number;
  num_positions: number;
};

export type BacktestDetailResponse = {
  backtest: BacktestSummary;
  config: Record<string, unknown>;
  metrics: BacktestMetrics;
  strategy_metrics: Record<string, BacktestMetrics>;
};

export type EquityPoint = { ts: string; balance_usd: number };

export type EquityResponse = {
  scope: string;
  points: EquityPoint[];
};

export type BacktestTrade = {
  id: number;
  strategy_id: string;
  symbol: string;
  side: string;
  qty: number;
  entry_ts: string;
  exit_ts: string | null;
  entry_px: number;
  exit_px: number | null;
  realized_pnl_usd: number;
};

export type BacktestTradesResponse = { trades: BacktestTrade[] };

/**
 * Orders are an opaque pass-through array (api-ws-redis §7.2): quantities are
 * strings, prices numbers, plus engine-defined fields. We type the keys we
 * render and keep the rest open.
 */
export type BacktestOrder = {
  client_order_id?: string;
  order_id?: string;
  instrument_id?: string;
  symbol?: string;
  side?: string;
  type?: string;
  order_type?: string;
  quantity?: string | number;
  qty?: string | number;
  price?: number | string;
  avg_px?: number | string;
  status?: string;
  ts_init?: string;
  ts_last?: string;
  [k: string]: unknown;
};

/** A scripted-strategy trade intent (POST /backtests body element). */
export type BacktestIntent = {
  date: string;
  ticker: string;
  side: "LONG" | "SHORT" | "FLAT";
  qty: number;
};

export type BacktestUniverseSpec = {
  start: string;
  end: string;
  table?: string;
};

export type RealisticParams = {
  slippage_bps?: number;
  commission_bps?: number;
  commission_per_share?: number;
};

export type CreateBacktestRequest = {
  start: string;
  end: string;
  tickers?: string[];
  universe?: BacktestUniverseSpec;
  starting_balance?: number;
  fill_profile?: FillProfile;
  strategy?: string;
  orb_symbol?: string;
  intents?: BacktestIntent[];
  kind?: string;
  seed?: number;
  run_ts?: string;
  realistic?: RealisticParams;
  actor?: string;
  max_attempts?: number;
  dedupe_key?: string;
};

// ---- Strategies ----

/** One parameter's schema: resolved default + optional numeric search bounds. */
export type ParamSchema = {
  name: string;
  type: "float" | "int" | "str" | "list" | string;
  default: unknown;
  search_low?: number;
  search_high?: number;
  description?: string;
};

/**
 * A production strategy's resolved metadata + active params + schema.
 *
 * `id` is the canonical loader stem (sepa|sector_rotation|pairs|intraday_breakout).
 * `backtest_id` is the token POST /backtests accepts (the only difference is ORB:
 * id "intraday_breakout" -> backtest_id "orb"). The list page links by `id` and
 * launches a run with `backtest_id`.
 */
export type StrategyMeta = {
  id: string;
  backtest_id: string;
  label: string;
  description: string;
  capital_pct: number | null;
  active: boolean;
  params_source: "db" | "file" | "baseline" | string;
  schema_version: number;
  parameters_count: number;
  parameters: ParamSchema[];
  active_values: Record<string, unknown>;
  error?: string;
};

export type StrategiesResponse = { strategies: StrategyMeta[] };
export type StrategyResponse = { strategy: StrategyMeta };

/** Strategy tokens the backtest dialog can launch directly. */
export const BACKTEST_STRATEGIES = [
  "scripted",
  "sepa",
  "sector_rotation",
  "pairs",
  "orb",
  "multi",
] as const;
export type BacktestStrategy = (typeof BACKTEST_STRATEGIES)[number];

// ---- WebSocket envelope ----

export type WsEventType =
  | "hello"
  | "job"
  | "sync"
  | "signal_intent"
  | "strategy_state"
  | "portfolio_health"
  | "watchlist"
  | "position"
  // P6 paper/live trading frames.
  | "order_update"
  | "fill_update"
  | "live_position"
  | "account_update";

export type WsEnvelope = {
  type: WsEventType;
  ts: string;
  payload: Record<string, unknown>;
};

export type JobEvent = {
  job_id: number;
  kind: string;
  event:
    | "enqueued"
    | "claimed"
    | "progress"
    | "succeeded"
    | "failed"
    | "requeued"
    | "released"
    | "canceled"
    | "cancel_requested"
    | "reaped";
  status: JobStatus;
  worker?: string;
  progress?: JobProgress;
  error?: string;
  ts: string;
};

// Dataset vocabulary used by the refresh dialog (distinct from tickers.table_name).
export const DATASETS = ["TICKERS", "SEP", "SFP", "SF1", "EVENTS"] as const;
export type Dataset = (typeof DATASETS)[number];

// ---- Hyperopt (NSGA-II walk-forward studies) ----

/** Strategies a study can optimize (handlers_hyperopt.go validation). */
export const HYPEROPT_STRATEGIES = [
  "sepa",
  "sector_rotation",
  "pairs",
  "joint",
] as const;
export type HyperoptStrategy = (typeof HYPEROPT_STRATEGIES)[number];

/** Lifecycle status of a study (DB source of truth; mirrors progress.json). */
export type StudyStatus = "RUNNING" | "COMPLETE" | "INTERRUPTED" | "FAIL";

/** Per-trial completion state. */
export type TrialState = "COMPLETE" | "RUNNING" | "FAIL" | "PRUNED" | string;

export type WalkForwardConfig = {
  enabled: boolean;
  folds: number;
  embargo_days: number;
};

/** Study config block (config.* in the API response). */
export type StudyConfig = {
  version: number;
  study_name: string;
  strategy: HyperoptStrategy | string;
  start: string;
  end: string;
  directions: string[];
  objectives: string[];
  seed: number;
  n_trials: number;
  workers: number;
  walk_forward: WalkForwardConfig;
  created_at: string;
  updated_at: string;
};

/** The best (trial, sharpe, calmar) tuple observed so far. */
export type CurrentBest = {
  trial: number;
  sharpe: number;
  calmar: number;
};

/** Study progress block (progress.* in the API response). */
export type StudyProgress = {
  status: StudyStatus;
  completed_trials: number;
  failed_trials: number;
  running_trials: number;
  total_trials: number;
  workers: number;
  started_at: string | null;
  last_heartbeat_at: string | null;
  coordinator_pid: number | null;
  current_best: CurrentBest | null;
  last_error: string | null;
};

/** One study, as returned by the list and detail endpoints. */
export type StudyRow = {
  ts: string;
  config: StudyConfig;
  progress: StudyProgress;
};

export type StudiesResponse = { studies: StudyRow[] };
export type StudyResponse = { study: StudyRow };

/** Per-fold metric breakdown for a trial (folds[] elements; open shape). */
export type TrialFold = {
  fold: number;
  sharpe?: number;
  calmar?: number;
  final_balance_usd?: number;
  max_drawdown_pct?: number;
  total_pnl_usd?: number;
  train_start?: string;
  train_end?: string;
  test_start?: string;
  test_end?: string;
  [k: string]: unknown;
};

/** Aggregate per-trial metrics (metrics.* — same vocabulary as backtests). */
export type TrialMetrics = {
  final_balance_usd?: number;
  total_pnl_usd?: number;
  sharpe?: number;
  calmar?: number;
  max_drawdown_pct?: number;
  num_orders?: number;
  num_filled_orders?: number;
  num_rejected_orders?: number;
  num_positions?: number;
  [k: string]: unknown;
};

export type TrialRow = {
  number: number;
  optuna_number: number;
  strategy: string;
  params: Record<string, unknown> | null;
  metrics: TrialMetrics | null;
  folds: TrialFold[];
  state: TrialState;
  sharpe: number | null;
  calmar: number | null;
  started_at: string;
  finished_at: string | null;
  duration_sec: number | null;
  run_dump_ts: string | null;
  error: string | null;
  pareto_front: boolean;
};

export type TrialsResponse = { trials: TrialRow[] };

/** POST /api/v1/hyperopt body. */
export type CreateStudyRequest = {
  strategy: HyperoptStrategy;
  start: string;
  end: string;
  population?: number;
  generations?: number;
  seed?: number;
  workers?: number;
  walk_forward?: boolean;
  folds?: number;
  embargo_days?: number;
  tickers?: string[];
  universe?: BacktestUniverseSpec;
  starting_balance?: number;
  study_ts?: string;
  actor?: string;
  dedupe_key?: string;
  max_attempts?: number;
};

/** POST /api/v1/hyperopt/{id}/promote body. */
export type PromoteRequest = {
  trial_id: number;
  actor?: string;
};

export type PromotedEntry = {
  strategy: string;
  param_set_id: number;
  version: number;
};

export type PromoteResponse = {
  study_ts: string;
  trial_id: number;
  promoted: PromotedEntry[];
};

// ---- Live (P5 cockpit) ----
//
// The live read surface (docs/api.md §Live). All reads come from PostgreSQL
// (the durable truth); Redis only powers the WS push deltas. The only write is
// the audited command-enqueue endpoint. Timestamps are RFC3339 UTC; `ts_event`
// is an epoch-nanosecond engine clock (api-ws-redis §5.x).

/** Live session lifecycle status (DB source of truth). */
export type LiveStatus = "RUNNING" | "STOPPED" | "CRASHED" | string;

/** Trading mode. paper/live are deferred to P6 but the contract carries them. */
export type LiveMode = "signal" | "paper" | "live" | string;

/** Halt kind (live.halts.kind). */
export type HaltKind =
  | "manual"
  | "daily_loss"
  | "reconciliation"
  | "data"
  | "broker"
  | "other"
  | string;

export type LiveHalt = {
  kind: HaltKind;
  reason: string;
  triggered_at: string;
};

export type LiveSession = {
  id: number;
  trader_id: string;
  mode: LiveMode;
  status: LiveStatus;
  started_at: string;
  ended_at: string | null;
  config: Record<string, unknown>;
  /** Active halt, or null when not halted. */
  halt: LiveHalt | null;
};

/** GET /api/v1/live/session — `{ session: null }` before any session ran. */
export type LiveSessionResponse = LiveSession | { session: null };

/** Narrows the union: true when a session exists. */
export function hasSession(
  r: LiveSessionResponse | undefined,
): r is LiveSession {
  return !!r && (r as LiveSession).id !== undefined;
}

/**
 * One recent signal intent (tms.signal_intents). `state` is the per-strategy
 * decision token (buy|forming|hold|exit|flat|…, strategy-defined). `intent` is
 * the unwrapped SignalIntentUnion variant (open shape).
 */
export type LiveIntent = {
  strategy_id: string;
  symbol: string;
  state: string;
  strength: number;
  generation: number;
  intent: Record<string, unknown>;
  ts: string;
  ts_event: number;
};

export type LiveIntentsResponse = { intents: LiveIntent[] };

/**
 * Latest portfolio-health snapshot. In signal mode this is the flat-book
 * informational NAV (day P&L 0, no halt — decision 6). Values are percent units
 * already (e.g. halt_headroom_pct 0.04 means a 4-point fraction; see formatter).
 */
export type LiveHealth = {
  day_pnl: number;
  day_pnl_pct: number;
  daily_loss_halt: boolean;
  halt_headroom_pct: number;
  concentration_pct: number;
  ts: string;
};

// `intents` (additive) carries the latest intent per symbol, frontier-windowed
// and ranked actionable-first by the API, so every watchlist row shows its state
// without a separate capped intents poll. Older servers omit it (undefined).
export type WatchlistResponse = { symbols: string[]; intents?: LiveIntent[] };

/** Live control command names (commands.Name; docs/api.md §POST live/commands). */
export type CommandName =
  | "start"
  | "stop"
  | "set_mode"
  | "halt"
  | "resume"
  | "kill"
  // P6 paper/live trading controls (confirm_token required for flatten /
  // emergency_kill; reconcile is read-only).
  | "flatten"
  | "emergency_kill"
  | "reconcile";

/** POST /api/v1/live/commands body. confirm_token is consumed, never persisted. */
export type LiveCommandRequest = {
  name: CommandName;
  mode?: LiveMode;
  reason?: string;
  confirm_token?: string;
};

export type LiveCommandResponse = {
  command_id: number;
  status: "pending";
};

// ---- Live WS push payloads (bridged Redis streams; docs/api.md §ws table) ----

/** `signal_intent` frame payload. */
export type WsSignalIntent = {
  strategy_id: string;
  symbol: string;
  intent_json: Record<string, unknown>;
  ts_event: number;
  ts_init: number;
};

/** `strategy_state` frame payload (`state_json` is a JSON string). */
export type WsStrategyState = {
  strategy_id: string;
  state_json: string;
  ts_event: number;
  ts_init: number;
};

/** `portfolio_health` frame payload. */
export type WsPortfolioHealth = {
  day_pnl: number;
  day_pnl_pct: number;
  daily_loss_halt: boolean;
  halt_headroom_pct: number;
  concentration_pct: number;
  ts_event: number;
  ts_init: number;
};

/** `watchlist` frame payload. */
export type WsWatchlist = {
  symbols: string[];
  ts_event: number;
  ts_init: number;
};

/** `position` frame payload (positions empty in signal mode). */
export type WsPosition = {
  positions: unknown[];
  ts_event: number;
  ts_init: number;
};

// ---- Live trading (P6, paper/live) ----
//
// The paper/live read surface (docs/api.md §"Live trading (P6, paper/live)").
// All reads come from PG (the durable system-of-record); the cockpit follows the
// Redis `data.*` streams live (WsOrderUpdate / WsFillUpdate / WsLivePosition /
// WsAccountUpdate) and reconstructs from these on (re)connect. READ-ONLY — the
// trading mutation surface stays on the audited command channel.

/**
 * Order lifecycle status (the moomoo→domain state machine, ADR-004). UPPERCASE
 * on the wire. Unknown values render neutral so a new state never breaks the
 * blotter.
 */
export type LiveOrderStatus =
  | "SUBMITTED"
  | "ACCEPTED"
  | "WORKING"
  | "PARTIALLY_FILLED"
  | "FILLED"
  | "REJECTED"
  | "CANCELED"
  | "EXPIRED"
  | string;

/** One order row (GET /api/v1/live/orders). Prices are floats (USD). */
export type LiveOrder = {
  client_order_id: string;
  venue_order_id?: string;
  strategy_id: string;
  symbol: string;
  side: string;
  qty: number;
  filled_qty: number;
  avg_fill_px: number;
  status: LiveOrderStatus;
  reason?: string;
  ts: string;
};

export type LiveOrdersResponse = { orders: LiveOrder[] };

/** One execution (GET /api/v1/live/fills). */
export type LiveFill = {
  trade_id: string;
  symbol: string;
  qty: number;
  price: number;
  commission: number;
  ts: string;
};

export type LiveFillsResponse = { fills: LiveFill[] };

/** One open position (GET /api/v1/live/positions). */
export type LiveTradePosition = {
  strategy_id: string;
  symbol: string;
  signed_qty: number;
  avg_entry_px: number;
  realized_pnl: number;
  status: string;
};

export type LivePositionsResponse = { positions: LiveTradePosition[] };

/** Account / buying-power + day-PnL snapshot (GET /api/v1/live/account). */
export type LiveAccount = {
  total_assets: number;
  cash: number;
  available_funds: number; // buying power
  market_value: number;
  day_pnl: number;
  ts: string;
};

// ---- Account registry (GET /api/v1/trade/accounts) ----
//
// The tms.accounts registry that backs the cockpit/desk account selector. Mirrors
// the Go wire type `api.TradeAccountInfo` (distinct from the funds snapshot above).
// `id` is the selector value (e.g. "moomoo:real:123", "sim:signal"); the
// positions/blotter/account reads pass it back as `?account_id=`.

/** One registered broker/sim account (GET /api/v1/trade/accounts). */
export type TradeAccountInfo = {
  id: string;
  venue: string;
  env: string;
  broker_acc_id: number;
  label: string;
};

export type TradeAccountsResponse = { accounts: TradeAccountInfo[] };

// Trade* aliases for the renamed cockpit (P5). The /trade/* wire shapes are
// byte-identical to the legacy Live* ones, so these are pure type aliases kept so
// callers can speak in the new vocabulary without a sweeping rename.
export type TradeOrderStatus = LiveOrderStatus;
export type TradeOrder = LiveOrder;
export type TradeOrdersResponse = LiveOrdersResponse;
export type TradeFill = LiveFill;
export type TradeFillsResponse = LiveFillsResponse;
export type TradePosition = LiveTradePosition;
export type TradePositionsResponse = LivePositionsResponse;
export type TradeAccountFunds = LiveAccount;

/** One reconciliation mismatch row (diff = broker_net − strategy_books_sum). */
export type ReconMismatch = {
  symbol: string;
  strategy_books_sum: number;
  broker_net: number;
  diff: number;
};

/** The latest reconciliation report (GET /api/v1/live/reconciliation). */
export type LiveReconciliation = {
  ts: string;
  has_issues: boolean;
  tolerance_shares: number;
  matched: string[];
  mismatches: ReconMismatch[];
  symbols_only_in_strategies: string[];
  symbols_only_at_broker: string[];
};

/** `{ reconciliation: null }` before any reconcile ran; the report otherwise. */
export type LiveReconciliationResponse =
  | LiveReconciliation
  | { reconciliation: null };

/** Narrows the union: true when a reconciliation report exists. */
export function hasReconciliation(
  r: LiveReconciliationResponse | undefined,
): r is LiveReconciliation {
  return !!r && (r as LiveReconciliation).ts !== undefined;
}

// ---- Live trading WS push payloads (P6; bridged Redis streams) ----

/** `order_update` frame payload. */
export type WsOrderUpdate = {
  client_order_id: string;
  venue_order_id?: string;
  strategy_id: string;
  symbol: string;
  side: string;
  qty: number;
  filled_qty: number;
  avg_fill_px: number;
  status: LiveOrderStatus;
  reason?: string;
  ts_event: number;
  ts_init: number;
};

/** `fill_update` frame payload. */
export type WsFillUpdate = {
  trade_id: string;
  client_order_id: string;
  venue_order_id?: string;
  strategy_id: string;
  symbol: string;
  side: string;
  qty: number;
  price: number;
  commission: number;
  ts_event: number;
  ts_init: number;
};

/** One position row in a `live_position` book snapshot. */
export type WsLivePositionRow = {
  strategy_id: string;
  symbol: string;
  signed_qty: number;
  avg_px: number;
  realized_pnl: number;
};

/** `live_position` frame payload (full book snapshot — replace, not delta). */
export type WsLivePosition = {
  positions: WsLivePositionRow[];
  ts_event: number;
  ts_init: number;
};

/** `account_update` frame payload (broker funds / buying power). */
export type WsAccountUpdate = {
  total_assets: number;
  cash: number;
  available_funds: number;
  market_value: number;
  day_pnl: number;
  ts_event: number;
  ts_init: number;
};

// ---- System status (GET /api/v1/system) ----
//
// The single aggregated component-health response the System page binds to:
// pg + redis + moomoo feed + active sessions + job-queue depth + data
// freshness in one call (the P7 "UI fully observable" capstone).

/** One component's health line. status: ok|degraded|down|idle|unknown|not_configured. */
export type SystemComponent = {
  status: string;
  detail?: string;
};

/** Structured numeric surface behind the component lines. */
export type SystemMetrics = {
  jobs_queued: number;
  jobs_running: number;
  active_sessions: number;
  latest_bar_date?: string;
  last_sync_at?: string | null;
  live_mode?: string;
  live_session_id?: number | null;
  health_age_seconds?: number | null;
};

/** GET /api/v1/system body. */
export type SystemResponse = {
  status: string;
  version: string;
  ts: string;
  components: Record<string, SystemComponent>;
  metrics: SystemMetrics;
};

// ---- /api/v1/live/preflight (go-live precondition report) ----

/** One preflight precondition check result. */
export type PreflightResult = {
  check: string;
  /** pass | warn | fail | skip */
  status: string;
  /** blocker | warn */
  severity: string;
  detail: string;
};

/** GET /api/v1/live/preflight body: the go/no-go report. ok is the verdict bit. */
export type PreflightReport = {
  mode: string;
  strategy: string;
  ts: string;
  ok: boolean;
  checks: PreflightResult[];
};

// ---- /api/v1/audit (append-only operational audit trail) ----

/** One tms.audit_log row. entity/entity_id/details are optional. */
export type AuditEntry = {
  id: number;
  ts: string;
  actor: string;
  action: string;
  entity?: string;
  entity_id?: string;
  details?: Record<string, unknown>;
};

/** GET /api/v1/audit body. next_before is the keyset cursor for the older page. */
export type AuditResponse = {
  entries: AuditEntry[];
  next_before: number | null;
};

/** Response of POST /api/v1/jobs/{id}/retry: the cloned (new) job + the source id. */
export type JobRetryResponse = {
  job: Job;
  source_job_id: number;
};

// ---- Manual trading desk (operator-driven, P6) ----
//
// The ONLY broker-mutation surface in the HTTP API (docs/api.md §"Manual trading
// desk"). An operator places / cancels / closes orders BY HAND against a paper or
// live account. Manual orders are attributed to the MANUAL pseudo-strategy
// (`MANUAL_STRATEGY_ID`), distinct from the auto strategies' books, so
// reconciliation + per-strategy accounting stay clean. SAFETY (paramount): a LIVE
// (real-money) order requires the full 4-factor live activation (server-side) PLUS
// the per-order typed confirm phrase below; a PAPER order requires the trade
// password. The server is the final, authoritative gate — there is NO path to a
// real order without it. The UI surfaces the server's 412 / 422 verbatim.

/** The pseudo-strategy id every manual order is attributed to (livetrade.ManualStrategyID). */
export const MANUAL_STRATEGY_ID = "MANUAL";

/**
 * The exact per-order confirmation phrase a LIVE (real-money) manual order
 * requires in `confirm_token` (livetrade.ManualLiveConfirmationPhrase). The UI
 * makes the operator type this verbatim; the live desk re-checks it at the
 * boundary and rejects (412) on any mismatch.
 */
export const MANUAL_LIVE_CONFIRM_PHRASE = "I CONFIRM THIS REAL MONEY MANUAL ORDER";

/** Manual order side / type vocabularies (server-validated). */
export type ManualSide = "BUY" | "SELL";
export type ManualOrderType = "MARKET" | "LIMIT";

/** POST /api/v1/trade/order body. `confirm_token` is consumed, never persisted. */
export type ManualOrderRequest = {
  /** Idempotency key — makes the client-order-id deterministic (no double-submit). */
  idempotency_key: string;
  symbol: string;
  side: ManualSide;
  qty: number;
  type?: ManualOrderType; // default MARKET
  limit_price?: number; // required (> 0) for LIMIT
  /** Explicit, audited operator override of a risk-gate rejection. */
  override?: boolean;
  /** Live confirm phrase (LIVE) or trade password (PAPER). */
  confirm_token?: string;
  reason?: string;
};

/** 200 response of POST /api/v1/trade/order. */
export type ManualOrderResponse = {
  client_order_id: string;
  submitted: boolean;
  status: "submitted";
};

/** 200 response of POST /api/v1/trade/order/{coid}/cancel. */
export type ManualCancelResponse = {
  client_order_id: string;
  status: "cancel_requested";
};

/** POST /api/v1/trade/position/{symbol}/close body. qty 0/omitted ⇒ full close. */
export type ManualCloseRequest = {
  qty?: number;
  confirm_token?: string;
};

/** 200 response of POST /api/v1/trade/position/{symbol}/close. */
export type ManualCloseResponse = {
  client_order_id: string;
  submitted: boolean;
  symbol: string;
  status: "close_submitted";
};

/**
 * The reconciliation summary embedded in a sync response (broker vs strategy
 * books). A lighter shape than the full `LiveReconciliation` read — the sync
 * endpoint returns just the go/no-go bits + the drift list.
 */
export type ManualSyncReconciliation = {
  has_issues: boolean;
  summary?: string;
  matched: number;
  mismatches: ReconMismatch[];
};

/**
 * 200 response of POST /api/v1/trade/sync — DIRECTION 2 (broker → TMS). The
 * operator traded DIRECTLY in moomoo; this pull is READ-ONLY at the broker
 * (`read_only: true`, places NO orders) and reflects the broker's actual
 * positions/orders/fills/account into the MANUAL/EXTERNAL book, then reconciles
 * vs the strategy books. Idempotent: re-syncing the same broker state reflects
 * nothing.
 */
export type ManualSyncResponse = {
  status: "synced";
  positions_observed: number;
  orders_observed: number;
  fills_observed: number;
  reflected: number;
  read_only: boolean;
  reconciliation: ManualSyncReconciliation;
};
