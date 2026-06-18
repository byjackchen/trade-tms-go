# Production deployment (Ubuntu + Docker Compose)

Target prd: a single **Ubuntu** host, code pulled from **GitHub**, the **full
stack run via `docker compose`**, **moomoo OpenD already installed on the host**.
This mirrors the local dev topology (compose + host OpenD), so the same
`compose.yaml` drives prd — only config (secrets, ports, real account) differs.

Golden rule: **schema comes from migrations (`tms migrate up`, run automatically
by the `migrate` service), data comes in selectively — never restore the whole
dev DB.**

## 0. Stack (compose profiles)

- base (always): `postgres` (TimescaleDB), `redis`, `migrate` (one-shot `tms migrate up`).
- `--profile app`: `tmsgo-api`, `tmsgo-worker`, `tmsgo-scheduler`, `ui`.
- `--profile live`: `tmsgo-live` (automated real/paper trading node → host OpenD).
- `--profile manual`: `tmsgo-mock-opend` + `tmsgo-live-manual` — **dev only**
  (mock OpenD). prd uses real OpenD on the host, so do NOT use `manual`.

Images build on the Ubuntu host from the Dockerfiles (`tms:dev` + `tms-ui:dev`);
no external registry needed.

## 1. Ubuntu host prerequisites

```bash
# Docker Engine + compose plugin
sudo apt-get update && sudo apt-get install -y docker.io docker-compose-v2
sudo usermod -aG docker $USER   # re-login after this
# OpenD: already installed + running on the host, listening on 11111.
#   the live container reaches it via host.docker.internal (compose already
#   maps extra_hosts: host.docker.internal:host-gateway on tmsgo-live).
```

## 2. Get the code

```bash
git clone https://github.com/byjackchen/trade-tms-go.git
cd trade-tms-go
git checkout main && git pull        # deploy a known commit/tag
```

## 3. Configure (NOT committed)

Create `.env` (prd values; compose reads it via `${VAR:-default}`):
```dotenv
# --- Postgres (strong password; in-network, do NOT expose the host port) ---
TMS_PG_USER=tms
TMS_PG_PASSWORD=<strong-random>
TMS_PG_DATABASE=tms
# --- API ---
TMS_API_TOKEN=<strong-random>                 # REQUIRED — api won't start without it
TMS_API_CORS_ORIGINS=https://<your-ui-host>   # or http://<host>:13000 if internal
TMS_LOG_LEVEL=info
# --- data / scheduler ---
NASDAQ_DATA_LINK_API_KEY=<key>                # required for `sync` (API->DB) + daily refresh
TMS_SHARADAR_CACHE_DIR=/srv/tms/sharadar      # only used by the parquet `import` path
TMS_RUNS_DIR=/srv/tms/runs
TMS_SCHEDULER_DAILY_AT=18:30
TMS_SCHEDULER_TZ=America/New_York
```

Create `secrets/moomoo.env` for the live node (gitignored; real-money secrets):
```dotenv
TMS_MANUAL_MOOMOO_ADDR=host.docker.internal:11111   # host OpenD
TMS_MOOMOO_ADDR=host.docker.internal:11111
# Live (real money) — the 4-factor gate; omit ALL of these for signal/paper-only:
TMS_LIVE_EXEC_POLICY=auto
TMS_LIVE_ENV=real
TMS_MOOMOO_UNLOCK_PASSWORD=<...>              # or GUI-unlocked OpenD
TMS_LIVE_CONFIRM=<the exact go-live phrase>
TMS_LIVE_TRADER_ID=TMS-LIVE-REAL-001
```
> The real account is NOT an env var: create it in the UI (Accounts → `/account`)
> and mark it default for `(moomoo, real)` — its number is stored in `tms.accounts`.
> A real order then requires ALL of: a default real account + `TMS_LIVE_CONFIRM`
> phrase + successful OpenD UnlockTrade + the `TMS-LIVE-REAL-001` trader id.
> Without them the node runs signal/paper only. Keep the secrets out of git, the
> image, and any dump.

### Host data directories — chown BEFORE the first `up`

The worker mounts `TMS_RUNS_DIR → /data/runs` (read-write) and
`TMS_SHARADAR_CACHE_DIR → /data/sharadar` (read-only), and it runs as the
distroless **`nonroot` user (uid/gid 65532)**. If you let Docker auto-create the
bind-mount source dirs they are owned by `root`, and the worker then can't write
them — e.g. a `hyperopt.run` job dies with
`mkdir /data/runs/hyperopt: permission denied` (data/eod refresh don't write
`runs/`, so the gap only surfaces on hyperopt / backtest-artifact writes).
Pre-create the runs dir and hand it to uid 65532 first:

```bash
sudo mkdir -p /srv/tms/runs /srv/tms/sharadar     # = TMS_RUNS_DIR / TMS_SHARADAR_CACHE_DIR
sudo chown -R 65532:65532 /srv/tms/runs           # worker writes runs/hyperopt/* + backtest artifacts
# TMS_SHARADAR_CACHE_DIR is mounted READ-ONLY, so root-owned 755 is fine for it.
# Already deployed and hit the error? Fix the existing dir without sudo via a root container:
#   docker run --rm -v /srv/tms/runs:/r alpine chown -R 65532:65532 /r
```

## 4. Bring up the stack

```bash
# build images + start base+app; migrate runs automatically (schema -> v19)
docker compose --profile app up -d --build --wait
docker compose logs migrate | tail        # confirm "migrations up to date" (v19)
```
This starts: postgres, redis, migrate (one-shot), api (:18080), worker, scheduler, ui (:13000).

## 5. Load market data (one-time)

Schema is built by migrations (which also seed the default compositions +
baseline params). Now load the heavy **market data** (~1.6 GB across 5 Sharadar
datasets: TICKERS→tickers, SEP+SFP→bars_daily, SF1→fundamentals_sf1,
EVENTS→events). Two independent ingest paths feed the DB:

- **`sync`** — pulls straight from the **Nasdaq Data Link API → TimescaleDB** (no
  local files). Use this on a fresh host with no cache. ← **default for prd.**
- **`import sharadar`** — bulk-loads a pre-existing local **parquet cache**.

**Option A — pull from the Nasdaq Data Link API (recommended; no cache needed):**

Prereqs: ① `NASDAQ_DATA_LINK_API_KEY` in `.env` (get one at
https://data.nasdaq.com/account/profile); ② the key's account is **subscribed to
the SHARADAR tables** (paid datasets — otherwise the API returns empty); ③
outbound HTTPS to `data.nasdaq.com`.
```bash
# one-time full backfill (TICKERS→SEP→SFP→SF1→EVENTS over the window);
# narrow --start to bound runtime — full history is many paginated calls.
docker compose run --rm tmsgo-worker sync bootstrap --start 2010-01-01 --end <T-1>
# then catch up to T-1 (watermark-driven incremental)
docker compose run --rm tmsgo-worker sync catchup
# verify
docker exec tmsgo-postgres psql -U tms -d tms -tAc \
  "SELECT count(*), max(t) FROM tms.bars_daily;"
```
After this, `tmsgo-scheduler` runs the daily `data.refresh source=api → eod.refresh`
pipeline automatically (the worker holds the key) — no manual sync thereafter.

**Option B — import from a local parquet cache** (only if you already have one):
```bash
# place the parquet cache at $TMS_SHARADAR_CACHE_DIR (mounted to /data/sharadar), then:
docker compose run --rm tmsgo-worker import sharadar --tables all   # idempotent upsert
```

**Option C — data-only dump/restore of the market tables** (from an existing dev
DB, e.g. no API subscription on the prd box). Hypertables need the TimescaleDB
pre/post wrapper:
```bash
# from the dev/source DB:
docker exec tmsgo-postgres pg_dump -U tms -d tms -a -Fc \
  -t tms.tickers -t tms.bars_daily -t tms.bars_intraday \
  -t tms.fundamentals_sf1 -t tms.events -t tms.universe_snapshots \
  -t tms.dataset_sync_runs -f /tmp/mkt.dump
# copy to the prd host, then into the running prd postgres:
docker exec -i tmsgo-postgres psql -U tms -d tms -c "SELECT timescaledb_pre_restore();"
docker exec -i tmsgo-postgres pg_restore -U tms -d tms --no-owner --data-only < mkt.dump
docker exec -i tmsgo-postgres psql -U tms -d tms -c "SELECT timescaledb_post_restore();"
```
Also restore `tms.param_sets` + `tms.active_params` the same way IF you have
tuned (promoted) params to carry over.

**Never load dev artifacts into prd**: sessions, signals, orders, fills,
positions, reconciliation_reports, runs, run_metrics, equity_curves, trades,
jobs, audit_log, hyperopt_studies/trials, commands, risk_events, halts.

## 6. Live / paper trading node

OpenD is on the host. Validate, then start the live node:
```bash
docker compose run --rm tmsgo-live preflight --exec-policy auto --env real \
  --check-opend            # data freshness + warmup + OpenD reachability (entrypoint is `tms`)
docker compose --profile live up -d --wait tmsgo-live
docker compose logs -f tmsgo-live
```
For paper instead of real: set `TMS_LIVE_ENV=paper` (exec auto on a paper
account — create it in the UI and mark it default for `(moomoo, paper)`) or leave
exec-policy=signal for emit-only.

## 7. Updates (redeploy a new version)

```bash
git pull                                      # new commit/tag
docker compose --profile app up -d --build --wait   # rebuild + migrate (new mig auto-applied)
# (--profile live too if the live node changed)
```
`tms migrate up` (the migrate service) applies only the new migrations; data is
preserved (named volume `tmsgo-pgdata`). **Never** `down -v` (drops the data volume).

## 8. Backup / restore (full DB)

```bash
# backup
docker exec tmsgo-postgres pg_dump -U tms -Fc -d tms -f /tmp/tms-$(date -u +%Y%m%d).dump
docker cp tmsgo-postgres:/tmp/tms-$(date -u +%Y%m%d).dump ./
# restore to a fresh DB (timescaledb extension must exist first)
docker exec -i tmsgo-postgres psql -U tms -d tms -c "SELECT timescaledb_pre_restore();"
docker exec -i tmsgo-postgres pg_restore -U tms -d tms --no-owner < tms-YYYYMMDD.dump
docker exec -i tmsgo-postgres psql -U tms -d tms -c "SELECT timescaledb_post_restore();"
```
Or snapshot the `tmsgo-pgdata` volume (fastest; ties restore to the exact
PG+TimescaleDB version). Schedule daily via cron + offsite copy.

## 9. prd hardening checklist

- **Strong `TMS_PG_PASSWORD` + `TMS_API_TOKEN`** in `.env` (not the `tms`/`tms` defaults).
- **Do not expose PG/Redis host ports** publicly: drop the `55432:5432` / `56379:6379`
  port mappings (use a `compose.override.yaml` that removes `ports:`) or firewall
  them — they only need to be reachable inside the compose network. Expose only
  the UI (and the API if external).
- **TLS**: front the UI/API with a reverse proxy (nginx/caddy) terminating HTTPS;
  set `TMS_API_CORS_ORIGINS` to the real origin.
- **Secrets** (`secrets/moomoo.env`, live creds): `chmod 600`, owned by the deploy
  user; never in git, image, or a dump. The 4-factor live gate must be fully
  present for any real order.
- **Health**: `GET /healthz` + `/version` (public); `GET /api/v1/system` (bearer);
  container probes `tms <svc> --health`.
- **Backups**: §8 daily, offsite.
