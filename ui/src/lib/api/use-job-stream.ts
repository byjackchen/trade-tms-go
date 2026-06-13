"use client";

import { useEffect, useRef, useState } from "react";
import type { WsEnvelope, JobEvent } from "./types";

export type BridgeState = "connecting" | "open" | "closed" | "error";

/**
 * Subscribe to the server-side SSE bridge (`/api/stream`) which relays the TMS
 * WebSocket job/sync fan-out. The bearer token stays on the UI server; the
 * browser only sees an `EventSource`.
 *
 * `onEnvelope` receives every well-formed frame. `onJobEvent` is a convenience
 * filtered to `type: "job"` frames. Both are kept in refs so re-renders of the
 * caller don't tear down the connection.
 */
export function useJobStream(handlers: {
  onEnvelope?: (env: WsEnvelope) => void;
  onJobEvent?: (ev: JobEvent) => void;
}): { connected: boolean; state: BridgeState } {
  const [state, setState] = useState<BridgeState>("connecting");
  const handlersRef = useRef(handlers);
  useEffect(() => {
    handlersRef.current = handlers;
  });

  useEffect(() => {
    const es = new EventSource("/api/stream");

    es.addEventListener("bridge", (e: MessageEvent) => {
      try {
        const parsed = JSON.parse(e.data) as { state?: BridgeState };
        if (parsed.state) setState(parsed.state);
      } catch {
        /* ignore */
      }
    });

    es.onmessage = (e: MessageEvent) => {
      if (!e.data) return;
      let env: WsEnvelope;
      try {
        env = JSON.parse(e.data) as WsEnvelope;
      } catch {
        return; // never sever the stream on one bad frame
      }
      handlersRef.current.onEnvelope?.(env);
      if (env.type === "job" && handlersRef.current.onJobEvent) {
        handlersRef.current.onJobEvent(env.payload as unknown as JobEvent);
      }
    };

    es.onopen = () => setState("open");
    es.onerror = () => {
      // EventSource auto-reconnects; reflect the transient drop.
      setState((s) => (s === "open" ? "connecting" : s));
    };

    return () => es.close();
  }, []);

  return { connected: state === "open", state };
}
