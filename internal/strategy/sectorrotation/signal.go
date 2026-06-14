package sectorrotation

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// DefaultUniverse is the canonical 11 Select Sector SPDR ETFs, in the
// reference's declaration order (signal.py:61-73 [MUST-MATCH]). Declaration
// order is load-bearing for state_dict ordering and as the tie-break-free
// iteration order of evaluate_intent's output.
var DefaultUniverse = []string{
	"XLK",  // Technology
	"XLF",  // Financials
	"XLE",  // Energy
	"XLV",  // Health Care
	"XLY",  // Consumer Discretionary
	"XLP",  // Consumer Staples
	"XLU",  // Utilities
	"XLB",  // Materials
	"XLI",  // Industrials
	"XLRE", // Real Estate
	"XLC",  // Communication Services
}

// EquityProvider returns live account equity as a float64 (the reference's
// Callable[[], Decimal] pulled through float() at sizing time). It is invoked
// at every rebalance and at every evaluate_intent so sizing/weights reflect the
// up-to-date account value rather than a stale figure (PA-D1).
type EquityProvider func() float64

// Config is the resolved SectorRotation configuration, mirroring
// SectorRotationSignalGeneratorConfig (signal.py:76-92). EquityProvider is
// required (no default); the remaining knobs come from resolved params.
type Config struct {
	EquityProvider   EquityProvider
	Universe         []string
	MomentumLookback int
	TopK             int
	Timezone         string
}

// Validate mirrors SectorRotationSignalGeneratorConfig.__post_init__
// (signal.py:80-92). The top_k message embeds len(universe), exactly as Python.
func (c Config) Validate() error {
	if c.EquityProvider == nil {
		return fmt.Errorf("equity_provider must be a callable returning Decimal")
	}
	if len(c.Universe) == 0 {
		return fmt.Errorf("universe must not be empty")
	}
	if c.MomentumLookback < 2 {
		return fmt.Errorf("momentum_lookback must be >= 2")
	}
	if !(c.TopK >= 1 && c.TopK <= len(c.Universe)) {
		return fmt.Errorf("top_k must be in [1, %d], got %d", len(c.Universe), c.TopK)
	}
	return nil
}

// SignalGenerator is the universe-wide momentum-rank signal generator. It holds
// per-symbol rolling close history plus the current holdings and the
// month-rollover anchor. NOT safe for concurrent use (driven from the single
// engine loop goroutine).
type SignalGenerator struct {
	cfg Config

	// universeSet for O(1) membership; insertion order preserved via cfg.Universe.
	universeSet map[string]struct{}

	// Per-symbol bounded close history (maxlen = lookback+1), oldest at index 0.
	history map[string]*priceDeque
	// Most recent close per universe symbol (snapshot used at rebalance time).
	lastClose map[string]domain.Price
	// Most recent bar date seen across the WHOLE universe — month rollover anchor.
	// nil until the first universe bar is ingested.
	lastUniverseDate *time.Time
	// Symbols currently held -> share count.
	currentPositions map[string]int64
	// Monotonic evaluate_intent counter (NOT persisted; restarts reset to 0).
	intentGeneration int64
}

// New constructs a SignalGenerator after validating cfg. It seeds an empty
// bounded deque and a zero position for every universe symbol, exactly like
// the reference __post_init__ (signal.py:120-126).
func New(cfg Config) (*SignalGenerator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	maxlen := cfg.MomentumLookback + 1
	sg := &SignalGenerator{
		cfg:              cfg,
		universeSet:      make(map[string]struct{}, len(cfg.Universe)),
		history:          make(map[string]*priceDeque, len(cfg.Universe)),
		lastClose:        make(map[string]domain.Price, len(cfg.Universe)),
		currentPositions: make(map[string]int64, len(cfg.Universe)),
	}
	for _, sym := range cfg.Universe {
		sg.universeSet[sym] = struct{}{}
		sg.history[sym] = newPriceDeque(maxlen)
		sg.currentPositions[sym] = 0
	}
	return sg, nil
}

// Config returns the generator's configuration (read-only use).
func (sg *SignalGenerator) Config() Config { return sg.cfg }

// ---------------------------------------------------------------------------
// Core: process one bar; rebalance on month rollover.
// ---------------------------------------------------------------------------

// OnBar processes one bar from any universe symbol and returns the signals
// produced (empty unless this bar is the first of a new month with full
// warmup). Out-of-universe symbols are ignored with no state change
// (signal.py:131-160 [MUST-MATCH]).
//
// Look-ahead guard: rebalance is computed BEFORE this bar is folded into
// history/lastClose, so the snapshot is consistent across all symbols.
func (sg *SignalGenerator) OnBar(bar domain.Bar) []domain.Signal {
	if _, ok := sg.universeSet[bar.Symbol]; !ok {
		return nil // not in our universe
	}

	barDate := dateOf(bar.TS)
	isFirstBarOfNewMonth := sg.lastUniverseDate != nil &&
		barDate.Month() != sg.lastUniverseDate.Month()

	var signals []domain.Signal
	if isFirstBarOfNewMonth && sg.hasFullWarmup() {
		signals = sg.computeRebalanceSignals(bar.TS)
	}

	// Now ingest this bar (after the rebalance snapshot).
	sg.history[bar.Symbol].push(bar.Close)
	sg.lastClose[bar.Symbol] = bar.Close
	sg.lastUniverseDate = &barDate

	return signals
}

// hasFullWarmup reports whether every universe symbol has at least lookback+1
// closes recorded (signal.py:162-167 [MUST-MATCH]).
func (sg *SignalGenerator) hasFullWarmup() bool {
	needed := sg.cfg.MomentumLookback + 1
	for _, sym := range sg.cfg.Universe {
		if sg.history[sym].len() < needed {
			return false
		}
	}
	return true
}

// computeRebalanceSignals ranks the universe by lookback return and emits the
// transition signals: FLAT for dropped holdings (sorted), LONG for new entries
// (sorted), nothing for stayers (signal.py:173-243 [MUST-MATCH]).
func (sg *SignalGenerator) computeRebalanceSignals(ts time.Time) []domain.Signal {
	// returns: only symbols with old close > 0 are eligible (signal.py:181-188).
	returns := make(map[string]float64, len(sg.cfg.Universe))
	var eligible []string
	for _, sym := range sg.cfg.Universe {
		h := sg.history[sym]
		old := h.front() // index 0
		new := h.back()  // index -1
		if old <= 0 {
			continue
		}
		returns[sym] = ratioReturn(old, new)
		eligible = append(eligible, sym)
	}
	if len(eligible) == 0 {
		return nil
	}

	// Top-K by return, descending. Python's sorted(..., reverse=True) is STABLE,
	// so ties keep the input order of returns.items() — which iterates in the
	// dict's insertion order == cfg.Universe order. We reproduce that by
	// ranking over `eligible` (in universe order) with a stable sort by
	// descending return.
	ranked := make([]string, len(eligible))
	copy(ranked, eligible)
	sort.SliceStable(ranked, func(i, j int) bool {
		return returns[ranked[i]] > returns[ranked[j]]
	})
	newTopK := make(map[string]struct{}, sg.cfg.TopK)
	for _, sym := range ranked[:min(sg.cfg.TopK, len(ranked))] {
		newTopK[sym] = struct{}{}
	}

	currentlyHeld := make(map[string]struct{})
	for sym, qty := range sg.currentPositions {
		if qty > 0 {
			currentlyHeld[sym] = struct{}{}
		}
	}

	var signals []domain.Signal

	// FLAT: held but no longer in top-K. Sorted ascending by symbol.
	var toFlat []string
	for sym := range currentlyHeld {
		if _, in := newTopK[sym]; !in {
			toFlat = append(toFlat, sym)
		}
	}
	sort.Strings(toFlat)
	for _, sym := range toFlat {
		heldQty := sg.currentPositions[sym]
		sg.currentPositions[sym] = 0
		signals = append(signals, domain.Signal{
			Symbol:    sym,
			TS:        ts,
			Side:      domain.SideFlat,
			TargetQty: 0,
			Reason: fmt.Sprintf(
				"Sector Rotation rebalance :: closing %s (was %d sh, no longer in top-%d)",
				sym, heldQty, sg.cfg.TopK,
			),
			Confidence: 1.0,
		})
	}

	// LONG: new top-K member not yet held. Pull live equity ONCE at rebalance
	// time (signal.py:225-227). Sorted ascending by symbol.
	equity := sg.cfg.EquityProvider()
	targetValue := equity / float64(sg.cfg.TopK)
	var toLong []string
	for sym := range newTopK {
		if _, held := currentlyHeld[sym]; !held {
			toLong = append(toLong, sym)
		}
	}
	sort.Strings(toLong)
	for _, sym := range toLong {
		price := sg.lastClose[sym].Float64()
		if price <= 0 {
			continue
		}
		targetShares := int64(math.Floor(targetValue / price))
		if targetShares <= 0 {
			continue
		}
		sg.currentPositions[sym] = targetShares
		momPct := returns[sym] * 100.0
		signals = append(signals, domain.Signal{
			Symbol:    sym,
			TS:        ts,
			Side:      domain.SideLong,
			TargetQty: domain.Qty(targetShares),
			Reason: fmt.Sprintf(
				"Sector Rotation rebalance :: top-%d entry, %d-bar return %s%%",
				sg.cfg.TopK, sg.cfg.MomentumLookback, formatSignedPct2(momPct),
			),
			Confidence: 1.0,
		})
	}

	return signals
}
