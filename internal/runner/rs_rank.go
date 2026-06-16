package runner

// rs_rank.go is a TMS ENHANCEMENT (not present in the Python SEPA reference): the
// cross-sectional Relative-Strength stamping pass of the EOD refresh. After the
// as-of SEPA intents are persisted (idempotent UPSERT, eod.go step 4), this pass:
//
//  1. loads the universe's trailing adjusted closes (close_adj) ending at as_of in
//     a SINGLE set-based query (one array_agg per ticker — never N+1);
//  2. computes the Minervini-weighted RS blend per symbol and percentile-ranks it
//     across the universe into [1,99] (indicators.RSRawScore + RSRankUniverse);
//  3. stamps rs_rank onto each SEPA intent for that as_of AND recomputes the
//     buy_readiness composite (which depends on RS) — both written into the
//     signal_intents.intent JSONB — in one batched UPDATE pass.
//
// The Python oracle never computes an RS rank (SEPASignalIntent.rs_rank is
// reserved-and-always-null); this is a deliberate TMS divergence to make every
// forming signal rankable on the watchlist.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/indicators"
)

// rsHistoryDays bounds the calendar lookback for the trailing-close query. The RS
// blend needs 253 TRADING bars (RSLookback252 + the current bar); ~370 calendar
// days comfortably covers that with holidays/weekends.
const rsHistoryDays = 370

// stampRSRank computes the universe RS rank as-of cfg.AsOf and stamps it (plus the
// RS-dependent buy_readiness) onto the persisted SEPA intent rows. It is a no-op
// when the pool is nil or the universe is empty. Errors are returned so the EOD
// run surfaces them, but a missing-history symbol is simply skipped (not ranked).
//
// TMS enhancement — not in the Python SEPA reference.
func stampRSRank(ctx context.Context, pool *pgxpool.Pool, asOf time.Time, universe []string) (int, error) {
	if pool == nil || len(universe) == 0 {
		return 0, nil
	}

	// (1) One set-based query: trailing close_adj arrays (chronological) per ticker
	// over [as_of-rsHistoryDays, as_of]. array_agg with ORDER BY ts keeps each
	// series oldest-first; close_adj NULLs are dropped (FILTER) so the blend never
	// divides by a NaN gap. This is a SINGLE round trip for the whole universe.
	start := asOf.AddDate(0, 0, -rsHistoryDays)
	rows, err := pool.Query(ctx, `
		SELECT ticker,
		       array_agg(close_adj ORDER BY ts ASC) FILTER (WHERE close_adj IS NOT NULL) AS closes
		  FROM tms.bars_daily
		 WHERE ticker = ANY($1) AND ts >= $2 AND ts <= $3
		 GROUP BY ticker`,
		universe, start.UTC(), asOf.UTC())
	if err != nil {
		return 0, fmt.Errorf("eod: rs-rank query: %w", err)
	}
	defer rows.Close()

	raw := make(map[string]float64, len(universe))
	for rows.Next() {
		var ticker string
		var closes []int64 // close_adj is BIGINT 1e-4 fixed-point
		if err := rows.Scan(&ticker, &closes); err != nil {
			return 0, fmt.Errorf("eod: rs-rank scan: %w", err)
		}
		series := make([]float64, len(closes))
		for i, c := range closes {
			series[i] = domain.Price(c).Float64()
		}
		if score, ok := indicators.RSRawScore(series); ok {
			raw[ticker] = score
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("eod: rs-rank read: %w", err)
	}

	ranks := indicators.RSRankUniverse(raw) // symbol -> [1,99]
	if len(ranks) == 0 {
		return 0, nil
	}

	// (2) Stamp each ranked SEPA intent for this as_of in one batched pass. We read
	// back the JSONB payload, set rs_rank, recompute buy_readiness from the (now
	// known) RS plus the payload's own proximity/risk/base facts, and write both
	// back. Batched via a single pgx.Batch — never one query per symbol.
	updated, err := applyRSRanks(ctx, pool, asOf, ranks)
	if err != nil {
		return 0, err
	}
	return updated, nil
}

// applyRSRanks reads the as_of SEPA rows for the ranked symbols, recomputes
// rs_rank + buy_readiness in their JSONB, and writes them back in a single batch.
func applyRSRanks(ctx context.Context, pool *pgxpool.Pool, asOf time.Time, ranks map[string]int) (int, error) {
	symbols := make([]string, 0, len(ranks))
	for s := range ranks {
		symbols = append(symbols, s)
	}
	rows, err := pool.Query(ctx, `
		SELECT symbol, intent
		  FROM tms.signal_intents
		 WHERE strategy_id = 'sepa' AND as_of = $1 AND symbol = ANY($2)`,
		asOf.UTC(), symbols)
	if err != nil {
		return 0, fmt.Errorf("eod: rs-rank read intents: %w", err)
	}

	type pending struct {
		symbol string
		intent map[string]any
	}
	var todo []pending
	for rows.Next() {
		var symbol string
		var payload map[string]any
		if err := rows.Scan(&symbol, &payload); err != nil {
			rows.Close()
			return 0, fmt.Errorf("eod: rs-rank scan intent: %w", err)
		}
		todo = append(todo, pending{symbol: symbol, intent: payload})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("eod: rs-rank read intents: %w", err)
	}

	batch := &pgx.Batch{}
	for _, p := range todo {
		rank := ranks[p.symbol]
		p.intent["rs_rank"] = rank
		// Recompute buy_readiness with the now-known RS (the streamed intent scored
		// it with RS unknown). Only when the trade-plan facts are present (forming/
		// buy rows carry them; no_setup/hold may not).
		if br, ok := recomputeReadiness(p.intent, rank); ok {
			p.intent["buy_readiness"] = br
		}
		body, merr := json.Marshal(p.intent)
		if merr != nil {
			return 0, fmt.Errorf("eod: rs-rank marshal %s: %w", p.symbol, merr)
		}
		batch.Queue(`
			UPDATE tms.signal_intents
			   SET intent = $3::jsonb
			 WHERE strategy_id = 'sepa' AND as_of = $1 AND symbol = $2`,
			asOf.UTC(), p.symbol, string(body))
	}

	br := pool.SendBatch(ctx, batch)
	defer br.Close()
	for range todo {
		if _, err := br.Exec(); err != nil {
			return 0, fmt.Errorf("eod: rs-rank update: %w", err)
		}
	}
	return len(todo), nil
}

// recomputeReadiness re-derives the buy_readiness composite from a JSONB intent
// payload now that the RS rank is known. Returns ok=false when the trade-plan
// facts (proximity/risk) are absent (e.g. a no_setup row), leaving readiness as is.
func recomputeReadiness(intent map[string]any, rsRank int) (float64, bool) {
	prox, ok1 := jsonFloat(intent["proximity_to_trigger_pct"])
	risk, ok2 := jsonFloat(intent["risk_pct"])
	if !ok1 || !ok2 {
		return 0, false
	}
	hasVCP := intent["base_depth_pct"] != nil
	depth, _ := jsonFloat(intent["base_depth_pct"])
	return indicators.BuyReadiness(indicators.BuyReadinessInputs{
		ProximityPct: prox,
		RSRank:       rsRank,
		HasVCP:       hasVCP,
		BaseDepthPct: depth,
		RiskPct:      risk,
	}), true
}

// jsonFloat coerces a decoded JSON number to float64 (json.Unmarshal into any
// yields float64). Returns ok=false for nil/non-number.
func jsonFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
