package handlers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/sharadar"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

// SharadarAPISyncer adapts the Nasdaq Data Link incremental sync engine
// (internal/data/sharadar.Syncer) to the APISyncer seam consumed by
// DataRefresh for source="api". It is the production entry point the
// data-sync engine previously lacked: cmd/tms/worker.go builds one from
// config.NasdaqDataLinkAPIKey and injects it via NewDataRefresh.
//
// Routing (spec docs/spec/data-sharadar.md §8 vs §9): source="api" maps to
// the *catchup* flow (Syncer.EnsureFresh — the relational counterpart of
// ensure_cache_fresh): it derives its own window from the SEP watermark and
// refreshes all five datasets holistically through T-1. Catchup never
// auto-bootstraps and, by its spec contract, cannot be scoped to a subset
// of tables/tickers or floored at an arbitrary "since" date.
//
// Therefore a source="api" job that carries Tables/Tickers/Since is a
// mis-routed bounded backfill: rather than silently ignore those fields
// (and leave the operator believing the sync was scoped), Sync fails fast
// and points at `tms sync bootstrap`, the explicit bounded-backfill entry
// point that does honor a date window and a ticker filter. A bare
// source="api" job (no scope fields) runs the catchup.
type SharadarAPISyncer struct {
	syncer catchupEngine
	log    zerolog.Logger
}

// catchupEngine is the slice of *sharadar.Syncer the adapter drives. The
// interface seam (satisfied by *sharadar.Syncer) lets the adapter's routing
// and result-rendering be unit-tested without a live client or database.
type catchupEngine interface {
	EnsureFresh(ctx context.Context) (*sharadar.CatchupReport, error)
}

// NewSharadarAPISyncer builds the adapter over a constructed Syncer. The
// Syncer carries the live client, the pgStore and the America/New_York
// calendar (P1 locked decision 2). A nil syncer is a programming error.
func NewSharadarAPISyncer(syncer *sharadar.Syncer, log zerolog.Logger) (*SharadarAPISyncer, error) {
	if syncer == nil {
		return nil, fmt.Errorf("handlers: nil sharadar syncer")
	}
	return &SharadarAPISyncer{
		syncer: syncer,
		log:    log.With().Str("component", "sharadar-api-sync").Logger(),
	}, nil
}

// Sync implements APISyncer: it runs the incremental catchup and reports
// the per-dataset outcome through report. Context cancellation (cooperative
// cancel / worker drain) propagates straight through EnsureFresh.
func (a *SharadarAPISyncer) Sync(ctx context.Context, req APISyncRequest, report jobs.ProgressFn) (any, error) {
	// Guard the catchup contract: catchup cannot honor a table/ticker
	// subset or a "since" floor (it is watermark-driven and whole-universe,
	// spec §8.2). Reject the mis-route loudly instead of pretending to
	// scope it.
	if scope := describeScope(req); scope != "" {
		return nil, fmt.Errorf(
			"data.refresh source=api runs the watermark-driven catchup (all datasets through T-1) and "+
				"cannot be scoped (%s); for a bounded/filtered backfill run "+
				"`tms sync bootstrap --start YYYY-MM-DD --end YYYY-MM-DD [--ticker T ...]`", scope)
	}

	a.log.Info().Msg("starting api catchup (ensure-fresh)")
	if report != nil {
		_ = report(ctx, map[string]any{"phase": "catchup", "state": "starting"})
	}

	rep, err := a.syncer.EnsureFresh(ctx)
	result := catchupResult(rep)
	if err != nil {
		// EnsureFresh returns a non-nil error only for cancellation or
		// store-level failures; the partial report still lands in the
		// final progress snapshot for diagnosis (failed jobs store no
		// result column).
		if report != nil {
			_ = report(ctx, map[string]any{"phase": "done", "summary": result})
		}
		return nil, fmt.Errorf("data.refresh: api catchup: %w", err)
	}

	if report != nil {
		_ = report(ctx, map[string]any{"phase": "done", "summary": result})
	}
	a.log.Info().Interface("summary", result).Msg("api catchup complete")
	return result, nil
}

// describeScope returns a human description of any scope fields set on the
// request, or "" when the request is a bare catchup.
func describeScope(req APISyncRequest) string {
	var parts []string
	if len(req.Tables) > 0 {
		parts = append(parts, fmt.Sprintf("tables=%v", req.Tables))
	}
	if len(req.Tickers) > 0 {
		parts = append(parts, fmt.Sprintf("%d ticker(s)", len(req.Tickers)))
	}
	if !req.Since.IsZero() {
		parts = append(parts, "since="+req.Since.Format("2006-01-02"))
	}
	return strings.Join(parts, ", ")
}

// catchupResult renders a *sharadar.CatchupReport as the job result object.
// A nil report (defensive) yields an empty-but-valid object.
func catchupResult(rep *sharadar.CatchupReport) map[string]any {
	out := map[string]any{
		"source":     "api",
		"flow":       "catchup",
		"finished":   time.Now().UTC().Format(time.RFC3339),
		"did_work":   false,
		"rows_added": map[string]int64{},
		"errors":     []string{},
	}
	if rep == nil {
		return out
	}
	out["did_work"] = rep.DidWork()
	out["days_attempted"] = rep.DaysAttempted
	out["days_succeeded"] = rep.DaysSucceeded
	if rep.SkippedReason != "" {
		out["skipped_reason"] = rep.SkippedReason
	}
	if rep.RowsAdded != nil {
		out["rows_added"] = rep.RowsAdded
	}
	if len(rep.Errors) > 0 {
		out["errors"] = rep.Errors
	}
	return out
}
