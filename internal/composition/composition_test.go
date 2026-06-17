package composition

import "testing"

// TestSeedCompositionsValidate guards the single-source-of-truth seeds against
// drift: every seed Composition must pass Validate (and so must match the DB
// CHECKs).
func TestSeedCompositionsValidate(t *testing.T) {
	seeds := SeedCompositions()
	if len(seeds) != 5 {
		t.Fatalf("SeedCompositions() = %d compositions, want 5", len(seeds))
	}
	for _, m := range seeds {
		if err := m.Validate(); err != nil {
			t.Errorf("seed %q failed Validate: %v", m.ID, err)
		}
	}
}

func TestCompositionValidate(t *testing.T) {
	base := func() Composition {
		return Composition{
			ID:      "m1",
			Name:    "M1",
			CashPct: 0.10,
			Risk:    Risk{SingleNamePct: 0.5, ConcentrationPct: 0.4, DailyLossHaltPct: 0.1},
			Members: []Member{{StrategyID: StrategySEPA, Weight: 0.4, Active: true}},
			Version: 1,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Composition)
		wantErr bool
	}{
		{"valid", func(*Composition) {}, false},
		{"empty id", func(m *Composition) { m.ID = "" }, true},
		{"empty name", func(m *Composition) { m.Name = "" }, true},
		{"cash 1.0", func(m *Composition) { m.CashPct = 1.0 }, true},
		{"cash negative", func(m *Composition) { m.CashPct = -0.1 }, true},
		{"no members", func(m *Composition) { m.Members = nil }, true},
		{"unknown strategy", func(m *Composition) { m.Members[0].StrategyID = "nope" }, true},
		{"weight 0", func(m *Composition) { m.Members[0].Weight = 0 }, true},
		{"weight >1", func(m *Composition) { m.Members[0].Weight = 1.5 }, true},
		{"risk 0", func(m *Composition) { m.Risk.SingleNamePct = 0 }, true},
		{"risk >1", func(m *Composition) { m.Risk.ConcentrationPct = 1.1 }, true},
		{"max gross 0", func(m *Composition) { v := 0.0; m.Risk.MaxGrossPct = &v }, true},
		{"max positions 0", func(m *Composition) { v := 0; m.Risk.MaxPositions = &v }, true},
		{"duplicate strategy", func(m *Composition) {
			m.Members = append(m.Members, Member{StrategyID: StrategySEPA, Weight: 0.2, Active: true})
		}, true},
		{"budget over 1", func(m *Composition) {
			m.CashPct = 0.5
			m.Members = []Member{
				{StrategyID: StrategySEPA, Weight: 0.4, Active: true},
				{StrategyID: StrategyPairs, Weight: 0.4, Active: true},
			}
		}, true},
		{"inactive over budget ok", func(m *Composition) {
			m.CashPct = 0.5
			m.Members = []Member{
				{StrategyID: StrategySEPA, Weight: 0.4, Active: true},
				{StrategyID: StrategyPairs, Weight: 0.9, Active: false},
			}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base()
			tt.mutate(&m)
			err := m.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
