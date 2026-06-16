package livetrade_test

// recovery_warmup_test.go is the crash-recovery / warmup ordering proof (FIXER
// round 1, blocker #1). In paper/live mode TradeSession.Prime runs
// RestoreStrategyState FIRST (restoring a sector/pairs generator's FULL
// cross-symbol history ring + lastUniverseDate + currentPositions from the
// snapshot) and THEN primes warmup. The batch warmup seam (BatchWarmupConsumer)
// APPENDS the pre-window bars by replaying them through the generator's OnBar
// (ring push), so re-warming a RESTORED strategy would push older pre-window bars
// onto the already-restored ring — resetting lastUniverseDate back to an OLDER
// pre-window month (scrambling the next-bar month-rollover detection) and
// corrupting the momentum window. The fix threads the restored strategy IDs into
// PrimeExcept so the warmup seams SKIP them ("recovery supersedes warmup").
//
// These tests assert that contract end-to-end through the real sector adapter:
//   - a RESTORED sector strategy is NOT re-warmed: after Prime its
//     lastUniverseDate + history equal the restored snapshot exactly (unchanged by
//     the pre-window batch);
//   - a COLD sector strategy (no stored state) in the SAME session IS warmed by the
//     same batch (control: the skip is per-strategy, not a blanket disable).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	"github.com/byjackchen/trade-tms-go/internal/commands"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sectoradapter"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sectorrotation"
)

// memStateStore is an in-memory livetrade.StateStore (no PG) keyed by strategy id.
type memStateStore struct {
	states map[string][]byte
}

func newMemStateStore() *memStateStore { return &memStateStore{states: map[string][]byte{}} }

func (m *memStateStore) SaveState(_ context.Context, id string, state []byte) error {
	m.states[id] = append([]byte(nil), state...)
	return nil
}

func (m *memStateStore) LoadState(_ context.Context, id string) ([]byte, bool, error) {
	s, ok := m.states[id]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), s...), true, nil
}

// sectorUniverse is the 4-ETF test universe for the recovery proof.
var sectorUniverse = []string{"E1", "E2", "E3", "E4"}

// newSectorStrat builds a real sector adapter (StatePersister + BatchWarmupConsumer)
// under id, lookback 2 / topK 2 over sectorUniverse.
func newSectorStrat(t *testing.T, id string) *sectoradapter.Strategy {
	t.Helper()
	sg, err := sectorrotation.New(sectorrotation.Config{
		EquityProvider:   func() float64 { return 100000 },
		Universe:         sectorUniverse,
		MomentumLookback: 2,
		TopK:             2,
		Timezone:         "America/New_York",
	})
	require.NoError(t, err)
	strat, err := sectoradapter.New(id, sg)
	require.NoError(t, err)
	return strat
}

// secBar is a sector ETF daily bar.
func secBar(sym string, y int, mo time.Month, d int, px float64) domain.Bar {
	p := domain.MustPrice(ftoa(px))
	return domain.Bar{
		Symbol: sym,
		TS:     time.Date(y, mo, d, 0, 0, 0, 0, time.UTC),
		Open:   p, High: p, Low: p, Close: p, Volume: 1000,
	}
}

// preWindowBatch is the OLD (pre-window) interleaved warmup stream: Nov 2023 ..
// Jan 2024. Replaying it sets lastUniverseDate into Jan 2024 and builds the Jan
// momentum ring.
func preWindowBatch() []domain.Bar {
	var instr []engine.InstrumentBars
	gain := map[string]float64{"E1": 12, "E2": 8, "E3": 4, "E4": 1}
	for _, s := range sectorUniverse {
		instr = append(instr, engine.InstrumentBars{Symbol: s, Bars: []domain.Bar{
			secBar(s, 2023, time.November, 1, 100+gain[s]*0),
			secBar(s, 2023, time.November, 15, 100+gain[s]*1),
			secBar(s, 2023, time.December, 1, 100+gain[s]*2),
			secBar(s, 2023, time.December, 15, 100+gain[s]*3),
			secBar(s, 2024, time.January, 2, 100+gain[s]*4),
			secBar(s, 2024, time.January, 16, 100+gain[s]*5),
		}})
	}
	return livengine.BatchBars(instr)
}

// liveStateSnapshot drives a throwaway generator over the pre-window stream AND a
// later run window (Feb..Mar 2024), returning the StateDict JSON of the resulting
// "live" state — this is what a session would have persisted just before a crash
// (lastUniverseDate in March 2024, history holding the Feb/Mar closes).
func liveStateSnapshot(t *testing.T) []byte {
	t.Helper()
	strat := newSectorStrat(t, "snapshot-src")
	strat.WarmupBatch(preWindowBatch())

	var run []engine.InstrumentBars
	gain := map[string]float64{"E1": 12, "E2": 8, "E3": 4, "E4": 1}
	for _, s := range sectorUniverse {
		run = append(run, engine.InstrumentBars{Symbol: s, Bars: []domain.Bar{
			secBar(s, 2024, time.February, 1, 200+gain[s]*6),
			secBar(s, 2024, time.February, 15, 200+gain[s]*7),
			secBar(s, 2024, time.March, 1, 200+gain[s]*8),
		}})
	}
	for _, b := range livengine.BatchBars(run) {
		_ = strat.Generator().OnBar(b)
	}
	js, err := json.Marshal(strat.StateDictJSON())
	require.NoError(t, err)
	return js
}

// stateDictOf marshals+unmarshals a strategy's StateDict snapshot for assertion.
func stateDictOf(t *testing.T, strat *sectoradapter.Strategy) sectorrotation.StateDict {
	t.Helper()
	js, err := json.Marshal(strat.StateDictJSON())
	require.NoError(t, err)
	var d sectorrotation.StateDict
	require.NoError(t, json.Unmarshal(js, &d))
	return d
}

// TestPrimeSkipsWarmupForRestoredStrategy proves the recovery-supersedes-warmup
// invariant for the append-based batch warmup consumer (sector). A restored sector
// strategy must NOT be re-warmed over its restored ring; a cold one in the same
// session must be.
func TestPrimeSkipsWarmupForRestoredStrategy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const restoredID = "SectorRestored-000"
	const coldID = "SectorCold-001"

	// The store holds a snapshot for the RESTORED strategy only (lastUniverseDate in
	// March 2024). The COLD strategy has no stored state.
	store := newMemStateStore()
	require.NoError(t, store.SaveState(ctx, restoredID, liveStateSnapshot(t)))

	restored := newSectorStrat(t, restoredID)
	cold := newSectorStrat(t, coldID)

	venue := moexec.NewMockVenue(paperAcc)
	acct := accounting.NewAccount(domain.MustMoney("100000.00"), nil)
	account := livetrade.NewAccountAdapter(acct)
	paperAcct := domain.NewBrokerAccount("moomoo", domain.EnvSimulate, paperAcc, "paper")
	exec, err := moexec.New(ctx, moexec.Config{
		Account: paperAcct, Client: venue,
		TraderID: "PAPER-TEST-001", Sink: &fillSink{}, Book: account,
	})
	require.NoError(t, err)
	halt := commands.NewHaltState(nil)

	ts, err := livetrade.NewTradeSession(livetrade.TradeSessionConfig{
		Acct:        paperAcct,
		Strategies:  []engine.Strategy{restored, cold},
		Account:     account,
		Executor:    exec,
		Halt:        halt,
		NAV:         domain.MustMoney("100000.00"),
		EmitGate:    halt.Emitting,
		StateStore:  store,
		WarmupBatch: preWindowBatch(), // OLD Nov..Jan stream offered to BOTH strategies
	})
	require.NoError(t, err)

	// Prime = restore (restored only) THEN warmup-except-restored.
	require.NoError(t, ts.Prime(ctx))

	// (1) RESTORED strategy: its state must EQUAL the persisted snapshot — the
	// pre-window batch was NOT replayed onto its restored ring. The decisive marker
	// is lastUniverseDate: had the batch re-warmed it, the Jan-2024 pre-window bars
	// would have RESET it back from March 2024 to January 2024.
	wantSnap := liveStateSnapshot(t)
	var want sectorrotation.StateDict
	require.NoError(t, json.Unmarshal(wantSnap, &want))

	gotRestored := stateDictOf(t, restored)
	require.NotNil(t, gotRestored.LastUniverseDate, "restored strategy must keep its snapshot last_universe_date")
	assert.Equal(t, "2024-03-01", *gotRestored.LastUniverseDate,
		"restored strategy must NOT be re-warmed: last_universe_date stays at the snapshot (March), not reset to the pre-window batch month")
	assert.Equal(t, want.History, gotRestored.History,
		"restored strategy history must equal the snapshot (no pre-window bars appended)")
	assert.Equal(t, want.LastClose, gotRestored.LastClose,
		"restored strategy last_close must equal the snapshot")
	assert.Equal(t, want.CurrentPositions, gotRestored.CurrentPositions,
		"restored strategy current_positions must equal the snapshot")

	// (2) COLD strategy (no stored state) in the SAME session IS warmed by the same
	// batch: it reaches the Jan-2024 pre-window state (the skip is per-strategy, not
	// a blanket disable). A cold reference warmed by the same batch is the oracle.
	coldRef := newSectorStrat(t, "cold-ref")
	coldRef.WarmupBatch(preWindowBatch())
	wantCold := stateDictOf(t, coldRef)
	gotCold := stateDictOf(t, cold)

	require.NotNil(t, gotCold.LastUniverseDate, "cold strategy must be warmed (last_universe_date set)")
	assert.Equal(t, *wantCold.LastUniverseDate, *gotCold.LastUniverseDate,
		"cold strategy must be warmed to the pre-window batch state")
	assert.Equal(t, wantCold.History, gotCold.History,
		"cold strategy history must equal the batch-warmed reference")

	// And the two strategies end in DIFFERENT states — proving the skip discriminated
	// between them (restored kept March; cold got January).
	assert.NotEqual(t, *gotRestored.LastUniverseDate, *gotCold.LastUniverseDate,
		"restored vs cold must diverge: warmup was skipped for the restored one only")
}
