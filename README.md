# trade-tms-go

A Go port of the Python reference `trade-multi-strategies` that **unifies five
trading modes on one engine**. The entire system ships as a single static binary
`tms` plus a Next.js control-plane UI, with **zero Python runtime dependency** in
production — Python is only the offline parity oracle.

The thesis: backtest, hyperopt, live-signal, paper, and live all run on the SAME
deterministic event-loop engine (`internal/core` + `internal/engine`), the SAME
strategy implementations, and the SAME portfolio (allocator / risk /
reconciliation) layer. Only the edge adapters — clock, data feed, executor,
publisher — differ between modes.

| Mode | Clock | Feed | Executor | Purpose |
|---|---|---|---|---|
| backtest | SimClock | historical (Postgres) | SimExecutor + FillModel | reproducible simulation |
| hyperopt | SimClock ×N | historical | SimExecutor | NSGA-II walk-forward search |
| live-signal | WallClock | moomoo OpenD stream | NoopExecutor | signals, no orders |
| paper | WallClock | moomoo OpenD stream | MoomooExecutor (paper) | simulated fills, real venue |
| live | WallClock | moomoo OpenD stream | MoomooExecutor (live, gated) | REAL money, 4-factor gate |

---

## The trifecta

Three hard requirements the system meets:

1. **Database-oriented.** All durable state lives in Postgres / TimescaleDB.
   Redis is reconstructable transport only — restarting it loses no system state.
   PG is authoritative on restart: a live node rehydrates its open session row,
   broker positions, strategy state, AND any latched trading halt (`tms.halts`)
   before resuming — a crash can never silently clear an operator/operational halt
   and re-arm a halted trader.
2. **Dockerized.** A clean `docker compose up` from zero brings up the whole
   system: Postgres, Redis, migrations, API, worker, UI.
3. **UI fully visual + controllable.** Every datum is observable AND every
   control is actionable from the UI (Data / Backtests / Strategies / Hyperopt /
   Live-cockpit), including kill / halt / flatten / mode-switch with confirmation.

---

## Quickstart

Host ports reserved for this project (do not change them to defaults):
**Postgres 55432, Redis 56379, API 18080, UI 13000, live node 18090.**

```bash
cp .env.example .env          # fill in TMS_API_TOKEN (openssl rand -hex 32), etc.

docker compose up -d --wait              # postgres + redis + migrate (schema in)
docker compose --profile app up -d --wait   # api (18080) + worker + ui (13000)
```

- UI:  <http://localhost:13000>
- API: <http://localhost:18080> — `/healthz` and `/version` are public; every
  `/api/*` route requires `Authorization: Bearer <TMS_API_TOKEN>`.

Start a live signal node (separate `live` profile — never started by `app`):

```bash
docker compose --profile live up -d tms-live   # signal mode, no credentials
```

Local development:

```bash
make build      # bin/tms (version injected)
make test       # go test -race ./...  (hermetic; no Python needed)
make vet fmt-check
make parity     # the Nautilus golden-parity gate (needs the read-only oracle venv)
```

---

## Documentation

| Doc | What |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Hexagonal design, five-modes-one-engine, deterministic event loop, sync-core/async-edge, DB-as-truth, Redis-as-transport, native moomoo client, 4-factor live gate |
| [docs/deployment.md](docs/deployment.md) | Compose profiles (app/live), env + secrets, host-OpenD (`host.docker.internal:11111`), first-boot migrate, Postgres backup/restore, scaling the worker |
| [docs/parity.md](docs/parity.md) | The production-grade accuracy proof ledger |
| [docs/api.md](docs/api.md) | REST + WebSocket API contract |
| [docs/runbooks/](docs/runbooks/) | Operational runbooks (e.g. the deferred live OpenD smoke) |
| [docs/spec/](docs/spec/) | Per-component specs ported from the reference |

---

## The parity story (P0–P6 evidence)

Every layer is **proven equal** to the Python reference, not merely close. See
[docs/parity.md](docs/parity.md) for the full ledger; the headline gates:

- **Fixed-point money** — exact int64 round-trip (`internal/domain`).
- **Indicators** — ≤ 1e-9 vs numpy/pandas (golden + incremental).
- **Data field parity** — Sharadar fields mirror the reference cache.
- **Strategy signals — 0 mismatch** across all four strategies.
- **Fill / engine parity vs Nautilus** — per-fill price/qty/timing exact,
  equity within a cent (`make parity`).
- **Metrics** — Sharpe/Calmar/MaxDD ≤ 1e-12 relative (Neumaier-compensated).
- **Hyperopt objective parity** — per-fold + stitched match (`parity_folds` tag).
- **moomoo protocol — byte-for-byte** vs the vendored Python SDK
  (`internal/adapters/moomoo`), proven with NO OpenD via a protocol-faithful mock.

The default `go test ./...` is hermetic — it consumes committed golden fixtures
and never shells to Python. Only the `parity` / `parity_folds` build tags invoke
the oracle, and neither is part of the shipped image.

---

## Known deferred items

- **Real-OpenD smoke.** The full live operations layer is built and proven
  against a protocol-faithful **mock** OpenD. Connecting to a real OpenD is
  deferred to market hours with a user-confirmed login — see
  [docs/runbooks/live-smoke.md](docs/runbooks/live-smoke.md). The mock-driven
  deterministic gate is the permanent CI path.

---

## Acceptance bar

ACCURATE (semantically equal to the reference unless an `[IMPROVE]` is noted) /
COMPLETE (no feature dropped) / NO SIMPLIFICATION / PRODUCTION-GRADE (error
handling, context cancellation, graceful shutdown, structured logging, zero panic
on the happy path) / SAFE (the 4-factor live activation gate + per-order
allocator/risk/halt gating).
