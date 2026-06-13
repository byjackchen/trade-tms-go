package pairs

// generator.go: the multi-pair, multi-leg Pairs SignalGenerator. Direct port
// of src/strategies/pairs/signal.py:106-336 (spec §6-§8). Pure: bars in,
// signals out; no I/O.

import (
	"fmt"
	"math"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/indicators"
)

// barDate is a calendar date (UTC year/month/day) used as the per-symbol
// vintage key. Mirrors Python `bar.ts.date()` (signal.py:156) — the UTC date
// of the timestamp, no timezone conversion (spec §6.1, §11).
type barDate struct {
	y int
	m time.Month
	d int
}

func dateOf(ts time.Time) barDate {
	u := ts.UTC()
	return barDate{y: u.Year(), m: u.Month(), d: u.Day()}
}

func (b barDate) iso() string {
	return fmt.Sprintf("%04d-%02d-%02d", b.y, int(b.m), b.d)
}

// priceRing is a fixed-capacity FIFO of closes mirroring Python's
// deque(maxlen=lookback+1) (signal.py:139-141). The +1 is load-bearing for
// state_dict round-trips (spec §4.3): the buffer may hold up to lookback+1
// closes although evaluation only ever uses the last lookback. We retain the
// original decimal string of each close so state_dict serialization is
// byte-identical to Python's str(Decimal) (the float64 path uses Price.Float64
// which equals float(Decimal(str(close))) exactly for the <=4dp price domain).
type priceRing struct {
	prices []domain.Price
	strs   []string
	cap    int
}

func newPriceRing(capacity int) *priceRing {
	return &priceRing{cap: capacity}
}

// append adds a close (with its canonical decimal string), evicting the oldest
// beyond capacity. str must be the exact decimal string the bar carried.
func (r *priceRing) append(p domain.Price, str string) {
	r.prices = append(r.prices, p)
	r.strs = append(r.strs, str)
	if len(r.prices) > r.cap {
		// evict oldest (deque maxlen semantics)
		r.prices = r.prices[1:]
		r.strs = r.strs[1:]
	}
}

func (r *priceRing) len() int { return len(r.prices) }

// lastN returns the last n closes as float64 (float(Decimal) equivalent,
// signal.py:189-190). Caller guarantees n <= r.len().
func (r *priceRing) lastNFloat(n int) []float64 {
	out := make([]float64, n)
	base := len(r.prices) - n
	for i := 0; i < n; i++ {
		out[i] = r.prices[base+i].Float64()
	}
	return out
}

// Generator is the Go port of PairsSignalGenerator (signal.py:106-143).
type Generator struct {
	cfg Config

	// Per-symbol state (shared across pairs by symbol, set-if-absent;
	// signal.py:117-125, spec §4.3, I-2).
	history     map[string]*priceRing
	lastClose   map[string]domain.Price
	lastCloseSt map[string]string // canonical decimal string of last close
	lastBarDate map[string]barDate

	// Per-pair state.
	pairState   map[PairKey]PairState
	legPosition map[string]int64 // signed: positive long, negative short

	// Read-side telemetry (observability only; never persisted, never feeds
	// signal logic; signal.py:126-131, spec §7.3).
	latestZ    map[PairKey]float64
	latestBeta map[PairKey]float64
	hasZ       map[PairKey]bool
	hasBeta    map[PairKey]bool

	intentGeneration int64
}

// New constructs a Generator after validating cfg (spec §4.2). It performs the
// set-if-absent state initialization of signal.py:115-143: each leg gets ONE
// shared history ring (cap lookback+1) and ONE shared leg-position slot; each
// pair starts FLAT.
func New(cfg Config) (*Generator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	g := &Generator{
		cfg:         cfg,
		history:     make(map[string]*priceRing),
		lastClose:   make(map[string]domain.Price),
		lastCloseSt: make(map[string]string),
		lastBarDate: make(map[string]barDate),
		pairState:   make(map[PairKey]PairState),
		legPosition: make(map[string]int64),
		latestZ:     make(map[PairKey]float64),
		latestBeta:  make(map[PairKey]float64),
		hasZ:        make(map[PairKey]bool),
		hasBeta:     make(map[PairKey]bool),
	}
	maxlen := cfg.Lookback + 1
	for _, pair := range cfg.Pairs {
		for _, sym := range [2]string{pair.LongLeg, pair.ShortLeg} {
			if _, ok := g.history[sym]; !ok {
				g.history[sym] = newPriceRing(maxlen)
			}
			if _, ok := g.legPosition[sym]; !ok {
				g.legPosition[sym] = 0
			}
		}
		if _, ok := g.pairState[pair.Key()]; !ok {
			g.pairState[pair.Key()] = StateFlat
		}
	}
	return g, nil
}

// Config returns the generator configuration.
func (g *Generator) Config() Config { return g.cfg }

// OnBar processes one bar and emits signals for any pair newly in-sync today.
// Mirrors signal.py:149-174 (spec §6). The bar's Close is the exact-decimal
// price; closeStr must be its canonical decimal string (e.g. the value the
// data layer parsed). Use OnBarString to pass it explicitly, or OnDomainBar
// which derives it from the Price.
func (g *Generator) OnBar(bar domain.Bar, closeStr string) []domain.Signal {
	ring, ok := g.history[bar.Symbol]
	if !ok {
		return nil // not part of any pair we trade (signal.py:150-151)
	}

	// Per-symbol bookkeeping — UNCONDITIONAL, before any sync check
	// (signal.py:154-157, spec §6.1).
	ring.append(bar.Close, closeStr)
	g.lastClose[bar.Symbol] = bar.Close
	g.lastCloseSt[bar.Symbol] = closeStr
	bd := dateOf(bar.TS)
	g.lastBarDate[bar.Symbol] = bd

	var signals []domain.Signal
	for _, pair := range g.cfg.Pairs {
		if bar.Symbol != pair.LongLeg && bar.Symbol != pair.ShortLeg {
			continue
		}
		if !g.pairInSync(pair, bd) {
			continue
		}
		signals = append(signals, g.evaluatePair(pair, bar.TS)...)
	}
	return signals
}

// OnDomainBar is OnBar with the canonical close string derived from the Price
// via the Python str(Decimal(str(float))) bridge (pyDecimalStr). For the <=4dp
// price domain this is exact; pass the original string via OnBar when available
// for guaranteed byte-identical state_dict on integer-valued closes.
func (g *Generator) OnDomainBar(bar domain.Bar) []domain.Signal {
	return g.OnBar(bar, pyDecimalStr(bar.Close))
}

func (g *Generator) pairInSync(pair Pair, cur barDate) bool {
	ld, lok := g.lastBarDate[pair.LongLeg]
	sd, sok := g.lastBarDate[pair.ShortLeg]
	return lok && sok && ld == cur && sd == cur
}

// evaluatePair runs the OLS + z-score + state machine for one in-sync pair
// (signal.py:180-231, spec §7).
func (g *Generator) evaluatePair(pair Pair, ts time.Time) []domain.Signal {
	lb := g.cfg.Lookback
	longRing := g.history[pair.LongLeg]
	shortRing := g.history[pair.ShortLeg]
	// Warmup: either leg with fewer than lookback closes => no signals
	// (signal.py:181-187, spec §7.1).
	if longRing.len() < lb || shortRing.len() < lb {
		return nil
	}

	longP := longRing.lastNFloat(lb)
	shortP := shortRing.lastNFloat(lb)

	// OLS hedge ratio: y = a + b*x with x = short, y = long (signal.py:192).
	beta, ok := indicators.OLSSlope(shortP, longP)
	if !ok {
		return nil // degenerate (den == 0) — telemetry NOT touched (§7.2)
	}

	// Spread + population z-score (signal.py:196-203, spec §7.3).
	spreads := make([]float64, lb)
	for i := 0; i < lb; i++ {
		spreads[i] = longP[i] - beta*shortP[i]
	}
	if len(spreads) < 2 {
		return nil // unreachable given lookback>=5, but replicate the guard
	}
	mean := indicators.FMean(spreads)
	std := indicators.PStdev(spreads)
	if std == 0 {
		return nil // telemetry NOT updated (§7.3)
	}
	z := (spreads[len(spreads)-1] - mean) / std

	// Telemetry, recorded ONLY after all numeric guards pass (§7.3).
	g.latestZ[pair.Key()] = z
	g.hasZ[pair.Key()] = true
	g.latestBeta[pair.Key()] = beta
	g.hasBeta[pair.Key()] = true

	state := g.pairState[pair.Key()]
	switch state {
	case StateFlat:
		if z > g.cfg.EntryZ {
			return g.openShortSpread(pair, beta, z, ts)
		}
		if z < -g.cfg.EntryZ {
			return g.openLongSpread(pair, beta, z, ts)
		}
		return nil
	case StateLongSpread:
		if math.Abs(z) < g.cfg.ExitZ || z > g.cfg.EntryZ {
			reason := "spread diverged"
			if math.Abs(z) < g.cfg.ExitZ {
				reason = "mean reversion"
			}
			return g.closePair(pair, z, ts, reason)
		}
		return nil
	case StateShortSpread:
		if math.Abs(z) < g.cfg.ExitZ || z < -g.cfg.EntryZ {
			reason := "spread diverged"
			if math.Abs(z) < g.cfg.ExitZ {
				reason = "mean reversion"
			}
			return g.closePair(pair, z, ts, reason)
		}
		return nil
	}
	return nil
}

// openLongSpread: z < -entry_z. LONG the long_leg, SHORT the short_leg
// (signal.py:237-264, spec §7.5).
func (g *Generator) openLongSpread(pair Pair, beta, z float64, ts time.Time) []domain.Signal {
	longQty, shortQty := g.computeLegQuantities(pair)
	if longQty <= 0 || shortQty <= 0 {
		return nil // abort entry; state + positions untouched
	}
	g.pairState[pair.Key()] = StateLongSpread
	g.legPosition[pair.LongLeg] = longQty
	g.legPosition[pair.ShortLeg] = -shortQty
	reason := fmt.Sprintf("Pairs %s/%s LONG_SPREAD :: z=%s, β=%s",
		pair.LongLeg, pair.ShortLeg, fmtZ(z), fmtBeta(beta))
	return []domain.Signal{
		domain.NewSignal(pair.LongLeg, ts, domain.SideLong, domain.Qty(longQty), reason),
		domain.NewSignal(pair.ShortLeg, ts, domain.SideShort, domain.Qty(shortQty), reason),
	}
}

// openShortSpread: z > entry_z. SHORT the long_leg, LONG the short_leg
// (signal.py:266-293, spec §7.5).
func (g *Generator) openShortSpread(pair Pair, beta, z float64, ts time.Time) []domain.Signal {
	longQty, shortQty := g.computeLegQuantities(pair)
	if longQty <= 0 || shortQty <= 0 {
		return nil
	}
	g.pairState[pair.Key()] = StateShortSpread
	g.legPosition[pair.LongLeg] = -longQty
	g.legPosition[pair.ShortLeg] = shortQty
	reason := fmt.Sprintf("Pairs %s/%s SHORT_SPREAD :: z=%s, β=%s",
		pair.LongLeg, pair.ShortLeg, fmtZ(z), fmtBeta(beta))
	return []domain.Signal{
		domain.NewSignal(pair.LongLeg, ts, domain.SideShort, domain.Qty(longQty), reason),
		domain.NewSignal(pair.ShortLeg, ts, domain.SideLong, domain.Qty(shortQty), reason),
	}
}

// closePair emits FLAT signals for each non-zero leg, long_leg first, and sets
// the pair FLAT unconditionally (signal.py:295-314, spec §7.6).
func (g *Generator) closePair(pair Pair, z float64, ts time.Time, reason string) []domain.Signal {
	fullReason := fmt.Sprintf("Pairs %s/%s close (%s) :: z=%s",
		pair.LongLeg, pair.ShortLeg, reason, fmtZ(z))
	var signals []domain.Signal
	for _, leg := range [2]string{pair.LongLeg, pair.ShortLeg} {
		if g.legPosition[leg] == 0 {
			continue
		}
		g.legPosition[leg] = 0
		signals = append(signals, domain.NewSignal(leg, ts, domain.SideFlat, 0, fullReason))
	}
	g.pairState[pair.Key()] = StateFlat
	return signals
}

// computeLegQuantities: equal-$-weighted legs, half the pair allocation each.
// beta is deliberately NOT used in sizing (signal.py:320-336, spec §8).
func (g *Generator) computeLegQuantities(pair Pair) (int64, int64) {
	longPrice := priceFloatOrZero(g.lastClose, pair.LongLeg)
	shortPrice := priceFloatOrZero(g.lastClose, pair.ShortLeg)
	if longPrice <= 0 || shortPrice <= 0 {
		return 0, 0
	}
	equity := g.cfg.EquityProvider() // live pull, every entry
	targetPerLeg := equity * g.cfg.CapitalPerPairPct / 2
	longQty := int64(math.Floor(targetPerLeg / longPrice))
	shortQty := int64(math.Floor(targetPerLeg / shortPrice))
	return longQty, shortQty
}

func priceFloatOrZero(m map[string]domain.Price, sym string) float64 {
	if p, ok := m[sym]; ok {
		return p.Float64()
	}
	return 0.0
}

// fmtZ formats z with explicit sign and 2 decimals (Python "%+.2f",
// round-half-even), matching signal.py reason strings (spec §7.5).
func fmtZ(z float64) string { return fmt.Sprintf("%+.2f", z) }

// fmtBeta formats beta with 3 decimals (Python ".3f", round-half-even).
func fmtBeta(b float64) string { return fmt.Sprintf("%.3f", b) }
