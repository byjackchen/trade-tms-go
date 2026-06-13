# Live OpenD smoke (DEFERRED to market hours)

P5 locked decision 7: a real-OpenD smoke is **deferred** to market hours with a
user-confirmed OpenD login. Do NOT connect to real OpenD in the build gate —
everything is gated through the protocol-faithful **mock** OpenD. This runbook
records the exact manual steps + acceptance for that deferred smoke.

> Status: the full live operations layer is built. The live-engine path
> (`internal/livengine`), the **native moomoo OpenD client**
> (`internal/adapters/moomoo`) + the **protocol-faithful mock OpenD server**
> (`internal/adapters/moomoo/mock`), the **streaming feed** + **Redis/PG sinks**
> (`internal/runner`, `internal/publish`), the **idempotent EOD engine-replay**
> (`tms eod` + `eod.refresh` job), the **ops.commands control plane**
> (`internal/commands`, halt/resume/kill/stop/set_mode), the **live REST + WS
> bridge** (`/api/v1/live/*`, `/api/v1/watchlist`) and the **`tms-live` compose
> service** (live profile) are all wired. The real-OpenD smoke below is the ONLY
> deferred piece (decision 7); the mock-driven deterministic gate is the
> permanent CI path.

## moomoo client / protocol fidelity (proven, no OpenD needed)

- **Protocol conformance gate** (`internal/adapters/moomoo`): the Go client's
  encoded request frames are byte-for-byte identical to the vendored Python
  moomoo SDK's `pack_pb_req` output (`TestEncodeFrameMatchesPythonSDK`), and the
  Go decoder parses SDK-encoded reply/push frames
  (`TestDecodeSDK{InitConnectReply,HistoryReply,UpdateKLPush}`). Regenerate the
  golden bytes after a proto change:

  ```sh
  # from the trade-multi-strategies venv:
  .venv/bin/python tmp/conformance/dump_frames.py  > internal/adapters/moomoo/testdata/conformance_frames.json
  .venv/bin/python tmp/conformance/dump_replies.py > internal/adapters/moomoo/testdata/conformance_replies.json
  ```

- **Client <-> mock round trip** (`internal/adapters/moomoo/mock`,
  `TestClientMockRoundTrip`): every P5 message over a real TCP socket —
  InitConnect, GetGlobalState, KeepAlive, Qot_Sub + Qot_UpdateKL push,
  RequestHistoryKL, GetKL, GetBasicQot, GetSubInfo.

- **Reconnect + re-subscribe** (`TestClientReconnectResubscribe`): a transient
  connection drop triggers exponential-backoff reconnect and replay of the
  subscription set; pushes resume without re-subscribing.

- **Postgres-backed mock** (`TestPGBarSourceThroughMock`, `-tags integration`,
  `make itest`): the mock serves real `tms.bars_daily` rows to the client,
  round-tripping exact fixed-point prices.

  ```sh
  go test ./internal/adapters/moomoo/... -race          # unit + conformance + mock
  make itest  # adds the Postgres-backed mock source test
  ```

To regenerate the Go protobuf bindings from the vendored `.proto`
(`internal/adapters/moomoo/proto/`):

```sh
GOBIN="$PWD/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
PATH="$PWD/bin:$PATH" protoc \
  --proto_path=internal/adapters/moomoo/proto \
  --go_out=internal/adapters/moomoo/pb \
  --go_opt=module=github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb \
  internal/adapters/moomoo/proto/*.proto
```

## What is already proven (Build1, no OpenD needed)

- `internal/core` streaming event loop with a `WallClock` (live) and a
  `VirtualClock` (deterministic). `go test -race ./internal/core/...`.
- `internal/livengine` signal-mode session: reuses the SAME strategy / portfolio
  / context / warmup code as backtest, with a `NoopExecutor` (records a
  `SignalIntent` per strategy per bar, submits NO orders).
- **Consistency proof** (the accuracy anchor, no Python live golden exists):
  `TestLiveStreamEqualsBatchReplay` + `TestLiveWarmupConsistency` assert the
  streaming (virtual-clock) live path emits SignalIntents IDENTICAL to a batch
  replay of the same bars.
- `tms live --mode signal` lifecycle: ctx cancellation (SIGINT/SIGTERM), graceful
  shutdown, structured logs.
- **EOD idempotency** (`internal/runner`, mock/PG-driven, `-race`): running the
  EOD engine-replay twice for the same `as_of` yields the SAME
  `tms.signal_intents` rows — the UPSERT on the partial-unique
  `(strategy_id, symbol, as_of)` index overwrites, never duplicates
  (`TestEODIdempotency`, `TestEODDeterministicContent`).
- **Signal-session DB emission**: a streaming session over mock-shaped bars
  appends one `tms.signal_intents` row per emitted intent
  (`TestSignalSessionAppendsToDB`).
- **Command halt stops new intents**: with the `EmitGate` halted, the session
  keeps processing bars (state stays warm) but emits NO new intents
  (`TestHaltStopsNewIntents`); the `ops.commands` consumer applies a `halt`
  command idempotently and audits it (`TestCommandConsumerHaltAudited`); the
  confirmation gate blocks an unconfirmed `set_mode` to paper/live
  (`TestEnqueueConfirmationGate`).
- **Mode-switch via graceful session restart**: `tms live`'s supervisor restarts
  the session on a `set_mode` command (P5 accepts only `signal`; paper/live are
  rejected, deferred to P6).

## Mock-driven gate (the permanent CI path; no OpenD)

The protocol-faithful mock OpenD (`internal/adapters/moomoo/mock`) serves bars
from our Postgres `bars_daily`/`bars_intraday` over the EXACT moomoo wire
framing, so the whole live path is exercised end-to-end without real OpenD:

```sh
# start the mock OpenD bound to a port, pointed at the dev Postgres, then:
export TMS_MOOMOO_ADDR=127.0.0.1:<mock-port>
tms live --mode signal --trader-id SIGNAL-GATE-001 --strategy sector_rotation
# observe tms.signal_intents rows appended + Redis streams populated +
# /api/v1/live/intents returning them + the cockpit WS bridging them.
```

The real-vs-mock switch is config-only (`TMS_MOOMOO_ADDR`): the same node code
runs against the mock in the gate and real OpenD at market hours.

## Deferred manual smoke (Build2, market hours, user confirms OpenD login)

Preconditions:

1. moomoo OpenD running and logged in on the host, listening on `127.0.0.1:11111`.
2. From a container, OpenD is reachable at `host.docker.internal:11111`.
3. The TMS stack is up (api 18080, ui 13000) per the canonical Docker restart.

Steps:

0. (Optional, fastest) Low-level moomoo connectivity check against real OpenD —
   confirms the native client handshakes and pulls a real history K-line before
   involving the full session. Run the conformance/round-trip suite first
   (`go test ./internal/adapters/moomoo/... -race`), then point the client at
   real OpenD via `TMS_MOOMOO_ADDR` and observe in the live-node logs:
   `moomoo connected connID=… keepAliveSec=…`, followed by a non-empty
   `Qot_RequestHistoryKL` reply for one ticker. A failure here (dial refused,
   `qotLogined=false`, empty reply) means OpenD is not logged in / not serving
   US market data — fix that before step 1.

1. Point the live node at real OpenD:

   ```sh
   export TMS_MOOMOO_ADDR=host.docker.internal:11111   # real-vs-mock switch (decision 2)
   ```

2. Start a signal session:

   ```sh
   tms live --mode signal --trader-id SIGNAL-SMOKE-001 --strategy multi \
     --tickers AAPL,MSFT,KO --moomoo-addr "$TMS_MOOMOO_ADDR"
   ```

3. Observe in the cockpit (ui 13000) on the live-signals page for SIGNAL-SMOKE-001:
   - intraday bars flowing from OpenD (`Qot_UpdateKL` push) drive `evaluate_intent`;
   - one SignalIntent per strategy per bar appears, newest-wins dedup on
     (symbol, strategy_id);
   - the portfolio-health panel updates each cadence (informational in signal
     mode: no positions, no daily-loss halt).

4. Exercise the control plane (the audited side channel) against the running
   node via the API:

   ```sh
   # halt: stops emitting NEW intents (state stays warm; no FLATTEN in signal mode)
   curl -XPOST -H "Authorization: Bearer $TMS_API_TOKEN" \
     localhost:18080/api/v1/live/commands -d '{"name":"halt","reason":"smoke"}'
   # observe: no new intents appear; GET /api/v1/live/session shows the active halt.
   # resume: intents flow again.
   curl -XPOST -H "Authorization: Bearer $TMS_API_TOKEN" \
     localhost:18080/api/v1/live/commands -d '{"name":"resume"}'
   # a paper/live switch without a token is rejected 412:
   curl -XPOST -H "Authorization: Bearer $TMS_API_TOKEN" \
     localhost:18080/api/v1/live/commands -d '{"name":"set_mode","mode":"live"}'
   ```

   Confirm each command produced a `tms.commands` row (status `completed`/
   `rejected`) and a `tms.audit_log` row.

5. Run the idempotent EOD refresh for a closed trading day and confirm a re-run
   does NOT duplicate rows:

   ```sh
   tms eod --as-of 2026-06-11 --strategy multi --tickers AAPL,MSFT,KO
   tms eod --as-of 2026-06-11 --strategy multi --tickers AAPL,MSFT,KO   # re-run
   # SELECT count(*) FROM tms.signal_intents WHERE as_of='2026-06-11';  -> unchanged
   ```

6. Send SIGINT (Ctrl-C) and confirm a clean graceful shutdown in the logs
   (`live node stopped`), no goroutine leak, no partial intent.

## Acceptance

- Real intraday bars from OpenD produce SignalIntents in the cockpit, matching
  the shape the mock-driven gate produces (same envelope, same per-strategy
  payload).
- No orders are submitted (signal mode; verify the order book stays empty).
- Graceful shutdown on signal; the session row in `tms.sessions` ends with
  `status = STOPPED`, `ended_at` set.
- The deterministic mock gate (CI) remains green for the same universe — the mock
  vs real switch is config-only.
