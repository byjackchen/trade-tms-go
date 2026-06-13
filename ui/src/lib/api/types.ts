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
  intents?: BacktestIntent[];
  kind?: string;
  seed?: number;
  run_ts?: string;
  realistic?: RealisticParams;
  actor?: string;
  max_attempts?: number;
  dedupe_key?: string;
};

// ---- WebSocket envelope ----

export type WsEventType = "hello" | "job" | "sync";

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
