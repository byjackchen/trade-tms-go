import { apiTarget } from "@/lib/server/api";

/**
 * Server-side fetch of the upstream API's public `/healthz` (and `/version`),
 * surfaced to the browser for the cockpit System page.
 *
 * `/healthz` is public (no bearer), but the browser must not learn the upstream
 * base URL, so the UI server proxies it here. The endpoint always returns 200
 * with a normalized shape; an unreachable upstream is reported as a degraded
 * dependency rather than an HTTP error so the System page can render a red dot
 * instead of throwing.
 */
export const dynamic = "force-dynamic";
export const runtime = "nodejs";

type Dep = { ok: boolean; error?: string };

export async function GET(): Promise<Response> {
  let target;
  try {
    target = apiTarget();
  } catch {
    return ok({
      status: "degraded",
      reachable: false,
      version: null,
      deps: {},
      error: "ui server is not configured",
    });
  }

  let health: {
    status?: string;
    version?: string;
    deps?: Record<string, Dep>;
  } = {};
  let reachable = true;
  let version: string | null = null;

  try {
    const resp = await fetch(`${target.httpBase}/healthz`, {
      cache: "no-store",
      headers: { Accept: "application/json" },
    });
    health = (await resp.json()) as typeof health;
  } catch {
    reachable = false;
  }

  // /version is best-effort; the System page degrades gracefully without it.
  try {
    const vresp = await fetch(`${target.httpBase}/version`, {
      cache: "no-store",
      headers: { Accept: "application/json" },
    });
    const vjson = (await vresp.json()) as { version?: string };
    version = vjson.version ?? health.version ?? null;
  } catch {
    version = health.version ?? null;
  }

  return ok({
    status: reachable ? (health.status ?? "ok") : "degraded",
    reachable,
    version,
    deps: health.deps ?? {},
  });
}

function ok(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
  });
}
