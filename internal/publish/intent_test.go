package publish

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orb"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepa"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepaadapter"
)

// TestNormalizeSEPAAdapterOutput is the regression guard for the round-3
// blocker: the SEPA adapter's EvaluateIntentJSON output must normalize through
// the REAL production path. The prior test only fed NormalizeIntent a
// hand-built sepa.SignalIntent — the type the broken adapter NEVER produced (it
// returned a private sepaadapter.intentJSON, which hit the default error case
// and aborted every SEPA/multi intent in the signal/paper/live/EOD modes). This
// drives the actual adapter so the bug cannot silently reappear.
func TestNormalizeSEPAAdapterOutput(t *testing.T) {
	gen, err := sepa.NewGenerator(sepa.Config{
		Symbol: "AAPL", EquityProvider: func() float64 { return 100000 },
		RiskPct: 1.0, MarketCapMinUSD: 5e8, HardStopPct: 7.5, PivotBufferPct: 1.5,
		BreakoutVolumeMultiple: 1.5, VCPLookback: 4, HistoryMaxBars: 1000,
		Timezone: "America/New_York",
	})
	require.NoError(t, err)
	adapter := sepaadapter.New("SEPARunner-000", gen)

	// The exact value the live signal node / EOD replay feeds the sink.
	payload := adapter.EvaluateIntentJSON(time.Date(2024, 1, 1, 21, 0, 0, 0, time.UTC))

	norms, err := NormalizeIntent(payload)
	require.NoError(t, err, "SEPA adapter output must normalize (five-modes-one-engine thesis)")
	require.Len(t, norms, 1)
	n := norms[0]
	assert.Equal(t, "sepa", n.StrategyID)
	assert.Equal(t, "AAPL", n.Symbol)

	body, err := n.IntentJSON()
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	// Full spec-faithful snake_case wire shape.
	for _, k := range []string{
		"symbol", "state", "strength", "proximity_to_trigger_pct", "updated_at",
		"generation", "strategy_id", "grade", "trend_template_pass", "base_age_days",
		"base_depth_pct", "volume_dryup", "pivot_price", "stop_price", "rs_rank",
	} {
		assert.Contains(t, m, k, "intent wire shape missing key %q", k)
	}
	assert.Equal(t, "sepa", m["strategy_id"])
}

// TestNormalizeSEPAWireShape proves a local sepa.SignalIntent (no json tags)
// normalizes to the spec-faithful snake_case domain wire shape with the head
// discriminator columns extracted.
func TestNormalizeSEPAWireShape(t *testing.T) {
	prox := 1.5
	in := sepa.SignalIntent{
		Symbol:              "AAPL",
		State:               sepa.StateBuy,
		Strength:            75,
		ProximityToTriggerP: &prox,
		UpdatedAt:           time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC),
		Generation:          7,
		StrategyID:          "sepa",
		Grade:               75,
		TrendTemplatePass:   true,
		PivotPrice:          "123.45",
		StopPrice:           "118.00",
	}
	norms, err := NormalizeIntent(in)
	require.NoError(t, err)
	require.Len(t, norms, 1)
	n := norms[0]

	assert.Equal(t, "sepa", n.StrategyID)
	assert.Equal(t, "AAPL", n.Symbol)
	assert.Equal(t, domain.StateBuy, n.State)
	assert.Equal(t, 75.0, n.Strength)
	require.NotNil(t, n.ProximityToTriggerPct)
	assert.Equal(t, 1.5, *n.ProximityToTriggerPct)
	assert.Equal(t, int64(7), n.Generation)

	body, err := n.IntentJSON()
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	// Spec field names (snake_case), NOT the local Go field names.
	assert.Equal(t, "sepa", m["strategy_id"])
	assert.Equal(t, "AAPL", m["symbol"])
	assert.Equal(t, "buy", m["state"])
	assert.Equal(t, float64(75), m["grade"])
	assert.Equal(t, true, m["trend_template_pass"])
	assert.Contains(t, m, "pivot_price")
	assert.Contains(t, m, "proximity_to_trigger_pct")
}

// TestNormalizeORBWireShape proves the local orb.SignalIntent normalizes too.
func TestNormalizeORBWireShape(t *testing.T) {
	in := orb.SignalIntent{
		Symbol:     "MSFT",
		State:      orb.StateNoSetup,
		Strength:   0,
		UpdatedAt:  time.Date(2026, 6, 12, 14, 30, 0, 0, time.UTC),
		Generation: 3,
		StrategyID: orb.StrategyID,
		ORBHigh:    "300.10",
		ORBLow:     "298.50",
	}
	norms, err := NormalizeIntent(in)
	require.NoError(t, err)
	require.Len(t, norms, 1)
	n := norms[0]
	assert.Equal(t, "intraday_breakout", n.StrategyID)
	assert.Equal(t, "MSFT", n.Symbol)
	assert.Equal(t, domain.StateNoSetup, n.State)

	body, err := n.IntentJSON()
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	assert.Equal(t, "intraday_breakout", m["strategy_id"])
	assert.Equal(t, "no_setup", m["state"])
	assert.Contains(t, m, "orb_high")
	assert.Contains(t, m, "orb_low")
	assert.Contains(t, m, "atr_at_open") // reserved, null
}

// TestNormalizePairsSlice proves a []domain.PairsSignalIntent fans out to one
// NormalizedIntent per leg, each addressed by its own symbol.
func TestNormalizePairsSlice(t *testing.T) {
	a := domain.NewPairsSignalIntent()
	a.Symbol = "KO"
	a.State = domain.StateHold
	a.PairID = "KO/PEP"
	b := domain.NewPairsSignalIntent()
	b.Symbol = "PEP"
	b.State = domain.StateHold
	b.PairID = "KO/PEP"

	norms, err := NormalizeIntent([]domain.PairsSignalIntent{a, b})
	require.NoError(t, err)
	require.Len(t, norms, 2)
	assert.Equal(t, "KO", norms[0].Symbol)
	assert.Equal(t, "PEP", norms[1].Symbol)
	for _, n := range norms {
		assert.Equal(t, "pairs", n.StrategyID)
		body, err := n.IntentJSON()
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		assert.Equal(t, "KO/PEP", m["pair_id"])
		assert.Equal(t, "pairs", m["strategy_id"])
	}
}

// TestNormalizeSectorSlice proves the sector slice path.
func TestNormalizeSectorSlice(t *testing.T) {
	it := domain.NewSectorRotationIntent()
	it.Symbol = "XLK"
	it.State = domain.StateBuy
	it.Rank = 1
	it.MomentumScore = 0.12

	norms, err := NormalizeIntent([]domain.SectorRotationIntent{it})
	require.NoError(t, err)
	require.Len(t, norms, 1)
	assert.Equal(t, "XLK", norms[0].Symbol)
	assert.Equal(t, "sector_rotation", norms[0].StrategyID)
	body, err := norms[0].IntentJSON()
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(body, &m))
	assert.Equal(t, float64(1), m["rank"])
	assert.Equal(t, "sector_rotation", m["strategy_id"])
}

// TestNormalizeUnknownTypeErrors proves an unregistered intent type fails loudly.
func TestNormalizeUnknownTypeErrors(t *testing.T) {
	_, err := NormalizeIntent(struct{ X int }{X: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported intent type")
}

// TestStreamKeyShape pins the reference per-trader stream key shape.
func TestStreamKeyShape(t *testing.T) {
	assert.Equal(t, "trader-SIGNAL-001:stream:data.SignalIntentUpdate",
		StreamKey("SIGNAL-001", TopicSignalIntent))
	assert.Equal(t, "trader-PAPER-X:stream:data.PortfolioHealthUpdate",
		StreamKey("PAPER-X", TopicPortfolioHealth))
}
