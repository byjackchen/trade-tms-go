# Redeploy playbook (after a repo change)

Companion to `first-deploy-playbook.md` (first-time deploy). This is the **recurring cycle** for porting an
`origin/main` change onto a running single-host Docker stack. It is **principle-first, not
rote steps** — every change is different. Work the five phases, but let *what actually
changed* decide how much of each you do.

Topology recap (so the phases make sense): one `tms` binary, run as different subcommands —
`tmsgo-api` (gateway: reads + enqueue + WS bridge, **no engine**), `tmsgo-worker` (batch
engine: jobs/hyperopt/eod), `tmsgo-live` (live strategy engine on the OpenD feed),
`tmsgo-scheduler` (cron enqueue), `tmsgo-ui` (Next.js + SSE proxy), `postgres`, `redis`.
**OpenD runs on the host, not in Docker.** Schema is owned by migrations, auto-applied by the
one-shot `migrate` service which all app services gate behind.

---

## Phase 1 — Pull & identify what changed (do NOT skip)

The blast radius of a change dictates everything downstream. After `git pull --ff-only`,
classify the diff:

- **Migrations?** `migrations/0000NN_*` added → schema will move; `migrate` runs it on `up`.
  Read the `.up.sql` — know if it's pure data cleanup vs. structural, and whether it needs
  data present first.
- **Go files?** → rebuild `tms:dev`; restart api/worker/scheduler/live.
- **UI files?** → rebuild `tms-ui:dev`; restart ui (+ **hard-refresh the browser** — the PWA
  service worker caches aggressively and will keep serving the old bundle).
- **`compose.yaml`?** service renames/removals, new profiles, changed env **defaults**, port
  shifts. This is the sneakiest — see the env phase.
- **`.env.example` / `secrets/*.example`?** the contract for your local config changed.

> Lesson: a commit message like "rename env / accounts now UI-managed" is a **config-contract
> change**, not just code. Treat docs/refactor commits as deploy-affecting until proven otherwise.

## Phase 2 — Reconcile local env / secrets (the silent breaker)

Your `.env` and `secrets/moomoo.env` are gitignored, so a repo change **cannot** update them —
you must. Diff the templates **and** the compose env block:

- Compare `.env.example` / `secrets/moomoo.env.example` against your live files for **added /
  removed / renamed** keys.
- Check `compose.yaml` for changed **defaults** (`${VAR:-default}`) — a renamed default can be
  *invalid* against new code while your `.env` is silent about it.
- Preserve your real secrets (API token, PG password, data-vendor key, `…_ADDR`); only move the
  keys that actually changed.

> Two real traps from today, both env-shaped:
> 1. A compose default was renamed to a value the new binary **rejects** → container crash-loops.
>    Fix = set the var explicitly in `.env`.
> 2. State that used to come from `.env` moved to **runtime/UI-managed** (DB rows). The service now
>    **requires that state to exist** before it starts (e.g. a default account) — nothing seeds it.
>    You must create it (UI or API) as part of the deploy.

## Phase 3 — Graceful shutdown

```
docker compose --profile <profiles> down -t 60        # NEVER -v
```

- `-t 60` lets the worker **drain in-flight jobs** and the live node close its OpenD session;
  raise it if a long hyperopt/eod job may be running.
- **Never `-v`** — that drops `tmsgo-pgdata`/`tmsgo-redisdata` (your 38M bars + state).
- Add `--remove-orphans` when a deploy renamed/removed services, so stragglers don't linger.
- **Leave OpenD running** — it's host-side; the live node reconnects on the way back up. Only
  touch OpenD if its own config changed.
- Lighter alternatives: `stop`/`start` (keep containers, fast resume); or stop a single service.

## Phase 4 — Rebuild & start fresh

```
docker compose build tmsgo-worker ui                  # one tms:dev producer + the ui image
docker compose --profile <profiles> up -d --wait      # migrate runs FIRST, gated, then apps
```

- **Build via a single `tms:dev` producer** (`tmsgo-worker`), never `up --build` across the app
  profile — four services share `tms:dev` and collide on parallel export. Then `up` *without*
  `--build`.
- `--no-cache` is unnecessary — changed source invalidates the COPY layer and recompiles.
- Migration application is **automatic and ordered** (the `migrate` service completes before
  api/worker/scheduler/live start). There is no separate "apply migration" step.
- Build *before* down to minimize downtime; the old stack keeps serving during the compile.

## Phase 5 — Verify (don't trust "up")

- `migrate` exited **0**, schema at the expected version, `dirty=false`.
- All services **healthy** (`docker compose ps`).
- `tmsgo-live` reconnected: logs show `moomoo connected` + `live session running` (and didn't
  crash-loop — `--wait` reporting one container "unhealthy" is your signal to read its logs).
- The actual feature/fix behaves. When the UI is the deliverable, **render it** (a headless
  browser against `:13000` cuts through stale-cache confusion faster than asking "did you refresh?").

---

## Cross-cutting gotchas (the landmines)

- **Stale UI ≠ broken backend.** Curl the API/proxy first; a grey/empty UI is often just the
  cached PWA bundle. Hard-refresh before debugging code.
- **`migrate` is gated, idempotent, and safe to re-run** — but read structural migrations before
  trusting them on real data.
- **Host-side prerequisites Docker won't fix:** OpenD must listen on `0.0.0.0:11111` (not
  loopback) for the containerized live node to reach it via `host.docker.internal`; the bind-mounted
  `runs/` dir must be owned by uid **65532** (distroless nonroot) or hyperopt fails `mkdir EACCES`.
- **Account / data-shape moves to "UI-managed" mean a manual seed step** the compose file can't do.
- **This shell quirk:** if your terminal predates being added to the `docker` group, prefix Docker
  commands with `sg docker -c '…'` until a fresh login.

## When the OOTB deploy is broken

Today's pull shipped a `compose.yaml` whose default `--env` was rejected by the new binary, and a
new hard requirement (a default account) with nothing to seed it. **A clean pull that won't come up
is a real upstream bug, not your mistake** — get it running with the minimal local override (set the
var, seed the row), then file/fix it upstream rather than carrying the workaround forever.

## Rollback

Code/UI: `git checkout <prev-sha>` → rebuild → `up`. **Migrations do not auto-revert** — a forward
migration that altered structure needs its `.down.sql` (`tms migrate down`) and may not be loss-free.
Prefer rolling *forward* with a fix; treat `down` on structural migrations as a last resort with a
backup in hand (`pg_dump`, or a `tmsgo-pgdata` volume snapshot taken before the deploy).
