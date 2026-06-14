package publish

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
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
	gen, err := sepa.New(sepa.Config{
		Symbol: "AAPL", EquityProvider: func() float64 { return 100000 },
		RiskPct: 1.0, MarketCapMinUSD: 5e8, HardStopPct: 7.5, PivotBufferPct: 1.5,
		BreakoutVolumeMultiple: 1.5, VCPLookback: 4, HistoryMaxBars: 1000,
		Timezone: "America/New_York",
	})
	require.NoError(t, err)
	adapter, err := sepaadapter.New("SEPARunner-000", gen)
	require.NoError(t, err)

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

// NOTE: the local sepa.SignalIntent / orb.SignalIntent → domain wire-shape
// conversion moved into sepaadapter/orbadapter (the sanctioned domain bridge,
// modularization-review.md §E3). Its byte-shape coverage now lives there
// (sepaadapter/intent_test.go, orbadapter/intent_test.go); publish only switches
// on the canonical domain types, exercised by TestNormalizeSEPAAdapterOutput
// (real adapter output) + the pairs/sector slice tests below.

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
