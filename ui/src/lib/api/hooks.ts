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
