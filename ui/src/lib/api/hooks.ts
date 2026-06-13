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
} from "./types";

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
