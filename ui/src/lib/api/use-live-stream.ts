"use client";

import { useEffect, useRef, useState } from "react";
import { subscribeSse, type BridgeState } from "./sse-bus";
import type {
  WsEnvelope,
  WsSignalIntent,
  WsStrategyState,
  WsPortfolioHealth,
  WsWatchlist,
  WsPosition,
} from "./types";

/**
 * Typed live event stream for the cockpit.
 *
 * Subscribes to the shared `/api/stream` SSE bus and dispatches the live frame
 * types (signal_intent / strategy_state / portfolio_health / watchlist /
 * position) to the supplied handlers. Handlers are stored in a ref so a
 * re-render of the caller (which is constant on every push) does not churn the
 * subscription. Returns the bridge connection state for disconnected-banner UI.
 */
export type LiveStreamHandlers = {
  onEnvelope?: (env: WsEnvelope) => void;
  onSignalIntent?: (p: WsSignalIntent) => void;
  onStrategyState?: (p: WsStrategyState) => void;
  onPortfolioHealth?: (p: WsPortfolioHealth) => void;
  onWatchlist?: (p: WsWatchlist) => void;
  onPosition?: (p: WsPosition) => void;
};

export function useLiveStream(handlers: LiveStreamHandlers): {
  connected: boolean;
  state: BridgeState;
} {
  const [state, setState] = useState<BridgeState>("connecting");
  const handlersRef = useRef(handlers);
  useEffect(() => {
    handlersRef.current = handlers;
  });

  useEffect(() => {
    const unsub = subscribeSse({
      onState: setState,
      onEnvelope: (env) => {
        const h = handlersRef.current;
        h.onEnvelope?.(env);
        switch (env.type) {
          case "signal_intent":
            h.onSignalIntent?.(env.payload as unknown as WsSignalIntent);
            break;
          case "strategy_state":
            h.onStrategyState?.(env.payload as unknown as WsStrategyState);
            break;
          case "portfolio_health":
            h.onPortfolioHealth?.(env.payload as unknown as WsPortfolioHealth);
            break;
          case "watchlist":
            h.onWatchlist?.(env.payload as unknown as WsWatchlist);
            break;
          case "position":
            h.onPosition?.(env.payload as unknown as WsPosition);
            break;
          default:
            break;
        }
      },
    });
    return unsub;
  }, []);

  return { connected: state === "open", state };
}
