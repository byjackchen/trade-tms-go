import type { ErrorEnvelope } from "./types";

/**
 * Browser-side API client.
 *
 * The browser never talks to the TMS API directly — it goes through the UI
 * server's `/api/proxy/*` route handler, which injects the bearer token. So
 * there is no token, no base URL and no CORS concern in browser code here.
 *
 * On a non-2xx response the upstream error envelope is parsed and surfaced as
 * an `ApiError`, consumed by TanStack Query's error state.
 */

export const PROXY_BASE = "/api/proxy";

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

async function parseError(resp: Response): Promise<ApiError> {
  let code = "internal";
  let message = `request failed (HTTP ${resp.status})`;
  try {
    const body = (await resp.json()) as Partial<ErrorEnvelope>;
    if (body?.error?.code) code = body.error.code;
    if (body?.error?.message) message = body.error.message;
  } catch {
    /* non-JSON error body — keep the generic message */
  }
  return new ApiError(resp.status, code, message);
}

/** GET `<proxy>/<path>` (path is the API path under /api/v1, e.g. "data/coverage"). */
export async function apiGet<T>(
  path: string,
  params?: Record<string, string | number | undefined>,
): Promise<T> {
  const qs = new URLSearchParams();
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== "") qs.set(k, String(v));
    }
  }
  const query = qs.toString();
  const url = `${PROXY_BASE}/${path}${query ? `?${query}` : ""}`;
  const resp = await fetch(url, { headers: { Accept: "application/json" } });
  if (!resp.ok) throw await parseError(resp);
  return (await resp.json()) as T;
}

/** POST a JSON body to `<proxy>/<path>`. */
export async function apiPost<T>(
  path: string,
  body: unknown,
): Promise<T> {
  return apiBody<T>("POST", path, body);
}

/** PUT a JSON body to `<proxy>/<path>` (full-replace mutations, e.g. Composition update). */
export async function apiPut<T>(path: string, body: unknown): Promise<T> {
  return apiBody<T>("PUT", path, body);
}

/** PATCH a JSON body to `<proxy>/<path>` (full-replace of mutable cols, e.g. Account update). */
export async function apiPatch<T>(path: string, body: unknown): Promise<T> {
  return apiBody<T>("PATCH", path, body);
}

/**
 * DELETE `<proxy>/<path>` (optionally with query params, e.g. ?actor= which the
 * DELETE handlers read in lieu of a JSON body).
 */
export async function apiDelete<T>(
  path: string,
  params?: Record<string, string | number | undefined>,
): Promise<T> {
  const qs = new URLSearchParams();
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== "") qs.set(k, String(v));
    }
  }
  const query = qs.toString();
  const url = `${PROXY_BASE}/${path}${query ? `?${query}` : ""}`;
  const resp = await fetch(url, {
    method: "DELETE",
    headers: { Accept: "application/json" },
  });
  if (!resp.ok) throw await parseError(resp);
  // A 204 No Content (e.g. account delete) has no JSON body to parse; callers
  // that don't need a payload type it as `void`.
  if (resp.status === 204) return undefined as T;
  const text = await resp.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

/** Shared body-method (POST/PUT/PATCH) sender. */
async function apiBody<T>(
  method: "POST" | "PUT" | "PATCH",
  path: string,
  body: unknown,
): Promise<T> {
  const resp = await fetch(`${PROXY_BASE}/${path}`, {
    method,
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(body ?? {}),
  });
  if (!resp.ok) throw await parseError(resp);
  return (await resp.json()) as T;
}
