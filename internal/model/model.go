// Package model is the Model domain: a named, persistable portfolio blueprint
// (which strategies, each weight + param reference + on/off, a cash reserve, and
// composite portfolio-level risk). It is the single source of truth the engine
// drops in for backtest / optimize / paper / live — replacing the weights and
// risk that used to be hardcoded constants scattered across the assembly path
// (docs/concept-alignment.md §0, §1.2).
package model

import (
	"errors"
	"fmt"
)

// Canonical strategy ids — the only values a Member.StrategyID may take, kept in
// lockstep with the tms.model_members CHECK and internal/params/loader.go.
const (
	StrategySEPA             = "sepa"
	StrategySectorRotation   = "sector_rotation"
	StrategyPairs            = "pairs"
	StrategyIntradayBreakout = "intraday_breakout"
)

// weightEpsilon absorbs float rounding when checking Σ(weights) + cash <= 1.
const weightEpsilon = 1e-9

// ErrNotFound is returned by the Store when no Model has the requested id.
var ErrNotFound = errors.New("model: not found")

// Model is a named portfolio blueprint. Members + CashPct must satisfy
// Σ(active weights) + CashPct <= 1; the leftover (if any) is unallocated.
type Model struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	CashPct     float64  `json:"cash_pct"`
	Risk        Risk     `json:"risk"`
	Members     []Member `json:"members"`
	Version     int      `json:"version"`
}

// Member is one strategy's slot in a Model: its capital weight, on/off flag and
// the params it runs with (ParamSetID nil = the strategy's active params).
type Member struct {
	StrategyID string  `json:"strategy_id"`
	Weight     float64 `json:"weight"`
	Active     bool    `json:"active"`
	ParamSetID *int64  `json:"param_set_id,omitempty"`
}

// Risk is the composite, portfolio-level risk of a Model. The three *Pct caps
// are required fractions in (0,1]; MaxGrossPct / MaxPositions are optional.
type Risk struct {
	SingleNamePct    float64  `json:"single_name_pct"`
	ConcentrationPct float64  `json:"concentration_pct"`
	DailyLossHaltPct float64  `json:"daily_loss_halt_pct"`
	MaxGrossPct      *float64 `json:"max_gross_pct,omitempty"`
	MaxPositions     *int     `json:"max_positions,omitempty"`
}

// canonicalStrategy reports whether id is one of the four canonical strategy ids.
func canonicalStrategy(id string) bool {
	switch id {
	case StrategySEPA, StrategySectorRotation, StrategyPairs, StrategyIntradayBreakout:
		return true
	default:
		return false
	}
}

// Validate checks the Model is internally consistent before persistence/use:
// non-empty id/name; cash_pct in [0,1); each member a canonical strategy with
// weight in (0,1] and no duplicates; required risk fractions in (0,1]; and the
// active capital budget (Σ active weights + cash_pct) within 1.0 (mirrors the
// DB CHECKs, with the cross-row budget the schema can't express per row).
func (m Model) Validate() error {
	if m.ID == "" {
		return errors.New("model: id must be non-empty")
	}
	if m.Name == "" {
		return fmt.Errorf("model %q: name must be non-empty", m.ID)
	}
	if m.CashPct < 0 || m.CashPct >= 1 {
		return fmt.Errorf("model %q: cash_pct %v out of range [0,1)", m.ID, m.CashPct)
	}
	if err := m.Risk.validate(m.ID); err != nil {
		return err
	}
	if len(m.Members) == 0 {
		return fmt.Errorf("model %q: must have at least one member", m.ID)
	}

	seen := make(map[string]bool, len(m.Members))
	activeSum := m.CashPct
	for _, mem := range m.Members {
		if !canonicalStrategy(mem.StrategyID) {
			return fmt.Errorf("model %q: unknown strategy_id %q", m.ID, mem.StrategyID)
		}
		if seen[mem.StrategyID] {
			return fmt.Errorf("model %q: duplicate strategy_id %q", m.ID, mem.StrategyID)
		}
		seen[mem.StrategyID] = true
		if mem.Weight <= 0 || mem.Weight > 1 {
			return fmt.Errorf("model %q: member %q weight %v out of range (0,1]", m.ID, mem.StrategyID, mem.Weight)
		}
		if mem.Active {
			activeSum += mem.Weight
		}
	}
	if activeSum > 1.0+weightEpsilon {
		return fmt.Errorf("model %q: Σ active weights + cash_pct = %v exceeds 1.0", m.ID, activeSum)
	}
	return nil
}

// validate checks the required risk fractions are in (0,1] and the optional
// caps, when present, are positive.
func (r Risk) validate(modelID string) error {
	for name, v := range map[string]float64{
		"risk_single_name_pct":     r.SingleNamePct,
		"risk_concentration_pct":   r.ConcentrationPct,
		"risk_daily_loss_halt_pct": r.DailyLossHaltPct,
	} {
		if v <= 0 || v > 1 {
			return fmt.Errorf("model %q: %s %v out of range (0,1]", modelID, name, v)
		}
	}
	if r.MaxGrossPct != nil && *r.MaxGrossPct <= 0 {
		return fmt.Errorf("model %q: risk_max_gross_pct %v must be > 0", modelID, *r.MaxGrossPct)
	}
	if r.MaxPositions != nil && *r.MaxPositions <= 0 {
		return fmt.Errorf("model %q: risk_max_positions %d must be > 0", modelID, *r.MaxPositions)
	}
	return nil
}

// SeedModels returns the five backward-compatible seed Models — the single Go
// source of truth, kept byte-for-byte in step with the INSERTs in
// migrations/000015_models.up.sql:
//
//	default-multi ← legacy strategy=multi (SEPA/Sector/Pairs blend)
//	{sepa,sector,pairs,orb}-only ← legacy single-strategy dispatch
//
// All members use ParamSetID nil (the strategy's active params).
// Seed returns the seed Model with the given id (one of default-multi /
// {sepa,sector,pairs,orb}-only) from SeedModels, or ErrNotFound. It is the
// in-process resolver for paths that map a legacy strategy selector to its seed
// Model without reaching for a model.Store / DB pool (the assembly callers).
func Seed(id string) (Model, error) {
	for _, m := range SeedModels() {
		if m.ID == id {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("%w: seed model %q", ErrNotFound, id)
}

func SeedModels() []Model {
	return []Model{
		{
			ID:          "default-multi",
			Name:        "Default Multi-Strategy",
			Description: "SEPA + Sector Rotation + Pairs blend (legacy strategy=multi).",
			CashPct:     0.10,
			Risk:        Risk{SingleNamePct: 0.50, ConcentrationPct: 0.40, DailyLossHaltPct: 0.10},
			Members: []Member{
				{StrategyID: StrategySEPA, Weight: 0.40, Active: true},
				{StrategyID: StrategySectorRotation, Weight: 0.30, Active: true},
				{StrategyID: StrategyPairs, Weight: 0.20, Active: true},
			},
			Version: 1,
		},
		{
			ID:          "sepa-only",
			Name:        "SEPA Only",
			Description: "Single-member Model: SEPA (legacy strategy=sepa).",
			CashPct:     0.00,
			Risk:        Risk{SingleNamePct: 0.20, ConcentrationPct: 0.30, DailyLossHaltPct: 0.05},
			Members:     []Member{{StrategyID: StrategySEPA, Weight: 1.00, Active: true}},
			Version:     1,
		},
		{
			ID:          "sector-only",
			Name:        "Sector Rotation Only",
			Description: "Single-member Model: Sector Rotation (legacy strategy=sector_rotation).",
			CashPct:     0.00,
			Risk:        Risk{SingleNamePct: 0.50, ConcentrationPct: 0.40, DailyLossHaltPct: 0.10},
			Members:     []Member{{StrategyID: StrategySectorRotation, Weight: 1.00, Active: true}},
			Version:     1,
		},
		{
			ID:          "pairs-only",
			Name:        "Pairs Only",
			Description: "Single-member Model: Pairs (legacy strategy=pairs).",
			CashPct:     0.00,
			Risk:        Risk{SingleNamePct: 0.20, ConcentrationPct: 0.30, DailyLossHaltPct: 0.05},
			Members:     []Member{{StrategyID: StrategyPairs, Weight: 1.00, Active: true}},
			Version:     1,
		},
		{
			ID:          "orb-only",
			Name:        "Intraday ORB Only",
			Description: "Single-member Model: Intraday Breakout (legacy strategy=intraday_breakout).",
			CashPct:     0.00,
			Risk:        Risk{SingleNamePct: 0.20, ConcentrationPct: 0.30, DailyLossHaltPct: 0.05},
			Members:     []Member{{StrategyID: StrategyIntradayBreakout, Weight: 1.00, Active: true}},
			Version:     1,
		},
	}
}
