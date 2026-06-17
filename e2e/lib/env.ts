/**
 * Central, single-source-of-truth environment for the e2e suite.
 *
 * Every host port and credential the suite touches is reserved by this project
 * (see compose.yaml): UI 13000, API 18080, postgres 55432, redis 56379. These
 * are deliberately non-default so the suite never collides with a separate
 * stack on the same machine.
 *
 * All values are overridable via env vars so the suite runs unchanged in CI,
 * against a remote stack, or with a rotated bearer token. Nothing here is a
 * secret default beyond the local-dev token, which the gate overrides.
 */

function fromEnv(name: string, fallback: string): string {
  const v = process.env[name];
  return v && v.trim() !== "" ? v.trim() : fallback;
}

function intFromEnv(name: string, fallback: number): number {
  const raw = process.env[name];
  if (!raw || raw.trim() === "") return fallback;
  const n = Number.parseInt(raw, 10);
  if (Number.isNaN(n)) {
    throw new Error(`env ${name}="${raw}" is not an integer`);
  }
  return n;
}

/** Host base URL of the Next.js UI (compose maps 13000 -> container 3000). */
export const UI_BASE_URL = fromEnv("TMS_E2E_UI_URL", "http://localhost:13000");

/** Host base URL of the TMS Go API (compose maps 18080 -> container 8080). */
export const API_BASE_URL = fromEnv("TMS_E2E_API_URL", "http://localhost:18080");

/**
 * Host base URL of the MANUAL trading desk's bearer-guarded listener (P6;
 * docs/api.md "Manual trading desk" — `--manual-api-addr`, default
 * 127.0.0.1:18091 inside the live node). In the compose gate the desk's
 * `/api/v1/trade/*` surface is reachable at the same host as the API (a reverse
 * proxy fronts both), so this defaults to API_BASE_URL; override via
 * TMS_E2E_MANUAL_URL when the desk listens on its own host port. When no manual
 * desk is connected every endpoint here returns 503 and the specs self-skip.
 */
export const MANUAL_BASE_URL = fromEnv("TMS_E2E_MANUAL_URL", API_BASE_URL);

/**
 * Bearer token guarding every /api/* route. Must equal the API's TMS_API_TOKEN
 * and the UI proxy's TMS_API_TOKEN. The local-dev default mirrors what the gate
 * seeds into .env; override via TMS_API_TOKEN for any real deployment.
 */
export const API_TOKEN = fromEnv("TMS_API_TOKEN", "local-e2e-token");

/** Direct postgres connection (host-mapped) for DB-truth assertions + seeding. */
export const PG = {
  host: fromEnv("TMS_PG_HOST", "127.0.0.1"),
  port: intFromEnv("TMS_PG_PORT", 55432),
  user: fromEnv("TMS_PG_USER", "tms"),
  password: fromEnv("TMS_PG_PASSWORD", "tms"),
  database: fromEnv("TMS_PG_DATABASE", "tms"),
} as const;

/**
 * How long (ms) to wait for the compose stack (API /healthz + UI) to become
 * reachable before the suite starts. The gate brings the stack up; on a cold
 * start the API waits on postgres/redis/migrate, so allow a generous window.
 */
export const STACK_READY_TIMEOUT_MS = intFromEnv(
  "TMS_E2E_STACK_TIMEOUT_MS",
  120_000,
);
