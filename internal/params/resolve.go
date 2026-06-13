package params

// resolve.go is the document-resolution layer: given a strategy id, produce the
// active parameter Document with the same precedence the Python loader uses
// (loader.py:64-96), adapted to the P0 DB schema:
//
//	DB active_params -> param_sets      (runs/active_params equivalent)
//	-> file TMS_STRATEGY_PARAMS_DIR/<strategy>.json (env-dir override)
//	-> embedded baseline                (package-shipped default)
//
// Like Python, resolution is per-strategy with baseline fallback: an absent DB
// row OR an absent file falls through to the next source rather than erroring,
// so a partial promotion (a sepa-only tuned set) still serves the rest from
// baseline (spec strategy-sepa.md §1.4 [MUST-MATCH]).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"

	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
)

// PayloadReader reads the active param document payload for a strategy from the
// DB (tms.active_params -> tms.param_sets.payload). It returns (nil, nil) when
// no active_params row exists for the strategy — that is the "no promotion =
// baseline" case (migrations/000003_strategy.up.sql: "No row = baseline"), and
// must fall through to the file/baseline sources rather than error.
type PayloadReader interface {
	ActivePayload(ctx context.Context, strategy string) (json.RawMessage, error)
}

// Resolver resolves parameter documents. DB and Dir are both optional: a nil DB
// skips the DB tier (embedded/file-only mode, matching a Python install with no
// TMS_STRATEGY_PARAMS_DIR), an empty Dir skips the file tier.
type Resolver struct {
	DB  PayloadReader // optional; nil = skip DB tier
	Dir string        // optional; "" = skip file tier (TMS_STRATEGY_PARAMS_DIR)
}

// Resolve returns the active Document for strategy, applying the precedence
// chain. The strategy id is validated to be one of the known baseline strategies
// so a typo fails fast rather than silently resolving nothing.
func (r *Resolver) Resolve(ctx context.Context, strategy string) (*Document, error) {
	if strategy == "" {
		return nil, fmt.Errorf("params: empty strategy id")
	}

	// 1. DB active_params -> param_sets.payload.
	if r.DB != nil {
		raw, err := r.DB.ActivePayload(ctx, strategy)
		if err != nil {
			return nil, fmt.Errorf("params: db resolve %q: %w", strategy, err)
		}
		if raw != nil {
			doc, err := ParseDocument(raw, strategy)
			if err != nil {
				return nil, fmt.Errorf("params: db payload for %q: %w", strategy, err)
			}
			doc.Source = OriginDB
			return doc, nil
		}
	}

	// 2. File env-dir override: only when <strategy>.json exists there
	//    (loader.py:84-86 — partial dirs fall through per-strategy).
	if r.Dir != "" {
		path := filepath.Join(r.Dir, strategy+".json")
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			doc, err := ParseDocument(raw, strategy)
			if err != nil {
				return nil, fmt.Errorf("params: file %s: %w", path, err)
			}
			doc.Source = OriginFile
			return doc, nil
		case errors.Is(err, os.ErrNotExist):
			// fall through to baseline (per-strategy fallback).
		default:
			return nil, fmt.Errorf("params: read %s: %w", path, err)
		}
	}

	// 3. Embedded baseline (package-shipped). hyperopt owns the embed FS and
	//    the identical baseline JSONs; re-read the raw bytes for the document.
	raw, err := hyperopt.BaselineRaw(strategy)
	if err != nil {
		return nil, err
	}
	doc, err := ParseDocument(raw, strategy)
	if err != nil {
		return nil, fmt.Errorf("params: baseline %q: %w", strategy, err)
	}
	doc.Source = OriginBaseline
	return doc, nil
}

// ---------------------------------------------------------------------------
// DB-backed PayloadReader (tms.active_params -> tms.param_sets).
// ---------------------------------------------------------------------------

// Querier is the subset of pgx used by DBPayloadReader (satisfied by *pgxpool.Pool
// and pgx.Tx), so the reader works inside or outside a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DBPayloadReader reads the active param payload from the P0 schema.
type DBPayloadReader struct {
	Q Querier
}

// ActivePayload returns the JSONB payload of the param_set currently promoted
// for strategy, or (nil, nil) when there is no active_params row.
func (d DBPayloadReader) ActivePayload(ctx context.Context, strategy string) (json.RawMessage, error) {
	const q = `
		SELECT ps.payload
		FROM tms.active_params ap
		JOIN tms.param_sets ps ON ps.id = ap.param_set_id
		WHERE ap.strategy = $1`
	var payload json.RawMessage
	err := d.Q.QueryRow(ctx, q, strategy).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return payload, nil
}
