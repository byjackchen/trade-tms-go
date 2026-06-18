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

// GET reads; POST enqueues / mutates; PUT + DELETE back the Compositions CRUD
// (full-replace + delete — docs/concept-alignment.md §3.3). PATCH backs the
// account-registry partial update / set-default surface (tms.accounts).
const ALLOWED_METHODS = new Set(["GET", "POST", "PUT", "PATCH", "DELETE"]);

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
  // Use a path-segment encoder (not encodeURIComponent): account ids contain
  // colons (e.g. `moomoo:simulate:6535389`) and other RFC 3986 `pchar`
  // sub-delims that are legal *unencoded* inside a path segment. Over-encoding
  // `:` -> `%3A` makes chi's router mis-match and 404 the row, so the UI could
  // never set-default / delete colon-id accounts the API otherwise accepts.
  const path = segments.map(encodePathSegment).join("/");
  const search = req.nextUrl.search;
  const url = `${target.httpBase}/api/v1/${path}${search}`;

  const headers: Record<string, string> = {
    Authorization: `Bearer ${target.token}`,
    Accept: "application/json",
  };

  let body: string | undefined;
  if (
    req.method === "POST" ||
    req.method === "PUT" ||
    req.method === "PATCH"
  ) {
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
  // A 204/304 (e.g. account DELETE) is a null-body status: the Response ctor
  // throws if handed a body for it (which would surface to the client as a
  // bogus 500 on an upstream success), so pass null and drop Content-Type.
  const text = await upstream.text();
  const nullBody = upstream.status === 204 || upstream.status === 304;
  return new Response(nullBody ? null : text, {
    status: upstream.status,
    headers: {
      ...(nullBody
        ? {}
        : {
            "Content-Type":
              upstream.headers.get("Content-Type") ?? "application/json",
          }),
      "Cache-Control": "no-store",
    },
  });
}

/**
 * Encode one path segment per RFC 3986 `pchar`. encodeURIComponent is too
 * aggressive: it percent-encodes characters that are legal *unencoded* inside a
 * path segment — including `:` which appears in legacy account ids like
 * `moomoo:simulate:6535389`. We start from encodeURIComponent (which safely
 * encodes `/`, `?`, `#`, spaces, etc. so a segment can't break routing) and then
 * un-escape the `pchar` set (`sub-delims` + `:` + `@`) so the upstream chi
 * router sees the same literal id the client typed.
 */
function encodePathSegment(seg: string): string {
  return encodeURIComponent(seg).replace(
    /%(3A|40|21|24|26|27|28|29|2A|2B|2C|3B|3D)/g,
    (m) => decodeURIComponent(m),
  );
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

export async function PATCH(req: NextRequest, ctx: Ctx): Promise<Response> {
  const { path } = await ctx.params;
  return forward(req, path);
}

export async function DELETE(req: NextRequest, ctx: Ctx): Promise<Response> {
  const { path } = await ctx.params;
  return forward(req, path);
}
