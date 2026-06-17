"use client";

import { useEffect } from "react";

/**
 * Registers the PWA service worker (/sw.js) on mount, in production only — a dev
 * service worker caches stale assets and fights HMR. Failures are swallowed (the
 * app works fine without the SW; the SW only adds installability + offline). It
 * renders nothing.
 */
export function ServiceWorkerRegister() {
  useEffect(() => {
    if (
      process.env.NODE_ENV !== "production" ||
      typeof navigator === "undefined" ||
      !("serviceWorker" in navigator)
    ) {
      return;
    }
    navigator.serviceWorker.register("/sw.js").catch(() => {
      /* SW is best-effort; the app is fully functional without it. */
    });
  }, []);
  return null;
}
