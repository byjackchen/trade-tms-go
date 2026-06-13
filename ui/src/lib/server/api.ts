import "server-only";

/**
 * Server-side TMS API access.
 *
 * The bearer token (TMS_API_TOKEN) lives ONLY on the UI server and is injected
 * here. It is never serialized into props, never sent to the browser, and never
 * logged. Browser code reaches the API exclusively through the `/api/proxy/*`
 * route handler and the `/api/stream` SSE bridge, both of which call into this
 * module.
 */

export type ApiTarget = {
  /** http base, e.g. http://tmsgo-api:8080 */
  httpBase: string;
  /** ws base, e.g. ws://tmsgo-api:8080 */
  wsBase: string;
  token: string;
};

class MissingConfigError extends Error {
  constructor(name: string) {
    super(`missing required env ${name}`);
    this.name = "MissingConfigError";
  }
}

/**
 * Resolve the upstream API target from the environment. Fails loud and fast —
 * an unauthenticated or misconfigured UI is a deployment error, not a degraded
 * mode (mirrors the API's own refuse-to-start-without-token stance).
 */
export function apiTarget(): ApiTarget {
  const token = process.env.TMS_API_TOKEN;
  if (!token) throw new MissingConfigError("TMS_API_TOKEN");

  // TMS_API_URL is the internal http base the UI server dials (compose:
  // http://tmsgo-api:8080). Defaults to the host-mapped port for local `dev`.
  const httpBase = (
    process.env.TMS_API_URL || "http://127.0.0.1:18080"
  ).replace(/\/+$/, "");

  // Derive the ws base from the http base unless explicitly overridden.
  const wsBase = (
    process.env.TMS_API_WS_URL ||
    httpBase.replace(/^http(s?):\/\//, (_m, s) => `ws${s}://`)
  ).replace(/\/+$/, "");

  return { httpBase, wsBase, token };
}

/** True when the env is configured enough to reach the API (used by /healthz of the UI itself). */
export function apiConfigured(): boolean {
  return Boolean(process.env.TMS_API_TOKEN);
}
