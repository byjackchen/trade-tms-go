"use client";

import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseQueryResult,
} from "@tanstack/react-query";
import { apiGet, apiPost } from "./client";
import type {
  CoverageResponse,
  TickerGapDetail,
  SyncRunsResponse,
  JobsResponse,
  JobResponse,
  Job,
  EnqueueResponse,
  DataRefreshRequest,
  UniverseLatestResponse,
  UniverseRebuildRequest,
  BacktestsResponse,
  BacktestDetailResponse,
  EquityResponse,
  BacktestTradesResponse,
  BacktestOrder,
  CreateBacktestRequest,
  StrategiesResponse,
  StrategyResponse,
  StudiesResponse,
  StudyResponse,
  TrialsResponse,
  CreateStudyRequest,
  PromoteRequest,
  PromoteResponse,
  LiveSessionResponse,
  LiveIntentsResponse,
  LiveHealth,
  WatchlistResponse,
  LiveCommandRequest,
  LiveCommandResponse,
  LiveOrdersResponse,
  LiveFillsResponse,
  LivePositionsResponse,
  LiveAccount,
  LiveReconciliationResponse,
  SystemResponse,
  PreflightReport,
  AuditResponse,
  JobRetryResponse,
  ManualOrderRequest,
  ManualOrderResponse,
  ManualCancelResponse,
  ManualCloseRequest,
  ManualCloseResponse,
  ManualSyncResponse,
} from "./types";
import { ApiError } from "./client";

export function useCoverage(): UseQueryResult<CoverageResponse, Error> {
  return useQuery({
    queryKey: ["coverage"],
    queryFn: () => apiGet<CoverageResponse>("data/coverage"),
  });
}

export function useTickerGaps(
  ticker: string | null,
): UseQueryResult<TickerGapDetail, Error> {
  return useQuery({
    queryKey: ["coverage", "ticker", ticker],
    queryFn: () =>
      apiGet<TickerGapDetail>("data/coverage", { ticker: ticker as string }),
    enabled: Boolean(ticker),
    refetchInterval: false,
  });
}

export function useSyncRuns(): UseQueryResult<SyncRunsResponse, Error> {
  return useQuery({
    queryKey: ["sync-runs"],
    queryFn: () => apiGet<SyncRunsResponse>("data/sync-runs", { limit: 50 }),
  });
}

export function useJobs(
  kind?: string,
): UseQueryResult<JobsResponse, Error> {
  return useQuery({
    queryKey: ["jobs", kind ?? "all"],
    queryFn: () =>
      apiGet<JobsResponse>("jobs", { kind, limit: 50 }),
  });
}

export function useUniverseLatest(): UseQueryResult<
  UniverseLatestResponse,
  Error
> {
  return useQuery({
    queryKey: ["universe", "latest"],
    queryFn: () => apiGet<UniverseLatestResponse>("universe/latest"),
    retry: (count, err) => {
      // 404 (no snapshot yet) is an expected empty state, not a retry case.
      return !(err as { status?: number })?.status && count < 1;
    },
  });
}

export function useRefreshData() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: DataRefreshRequest) =>
      apiPost<EnqueueResponse>("data/refresh", body),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["jobs"] });
      void qc.invalidateQueries({ queryKey: ["sync-runs"] });
    },
  });
}

export function useRebuildUniverse() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: UniverseRebuildRequest) =>
      apiPost<EnqueueResponse>("universe/rebuild", body),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["jobs"] });
    },
  });
}

/** Fetch a single job (used for completion reconciliation after the WS drops). */
export function useJob(
  id: number | null,
  enabled: boolean,
): UseQueryResult<JobResponse, Error> {
  return useQuery({
    queryKey: ["job", id],
    queryFn: () => apiGet<JobResponse>(`jobs/${id}`),
    enabled: enabled && id != null,
    refetchInterval: enabled ? 4000 : false,
  });
}

// ---- Backtests ----

export function useBacktests(filters?: {
  status?: string;
  kind?: string;
}): UseQueryResult<BacktestsResponse, Error> {
  return useQuery({
    queryKey: ["backtests", filters?.status ?? "all", filters?.kind ?? "all"],
    queryFn: () =>
      apiGet<BacktestsResponse>("backtests", {
        status: filters?.status,
        kind: filters?.kind,
        limit: 200,
      }),
    // While a run is in flight (RUNNING rows present) the list self-refreshes so
    // the table converges to terminal status without a manual reload.
    refetchInterval: (query) => {
      const rows = query.state.data?.backtests ?? [];
      return rows.some((b) => b.status === "RUNNING") ? 4000 : false;
    },
  });
}

export function useBacktest(
  id: number | null,
): UseQueryResult<BacktestDetailResponse, Error> {
  return useQuery({
    queryKey: ["backtest", id],
    queryFn: () => apiGet<BacktestDetailResponse>(`backtests/${id}`),
    enabled: id != null,
    // Poll while the run is still active so the detail page fills in on complete.
    refetchInterval: (query) =>
      query.state.data?.backtest.status === "RUNNING" ? 4000 : false,
  });
}

export function useBacktestEquity(
  id: number | null,
  strategy?: string,
): UseQueryResult<EquityResponse, Error> {
  return useQuery({
    queryKey: ["backtest", id, "equity", strategy ?? "portfolio"],
    queryFn: () =>
      apiGet<EquityResponse>(`backtests/${id}/equity`, { strategy }),
    enabled: id != null,
  });
}

export function useBacktestTrades(
  id: number | null,
): UseQueryResult<BacktestTradesResponse, Error> {
  return useQuery({
    queryKey: ["backtest", id, "trades"],
    queryFn: () => apiGet<BacktestTradesResponse>(`backtests/${id}/trades`),
    enabled: id != null,
  });
}

export function useBacktestOrders(
  id: number | null,
): UseQueryResult<BacktestOrder[], Error> {
  return useQuery({
    queryKey: ["backtest", id, "orders"],
    queryFn: () => apiGet<BacktestOrder[]>(`backtests/${id}/orders`),
    enabled: id != null,
  });
}

export function useCreateBacktest() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateBacktestRequest) =>
      apiPost<EnqueueResponse>("backtests", body),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["backtests"] });
      void qc.invalidateQueries({ queryKey: ["jobs"] });
    },
  });
}

// ---- Strategies ----

export function useStrategies(): UseQueryResult<StrategiesResponse, Error> {
  return useQuery({
    queryKey: ["strategies"],
    queryFn: () => apiGet<StrategiesResponse>("strategies"),
  });
}

export function useStrategy(
  id: string | null,
): UseQueryResult<StrategyResponse, Error> {
  return useQuery({
    queryKey: ["strategy", id],
    queryFn: () => apiGet<StrategyResponse>(`strategies/${id}`),
    enabled: id != null && id !== "",
    retry: (count, err) => {
      // 404 (unknown strategy id) is a terminal empty state, not a retry case.
      return (err as { status?: number })?.status !== 404 && count < 2;
    },
  });
}

export function useCancelJob() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { id: number; actor?: string }) =>
      apiPost<{ outcome: string; job: Job }>(`jobs/${args.id}/cancel`, {
        actor: args.actor,
      }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["jobs"] });
      void qc.invalidateQueries({ queryKey: ["ops", "jobs"] });
    },
  });
}

// ---- Ops (job queue + audit log) ----
//
// The Ops workspace surfaces the operational layer. The job queue self-refreshes
// while any job is still active (queued/running) so the table + drawer converge
// to terminal state without a reload; the WS/SSE job stream layers live progress
// on top (the page also invalidates these queries on each job event).

/**
 * Full job list for the Ops queue, with optional kind + status filters. Polls
 * while any row is non-terminal so progress + terminal transitions land without
 * a manual reload (the SSE bridge gives sub-second updates on top of this).
 */
export function useOpsJobs(filters?: {
  kind?: string;
  status?: string;
}): UseQueryResult<JobsResponse, Error> {
  return useQuery({
    queryKey: ["ops", "jobs", filters?.kind ?? "all", filters?.status ?? "all"],
    queryFn: () =>
      apiGet<JobsResponse>("jobs", {
        kind: filters?.kind,
        status: filters?.status,
        limit: 200,
      }),
    refetchInterval: (query) => {
      const rows = query.state.data?.jobs ?? [];
      return rows.some((j) => j.status === "queued" || j.status === "running")
        ? 3000
        : false;
    },
  });
}

/**
 * One job's full detail (the Ops drawer). Polls while the job is active so the
 * drawer's progress/result/error fill in live; stops once terminal.
 */
export function useOpsJob(
  id: number | null,
): UseQueryResult<JobResponse, Error> {
  return useQuery({
    queryKey: ["ops", "job", id],
    queryFn: () => apiGet<JobResponse>(`jobs/${id}`),
    enabled: id != null,
    retry: (count, err) =>
      (err as { status?: number })?.status !== 404 && count < 2,
    refetchInterval: (query) => {
      const s = query.state.data?.job.status;
      return s === "queued" || s === "running" ? 3000 : false;
    },
  });
}

/** Retry a terminal (failed/canceled) job: enqueues a fresh clone of its
 * kind+payload. Invalidates the queue so the new row appears immediately. */
export function useRetryJob() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { id: number; actor?: string }) =>
      apiPost<JobRetryResponse>(`jobs/${args.id}/retry`, { actor: args.actor }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["ops", "jobs"] });
      void qc.invalidateQueries({ queryKey: ["jobs"] });
    },
  });
}

/**
 * Audit-log stream for the Ops AUDIT LOG panel: newest-first, with optional
 * exact-match filters. A 503 (no audit reader wired) is an expected degraded
 * state surfaced as an empty panel, not retried. Polls slowly so new rows land
 * without a reload (the trail is append-only).
 */
export function useAudit(filters?: {
  actor?: string;
  action?: string;
  entity?: string;
}): UseQueryResult<AuditResponse, Error> {
  return useQuery({
    queryKey: [
      "ops",
      "audit",
      filters?.actor ?? "all",
      filters?.action ?? "all",
      filters?.entity ?? "all",
    ],
    queryFn: () =>
      apiGet<AuditResponse>("audit", {
        actor: filters?.actor,
        action: filters?.action,
        entity: filters?.entity,
        limit: 100,
      }),
    refetchInterval: 8000,
    retry: (count, err) => {
      const status = err instanceof ApiError ? err.status : undefined;
      if (status === 503 || (status !== undefined && status < 500)) return false;
      return count < 2;
    },
  });
}

// ---- Hyperopt ----

export function useStudies(
  strategy?: string,
): UseQueryResult<StudiesResponse, Error> {
  return useQuery({
    queryKey: ["hyperopt", "studies", strategy ?? "all"],
    queryFn: () =>
      apiGet<StudiesResponse>("hyperopt", { strategy, limit: 200 }),
    // While any study is still RUNNING the list self-refreshes so the table
    // converges (trial counts, best-so-far, terminal status) without a reload.
    refetchInterval: (query) => {
      const rows = query.state.data?.studies ?? [];
      return rows.some((s) => s.progress.status === "RUNNING") ? 4000 : false;
    },
  });
}

export function useStudy(
  id: string | null,
): UseQueryResult<StudyResponse, Error> {
  return useQuery({
    queryKey: ["hyperopt", "study", id],
    queryFn: () => apiGet<StudyResponse>(`hyperopt/${id}`),
    enabled: id != null && id !== "",
    retry: (count, err) => {
      // 404 (unknown study) / 400 (malformed ts) are terminal empty states.
      const status = (err as { status?: number })?.status;
      return status !== 404 && status !== 400 && count < 2;
    },
    // Poll while the study is still running so detail fills in on completion.
    refetchInterval: (query) =>
      query.state.data?.study.progress.status === "RUNNING" ? 4000 : false,
  });
}

export function useStudyTrials(
  id: string | null,
  status?: StudyStatusForPoll,
): UseQueryResult<TrialsResponse, Error> {
  return useQuery({
    queryKey: ["hyperopt", "trials", id],
    queryFn: () => apiGet<TrialsResponse>(`hyperopt/${id}/trials`),
    enabled: id != null && id !== "",
    retry: (count, err) => {
      const code = (err as { status?: number })?.status;
      return code !== 404 && code !== 400 && count < 2;
    },
    // While the study is RUNNING new trials land continuously; refresh so the
    // Pareto front / table / convergence chart grow live.
    refetchInterval: status === "RUNNING" ? 5000 : false,
  });
}

type StudyStatusForPoll = "RUNNING" | "COMPLETE" | "INTERRUPTED" | "FAIL" | undefined;

export function useCreateStudy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateStudyRequest) =>
      apiPost<EnqueueResponse>("hyperopt", body),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["hyperopt", "studies"] });
      void qc.invalidateQueries({ queryKey: ["jobs"] });
    },
  });
}

export function usePromoteTrial(studyTS: string | null) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: PromoteRequest) =>
      apiPost<PromoteResponse>(`hyperopt/${studyTS}/promote`, body),
    onSuccess: () => {
      // The promoted strategy's active_params changed: bust strategy caches so
      // the audit (active_values / params_source) reflects immediately.
      void qc.invalidateQueries({ queryKey: ["strategies"] });
      void qc.invalidateQueries({ queryKey: ["strategy"] });
      void qc.invalidateQueries({ queryKey: ["hyperopt", "trials", studyTS] });
    },
  });
}

// ---- Live (P5 cockpit) ----
//
// All live reads are PG-backed snapshots; the WS push (useLiveStream) layers
// real-time deltas on top of them. A 503 from these endpoints means the API was
// started without a live reader (or the producer has not run) — that is an
// expected degraded state, NOT an error to retry into, so we surface the 503
// rather than spinning.

/** Don't retry deterministic 4xx/503 (degraded-but-expected) live responses. */
function liveRetry(count: number, err: Error): boolean {
  const status = err instanceof ApiError ? err.status : undefined;
  if (status === 503 || (status !== undefined && status < 500)) return false;
  return count < 2;
}

export function useLiveSession(): UseQueryResult<LiveSessionResponse, Error> {
  return useQuery({
    queryKey: ["live", "session"],
    queryFn: () => apiGet<LiveSessionResponse>("live/session"),
    // Session state changes via commands; a short poll keeps mode/halt/status
    // current even if a WS frame is missed (the WS carries deltas, not session).
    refetchInterval: 5000,
    retry: liveRetry,
  });
}

export function useLiveIntents(
  strategyId?: string,
): UseQueryResult<LiveIntentsResponse, Error> {
  return useQuery({
    queryKey: ["live", "intents", strategyId ?? "all"],
    queryFn: () =>
      apiGet<LiveIntentsResponse>("live/intents", {
        strategy_id: strategyId,
        limit: 200,
      }),
    // WS pushes new intents live; this poll is the reconnect-reconciliation
    // backstop and the initial hydrate.
    refetchInterval: 15000,
    retry: liveRetry,
  });
}

export function useLiveHealth(): UseQueryResult<LiveHealth, Error> {
  return useQuery({
    queryKey: ["live", "health"],
    queryFn: () => apiGet<LiveHealth>("live/health"),
    // Minute cadence on the producer; poll at 20s so the strip stays fresh
    // between WS pushes without hammering.
    refetchInterval: 20000,
    retry: liveRetry,
  });
}

/**
 * Go-live preflight report for a session (mode/strategy). Surfaces the
 * precondition checks (data freshness, warmup, caps, universe, OpenD, PG/Redis)
 * the System page renders. A 503 means the API has no preflight runner wired.
 */
export function useLivePreflight(
  params: { mode?: string; strategy?: string; check_opend?: boolean } = {},
): UseQueryResult<PreflightReport, Error> {
  const mode = params.mode ?? "signal";
  const strategy = params.strategy ?? "multi";
  return useQuery({
    queryKey: ["live", "preflight", mode, strategy, params.check_opend ?? false],
    queryFn: () =>
      apiGet<PreflightReport>("live/preflight", {
        mode,
        strategy,
        check_opend: params.check_opend ? "1" : undefined,
      }),
    // Preconditions drift (a sync completes, params get promoted); re-poll so the
    // panel stays current without a manual refresh.
    refetchInterval: 30000,
    retry: liveRetry,
  });
}

export function useWatchlist(): UseQueryResult<WatchlistResponse, Error> {
  return useQuery({
    queryKey: ["live", "watchlist"],
    queryFn: () => apiGet<WatchlistResponse>("watchlist"),
    refetchInterval: 30000,
    retry: liveRetry,
  });
}

// ---- Live trading (P6, paper/live) ----
//
// PG-backed snapshots; the WS push (useLiveStream order_update/fill_update/
// live_position/account_update) layers real-time deltas on top. A 503 means the
// API has no trading reader (signal-only deployment) — an expected degraded
// state, surfaced rather than retried (liveRetry).

export function useLiveOrders(
  symbol?: string,
): UseQueryResult<LiveOrdersResponse, Error> {
  return useQuery({
    queryKey: ["live", "orders", symbol ?? "all"],
    queryFn: () =>
      apiGet<LiveOrdersResponse>("live/orders", { symbol, limit: 200 }),
    // WS pushes order-state transitions live; this poll is the reconnect-
    // reconciliation backstop and the initial hydrate.
    refetchInterval: 15000,
    retry: liveRetry,
  });
}

export function useLiveFills(
  symbol?: string,
): UseQueryResult<LiveFillsResponse, Error> {
  return useQuery({
    queryKey: ["live", "fills", symbol ?? "all"],
    queryFn: () =>
      apiGet<LiveFillsResponse>("live/fills", { symbol, limit: 200 }),
    refetchInterval: 15000,
    retry: liveRetry,
  });
}

export function useLivePositions(): UseQueryResult<
  LivePositionsResponse,
  Error
> {
  return useQuery({
    queryKey: ["live", "positions"],
    queryFn: () => apiGet<LivePositionsResponse>("live/positions"),
    // The live_position WS frame replaces the book; poll backstops reconnect.
    refetchInterval: 15000,
    retry: liveRetry,
  });
}

export function useLiveAccount(): UseQueryResult<LiveAccount, Error> {
  return useQuery({
    queryKey: ["live", "account"],
    queryFn: () => apiGet<LiveAccount>("live/account"),
    // account_update rides the Redis stream; poll keeps day-PnL fresh.
    refetchInterval: 15000,
    retry: liveRetry,
  });
}

export function useLiveReconciliation(): UseQueryResult<
  LiveReconciliationResponse,
  Error
> {
  return useQuery({
    queryKey: ["live", "reconciliation"],
    queryFn: () =>
      apiGet<LiveReconciliationResponse>("live/reconciliation"),
    // Reconciliation runs periodically + on demand; poll at 20s so a fresh
    // report (or a mismatch-triggered halt) surfaces without a reload.
    refetchInterval: 20000,
    retry: liveRetry,
  });
}

/** Normalized upstream-health shape from the UI server's /api/system-health route. */
export type SystemHealth = {
  status: "ok" | "degraded" | string;
  reachable: boolean;
  version: string | null;
  deps: Record<string, { ok: boolean; error?: string }>;
  error?: string;
};

/**
 * Poll the UI server's system-health route (which proxies the upstream public
 * /healthz). Lives on the UI origin, so it bypasses the /api/proxy bearer path.
 */
export function useSystemHealth(): UseQueryResult<SystemHealth, Error> {
  return useQuery({
    queryKey: ["system-health"],
    queryFn: async () => {
      const resp = await fetch("/api/system-health", {
        headers: { Accept: "application/json" },
      });
      // The route is contractually 200; treat a non-200 as a degraded read.
      if (!resp.ok) {
        return {
          status: "degraded",
          reachable: false,
          version: null,
          deps: {},
          error: `health probe failed (HTTP ${resp.status})`,
        } as SystemHealth;
      }
      return (await resp.json()) as SystemHealth;
    },
    refetchInterval: 10000,
  });
}

/**
 * Poll the aggregated GET /api/v1/system endpoint (bearer-guarded, via the
 * proxy): the single call that backs the System page's component grid + metrics.
 * A 503 means the API has no system reader configured (a deployment choice), an
 * expected degraded state surfaced rather than retried.
 */
export function useSystem(): UseQueryResult<SystemResponse, Error> {
  return useQuery({
    queryKey: ["system"],
    queryFn: () => apiGet<SystemResponse>("system"),
    // Refresh on the same cadence as the legacy /healthz proxy so the dots +
    // metrics stay current without hammering the aggregate query.
    refetchInterval: 10000,
    retry: (count, err) => {
      const status = err instanceof ApiError ? err.status : undefined;
      if (status === 503 || (status !== undefined && status < 500)) return false;
      return count < 2;
    },
  });
}

/**
 * Enqueue an audited live command. On success we invalidate the session query
 * so the cockpit converges to the new mode/halt/status without waiting for the
 * next poll (the command applies asynchronously in tms-live, so the session
 * reflects it within a poll cycle regardless).
 */
export function useLiveCommand() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: LiveCommandRequest) =>
      apiPost<LiveCommandResponse>("live/commands", body),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["live", "session"] });
      void qc.invalidateQueries({ queryKey: ["live", "health"] });
    },
  });
}

// ---- Manual trading desk (P6, operator-driven mutations) ----
//
// The ONLY broker-mutation surface in the HTTP API. SAFETY is paramount: the
// server is the authoritative gate (412 confirmation_required / 422 risk_violation
// / 503 no-desk). These mutations are NEVER retried — a retry on a partial /
// unknown outcome must never silently double-submit (idempotency_key already
// prevents a true duplicate, but a retry here would also re-surface the safety
// dialog, which must be an explicit operator action). On success we invalidate the
// live orders / positions / account queries so the blotter + book reconcile to the
// durable PG truth immediately (the WS push layers on top).

/** Bust the live trading read snapshots after a successful manual mutation. */
function invalidateTradingReads(qc: ReturnType<typeof useQueryClient>) {
  void qc.invalidateQueries({ queryKey: ["live", "orders"] });
  void qc.invalidateQueries({ queryKey: ["live", "fills"] });
  void qc.invalidateQueries({ queryKey: ["live", "positions"] });
  void qc.invalidateQueries({ queryKey: ["live", "account"] });
}

/**
 * Place a manual order (POST /api/v1/trade/order). The caller inspects the
 * thrown `ApiError` for the gate codes: 412 (`confirmation_required` — needs the
 * per-order confirm phrase / trade password) and 422 (`risk_violation` — the gate
 * rejected an opening order; the operator may re-submit with `override: true`).
 */
export function useManualOrder() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: ManualOrderRequest) =>
      apiPost<ManualOrderResponse>("trade/order", body),
    retry: false,
    onSuccess: () => invalidateTradingReads(qc),
  });
}

/** Cancel a working manual order by client-order-id (idempotent on the server). */
export function useCancelManualOrder() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (coid: string) =>
      apiPost<ManualCancelResponse>(
        `trade/order/${encodeURIComponent(coid)}/cancel`,
        {},
      ),
    retry: false,
    onSuccess: () => invalidateTradingReads(qc),
  });
}

/**
 * Close (flatten) the MANUAL position in one symbol. A LIVE close still requires a
 * `confirm_token` (412 without it); a close bypasses the allocator budget. An
 * already-flat symbol is an idempotent no-op (`submitted: false`).
 */
export function useCloseManualPosition() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: { symbol: string; body: ManualCloseRequest }) =>
      apiPost<ManualCloseResponse>(
        `trade/position/${encodeURIComponent(args.symbol)}/close`,
        args.body,
      ),
    retry: false,
    onSuccess: () => invalidateTradingReads(qc),
  });
}

/**
 * Sync from broker (POST /api/v1/trade/sync) — DIRECTION 2 (broker → TMS, the
 * operator's primary case). The operator traded DIRECTLY in moomoo; this pulls the
 * account's ACTUAL positions/orders/fills/account and reflects them into the
 * MANUAL/EXTERNAL book, then reconciles vs the strategy books. READ-ONLY at the
 * broker (places NO orders) and therefore safe in ALL modes incl signal — no
 * confirm token or risk gate applies. Idempotent: re-syncing the same broker state
 * reflects nothing. On success we invalidate the trading reads so the
 * positions/blotter/account panels reflect broker truth immediately. Also busts the
 * reconciliation read so its panel converges.
 */
export function useManualSync() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiPost<ManualSyncResponse>("trade/sync", {}),
    retry: false,
    onSuccess: () => {
      invalidateTradingReads(qc);
      void qc.invalidateQueries({ queryKey: ["live", "reconciliation"] });
    },
  });
}
