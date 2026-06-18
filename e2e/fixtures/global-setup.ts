/**
 * Playwright global setup: block the whole suite until the compose stack is
 * ready, so individual specs never race a cold-starting API/UI.
 *
 * "Ready" means:
 *   1. The API's public GET /healthz answers 200 (process is serving HTTP).
 *      Per docs/api.md /healthz is ALWAYS 200 — even degraded — so we also
 *      require deps.postgres.ok, because the Data workspace is meaningless
 *      without postgres.
 *   2. The UI answers its own /api/healthz with 200 + configured:true (the
 *      bearer token / upstream URL are wired), so the proxy can reach the API.
 *
 * The gate runs `compose up --wait` before the suite, but --wait only gates on
 * container healthchecks; this adds an application-level barrier and a generous
 * poll window for the first cold boot.
 */

import type { FullConfig } from "@playwright/test";
import { UI_BASE_URL, STACK_READY_TIMEOUT_MS } from "../lib/env";
import { getHealthz } from "../lib/api";

async function pollUntil(
  label: string,
  deadlineMs: number,
  check: () => Promise<boolean>,
): Promise<void> {
  const start = Date.now();
  let lastErr: unknown;
  let attempt = 0;
  while (Date.now() - start < deadlineMs) {
    attempt += 1;
    try {
      if (await check()) {
        // eslint-disable-next-line no-console
        console.log(`[global-setup] ${label} ready after ${attempt} attempt(s)`);
        return;
      }
    } catch (err) {
      lastErr = err;
    }
    await new Promise((r) => setTimeout(r, 2_000));
  }
  throw new Error(
    `[global-setup] ${label} not ready within ${deadlineMs}ms` +
      (lastErr ? ` (last error: ${String(lastErr)})` : ""),
  );
}

async function apiReady(): Promise<boolean> {
  const res = await getHealthz();
  if (res.status !== 200 || typeof res.body !== "object" || res.body === null) {
    return false;
  }
  const body = res.body as {
    deps?: { postgres?: { ok?: boolean } };
  };
  return body.deps?.postgres?.ok === true;
}

async function uiReady(): Promise<boolean> {
  const res = await fetch(`${UI_BASE_URL}/api/healthz`, {
    headers: { Accept: "application/json" },
  });
  if (res.status !== 200) return false;
  const body = (await res.json()) as { status?: string; configured?: boolean };
  return body.status === "ok" && body.configured === true;
}

// eslint-disable-next-line @typescript-eslint/no-unused-vars
export default async function globalSetup(_config: FullConfig): Promise<void> {
  // eslint-disable-next-line no-console
  console.log("[global-setup] waiting for compose stack (API + UI)…");
  await pollUntil("API /healthz (postgres up)", STACK_READY_TIMEOUT_MS, apiReady);
  await pollUntil("UI /api/healthz (configured)", STACK_READY_TIMEOUT_MS, uiReady);
  // eslint-disable-next-line no-console
  console.log("[global-setup] compose stack ready.");
}
