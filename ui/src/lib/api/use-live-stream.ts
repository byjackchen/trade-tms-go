"use client";

import { useEffect, useRef, useState } from "react";
import { subscribeSse, type BridgeState } from "./sse-bus";
import type {
  WsEnvelope,
  WsSignal,
  WsStrategyState,
  WsPortfolioHealth,
  WsWatchlist,
  WsPosition,
  WsOrderUpdate,
  WsFillUpdate,
  WsLivePosition,
  WsAccountUpdate,
} from "./types";

/**
 * Typed live event stream for the cockpit.
 *
 * Subscribes to the shared `/api/stream` SSE bus and dispatches the live frame
 * types (signal / strategy_state / portfolio_health / watchlist /
 * position) to the supplied handlers. Handlers are stored in a ref so a
 * re-render of the caller (which is constant on every push) does not churn the
 * subscription. Returns the bridge connection state for disconnected-banner UI.
 */
export type LiveStreamHandlers = {
  onEnvelope?: (env: WsEnvelope) => void;
  onSignal?: (p: WsSignal) => void;
  onStrategyState?: (p: WsStrategyState) => void;
  onPortfolioHealth?: (p: WsPortfolioHealth) => void;
  onWatchlist?: (p: WsWatchlist) => void;
  onPosition?: (p: WsPosition) => void;
  // P6 paper/live trading frames.
  onOrderUpdate?: (p: WsOrderUpdate) => void;
  onFillUpdate?: (p: WsFillUpdate) => void;
  onLivePosition?: (p: WsLivePosition) => void;
  onAccountUpdate?: (p: WsAccountUpdate) => void;
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
          case "signal":
            h.onSignal?.(env.payload as unknown as WsSignal);
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
          case "order_update":
            h.onOrderUpdate?.(env.payload as unknown as WsOrderUpdate);
            break;
          case "fill_update":
            h.onFillUpdate?.(env.payload as unknown as WsFillUpdate);
            break;
          case "live_position":
            h.onLivePosition?.(env.payload as unknown as WsLivePosition);
            break;
          case "account_update":
            h.onAccountUpdate?.(env.payload as unknown as WsAccountUpdate);
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
