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
  const resp = await fetch(`${PROXY_BASE}/${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify(body ?? {}),
  });
  if (!resp.ok) throw await parseError(resp);
  return (await resp.json()) as T;
}
