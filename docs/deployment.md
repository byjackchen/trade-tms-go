# Deployment

The whole system ships as one image (`tms:dev`, built from the root `Dockerfile`
— multi-stage `golang:1.26` → distroless static nonroot) plus the UI image
(`tms-ui:dev`, a Next.js standalone bundle). A clean `docker compose up` from
zero brings up Postgres/TimescaleDB, Redis, runs migrations, and starts the
control plane. **There is zero Python runtime dependency in production** — Python
is only the offline parity oracle (see `docs/parity.md`).

Host ports reserved for this project (this machine runs other stacks — do not
change them to defaults): **Postgres 55432, Redis 56379, API 18080, UI 13000,
live node 18090.**

---

## 1. Compose profiles

`compose.yaml` defines two profiles plus an always-on base.

| Profile | Services | Bring up |
|---|---|---|
| (base, no profile) | `postgres`, `redis`, `migrate` | `docker compose up -d --wait` |
| `app` | `tmsgo-api` (→18080), `tmsgo-worker`, `ui` (→13000) | `docker compose --profile app up -d` |
| `live` | `tmsgo-live` (→18090) | `docker compose --profile live up -d` |

Notes:

- The `migrate` service runs `tms migrate up` once and exits; every app service
  `depends_on` it `service_completed_successfully`, so nothing starts against an
  un-migrated database.
- The `live` profile is **separate from `app` on purpose** — a trading node is
  started deliberately, never by an `app` bring-up.
- App-profile containers use the `tmsgo-` prefix (`tmsgo-api`, `tmsgo-worker`,
  `tmsgo-ui`, `tmsgo-live`) so they never collide with the Python reference
  stack's `tms-api` / `tms-ui` containers on a shared host. A regression guard
  (`internal/app/deployguard_test.go`) pins this.
- The runtime image is distroless (no shell), so healthchecks exec the binary
  itself: `tms api --health`, `tms worker --health`, `tms live --health` each GET
  the service's own loopback `/healthz` inside the shared container netns.

### Full system from zero

```bash
cp .env.example .env          # fill in real values (see §2)
docker compose up -d --wait              # postgres + redis + migrate
docker compose --profile app up -d --wait   # api + worker + ui
# UI:  http://localhost:13000
# API: http://localhost:18080  (Bearer-token protected; /healthz and /version are public)
```

---

## 2. Environment and secrets

All configuration flows through `internal/config`: `.env` is loaded by walking
up from the working directory, and **real environment variables take precedence**
(override=false), matching the Python reference. Missing required config fails
loud at startup with a `MissingConfig` error and a setup hint — there is no
silent default for anything security-relevant.

Copy `.env.example` to `.env` and fill in:

| Variable | Purpose |
|---|---|
| `TMS_PG_HOST/PORT/USER/PASSWORD/DATABASE/SSLMODE` | Postgres connection. Host-side CLI uses `TMS_PG_PORT=55432`; in-compose services override to `postgres:5432`. |
| `TMS_PG_MAX_CONNS` / `TMS_PG_MIN_CONNS` | pgx pool sizing (`MAX>=1`, `MIN` in `[0,MAX]`). |
| `TMS_REDIS_ADDR` / `TMS_REDIS_DB` / `TMS_REDIS_PASSWORD` | Redis transport. Host-side `127.0.0.1:56379`; in-compose `redis:6379`. |
| `TMS_API_TOKEN` | **Required.** Bearer token for every `/api/*` route. The API refuses to start without it. Generate with `openssl rand -hex 32`; the UI must use the same value. |
| `TMS_API_CORS_ORIGINS` | Comma-separated browser-origin allowlist (default: the UI on 13000). |
| `TMS_WORKER_CONCURRENCY` | Worker executor count (>=1). |
| `NASDAQ_DATA_LINK_API_KEY` | Sharadar import via `--source api` (validated at run time, not at boot). |
| `TMS_SHARADAR_CACHE_DIR` | Local Parquet cache root (default `./cache/sharadar`). |
| `TMS_STRATEGY_PARAMS_DIR` | Tuned-params directory; empty = built-in baseline. |
| `TMS_RUNS_DIR` | Legacy backtest artifact dir (default `./runs`); DB is the source of truth. |
| `TMS_MOOMOO_ADDR` | OpenD address — the real/mock switch (see §3). |
| `TMS_MOOMOO_MAX_SUB` | Per-connection subscription cap (FutuOpenD's documented 100). |
| `TMS_LIVE_TRADER_ID` | `sessions.trader_id` + Redis namespace. |

### Secrets handling

- `.env*` is gitignored AND excluded by `.dockerignore`, so a developer's `.env`
  is never copied into an intermediate builder layer (also pinned by
  `deployguard_test.go`).
- moomoo trading credentials and the live-activation material live ONLY in the
  gitignored file `./secrets/moomoo.env`, mounted at runtime via `env_file` and
  `/run/secrets/moomoo`. They are **never baked into the image and never logged**:
  - `TMS_MOOMOO_PAPER_ACC_ID`, `TMS_MOOMOO_LIVE_ACC_ID`
  - `TMS_MOOMOO_UNLOCK_PASSWORD`
  - `TMS_LIVE_CONFIRM` (the typed confirmation phrase)
- For market-data + signal mode on localhost OpenD, no credentials are required
  (`encrypt=off`); the secrets file is optional.

---

## 3. Host-OpenD connection

`TMS_MOOMOO_ADDR` is the single real-vs-mock switch.

- **Real OpenD** (default for the `tmsgo-live` service): OpenD runs on the host at
  `127.0.0.1:11111`. The container reaches it as `host.docker.internal:11111`;
  the compose service maps `host.docker.internal` → host gateway via:

  ```yaml
  extra_hosts:
    - "host.docker.internal:host-gateway"
  ```

- **Mock OpenD** (gate / CI): point `TMS_MOOMOO_ADDR` at the in-repo
  protocol-faithful mock server's address. The mock is driven from the Postgres
  bars and exercises the full message set deterministically.

The real-OpenD smoke is deferred to market hours with a user-confirmed OpenD
login — see `docs/runbooks/live-smoke.md`. The mock-driven path is the permanent
CI path.

### Live mode bring-up

```bash
# signal mode (no orders, no credentials) — safe default:
docker compose --profile live up -d tmsgo-live

# paper / live: set TMS_LIVE_MODE and the secrets in ./secrets/moomoo.env first.
# live (REAL money) additionally requires all 4 activation factors
# (real acc id + unlock password + TMS_LIVE_CONFIRM phrase + bound trader id);
# the node refuses to start without them.
```

The shipped default strategy is `sector_rotation` (derives its ETF universe from
params, needs no `--tickers`), so a fresh stack runs a working signal session
without a loaded stock universe. Override with `TMS_LIVE_STRATEGY` (and pass
`--tickers` for `sepa` / `multi`) once bars are loaded.

---

## 4. First boot (migrate)

The database schema enters any environment only via embedded migrations:

```bash
docker compose up -d --wait postgres        # start DB
docker compose run --rm migrate             # tms migrate up (idempotent)
# or simply: docker compose up -d --wait    # base profile runs migrate automatically
```

Check status from the host CLI (with `.env` pointing at port 55432):

```bash
bin/tms migrate status
bin/tms migrate up
```

Migrations are embedded in the binary, so the image and the schema version are
always consistent — there is no separate migration artifact to ship.

---

## 5. Backup and restore of Postgres

Postgres is the single source of truth, so backing up the `tms` schema (and the
TimescaleDB chunks) is the complete backup. Redis is reconstructable and does
not need backing up.

```bash
# Backup (custom format, parallelizable, includes Timescale hypertable chunks):
docker exec tmsgo-postgres pg_dump -U tms -d tms -Fc -f /tmp/tms.dump
docker cp tmsgo-postgres:/tmp/tms.dump ./backups/tms-$(date +%Y%m%d).dump

# Restore into a fresh database:
docker cp ./backups/tms-YYYYMMDD.dump tmsgo-postgres:/tmp/tms.dump
docker exec tmsgo-postgres pg_restore -U tms -d tms --clean --if-exists /tmp/tms.dump
```

Notes:

- TimescaleDB hypertables (`bars_daily`, `bars_intraday`, `equity_curves`)
  restore through `pg_restore` like ordinary tables; the extension is recreated
  by `000001_init` if you restore into an empty DB after running `tms migrate up`.
- For a logical, schema-only baseline, `pg_dump --schema-only -n tms` captures
  the structure that `tms migrate up` would also produce.
- The Docker named volume `tmsgo-pgdata` is the live data directory; a
  volume-level snapshot is an alternative whole-cluster backup, but the logical
  `pg_dump` above is the portable, version-independent path.

---

## 6. Scaling tms-worker

`tms-worker` drains the durable `tms.jobs` queue. The queue is claimed with
`FOR UPDATE SKIP LOCKED`, with heartbeats, stale-claim recovery, cooperative
cancel, and per-job panic isolation — so the worker is **horizontally scalable
without coordination**:

- Increase intra-process concurrency via `TMS_WORKER_CONCURRENCY` (default 4).
- Run multiple worker replicas:

  ```bash
  docker compose --profile app up -d --scale tms-worker=3
  ```

  Because claims are `SKIP LOCKED`, replicas never double-process a job; a
  crashed worker's in-flight claims are recovered by stale-claim detection.

The worker drains gracefully on SIGTERM (finishes or releases in-flight jobs),
so rolling restarts and scale-downs are safe.

---

## 7. Health and observability

- `/healthz` and `/version` are public on every HTTP service; all other API
  routes require the bearer token.
- Compose healthchecks gate `depends_on` ordering and surface readiness.
- Logs are structured (`TMS_LOG_FORMAT=json` in containers); set
  `TMS_LOG_LEVEL` per service.
- Live nodes expose `/healthz` on host port 18090.
