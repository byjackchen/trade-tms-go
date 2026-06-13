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

export function useWatchlist(): UseQueryResult<WatchlistResponse, Error> {
  return useQuery({
    queryKey: ["live", "watchlist"],
    queryFn: () => apiGet<WatchlistResponse>("watchlist"),
    refetchInterval: 30000,
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
