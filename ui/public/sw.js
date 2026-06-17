/* TMS PWA service worker.
 *
 * Strategy:
 *   - /api/* (proxy, stream, healthz, system-health): NEVER cached — always live.
 *   - navigations: network-first, cache a copy, fall back to the cached page (or
 *     the /systems shell) when offline.
 *   - static build assets (/_next/static, /icons, fonts): cache-first.
 * Bumping CACHE invalidates the old cache on activate.
 */
const CACHE = "tms-v1";
const PRECACHE = [
  "/icons/icon-192.png",
  "/icons/icon-512.png",
  "/manifest.webmanifest",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      .then((c) => c.addAll(PRECACHE))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))),
      )
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const { request } = event;
  if (request.method !== "GET") return;

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;

  // Live data must never be served stale.
  if (url.pathname.startsWith("/api/")) return;

  // Navigations: network-first with an offline cache fallback.
  if (request.mode === "navigate") {
    event.respondWith(
      fetch(request)
        .then((resp) => {
          const copy = resp.clone();
          caches.open(CACHE).then((c) => c.put(request, copy));
          return resp;
        })
        .catch(() =>
          caches
            .match(request)
            .then((r) => r || caches.match("/systems")),
        ),
    );
    return;
  }

  // Static assets: cache-first, populate on first fetch.
  if (
    url.pathname.startsWith("/_next/static") ||
    url.pathname.startsWith("/icons/") ||
    /\.(?:woff2?|ttf|png|svg|ico)$/.test(url.pathname)
  ) {
    event.respondWith(
      caches.match(request).then(
        (cached) =>
          cached ||
          fetch(request).then((resp) => {
            if (resp.ok) {
              const copy = resp.clone();
              caches.open(CACHE).then((c) => c.put(request, copy));
            }
            return resp;
          }),
      ),
    );
  }
});
