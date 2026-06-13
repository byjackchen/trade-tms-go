/**
 * Thin client for the TMS Go API over its host-mapped port (18080).
 *
 * Used for (a) the auth test — a token-less call must be 401 — and (b) the
 * stack-readiness probe in the fixture. Tests that exercise the UI go through
 * the browser + the UI's server-side proxy, never this client.
 */

import { API_BASE_URL, API_TOKEN } from "./env";

export type ApiResult = {
  status: number;
  /** Parsed JSON body, or undefined when the body was empty/non-JSON. */
  body: unknown;
  headers: Headers;
};

async function parse(res: Response): Promise<unknown> {
  const text = await res.text();
  if (text === "") return undefined;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/** GET an /api/v1 path WITHOUT a bearer token (for the auth-rejection test). */
export async function getNoAuth(path: string): Promise<ApiResult> {
  const res = await fetch(`${API_BASE_URL}/api/v1/${path}`, {
    method: "GET",
    headers: { Accept: "application/json" },
  });
  return { status: res.status, body: await parse(res), headers: res.headers };
}

/** GET an /api/v1 path WITH the bearer token. */
export async function getAuthed(path: string): Promise<ApiResult> {
  const res = await fetch(`${API_BASE_URL}/api/v1/${path}`, {
    method: "GET",
    headers: {
      Accept: "application/json",
      Authorization: `Bearer ${API_TOKEN}`,
    },
  });
  return { status: res.status, body: await parse(res), headers: res.headers };
}

/** GET the public /healthz probe (no token required). */
export async function getHealthz(): Promise<ApiResult> {
  const res = await fetch(`${API_BASE_URL}/healthz`, {
    method: "GET",
    headers: { Accept: "application/json" },
  });
  return { status: res.status, body: await parse(res), headers: res.headers };
}

/** POST a JSON body to an /api/v1 path WITH the bearer token. Used by helpers
 * that probe the live command surface directly (e.g. to confirm the
 * confirmation guard on set_mode without going through the browser). */
export async function postAuthed(
  path: string,
  body: unknown,
): Promise<ApiResult> {
  const res = await fetch(`${API_BASE_URL}/api/v1/${path}`, {
    method: "POST",
    headers: {
      Accept: "application/json",
      "Content-Type": "application/json",
      Authorization: `Bearer ${API_TOKEN}`,
    },
    body: JSON.stringify(body),
  });
  return { status: res.status, body: await parse(res), headers: res.headers };
}
