import { apiConfigured } from "@/lib/server/api";

/**
 * UI liveness probe (distinct from the upstream API /healthz). Always 200 when
 * the process serves HTTP; `configured` reflects whether the bearer token /
 * upstream URL are present. Used by the compose healthcheck.
 */
export const dynamic = "force-dynamic";
export const runtime = "nodejs";

export function GET(): Response {
  return new Response(
    JSON.stringify({ status: "ok", configured: apiConfigured() }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
}
