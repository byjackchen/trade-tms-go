import { NextRequest } from "next/server";
import { apiTarget } from "@/lib/server/api";

/**
 * Server-Sent Events bridge for the TMS WebSocket job/sync stream.
 *
 * Next App Router route handlers cannot upgrade a raw browser WebSocket, and we
 * must never ship the bearer token to the browser. So the UI server opens the
 * upstream `/api/v1/ws` connection itself (token in `?token=`, server-side
 * only) and relays every JSON frame to the browser as an SSE `message` event.
 * The browser consumes it with the native `EventSource` API.
 *
 * SSE auto-reconnects on the browser side; on each (re)connect the upstream WS
 * sends a `hello` frame and the client reconciles durable state via REST, so no
 * events are lost beyond the best-effort delivery the API already documents.
 */

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

export async function GET(req: NextRequest): Promise<Response> {
  let target;
  try {
    target = apiTarget();
  } catch {
    return new Response("ui server is not configured", { status: 500 });
  }

  const wsUrl = `${target.wsBase}/api/v1/ws?token=${encodeURIComponent(target.token)}`;

  const encoder = new TextEncoder();
  let ws: WebSocket | null = null;
  let heartbeat: ReturnType<typeof setInterval> | null = null;

  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      const safeEnqueue = (chunk: string) => {
        try {
          controller.enqueue(encoder.encode(chunk));
        } catch {
          // Controller already closed (client gone) — ignore.
        }
      };

      // Tell the EventSource our preferred reconnect backoff, then announce we
      // are bridging (distinct from the upstream `hello`).
      safeEnqueue("retry: 3000\n\n");
      safeEnqueue(`event: bridge\ndata: {"state":"connecting"}\n\n`);

      try {
        ws = new WebSocket(wsUrl);
      } catch {
        safeEnqueue(`event: bridge\ndata: {"state":"error"}\n\n`);
        controller.close();
        return;
      }

      ws.onopen = () => {
        safeEnqueue(`event: bridge\ndata: {"state":"open"}\n\n`);
      };

      ws.onmessage = (ev: MessageEvent) => {
        const data = typeof ev.data === "string" ? ev.data : "";
        if (!data) return;
        // One SSE `message` per WS frame; data is the raw JSON envelope.
        safeEnqueue(`data: ${data.replace(/\n/g, " ")}\n\n`);
      };

      ws.onerror = () => {
        safeEnqueue(`event: bridge\ndata: {"state":"error"}\n\n`);
      };

      ws.onclose = () => {
        safeEnqueue(`event: bridge\ndata: {"state":"closed"}\n\n`);
        if (heartbeat) clearInterval(heartbeat);
        try {
          controller.close();
        } catch {
          /* already closed */
        }
      };

      // SSE comment heartbeat keeps intermediaries from idling the connection.
      heartbeat = setInterval(() => safeEnqueue(": ping\n\n"), 15000);
    },
    cancel() {
      // Browser disconnected (navigation / tab close) — tear down the upstream.
      if (heartbeat) clearInterval(heartbeat);
      try {
        ws?.close();
      } catch {
        /* ignore */
      }
    },
  });

  // Abort upstream when the request is aborted (covers some runtimes' cancel).
  req.signal.addEventListener("abort", () => {
    if (heartbeat) clearInterval(heartbeat);
    try {
      ws?.close();
    } catch {
      /* ignore */
    }
  });

  return new Response(stream, {
    headers: {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-store, no-transform",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    },
  });
}
