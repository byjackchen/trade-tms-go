package study

// promote.go is the promotion path (spec §8, locked decision 6: the UI/CLI
// one-click promotion that replaces Python's git-review gate). It writes a chosen
// trial's params into tms.active_params with full audit (promoted_by / promoted_at
// / source_trial / source_study), via an intermediate immutable tms.param_sets
// row carrying the tuned document (the §8.2 metadata-rewritten baseline). For a
// joint study every sub-strategy (sepa, sector_rotation, pairs) is promoted.
//
// The promotion is validated (the trial must exist, be COMPLETE, and belong to
// the study) and idempotent: re-promoting the same (study, trial, strategy)
// reuses the existing tuned param_set (matched by tuned_from_study/trial +
// payload) rather than versioning a new identical row, and the active_params
// upsert is naturally idempotent on the strategy PK. The effect is next-run-only
// (live processes read params at startup; §8.4).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
	"github.com/byjackchen/trade-tms-go/internal/params"
)

// ErrTrialNotPromotable is returned when the target trial is missing, not
// COMPLETE, or has no tunable params for the strategy.
var ErrTrialNotPromotable = errors.New("hyperopt: trial not promotable")

// ErrInvalidParams is returned when a trial's recorded params are out of the
// search range or fail the strategy validators — such a set MUST NOT be promoted
// to active_params (it would silently activate an invalid configuration). A
// malformed / manually-inserted trial row trips this gate; the normal in-bounds
// NSGA-II flow never does.
var ErrInvalidParams = errors.New("hyperopt: refusing to promote invalid params")

// PromoteInput parameterizes a promotion.
type PromoteInput struct {
	StudyTS     string    // study to promote from
	TrialNumber int       // artifact trial number to promote
	PromotedBy  string    // audit identity (required)
	Now         time.Time // promotion clock (UTC); zero => time.Now()
}

// Promoter performs promotions over a pgx pool.
type Promoter struct {
	pool  *pgxpool.Pool
	comps *composition.Store
	now   func() time.Time
}

// NewPromoter wraps a pool. It builds a composition.Store over the same pool for
// the IN-PLACE composition promotion path (decision 3).
func NewPromoter(pool *pgxpool.Pool) *Promoter {
	return &Promoter{pool: pool, comps: composition.NewStore(pool), now: time.Now}
}

// PromotedStrategy reports one strategy that was promoted (audit echo).
type PromotedStrategy struct {
	Strategy   string
	ParamSetID int64
	Version    int
}

// Promote writes the chosen trial's params to active_params (with audit) for the
// study's strategy (or all three sub-strategies for joint). It runs in one
// transaction; on any error nothing is committed. The returned slice lists each
// promoted strategy. PromotedBy is required.
func (p *Promoter) Promote(ctx context.Context, in PromoteInput) ([]PromotedStrategy, error) {
	if in.PromotedBy == "" {
		return nil, fmt.Errorf("hyperopt: promote requires promoted_by")
	}
	now := in.Now
	if now.IsZero() {
		now = p.now()
	}
	now = now.UTC()

	// Read the study strategy + study name.
	var strategy, studyName string
	err := p.pool.QueryRow(ctx,
		`SELECT strategy, study_name FROM tms.hyperopt_studies WHERE study_ts = $1`,
		in.StudyTS).Scan(&strategy, &studyName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("hyperopt: promote: read study: %w", err)
	}

	// Read the trial: must be COMPLETE; params are the OPTUNA-recorded values.
	var (
		state     string
		paramsRaw json.RawMessage
		optuna    *int
	)
	err = p.pool.QueryRow(ctx,
		`SELECT state, params, optuna_number FROM tms.hyperopt_trials WHERE study_ts = $1 AND number = $2`,
		in.StudyTS, in.TrialNumber).Scan(&state, &paramsRaw, &optuna)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: trial %d not found in study %s", ErrTrialNotPromotable, in.TrialNumber, in.StudyTS)
	}
	if err != nil {
		return nil, fmt.Errorf("hyperopt: promote: read trial: %w", err)
	}
	if TrialState(state) != TrialComplete {
		return nil, fmt.Errorf("%w: trial %d is %s (only COMPLETE trials promote)", ErrTrialNotPromotable, in.TrialNumber, state)
	}

	tuned, err := tunedPerStrategy(strategy, paramsRaw)
	if err != nil {
		return nil, err
	}
	if len(tuned) == 0 {
		return nil, fmt.Errorf("%w: trial %d has no tunable params", ErrTrialNotPromotable, in.TrialNumber)
	}

	sourceTrial := int64(in.TrialNumber)
	if optuna != nil {
		sourceTrial = int64(*optuna)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("hyperopt: promote: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var promoted []PromotedStrategy
	for _, sub := range orderedStrategies(strategy) {
		vals, ok := tuned[sub]
		if !ok || len(vals) == 0 {
			continue
		}
		// Promotion-safety gate: every tuned value must lie within its search
		// range AND the resulting merged document must parse through the strategy
		// validators. Refuse otherwise (a malformed / out-of-bounds row never
		// reaches active_params).
		if err := validateTunedForPromotion(sub, vals); err != nil {
			return nil, err
		}
		body, err := TuneBaseline(TuneInput{
			Strategy:    sub,
			Tuned:       vals,
			StudyName:   studyName,
			TrialNumber: int(sourceTrial),
			Now:         now,
		})
		if err != nil {
			return nil, fmt.Errorf("hyperopt: promote: tune %s: %w", sub, err)
		}
		psID, version, err := upsertParamSet(ctx, tx, sub, in.StudyTS, sourceTrial, body)
		if err != nil {
			return nil, err
		}
		if err := upsertActiveParams(ctx, tx, sub, psID, in.StudyTS, sourceTrial, in.PromotedBy, now); err != nil {
			return nil, err
		}
		promoted = append(promoted, PromotedStrategy{Strategy: sub, ParamSetID: psID, Version: version})
	}
	if len(promoted) == 0 {
		return nil, fmt.Errorf("%w: nothing to promote for %s", ErrTrialNotPromotable, strategy)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("hyperopt: promote: commit: %w", err)
	}
	return promoted, nil
}

// PromoteCompositionInput parameterizes an in-place composition promotion.
type PromoteCompositionInput struct {
	CompositionID string    // the composition the study targets (path id; must match the study)
	StudyTS       string    // composition study to promote from
	TrialNumber   int       // artifact trial number to promote
	PromotedBy    string    // audit identity (required)
	Now           time.Time // promotion clock (UTC); zero => time.Now()
}

// PromotedComposition echoes the in-place promotion result (audit): the new
// blueprint values written to tms.compositions / composition_members.
type PromotedComposition struct {
	CompositionID string
	CashPct       float64
	Risk          composition.Risk
	Weights       map[string]float64
	Version       int
}

// PromoteComposition OVERWRITES a composition's risk_* + cash_pct + each active
// member's weight IN PLACE from a chosen composition-study trial (decision 3). It
// reads the trial's decoded, simplex-normalized blueprint values (what the
// composition study recorded) and never touches param_sets. The trial must exist,
// be COMPLETE, and belong to a kind=composition study whose composition_id matches
// CompositionID. PromotedBy is required.
func (p *Promoter) PromoteComposition(ctx context.Context, in PromoteCompositionInput) (*PromotedComposition, error) {
	if in.PromotedBy == "" {
		return nil, fmt.Errorf("hyperopt: promote requires promoted_by")
	}

	// Read the study: must be kind=composition and target CompositionID.
	var (
		kind   string
		compID *string
	)
	err := p.pool.QueryRow(ctx,
		`SELECT kind, composition_id FROM tms.hyperopt_studies WHERE study_ts = $1`,
		in.StudyTS).Scan(&kind, &compID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("hyperopt: promote composition: read study: %w", err)
	}
	if StudyKind(kind) != KindComposition {
		return nil, fmt.Errorf("%w: study %s is kind %q, not composition", ErrTrialNotPromotable, in.StudyTS, kind)
	}
	if compID == nil || *compID != in.CompositionID {
		got := "<nil>"
		if compID != nil {
			got = *compID
		}
		return nil, fmt.Errorf("%w: study %s targets composition %q, not %q", ErrTrialNotPromotable, in.StudyTS, got, in.CompositionID)
	}

	// Read the trial: must be COMPLETE; params are the decoded normalized blueprint.
	var (
		state     string
		paramsRaw json.RawMessage
	)
	err = p.pool.QueryRow(ctx,
		`SELECT state, params FROM tms.hyperopt_trials WHERE study_ts = $1 AND number = $2`,
		in.StudyTS, in.TrialNumber).Scan(&state, &paramsRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: trial %d not found in study %s", ErrTrialNotPromotable, in.TrialNumber, in.StudyTS)
	}
	if err != nil {
		return nil, fmt.Errorf("hyperopt: promote composition: read trial: %w", err)
	}
	if TrialState(state) != TrialComplete {
		return nil, fmt.Errorf("%w: trial %d is %s (only COMPLETE trials promote)", ErrTrialNotPromotable, in.TrialNumber, state)
	}

	rw, err := decodeCompositionTrial(paramsRaw)
	if err != nil {
		return nil, err
	}
	if err := p.comps.UpdateRiskAndWeights(ctx, in.CompositionID, rw); errors.Is(err, composition.ErrNotFound) {
		return nil, fmt.Errorf("%w: composition %q not found", ErrTrialNotPromotable, in.CompositionID)
	} else if err != nil {
		return nil, fmt.Errorf("hyperopt: promote composition: update: %w", err)
	}

	// Read back the new version for the audit echo.
	updated, err := p.comps.Get(ctx, in.CompositionID)
	if err != nil {
		return nil, fmt.Errorf("hyperopt: promote composition: read-back: %w", err)
	}
	return &PromotedComposition{
		CompositionID: in.CompositionID,
		CashPct:       rw.CashPct,
		Risk:          rw.Risk,
		Weights:       rw.Weights,
		Version:       updated.Version,
	}, nil
}

// decodeCompositionTrial extracts the promotion payload (cash + risk caps + the
// per-member normalized weights) from a composition trial's recorded params JSON
// (the shape CompositionSpace.RecordedParams writes).
func decodeCompositionTrial(paramsRaw json.RawMessage) (composition.RiskWeights, error) {
	var rec struct {
		CashPct          float64            `json:"cash_pct"`
		SingleNamePct    float64            `json:"single_name_pct"`
		ConcentrationPct float64            `json:"concentration_pct"`
		DailyLossHaltPct float64            `json:"daily_loss_halt_pct"`
		Weights          map[string]float64 `json:"weights"`
	}
	if err := json.Unmarshal(paramsRaw, &rec); err != nil {
		return composition.RiskWeights{}, fmt.Errorf("%w: decode composition trial params: %v", ErrTrialNotPromotable, err)
	}
	if len(rec.Weights) == 0 {
		return composition.RiskWeights{}, fmt.Errorf("%w: composition trial has no member weights", ErrTrialNotPromotable)
	}
	return composition.RiskWeights{
		CashPct: rec.CashPct,
		Risk: composition.Risk{
			SingleNamePct:    rec.SingleNamePct,
			ConcentrationPct: rec.ConcentrationPct,
			DailyLossHaltPct: rec.DailyLossHaltPct,
		},
		Weights: rec.Weights,
	}, nil
}

// validateTunedForPromotion enforces the promotion-safety gate for one
// sub-strategy: (1) every tuned key must exist in the baseline parameters, and a
// key carrying a search range must have a value within [low, high]; (2) the
// merged document (baseline defaults overlaid with the tuned values) must parse
// through the strategy's typed validator (params.SEPAFromMap /
// SectorRotationFromMap / PairsFromMap). Any violation returns ErrInvalidParams.
func validateTunedForPromotion(sub string, tuned map[string]float64) error {
	sp, err := hyperopt.LoadBaselineParams(sub)
	if err != nil {
		return fmt.Errorf("hyperopt: promote: load baseline %s: %w", sub, err)
	}
	// (1) existence + range check.
	for name, v := range tuned {
		spec, ok := sp.Param(name)
		if !ok {
			return fmt.Errorf("%w: strategy %q has no param %q", ErrInvalidParams, sub, name)
		}
		if spec.Search != nil {
			if v < spec.Search.Low || v > spec.Search.High {
				return fmt.Errorf("%w: %s.%s = %g outside search range [%g, %g]",
					ErrInvalidParams, sub, name, v, spec.Search.Low, spec.Search.High)
			}
		}
	}
	// (2) merged-document parse through the strategy validator.
	defaults, err := hyperopt.DefaultsDict(sp)
	if err != nil {
		return fmt.Errorf("hyperopt: promote: defaults %s: %w", sub, err)
	}
	merged := make(map[string]any, len(defaults))
	for k, v := range defaults {
		merged[k] = v
	}
	for k, v := range tuned {
		merged[k] = v
	}
	switch sub {
	case "sepa":
		if _, err := params.SEPAFromMap(merged); err != nil {
			return fmt.Errorf("%w: %s params invalid: %v", ErrInvalidParams, sub, err)
		}
	case "sector_rotation":
		if _, err := params.SectorRotationFromMap(merged); err != nil {
			return fmt.Errorf("%w: %s params invalid: %v", ErrInvalidParams, sub, err)
		}
	case "pairs":
		if _, err := params.PairsFromMap(merged); err != nil {
			return fmt.Errorf("%w: %s params invalid: %v", ErrInvalidParams, sub, err)
		}
	default:
		return fmt.Errorf("%w: unknown sub-strategy %q", ErrInvalidParams, sub)
	}
	return nil
}

// orderedStrategies returns the sub-strategies a study strategy promotes: joint
// -> the three; otherwise the single one.
func orderedStrategies(strategy string) []string {
	if strategy == "joint" {
		return []string{"sepa", "sector_rotation", "pairs"}
	}
	return []string{strategy}
}

// tunedPerStrategy extracts the per-sub-strategy tuned param maps (name ->
// float64) from a trial's recorded params JSON. For a single strategy the params
// are flat; for joint they are nested under each sub-strategy key.
func tunedPerStrategy(strategy string, paramsRaw json.RawMessage) (map[string]map[string]float64, error) {
	out := map[string]map[string]float64{}
	if strategy == "joint" {
		var nested map[string]map[string]float64
		if err := json.Unmarshal(paramsRaw, &nested); err != nil {
			return nil, fmt.Errorf("hyperopt: promote: decode joint params: %w", err)
		}
		for _, sub := range []string{"sepa", "sector_rotation", "pairs"} {
			if m, ok := nested[sub]; ok && len(m) > 0 {
				out[sub] = m
			}
		}
		return out, nil
	}
	var flat map[string]float64
	if err := json.Unmarshal(paramsRaw, &flat); err != nil {
		return nil, fmt.Errorf("hyperopt: promote: decode params: %w", err)
	}
	if len(flat) > 0 {
		out[strategy] = flat
	}
	return out, nil
}

// upsertParamSet inserts (or reuses) an immutable tuned param_set for the
// strategy. Idempotency: an existing row with the same (strategy, tuned_from_study,
// tuned_from_trial) and identical payload is reused; otherwise a new version
// (max+1) is inserted. Returns the param_set id and version.
func upsertParamSet(ctx context.Context, tx pgx.Tx, strategy, studyTS string, sourceTrial int64, payload []byte) (int64, int, error) {
	// Reuse an identical tuned set from the same study/trial.
	var (
		id      int64
		version int
	)
	err := tx.QueryRow(ctx, `
		SELECT id, version FROM tms.param_sets
		WHERE strategy = $1 AND source = 'tuned'
		  AND tuned_from_study = $2 AND tuned_from_trial = $3
		  AND payload = $4::jsonb
		ORDER BY version DESC LIMIT 1`,
		strategy, studyTS, sourceTrial, string(payload)).Scan(&id, &version)
	if err == nil {
		return id, version, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("hyperopt: promote: lookup param_set: %w", err)
	}

	// Next version for this strategy.
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(max(version), 0) + 1 FROM tms.param_sets WHERE strategy = $1`,
		strategy).Scan(&version); err != nil {
		return 0, 0, fmt.Errorf("hyperopt: promote: next version: %w", err)
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO tms.param_sets
		    (strategy, version, schema_version, source, payload, tuned_from_study, tuned_from_trial)
		VALUES ($1, $2, 1, 'tuned', $3::jsonb, $4, $5)
		RETURNING id`,
		strategy, version, string(payload), studyTS, sourceTrial).Scan(&id)
	if err != nil {
		return 0, 0, fmt.Errorf("hyperopt: promote: insert param_set: %w", err)
	}
	return id, version, nil
}

// upsertActiveParams points active_params.<strategy> at the param_set with full
// audit (source_id "hyperopt:<ts>", promoted_by/at, source_study, source_trial).
func upsertActiveParams(ctx context.Context, tx pgx.Tx, strategy string, paramSetID int64, studyTS string, sourceTrial int64, promotedBy string, now time.Time) error {
	sourceID := "hyperopt:" + studyTS
	_, err := tx.Exec(ctx, `
		INSERT INTO tms.active_params
		    (strategy, param_set_id, source_id, promoted_by, promoted_at, source_study, source_trial)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (strategy) DO UPDATE SET
		    param_set_id = EXCLUDED.param_set_id,
		    source_id = EXCLUDED.source_id,
		    promoted_by = EXCLUDED.promoted_by,
		    promoted_at = EXCLUDED.promoted_at,
		    source_study = EXCLUDED.source_study,
		    source_trial = EXCLUDED.source_trial`,
		strategy, paramSetID, sourceID, promotedBy, now, studyTS, sourceTrial)
	if err != nil {
		return fmt.Errorf("hyperopt: promote: upsert active_params %s: %w", strategy, err)
	}
	return nil
}
