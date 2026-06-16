# `live` → `trade` refactor + 2D (execution × account) separation

Status: phases 1-6 complete. Locked decisions (operator, 2026-06-16):

- **Full first-class account model** — a `tms.accounts` registry + `account_id` FK
  on `sessions/orders/positions/fills/reconciliation_reports`; positions key on
  `(account_id, strategy_id, symbol)`. Enables position management by account.
- **One account per node** — each trade node binds exactly ONE account (as today).
  Multi-account is a READ/aggregation concern: the UI/API aggregate a view across
  several nodes/accounts. The deterministic single-goroutine core is untouched.
- **Hard rename** `live` → `trade` across routes/packages/types/hooks (pre-1.0,
  no external consumers). Old `/live/*` routes 301-redirect briefly, then drop.
- **Build order**: phases 1–3 (backend) end-to-end → batched review → 4–6 (API/UI).

## The problem

`Mode {signal, paper, live}` is duplicated in three enums
(`domain/enums.go`, `livengine/session.go`, `exec/moomoo/executor.go`) and
conflates two orthogonal axes. Account selection is `accID = mode==live ?
LiveAccID : PaperAccID` (runner/live.go ~700). The DB has no account dimension.

## The model — two orthogonal axes + a first-class Account

- **Execution policy** (`domain.ExecutionPolicy`): `signal` (emit intents, no
  auto orders; operator executes manually) vs `auto` (auto-submit orders).
- **Account** (`domain.Account`): `{ id, venue, env, broker_acc_id, label }`,
  where `env ∈ {sim, simulate, real}`. "paper vs real" is `account.env`, not a
  mode. The manual desk binds to any account independent of execution policy.

Legacy `Mode` maps as: `signal → (Signal, no/informational acct)`,
`paper → (Auto, Simulate acct)`, `live → (Auto, Real acct)`. The 5 unified
runtimes (backtest/hyperopt/signal/paper/live) become points in (policy × account).

## Phases

0. Design sign-off — DONE.
1. **Domain abstractions** — add `ExecutionPolicy`, `BrokerEnv`, `Account` + a
   bridge from legacy `Mode`. Additive; no behavior change; tree stays green. DONE.
2. **DB account dimension** — `000014_accounts` migration: `tms.accounts` +
   `account_id` on sessions/orders/positions/fills/recon; backfill existing rows
   to a derived default account. Persistence writes/reads the account. DONE.
3. **runner/exec decoupling** — node config takes `(execution policy, account)`;
   executor binds an `Account` (TrdEnv derived from `account.env`); drop the
   `mode==live ? live : paper` account selection. Collapse the 3 Mode enums. DONE.
4. **API + CLI** — `tms trade` command (`--exec`, `--account`); `/trade/*` read
   surface (consolidate with mutations); `/live/*` → 301. DONE.
5. **UI** — `/live` → `/trade`; account selector; per-account position/blotter
   views; exec-policy + account pickers replace the mode switch. DONE.
6. **Cleanup** — cosmetic renames (`cmd/tms/live.go` → `trade_run.go` + its
   `live*` helpers → `trade*`; residual `TestLive*` → `TestTrade*`, keeping the
   `/live` alias test as `TestLegacyLiveRedirects`); e2e specs/helpers/README
   moved to `/trade` + `trade/*` with a new account-selector spec; docs/README +
   compose `tms trade run` and `/trade/*`; runbook `live-smoke.md` → `trade-smoke.md`.
   DONE. (The runtime node type `internal/runner.Live`/`NewLive`/`LiveConfig` is
   intentionally NOT renamed — out of scope.)

## Invariants preserved

- The deterministic single-goroutine engine core (`internal/core`,
  `internal/engine`) and the `OrderSubmitter`/`PositionReader` seams are unchanged.
- `domain.Order`/`domain.Fill`/`StrategySymbol` keep their shapes (position key
  gains an account dimension at the persistence layer, not in the hot path).
- The 4-factor live-activation gate + per-order confirm remain; "real" is now
  `account.env == real` but the gate logic is unchanged.
