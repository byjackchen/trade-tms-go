"use client";

import * as React from "react";
import type { ManualSide } from "@/lib/api/types";

/**
 * A tiny in-page bus that lets a "Trade" button anywhere on the manual desk
 * (a positions row, a fills row) pre-fill the order ticket without prop-drilling
 * through every panel. A `seq` counter is bumped on every prefill so the ticket
 * reacts even when the same (symbol, side) is requested twice in a row.
 *
 * Cross-PAGE prefills (the Portfolio view's Trade button lives on /live, the
 * ticket on /live/trade) go through the URL instead (?symbol=&side=), which the
 * ticket reads on mount — this context only carries same-page requests.
 */
export type TradePrefill = {
  symbol: string;
  side: ManualSide;
  seq: number;
};

type TradeDeskValue = {
  prefill: TradePrefill | null;
  requestTrade: (symbol: string, side: ManualSide) => void;
};

const TradeDeskContext = React.createContext<TradeDeskValue | null>(null);

export function TradeDeskProvider({ children }: { children: React.ReactNode }) {
  const [prefill, setPrefill] = React.useState<TradePrefill | null>(null);
  const seqRef = React.useRef(0);

  const requestTrade = React.useCallback(
    (symbol: string, side: ManualSide) => {
      seqRef.current += 1;
      setPrefill({ symbol: symbol.toUpperCase(), side, seq: seqRef.current });
      // Surface the ticket if it has scrolled out of view (best-effort; the
      // ticket also pulses on prefill).
      if (typeof document !== "undefined") {
        const el = document.querySelector('[data-testid="trade-ticket"]');
        el?.scrollIntoView({ behavior: "smooth", block: "start" });
      }
    },
    [],
  );

  const value = React.useMemo(
    () => ({ prefill, requestTrade }),
    [prefill, requestTrade],
  );

  return (
    <TradeDeskContext.Provider value={value}>
      {children}
    </TradeDeskContext.Provider>
  );
}

/**
 * Access the trade-desk bus. Returns a no-op `requestTrade` outside a provider so
 * a "Trade" button rendered on the Portfolio view (no provider there) degrades to its
 * URL-link fallback instead of throwing.
 */
export function useTradeDesk(): TradeDeskValue {
  return (
    React.useContext(TradeDeskContext) ?? {
      prefill: null,
      requestTrade: () => {},
    }
  );
}
