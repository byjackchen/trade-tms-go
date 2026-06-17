package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

// KindUniverseRebuild is the dispatch key served by UniverseRebuild.
const KindUniverseRebuild = "universe.rebuild"

// UniverseRebuild handles "universe.rebuild" jobs: it recomputes the SEPA
// universe (internal/data/universe Builder — NY as-of date, 730-day warmup
// window, exclusions, optional top-N market-cap cap, screener ranking) and
// persists the result as an append-only tms.universe_snapshots row.
//
// Payload (JSON object; unknown fields rejected):
//
//	{
//	  "kind":     "live"|"eod"|"backtest"|"manual",  // optional; default "manual"
//	  "limit":    85,        // optional top-N cap; omitted/null = env default
//	                         //   (TMS_LIVE_UNIVERSE_LIMIT, fallback 85)
//	  "uncapped": false,     // optional; true skips the cap (full backtest universe)
//	  "top_k":    0          // optional ranked-candidate bound; <=0 = all
//	}
//
// Re-runs are safe: snapshots are append-only audit records, the latest
// one wins for readers (LatestSnapshot orders by as_of DESC, id DESC).
type UniverseRebuild struct {
	pool *pgxpool.Pool
	cal  *calendar.Calendar
	log  zerolog.Logger
}

// NewUniverseRebuild builds the handler.
func NewUniverseRebuild(pool *pgxpool.Pool, cal *calendar.Calendar, log zerolog.Logger) (*UniverseRebuild, error) {
	if pool == nil {
		return nil, errors.New("universe.rebuild: nil connection pool")
	}
	if cal == nil {
		return nil, errors.New("universe.rebuild: nil trading calendar")
	}
	return &UniverseRebuild{
		pool: pool,
		cal:  cal,
		log:  log.With().Str("component", "universe-rebuild").Logger(),
	}, nil
}

// Kind implements jobs.Handler.
func (h *UniverseRebuild) Kind() string { return KindUniverseRebuild }

// universeRebuildParams is the payload wire shape. Limit is a pointer so
// "absent" (use the env default) is distinguishable from an explicit 0
// (which yields an empty universe).
type universeRebuildParams struct {
	Kind     string `json:"kind"`
	Limit    *int   `json:"limit"`
	Uncapped bool   `json:"uncapped"`
	TopK     int    `json:"top_k"`
}

// parseUniverseRebuildParams validates the payload strictly; bad input
// fails the job immediately instead of half-running.
func parseUniverseRebuildParams(payload json.RawMessage) (universeRebuildParams, error) {
	var p universeRebuildParams
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("universe.rebuild: invalid payload: %w", err)
	}
	switch p.Kind {
	case "":
		p.Kind = universe.KindManual
	case universe.KindLive, universe.KindEOD, universe.KindBacktest, universe.KindManual:
	default:
		return p, fmt.Errorf("universe.rebuild: unknown kind %q (want live|eod|backtest|manual)", p.Kind)
	}
	return p, nil
}

// Run implements jobs.Handler.
func (h *UniverseRebuild) Run(ctx context.Context, job *jobs.Job, report jobs.ProgressFn) (any, error) {
	p, err := parseUniverseRebuildParams(job.Payload)
	if err != nil {
		return nil, err
	}
	log := h.log.With().Int64("job_id", job.ID).Str("kind", p.Kind).Logger()

	limit := 0
	if !p.Uncapped {
		if p.Limit != nil {
			limit = *p.Limit
		} else {
			limit, err = universe.ResolveUniverseLimit(nil)
			if err != nil {
				return nil, fmt.Errorf("universe.rebuild: %w", err)
			}
		}
	}

	if rerr := report(ctx, map[string]any{
		"phase": "build", "kind": p.Kind, "limit": limit, "uncapped": p.Uncapped,
	}); rerr != nil && ctx.Err() == nil {
		log.Warn().Err(rerr).Msg("progress report failed; continuing rebuild")
	}

	store := universe.NewStore(h.pool)
	builder := universe.NewBuilder(store, h.cal, log)
	res, err := builder.Build(ctx, universe.BuildParams{
		Limit:    limit,
		Uncapped: p.Uncapped,
		Kind:     p.Kind,
		TopK:     p.TopK,
	})
	if err != nil {
		return nil, fmt.Errorf("universe.rebuild: %w", err)
	}

	snap := universe.SnapshotFromResult(res)
	if err := store.InsertSnapshot(ctx, snap); err != nil {
		return nil, fmt.Errorf("universe.rebuild: %w", err)
	}

	if rerr := report(ctx, map[string]any{
		"phase": "done", "snapshot_id": snap.ID, "tickers": len(snap.Tickers),
	}); rerr != nil && ctx.Err() == nil {
		log.Warn().Err(rerr).Msg("progress report failed; rebuild already persisted")
	}
	log.Info().Int64("snapshot_id", snap.ID).Str("as_of", res.AsOf.String()).
		Int("tickers", len(snap.Tickers)).Int("warmed", res.Warmed).
		Int("warmup_errors", len(res.WarmupErrors)).Msg("universe snapshot persisted")

	return map[string]any{
		"snapshot_id":   snap.ID,
		"as_of":         res.AsOf.String(),
		"kind":          p.Kind,
		"limit":         res.Limit,
		"uncapped":      p.Uncapped,
		"tickers":       len(snap.Tickers),
		"excluded":      len(snap.Excluded),
		"candidates":    len(snap.Members),
		"warmed":        res.Warmed,
		"warmup_errors": len(res.WarmupErrors),
	}, nil
}
