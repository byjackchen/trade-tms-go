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

// ---------------------------------------------------------------------------
// Manual trading desk (P6, operator-driven) — docs/api.md "Manual trading desk".
//
// The MANUAL desk is the ONLY broker-mutation surface in the HTTP API: place /
// cancel / close BY HAND against a paper or live account, attributed to the
// `MANUAL` pseudo-strategy. It is served by the live-node process on a separate
// bearer-guarded listener (`--manual-api-addr`, default 127.0.0.1:18091); when no
// manual desk is connected every `/api/v1/trade/*` endpoint returns 503.
//
// In the e2e gate the desk's host base URL is reachable at TMS_E2E_MANUAL_URL
// (defaults to the API base — the compose stack reverse-proxies /api/v1/trade/*
// onto the live node's manual listener so the suite hits one host). These thin
// clients let the safety specs probe the boundary guards directly (a 412/422/503
// proves the gate held) without going through the browser.
// ---------------------------------------------------------------------------

import { MANUAL_BASE_URL } from "./env";

/** POST a JSON body to a manual-desk /api/v1/trade/* path WITH the bearer token. */
export async function postManual(
  path: string,
  body: unknown,
): Promise<ApiResult> {
  const res = await fetch(`${MANUAL_BASE_URL}/api/v1/${path}`, {
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

/** GET a manual-desk /api/v1/trade/* path WITH the bearer token. */
export async function getManual(path: string): Promise<ApiResult> {
  const res = await fetch(`${MANUAL_BASE_URL}/api/v1/${path}`, {
    method: "GET",
    headers: {
      Accept: "application/json",
      Authorization: `Bearer ${API_TOKEN}`,
    },
  });
  return { status: res.status, body: await parse(res), headers: res.headers };
}
