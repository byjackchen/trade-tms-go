"use client";

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import {
  UI_MODE_COOKIE,
  UI_MODE_COOKIE_MAX_AGE,
  UI_MODE_MOBILE_MEDIA,
  parseUiModePref,
  type UiMode,
  type UiModePref,
} from "@/lib/ui-mode";

type UiModeContextValue = {
  /** The resolved surface to render against. */
  mode: UiMode;
  /** The user's stored preference (`auto` defers to the device). */
  pref: UiModePref;
  /** Set the preference; persists to the `ui-mode` cookie. */
  setPref: (pref: UiModePref) => void;
};

const UiModeContext = createContext<UiModeContextValue | null>(null);

/** Write the preference cookie (path=/ so it covers every route; year TTL). */
function writePrefCookie(pref: UiModePref) {
  document.cookie = `${UI_MODE_COOKIE}=${pref}; path=/; max-age=${UI_MODE_COOKIE_MAX_AGE}; samesite=lax`;
}

/** Resolve `auto` against the live viewport via matchMedia (client only). */
function matchMobile(): boolean {
  if (typeof window === "undefined" || !window.matchMedia) return false;
  return window.matchMedia(UI_MODE_MOBILE_MEDIA).matches;
}

/**
 * Provides the resolved UI mode + preference to the client tree. Seeded from the
 * server-resolved `initialMode`/`initialPref` (read from the `ui-mode` cookie /
 * User-Agent in the root layout) so the first client render matches SSR exactly
 * — no hydration mismatch.
 *
 * After mount, when `pref === "auto"` the resolved mode tracks `matchMedia`
 * live (e.g. a desktop window dragged narrow). An explicit `desktop`/`mobile`
 * preference pins the mode and ignores the viewport.
 */
export function UiModeProvider({
  initialMode,
  initialPref,
  children,
}: {
  initialMode: UiMode;
  initialPref: UiModePref;
  children: React.ReactNode;
}) {
  const [pref, setPrefState] = useState<UiModePref>(initialPref);
  // Seed from the server-resolved mode so SSR === first CSR paint. We only start
  // consulting matchMedia after mount (below), guarded so the initial render is
  // deterministic.
  const [autoMode, setAutoMode] = useState<UiMode>(initialMode);

  // Track the viewport while pref is `auto`. The listener stays attached
  // regardless of pref (cheap) but only `auto` reads `autoMode` for the result,
  // so an explicit pref simply ignores these updates.
  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const mql = window.matchMedia(UI_MODE_MOBILE_MEDIA);
    const sync = () => setAutoMode(mql.matches ? "mobile" : "desktop");
    sync();
    mql.addEventListener("change", sync);
    return () => mql.removeEventListener("change", sync);
  }, []);

  const setPref = useCallback((next: UiModePref) => {
    setPrefState(next);
    writePrefCookie(next);
    // Reflect an explicit choice immediately; `auto` re-reads the viewport.
    if (next === "desktop" || next === "mobile") setAutoMode(next);
    else setAutoMode(matchMobile() ? "mobile" : "desktop");
  }, []);

  const mode: UiMode =
    pref === "desktop" || pref === "mobile" ? pref : autoMode;

  const value = useMemo<UiModeContextValue>(
    () => ({ mode, pref, setPref }),
    [mode, pref, setPref],
  );

  return (
    <UiModeContext.Provider value={value}>{children}</UiModeContext.Provider>
  );
}

/**
 * Read the resolved UI mode, the stored preference, and a setter. Must be used
 * under <UiModeProvider> (the root layout mounts it).
 */
export function useUiMode(): UiModeContextValue {
  const ctx = useContext(UiModeContext);
  if (!ctx) {
    throw new Error("useUiMode must be used within <UiModeProvider>");
  }
  return ctx;
}

/** Re-export so consumers can `parseUiModePref` without reaching into lib. */
export { parseUiModePref };
