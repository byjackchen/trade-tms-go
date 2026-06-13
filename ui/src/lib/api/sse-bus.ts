"use client";

import type { WsEnvelope } from "./types";

/**
 * Shared `/api/stream` SSE subscriber.
 *
 * The cockpit mounts several independent live panels (health strip, intents
 * stream, watchlist, strategy cards, system status). Each one wants the live
 * event stream, but opening a fresh `EventSource` per panel would multiply the
 * upstream WebSocket connections the UI server holds open to the API. So a
 * single browser-wide `EventSource` is reference-counted here and every panel
 * subscribes through it; the connection is torn down only when the last
 * subscriber unmounts.
 *
 * The bearer token stays server-side (the bridge route injects it); the browser
 * only ever sees an `EventSource` over the same-origin `/api/stream` path.
 */

export type BridgeState = "connecting" | "open" | "closed" | "error";

type Subscriber = {
  onEnvelope?: (env: WsEnvelope) => void;
  onState?: (state: BridgeState) => void;
};

let source: EventSource | null = null;
let refCount = 0;
let lastState: BridgeState = "connecting";
const subscribers = new Set<Subscriber>();

function emitState(state: BridgeState) {
  lastState = state;
  for (const sub of subscribers) sub.onState?.(state);
}

function handleMessage(e: MessageEvent) {
  if (!e.data) return;
  let env: WsEnvelope;
  try {
    env = JSON.parse(e.data) as WsEnvelope;
  } catch {
    return; // one malformed frame must never sever the stream
  }
  for (const sub of subscribers) sub.onEnvelope?.(env);
}

function handleBridge(e: MessageEvent) {
  try {
    const parsed = JSON.parse(e.data) as { state?: BridgeState };
    if (parsed.state) emitState(parsed.state);
  } catch {
    /* ignore */
  }
}

function open() {
  if (source) return;
  lastState = "connecting";
  const es = new EventSource("/api/stream");
  source = es;
  es.addEventListener("bridge", handleBridge as EventListener);
  es.onmessage = handleMessage;
  es.onopen = () => emitState("open");
  es.onerror = () => {
    // EventSource auto-reconnects; reflect the transient drop without claiming a
    // hard close (the browser will retry per the server's `retry:` hint).
    if (lastState === "open") emitState("connecting");
  };
}

function close() {
  if (!source) return;
  source.removeEventListener("bridge", handleBridge as EventListener);
  source.close();
  source = null;
}

/**
 * Subscribe to the shared SSE bus. Returns an unsubscribe function. The first
 * subscriber opens the connection; the last to leave closes it.
 */
export function subscribeSse(sub: Subscriber): () => void {
  subscribers.add(sub);
  refCount += 1;
  if (refCount === 1) open();
  // Replay the current bridge state so a late subscriber isn't stuck on the
  // default "connecting" until the next transition.
  sub.onState?.(lastState);
  return () => {
    subscribers.delete(sub);
    refCount -= 1;
    if (refCount <= 0) {
      refCount = 0;
      close();
    }
  };
}
