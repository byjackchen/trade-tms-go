package publish

// store.go persists normalized signals into tms.signals — the
// durable system-of-record (decision 5: PG is truth, Redis is transport). Two
// write modes share one row shape:
//
//   - Append (streaming live path): one row per evaluate_intent per bar, as_of
//     NULL — the append-only SignalUpdate audit trail.
//   - Upsert (EOD engine-replay, decision 4): as_of = the refresh date; ON
//     CONFLICT (strategy_id, symbol, as_of) DO UPDATE so a re-run OVERWRITES
//     rather than duplicates. Idempotency MUST hold: run twice -> same rows.
//
// The (strategy_id, symbol, as_of) partial-unique index (migration 000010) is
// the idempotency target; it only constrains as_of-bearing EOD rows, leaving
// the streaming append path unconstrained.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Store writes signal intents to Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds a Store over a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// IntentRow is the persisted shape of one normalized intent, with the engine
// timing context the columns require.
type IntentRow struct {
	// Norm is the normalized intent (carries strategy_id, symbol, state,
	// strength, proximity, generation, payload).
	Norm NormalizedSignal
	// SessionID is the owning live session (nil for EOD / detached refresh).
	SessionID *int64
	// AsOf is the EOD refresh date; nil for the append-only streaming path.
	// When set, the write UPSERTs on (strategy_id, symbol, as_of).
	AsOf *time.Time
	// TSEvent is the engine bar timestamp (UTC); persisted as ts_event_ns +
	// ts.
	TSEvent time.Time
}

// appendSQL inserts one streaming intent (as_of NULL, append-only).
const appendSQL = `
INSERT INTO tms.signals
    (session_id, strategy_id, symbol, state, strength, proximity_to_trigger_pct,
     generation, signal, ts_event_ns, ts, as_of)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, NULL)`

// upsertSQL inserts-or-overwrites one EOD intent keyed on
// (strategy_id, symbol, as_of). The conflict target is the partial-unique
// index signals_eod_idem_idx; a re-run UPDATES every mutable column so
// the row is byte-identical to a fresh insert (idempotency).
const upsertSQL = `
INSERT INTO tms.signals
    (session_id, strategy_id, symbol, state, strength, proximity_to_trigger_pct,
     generation, signal, ts_event_ns, ts, as_of)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11)
ON CONFLICT (strategy_id, symbol, as_of) WHERE as_of IS NOT NULL
DO UPDATE SET
    session_id               = EXCLUDED.session_id,
    state                    = EXCLUDED.state,
    strength                 = EXCLUDED.strength,
    proximity_to_trigger_pct = EXCLUDED.proximity_to_trigger_pct,
    generation               = EXCLUDED.generation,
    signal                   = EXCLUDED.signal,
    ts_event_ns              = EXCLUDED.ts_event_ns,
    ts                       = EXCLUDED.ts`

// Append inserts one streaming intent (as_of NULL) via the pool.
func (s *Store) Append(ctx context.Context, row IntentRow) error {
	return appendOrUpsert(ctx, s.pool, row, false)
}

// Upsert inserts-or-overwrites one EOD intent (as_of set) via the pool. AsOf
// must be non-nil.
func (s *Store) Upsert(ctx context.Context, row IntentRow) error {
	if row.AsOf == nil {
		return fmt.Errorf("publish: Upsert requires a non-nil AsOf date")
	}
	return appendOrUpsert(ctx, s.pool, row, true)
}

// AppendTx / UpsertTx run within a caller-supplied transaction (the EOD writer
// batches a whole refresh transactionally so a partial failure rolls back).
func AppendTx(ctx context.Context, tx pgx.Tx, row IntentRow) error {
	return appendOrUpsertTx(ctx, tx, row, false)
}

// UpsertTx upserts within a transaction.
func UpsertTx(ctx context.Context, tx pgx.Tx, row IntentRow) error {
	if row.AsOf == nil {
		return fmt.Errorf("publish: UpsertTx requires a non-nil AsOf date")
	}
	return appendOrUpsertTx(ctx, tx, row, true)
}

// rowArgs builds the positional args for the append/upsert statements.
func rowArgs(row IntentRow, upsert bool) ([]any, string, error) {
	body, err := row.Norm.SignalJSON()
	if err != nil {
		return nil, "", err
	}
	state, err := domainState(row.Norm.State)
	if err != nil {
		return nil, "", err
	}
	ts := row.TSEvent.UTC()
	var prox any
	if row.Norm.ProximityToTriggerPct != nil {
		prox = *row.Norm.ProximityToTriggerPct
	}
	args := []any{
		row.SessionID,               // $1
		string(row.Norm.StrategyID), // $2
		row.Norm.Symbol,             // $3
		state,                       // $4
		row.Norm.Strength,           // $5
		prox,                        // $6
		row.Norm.Generation,         // $7
		string(body),                // $8
		ts.UnixNano(),               // $9  ts_event_ns — the exact source-bar instant
		ts,                          // $10 ts — source-bar instant (live append path)
	}
	if upsert {
		// EOD snapshot: the persisted `ts` is the as_of DATE, NOT the source-bar
		// instant. This gives every as_of refresh a DISTINCT ts, so two refreshes
		// computed from the SAME latest bar — e.g. an as_of=T run started before
		// T's bar was loaded stamps the T-1 bar's instant — never collide on ts.
		// The watchlist's max(ts) frontier then anchors on the LATEST refresh, and
		// no cross-as_of rows pile up at one ts. ts_event_ns ($9) still records the
		// exact source bar for audit/precision.
		asOf := row.AsOf.UTC()
		args[9] = asOf            // $10 ts := as_of (snapshot identity)
		args = append(args, asOf) // $11 as_of
		return args, upsertSQL, nil
	}
	return args, appendSQL, nil
}

func appendOrUpsert(ctx context.Context, pool *pgxpool.Pool, row IntentRow, upsert bool) error {
	args, sql, err := rowArgs(row, upsert)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("publish: persist intent %s/%s: %w", row.Norm.StrategyID, row.Norm.Symbol, err)
	}
	return nil
}

func appendOrUpsertTx(ctx context.Context, tx pgx.Tx, row IntentRow, upsert bool) error {
	args, sql, err := rowArgs(row, upsert)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("publish: persist intent %s/%s: %w", row.Norm.StrategyID, row.Norm.Symbol, err)
	}
	return nil
}

// domainState is a guard ensuring the State value satisfies the DB CHECK
// (no_setup|forming|buy|hold|exit|stop_hit). Unknown states are rejected before
// the DB round-trip with a clear error.
func domainState(s domain.SignalState) (string, error) {
	switch s {
	case domain.StateNoSetup, domain.StateForming, domain.StateBuy,
		domain.StateHold, domain.StateExit, domain.StateStopHit:
		return string(s), nil
	default:
		return "", fmt.Errorf("publish: invalid signal state %q (not in DB CHECK set)", s)
	}
}
