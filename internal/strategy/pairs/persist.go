package pairs

// persist.go: state_dict / load_state and the Python decimal-string bridge.
// Ports signal.py:445-497 (spec §12).

import (
	"sort"
	"strconv"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// StateConfig is the "config" block of state_dict (signal.py:447-460).
// equity_at_snapshot is float(equity_provider()) captured AT SAVE TIME; it is
// informational only — load_state never reads it (spec §12.1). There is
// deliberately no account_size key.
type StateConfig struct {
	Pairs             [][]string `json:"pairs"` // list-of-2-lists, config order
	Lookback          int        `json:"lookback"`
	EntryZ            float64    `json:"entry_z"`
	ExitZ             float64    `json:"exit_z"`
	EquityAtSnapshot  float64    `json:"equity_at_snapshot"`
	CapitalPerPairPct float64    `json:"capital_per_pair_pct"`
	Timezone          string     `json:"timezone"`
}

// StateDict is the full persisted state (signal.py:445-466, spec §12.1).
// Maps serialize with sorted keys under Go's encoding/json; Python preserves
// insertion order. Parity is semantic (parse-and-compare), not byte order.
type StateDict struct {
	Config      StateConfig         `json:"config"`
	History     map[string][]string `json:"history"`       // str(Decimal), oldest→newest
	LastClose   map[string]string   `json:"last_close"`    // str(Decimal)
	LastBarDate map[string]string   `json:"last_bar_date"` // ISO date
	PairState   map[string]string   `json:"pair_state"`    // "long|short" -> state
	LegPosition map[string]int64    `json:"leg_position"`  // signed ints
}

// StateDict serializes the generator state (signal.py:445-466).
func (g *Generator) StateDict() StateDict {
	cfgPairs := make([][]string, 0, len(g.cfg.Pairs))
	for _, p := range g.cfg.Pairs {
		cfgPairs = append(cfgPairs, []string{p.LongLeg, p.ShortLeg})
	}
	hist := make(map[string][]string, len(g.history))
	for sym, ring := range g.history {
		closes := make([]string, len(ring.strs))
		copy(closes, ring.strs)
		hist[sym] = closes
	}
	lastClose := make(map[string]string, len(g.lastCloseSt))
	for sym, s := range g.lastCloseSt {
		lastClose[sym] = s
	}
	lastBarDate := make(map[string]string, len(g.lastBarDate))
	for sym, bd := range g.lastBarDate {
		lastBarDate[sym] = bd.iso()
	}
	pairState := make(map[string]string, len(g.pairState))
	for key, s := range g.pairState {
		pairState[key.Long+"|"+key.Short] = string(s)
	}
	legPos := make(map[string]int64, len(g.legPosition))
	for sym, q := range g.legPosition {
		legPos[sym] = q
	}
	return StateDict{
		Config: StateConfig{
			Pairs:             cfgPairs,
			Lookback:          g.cfg.Lookback,
			EntryZ:            g.cfg.EntryZ,
			ExitZ:             g.cfg.ExitZ,
			EquityAtSnapshot:  g.cfg.EquityProvider(),
			CapitalPerPairPct: g.cfg.CapitalPerPairPct,
			Timezone:          g.cfg.Timezone,
		},
		History:     hist,
		LastClose:   lastClose,
		LastBarDate: lastBarDate,
		PairState:   pairState,
		LegPosition: legPos,
	}
}

// LoadState restores state from a StateDict (signal.py:468-497, spec §12.2).
// History rings are rebuilt with capacity from the CURRENT config (lookback+1);
// if lookback shrank, oldest entries are evicted on load (deque semantics).
// Empty buffers / FLAT states / 0 positions are seeded for any configured
// leg/pair missing from the snapshot. The "config" block is ignored.
// _latest_z / _latest_beta / _intent_generation are NOT restored.
func (g *Generator) LoadState(d StateDict) error {
	maxlen := g.cfg.Lookback + 1

	g.history = make(map[string]*priceRing)
	// Deterministic load order (Python iterates the snapshot dict in its
	// order; for ring eviction the per-symbol order is what matters and is
	// preserved within each list).
	syms := make([]string, 0, len(d.History))
	for sym := range d.History {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	for _, sym := range syms {
		ring := newPriceRing(maxlen)
		for _, s := range d.History[sym] {
			p, err := domain.ParsePrice(s)
			if err != nil {
				return err
			}
			ring.append(p, s)
		}
		g.history[sym] = ring
	}
	for _, pair := range g.cfg.Pairs {
		for _, sym := range [2]string{pair.LongLeg, pair.ShortLeg} {
			if _, ok := g.history[sym]; !ok {
				g.history[sym] = newPriceRing(maxlen)
			}
		}
	}

	g.lastClose = make(map[string]domain.Price)
	g.lastCloseSt = make(map[string]string)
	for sym, s := range d.LastClose {
		p, err := domain.ParsePrice(s)
		if err != nil {
			return err
		}
		g.lastClose[sym] = p
		g.lastCloseSt[sym] = s
	}

	g.lastBarDate = make(map[string]barDate)
	for sym, iso := range d.LastBarDate {
		bd, err := parseISODate(iso)
		if err != nil {
			return err
		}
		g.lastBarDate[sym] = bd
	}

	g.pairState = make(map[PairKey]PairState)
	for k, s := range d.PairState {
		long, short := splitFirstPipe(k)
		g.pairState[PairKey{Long: long, Short: short}] = PairState(s)
	}
	for _, pair := range g.cfg.Pairs {
		if _, ok := g.pairState[pair.Key()]; !ok {
			g.pairState[pair.Key()] = StateFlat
		}
	}

	g.legPosition = make(map[string]int64)
	for sym, q := range d.LegPosition {
		g.legPosition[sym] = q
	}
	for _, pair := range g.cfg.Pairs {
		for _, sym := range [2]string{pair.LongLeg, pair.ShortLeg} {
			if _, ok := g.legPosition[sym]; !ok {
				g.legPosition[sym] = 0
			}
		}
	}
	return nil
}

// splitFirstPipe splits a "long|short" key on the FIRST '|' (signal.py:486).
func splitFirstPipe(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// parseISODate parses "YYYY-MM-DD".
func parseISODate(s string) (barDate, error) {
	var bd barDate
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return bd, errBadDate(s)
	}
	y, err := strconv.Atoi(s[0:4])
	if err != nil {
		return bd, err
	}
	mo, err := strconv.Atoi(s[5:7])
	if err != nil {
		return bd, err
	}
	dd, err := strconv.Atoi(s[8:10])
	if err != nil {
		return bd, err
	}
	bd.y = y
	bd.m = monthFromInt(mo)
	bd.d = dd
	return bd, nil
}
