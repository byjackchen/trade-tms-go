# tms UI — control plane (P1)

Next.js 16 (App Router, React 19, Tailwind v4) cockpit for the TMS control
plane. P1 ships the **Data** workspace; Backtests / Hyperopt / Live / Ops render
"coming in P2+" placeholders.

Aesthetics use the neutral oklch "base-nova" palette with a dark cockpit
default. The design tokens are inlined in `src/app/globals.css` (no fragile
`shadcn/tailwind.css` import) so the build is hermetic.

## Security model — the token never reaches the browser

The TMS API requires `Authorization: Bearer <TMS_API_TOKEN>` on every `/api/*`
route. That token lives **only on the UI server**:

- `src/lib/server/api.ts` (`import "server-only"`) reads `TMS_API_TOKEN` and the
  upstream base URL. It is never serialized into props or logs.
- Browser → `/api/proxy/<path>` (`src/app/api/proxy/[...path]/route.ts`) which
  forwards to `<TMS_API_URL>/api/v1/<path>` with the bearer header injected and
  passes the upstream status + error envelope through verbatim.
- Live job/sync events: the WS contract uses `?token=` (browser WS can't set
  headers), so instead of exposing the token, the UI server opens the upstream
  `/api/v1/ws` itself and relays frames to the browser as **Server-Sent Events**
  via `/api/stream` (`src/app/api/stream/route.ts`). The browser consumes an
  `EventSource` (`src/lib/api/use-job-stream.ts`).

The UI's own liveness route is `/api/healthz` (always 200; `configured` flags
whether the token is present) — used by the Docker/compose healthcheck.

## Data workspace (`/data`)

- **Coverage table** — per-table rows, tickers, date range, freshness badge
  (NYSE-session lag) and `bars_daily` gap summary.
- **Gap heatmap** — per-ticker calendar strip over the ticker's bar span; each
  missing NYSE session lights up red, weekends dimmed. Driven by the coverage
  table's "Gaps" action or a ticker search.
- **Sync history** — per-dataset watermarks + import run history.
- **Universe card** — latest snapshot summary + rebuild action.
- **Refresh data** — dialog (source, dataset multi-select, optional
  tickers/since) → `POST /api/v1/data/refresh` → live job progress (progress bar
  + streaming log) → completion. Universe rebuild reuses the same job-tracker
  flow. Both reconcile terminal state via a REST poll, so completion is observed
  even if an SSE frame is dropped (the WS stream is best-effort by contract).

Every data fetch has explicit loading / error / empty states, and every
interactive element + key data cell carries a stable `data-testid` for e2e.

## Develop

```bash
cp .env.example .env.local   # set TMS_API_TOKEN + TMS_API_URL
pnpm install
pnpm dev                     # http://localhost:3000
```

`TMS_API_URL` defaults to `http://127.0.0.1:18080` (the host-mapped API port).

## Build / run

```bash
pnpm build                   # standalone output; must pass with no type errors
pnpm start
```

## Docker / compose

`Dockerfile` is a multi-stage standalone build running as a non-root user.
The `ui` service in the repo `compose.yaml` (profile `app`, host port 13000)
depends on `tms-api` being healthy:

```bash
docker compose --profile app up -d --wait ui
```
