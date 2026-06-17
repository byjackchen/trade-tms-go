package api

// handlers_compositions_hyperopt.go serves the Composition-level hyperopt control
// plane — tuning a Composition's CAPITAL shape (member weights + cash + composite
// risk) while every member's SIGNAL params stay FIXED (docs/concept-alignment.md
// §1.2, decision 4):
//
//	POST /api/v1/compositions/{id}/hyperopt                       enqueue a composition study
//	POST /api/v1/compositions/{id}/hyperopt/{study_ts}/promote    promote a trial IN PLACE
//
// The study/trials are read through the EXISTING GET /api/v1/hyperopt[/{id}/trials]
// (they now carry kind=composition). Enqueue resolves the target Composition from
// its id, packs kind=composition + the resolved blueprint + the standard hyperopt
// knobs + optional BE-Space range overrides (decision 2) into the hyperopt.run
// payload, and dispatches it like the per-strategy /hyperopt enqueue. Promote
// OVERWRITES tms.compositions.risk_*/cash_pct + composition_members.weight from the
// trial's decoded normalized values (decision 3); it never touches param_sets.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// ---------------------------------------------------------------------------
// POST /api/v1/compositions/{id}/hyperopt
// ---------------------------------------------------------------------------

// compositionRangePair is a [low, high] range override for one BE-Space dimension
// (decision 2: global defaults, overridable per launch). nil => the global default.
type compositionRangePair = *[2]float64

// compositionHyperoptRequest is the launch body: optional BE-Space range overrides
// + the standard hyperopt knobs. The window (start/end) is required; everything
// else defaults.
type compositionHyperoptRequest struct {
	Start string `json:"start"`
	End   string `json:"end"`

	// Optional BE-Space range overrides (decision 2). Each is a [low, high] pair.
	Weight        compositionRangePair `json:"weight"`
	Cash          compositionRangePair `json:"cash"`
	SingleName    compositionRangePair `json:"single_name"`
	Concentration compositionRangePair `json:"concentration"`
	DailyLoss     compositionRangePair `json:"daily_loss"`

	Population      int      `json:"population"`
	Generations     int      `json:"generations"`
	Seed            int64    `json:"seed"`
	Workers         int      `json:"workers"`
	WalkForward     *bool    `json:"walk_forward"`
	Folds           int      `json:"folds"`
	EmbargoDays     int      `json:"embargo_days"`
	Tickers         []string `json:"tickers"`
	Universe        map[string]any `json:"universe"`
	StartingBalance *float64 `json:"starting_balance"`
	StudyTS         string   `json:"study_ts"`
	TrialTimeoutSec *int     `json:"trial_timeout_sec"`

	Actor       string `json:"actor"`
	MaxAttempts int32  `json:"max_attempts"`
	DedupeKey   string `json:"dedupe_key"`
}

func (s *Server) handleCompositionHyperoptEnqueue(w http.ResponseWriter, r *http.Request) {
	if s.compositions == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "composition store not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	comp, err := s.compositions.Get(r.Context(), id)
	if errors.Is(err, composition.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("composition %q not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "composition hyperopt: get composition", err)
		return
	}

	var req compositionHyperoptRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	if strings.TrimSpace(req.Start) == "" || strings.TrimSpace(req.End) == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, `"start" and "end" are required (YYYY-MM-DD)`)
		return
	}
	if req.Generations < 0 || req.Population < 0 || req.Folds < 0 || req.EmbargoDays < 0 {
		writeError(w, http.StatusBadRequest, CodeValidation, "population/generations/folds/embargo_days must be >= 0")
		return
	}
	if req.MaxAttempts < 0 || req.MaxAttempts > 10 {
		writeError(w, http.StatusBadRequest, CodeValidation, "max_attempts must be in [0, 10] (0 = default 1)")
		return
	}
	// A composition with an ACTIVE SEPA member needs a stock universe to trade.
	if compositionHasActiveSEPA(*comp) && len(req.Tickers) == 0 && req.Universe == nil {
		writeError(w, http.StatusBadRequest, CodeValidation,
			"composition has an active SEPA member and requires a stock universe (\"tickers\" or \"universe\")")
		return
	}

	payload := map[string]any{
		"kind":        string(study.KindComposition),
		"composition_id": comp.ID,
		"composition": comp,
		"start":       req.Start,
		"end":         req.End,
	}
	if ranges := compositionRangesPayload(req); ranges != nil {
		payload["ranges"] = ranges
	}
	if req.Population != 0 {
		payload["population"] = req.Population
	}
	if req.Generations != 0 {
		payload["generations"] = req.Generations
	}
	if req.Seed != 0 {
		payload["seed"] = req.Seed
	}
	if req.Workers != 0 {
		payload["workers"] = req.Workers
	}
	if req.WalkForward != nil {
		payload["walk_forward"] = *req.WalkForward
	}
	if req.Folds != 0 {
		payload["folds"] = req.Folds
	}
	if req.EmbargoDays != 0 {
		payload["embargo_days"] = req.EmbargoDays
	}
	if len(req.Tickers) > 0 {
		payload["tickers"] = req.Tickers
	}
	if req.Universe != nil {
		payload["universe"] = req.Universe
	}
	if req.StartingBalance != nil {
		payload["starting_balance"] = *req.StartingBalance
	}
	if req.StudyTS != "" {
		payload["study_ts"] = req.StudyTS
	}
	if req.TrialTimeoutSec != nil {
		if *req.TrialTimeoutSec < 0 {
			writeError(w, http.StatusBadRequest, CodeValidation, "trial_timeout_sec must be >= 0 (0 disables)")
			return
		}
		payload["trial_timeout_sec"] = *req.TrialTimeoutSec
	}

	job, deduped, err := s.jobs.Enqueue(r.Context(), jobs.EnqueueParams{
		Kind:        handlers.KindHyperoptRun,
		Payload:     payload,
		DedupeKey:   strings.TrimSpace(req.DedupeKey),
		MaxAttempts: req.MaxAttempts,
		Actor:       actorOrDefault(req.Actor),
	})
	if err != nil {
		internalError(w, s.log, "enqueue composition hyperopt.run", err)
		return
	}
	// Audit the launch alongside the other /compositions mutations.
	s.auditComposition(r, actorOrDefault(req.Actor), "composition.hyperopt", comp.ID, map[string]any{
		"job_id": job.ID, "start": req.Start, "end": req.End,
	})
	s.log.Info().Int64("job_id", job.ID).Str("composition_id", comp.ID).Bool("deduped", deduped).
		Str("actor", actorOrDefault(req.Actor)).Msg("composition hyperopt.run enqueued")
	writeJSON(w, http.StatusAccepted, map[string]any{"job": jobToJSON(job), "deduped": deduped})
}

// ---------------------------------------------------------------------------
// POST /api/v1/compositions/{id}/hyperopt/{study_ts}/promote
// ---------------------------------------------------------------------------

type compositionPromoteRequest struct {
	TrialID *int   `json:"trial_id"`
	Actor   string `json:"actor"`
}

func (s *Server) handleCompositionHyperoptPromote(w http.ResponseWriter, r *http.Request) {
	if s.promoter == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "hyperopt promoter not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	ts := chi.URLParam(r, "study_ts")
	if !validStudyTS(ts) {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("invalid study id %q (want UTC %%Y-%%m-%%d_%%H-%%M-%%S)", ts))
		return
	}
	var req compositionPromoteRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	if req.TrialID == nil || *req.TrialID < 0 {
		writeError(w, http.StatusBadRequest, CodeValidation, `"trial_id" is required (non-negative integer)`)
		return
	}
	promoted, err := s.promoter.PromoteComposition(r.Context(), study.PromoteCompositionInput{
		CompositionID: id,
		StudyTS:       ts,
		TrialNumber:   *req.TrialID,
		PromotedBy:    actorOrDefault(req.Actor),
	})
	switch {
	case errors.Is(err, study.ErrStudyNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("study %s not found", ts))
		return
	case errors.Is(err, study.ErrTrialNotPromotable):
		writeError(w, http.StatusUnprocessableEntity, CodeValidation, err.Error())
		return
	case err != nil:
		internalError(w, s.log, "composition hyperopt promote", err)
		return
	}
	s.auditComposition(r, actorOrDefault(req.Actor), "composition.hyperopt.promote", id, map[string]any{
		"study_ts": ts, "trial_id": *req.TrialID, "version": promoted.Version,
	})
	s.log.Info().Str("composition_id", id).Str("study_ts", ts).Int("trial", *req.TrialID).
		Str("actor", actorOrDefault(req.Actor)).Msg("composition hyperopt trial promoted in place")
	writeJSON(w, http.StatusOK, map[string]any{
		"composition_id": id,
		"study_ts":       ts,
		"trial_id":       *req.TrialID,
		"promoted": map[string]any{
			"cash_pct":            promoted.CashPct,
			"single_name_pct":     promoted.Risk.SingleNamePct,
			"concentration_pct":   promoted.Risk.ConcentrationPct,
			"daily_loss_halt_pct": promoted.Risk.DailyLossHaltPct,
			"weights":             promoted.Weights,
			"version":             promoted.Version,
		},
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// compositionHasActiveSEPA reports whether the composition has an ACTIVE SEPA
// member (the driver for requiring a stock universe at launch).
func compositionHasActiveSEPA(c composition.Composition) bool {
	for _, m := range c.Members {
		if m.Active && m.StrategyID == composition.StrategySEPA {
			return true
		}
	}
	return false
}

// compositionRangesPayload projects any BE-Space range overrides into the
// hyperopt.run payload's "ranges" object, or nil when none were supplied.
func compositionRangesPayload(req compositionHyperoptRequest) map[string]any {
	out := map[string]any{}
	if req.Weight != nil {
		out["weight"] = req.Weight
	}
	if req.Cash != nil {
		out["cash"] = req.Cash
	}
	if req.SingleName != nil {
		out["single_name"] = req.SingleName
	}
	if req.Concentration != nil {
		out["concentration"] = req.Concentration
	}
	if req.DailyLoss != nil {
		out["daily_loss"] = req.DailyLoss
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
