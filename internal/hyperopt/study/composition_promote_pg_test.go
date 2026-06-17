//go:build integration

package study_test

// composition_promote_pg_test.go exercises the composition-study DB round-trip
// (kind=composition, composition_id, search_config persisted + read back) and the
// IN-PLACE promotion (decision 3): a chosen composition trial OVERWRITES the
// target composition's risk_*/cash_pct + member weights, never touching param_sets.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
)

func seedComposition(t *testing.T, pool *pgxpool.Pool, c composition.Composition) {
	t.Helper()
	cs := composition.NewStore(pool)
	// Clean any prior run's row (truncate does not cover the composition tables).
	_ = cs.Delete(context.Background(), c.ID)
	if err := cs.Create(context.Background(), c); err != nil {
		t.Fatalf("seed composition: %v", err)
	}
	t.Cleanup(func() { _ = cs.Delete(context.Background(), c.ID) })
}

func seedCompositionStudy(t *testing.T, store *study.Store, ts, compID string) {
	t.Helper()
	now := time.Now().UTC()
	ranges := study.DefaultCompositionRanges()
	cfg := study.StudyConfig{
		Version:       1,
		StudyName:     "hyperopt-composition-" + ts,
		Strategy:      string(study.KindComposition),
		Kind:          study.KindComposition,
		CompositionID: compID,
		SearchConfig:  &ranges,
		Start:         "2023-01-02",
		End:           "2023-12-29",
		Directions:    []string{"maximize", "maximize"},
		Objectives:    []string{"sharpe", "calmar"},
		Seed:          42,
		NTrials:       4,
		Workers:       1,
		WalkForward:   study.WalkForward{Enabled: true, Folds: 2, EmbargoDays: 5},
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	prog := study.Progress{Status: study.StatusRunning, TotalTrials: 4, Workers: 1, StartedAt: &now, UpdatedAt: &now}
	if err := store.UpsertStudy(context.Background(), cfg, prog); err != nil {
		t.Fatalf("UpsertStudy(composition): %v", err)
	}
}

func TestCompositionStudyRoundTripAndPromoteInPlace(t *testing.T) {
	pool := requirePG(t)
	truncate(t, pool)
	ctx := context.Background()
	store := study.NewStore(pool)

	comp := composition.Composition{
		ID:      "tune-me",
		Name:    "Tune Me",
		CashPct: 0.10,
		Risk:    composition.Risk{SingleNamePct: 0.50, ConcentrationPct: 0.40, DailyLossHaltPct: 0.10},
		Members: []composition.Member{
			{StrategyID: composition.StrategySectorRotation, Weight: 0.50, Active: true},
			{StrategyID: composition.StrategyPairs, Weight: 0.40, Active: true},
		},
		Version: 1,
	}
	seedComposition(t, pool, comp)

	ts := "2026-03-04_05-06-07"
	seedCompositionStudy(t, store, ts, comp.ID)

	// Study round-trips with kind=composition + composition_id.
	got, err := store.Get(ctx, ts)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != study.KindComposition {
		t.Fatalf("kind = %q, want composition", got.Kind)
	}
	if got.CompositionID != comp.ID {
		t.Fatalf("composition_id = %q, want %q", got.CompositionID, comp.ID)
	}

	// A COMPLETE composition trial: the recorded normalized blueprint values.
	now := time.Now().UTC()
	fin := now.Add(time.Second)
	trial := study.TrialArtifact{
		Number:   2,
		Strategy: string(study.KindComposition),
		Params: map[string]any{
			"cash_pct":            0.20,
			"single_name_pct":     0.35,
			"concentration_pct":   0.45,
			"daily_loss_halt_pct": 0.06,
			"weights": map[string]any{
				composition.StrategySectorRotation: 0.30,
				composition.StrategyPairs:          0.50,
			},
		},
		State:      study.TrialComplete,
		StartedAt:  now,
		FinishedAt: &fin,
		DurationS:  1.0,
	}
	trial.Metrics.Sharpe = 1.2
	trial.Metrics.Calmar = 1.8
	if err := store.UpsertTrial(ctx, ts, trial); err != nil {
		t.Fatalf("UpsertTrial: %v", err)
	}

	promoter := study.NewPromoter(pool)
	res, err := promoter.PromoteComposition(ctx, study.PromoteCompositionInput{
		CompositionID: comp.ID,
		StudyTS:       ts,
		TrialNumber:   2,
		PromotedBy:    "tester",
	})
	if err != nil {
		t.Fatalf("PromoteComposition: %v", err)
	}
	if res.Version != comp.Version+1 {
		t.Errorf("version bump = %d, want %d", res.Version, comp.Version+1)
	}

	// The composition was OVERWRITTEN in place.
	after, err := composition.NewStore(pool).Get(ctx, comp.ID)
	if err != nil {
		t.Fatalf("read-back composition: %v", err)
	}
	if after.CashPct != 0.20 {
		t.Errorf("cash_pct = %v, want 0.20", after.CashPct)
	}
	if after.Risk.SingleNamePct != 0.35 || after.Risk.ConcentrationPct != 0.45 || after.Risk.DailyLossHaltPct != 0.06 {
		t.Errorf("risk = %+v, want 0.35/0.45/0.06", after.Risk)
	}
	byID := map[string]composition.Member{}
	for _, m := range after.Members {
		byID[m.StrategyID] = m
	}
	if byID[composition.StrategySectorRotation].Weight != 0.30 {
		t.Errorf("sector weight = %v, want 0.30", byID[composition.StrategySectorRotation].Weight)
	}
	if byID[composition.StrategyPairs].Weight != 0.50 {
		t.Errorf("pairs weight = %v, want 0.50", byID[composition.StrategyPairs].Weight)
	}

	// Wrong composition id for this study => not promotable.
	if _, err := promoter.PromoteComposition(ctx, study.PromoteCompositionInput{
		CompositionID: "some-other", StudyTS: ts, TrialNumber: 2, PromotedBy: "tester",
	}); err == nil {
		t.Error("expected ErrTrialNotPromotable for a mismatched composition id")
	}
}
