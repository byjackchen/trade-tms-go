package paramsdb

// Package paramsdb is the pgx-backed adapter for internal/params: it implements
// params.PayloadReader against the P0 Postgres schema (tms.active_params ->
// tms.param_sets.payload). It is split out of the params core so that the
// params package — and therefore the pure strategy packages and the golden
// dependency closure — import NO pgx driver.
//
// Usage (engine assembly / api / jobs):
//
//	ld := params.NewLoader(paramsdb.NewReader(pool), cfg.StrategyParamsDir)
//	sepa, _, err := ld.SEPA(ctx)

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/byjackchen/trade-tms-go/internal/params"
)

// Querier is the subset of pgx used by Reader (satisfied by *pgxpool.Pool and
// pgx.Tx), so the reader works inside or outside a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Reader reads the active param payload from the P0 schema. It implements
// params.PayloadReader.
type Reader struct {
	Q Querier
}

// NewReader builds a Reader over q (e.g. a *pgxpool.Pool or pgx.Tx).
func NewReader(q Querier) Reader {
	return Reader{Q: q}
}

// ActivePayload returns the JSONB payload of the param_set currently promoted
// for strategy, or (nil, params.ErrNoActivePayload) when there is no
// active_params row. The sentinel is the params-core no-row signal: Resolve
// treats it as "fall through to file/baseline" rather than an error, so a
// partial promotion still serves the rest from baseline.
func (d Reader) ActivePayload(ctx context.Context, strategy string) (json.RawMessage, error) {
	const q = `
		SELECT ps.payload
		FROM tms.active_params ap
		JOIN tms.param_sets ps ON ps.id = ap.param_set_id
		WHERE ap.strategy = $1`
	var payload json.RawMessage
	err := d.Q.QueryRow(ctx, q, strategy).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, params.ErrNoActivePayload
	}
	if err != nil {
		return nil, err
	}
	return payload, nil
}

// Ensure Reader satisfies the PayloadReader seam.
var _ params.PayloadReader = Reader{}
