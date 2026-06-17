import { NextRequest } from "next/server";
import { apiTarget } from "@/lib/server/api";

/**
 * Server-side REST proxy to the TMS API.
 *
 * The browser calls `/api/proxy/<api-path>` (e.g. `/api/proxy/data/coverage`)
 * and this handler forwards to `<TMS_API_URL>/api/v1/<api-path>` with the
 * bearer token injected. The token never reaches the browser. Only the data /
 * jobs / universe surface under `/api/v1` is reachable; arbitrary upstream
 * paths cannot be requested.
 *
 * The upstream error envelope and status code are passed through verbatim so
 * the client can render `error.code` / `error.message` consistently.
 */

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// GET reads; POST enqueues / mutates; PUT + DELETE back the Models CRUD
// (full-replace + delete — docs/concept-alignment.md §3.3).
const ALLOWED_METHODS = new Set(["GET", "POST", "PUT", "DELETE"]);

async function forward(req: NextRequest, segments: string[]): Promise<Response> {
  if (!ALLOWED_METHODS.has(req.method)) {
    return jsonError(405, "validation", "method not allowed");
  }

  let target;
  try {
    target = apiTarget();
  } catch {
    // Misconfiguration (no token / no upstream). Do not leak which.
    return jsonError(500, "internal", "ui server is not configured");
  }

  // Reconstruct the upstream path under /api/v1 and carry the query string.
  const path = segments.map(encodeURIComponent).join("/");
  const search = req.nextUrl.search;
  const url = `${target.httpBase}/api/v1/${path}${search}`;

  const headers: Record<string, string> = {
    Authorization: `Bearer ${target.token}`,
    Accept: "application/json",
  };

  let body: string | undefined;
  if (req.method === "POST" || req.method === "PUT") {
    body = await req.text();
    headers["Content-Type"] = "application/json";
  }

  let upstream: Response;
  try {
    upstream = await fetch(url, {
      method: req.method,
      headers,
      body,
      cache: "no-store",
    });
  } catch {
    return jsonError(
      502,
      "internal",
      "upstream API is unreachable; see UI server logs",
    );
  }

  // Pass through the body and status. Strip hop-by-hop / auth-leaking headers.
  const text = await upstream.text();
  return new Response(text, {
    status: upstream.status,
    headers: {
      "Content-Type":
        upstream.headers.get("Content-Type") ?? "application/json",
      "Cache-Control": "no-store",
    },
  });
}

function jsonError(status: number, code: string, message: string): Response {
  return new Response(JSON.stringify({ error: { code, message } }), {
    status,
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
  });
}

type Ctx = { params: Promise<{ path: string[] }> };

export async function GET(req: NextRequest, ctx: Ctx): Promise<Response> {
  const { path } = await ctx.params;
  return forward(req, path);
}

export async function POST(req: NextRequest, ctx: Ctx): Promise<Response> {
  const { path } = await ctx.params;
  return forward(req, path);
}

export async function PUT(req: NextRequest, ctx: Ctx): Promise<Response> {
  const { path } = await ctx.params;
  return forward(req, path);
}

export async function DELETE(req: NextRequest, ctx: Ctx): Promise<Response> {
  const { path } = await ctx.params;
  return forward(req, path);
}
