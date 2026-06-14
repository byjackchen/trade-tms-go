package sectorrotation

import (
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// StateSummary is the light user-visible state for UI / monitoring (P-UI.1),
// mirroring signal.py:343-377 [MUST-MATCH]. It excludes the heavy per-symbol
// history. CurrentHoldings contains ONLY positive-qty entries. LastUniverseDate
// is the ISO date string (YYYY-MM-DD) or nil. The struct is JSON-serializable
// to the same shape the reference's state_summary() dict produces.
type StateSummary struct {
	CurrentHoldings  map[string]int64 `json:"current_holdings"`
	LastUniverseDate *string          `json:"last_universe_date"`
	TopK             int              `json:"top_k"`
	UniverseSize     int              `json:"universe_size"`
}

// StateSummary returns the current monitoring snapshot.
func (sg *SignalGenerator) StateSummary() StateSummary {
	holdings := make(map[string]int64)
	for _, sym := range sg.cfg.Universe {
		if qty := sg.currentPositions[sym]; qty > 0 {
			holdings[sym] = qty
		}
	}
	var lastDate *string
	if sg.lastUniverseDate != nil {
		s := sg.lastUniverseDate.Format("2006-01-02")
		lastDate = &s
	}
	return StateSummary{
		CurrentHoldings:  holdings,
		LastUniverseDate: lastDate,
		TopK:             sg.cfg.TopK,
		UniverseSize:     len(sg.cfg.Universe),
	}
}

// StateDictConfig is the config slice of state_dict (signal.py:384-393).
type StateDictConfig struct {
	Universe         []string `json:"universe"`
	MomentumLookback int      `json:"momentum_lookback"`
	TopK             int      `json:"top_k"`
	EquityAtSnapshot float64  `json:"equity_at_snapshot"`
	Timezone         string   `json:"timezone"`
}

// StateDict is the crash-recovery snapshot (signal.py:382-405 [MUST-MATCH]).
// History/last_close are serialized as canonical decimal strings (Python
// str(Decimal)); current_positions covers EVERY universe symbol (including
// zeros). last_universe_date is an ISO date string or nil.
type StateDict struct {
	Config           StateDictConfig     `json:"config"`
	History          map[string][]string `json:"history"`
	LastClose        map[string]string   `json:"last_close"`
	LastUniverseDate *string             `json:"last_universe_date"`
	CurrentPositions map[string]int64    `json:"current_positions"`
}

// StateDict returns the serializable state snapshot. The equity snapshot is
// pulled live (provider is not serialized), exactly like the reference.
func (sg *SignalGenerator) StateDict() StateDict {
	history := make(map[string][]string, len(sg.history))
	for sym, deq := range sg.history {
		snap := deq.snapshot()
		closes := make([]string, len(snap))
		for i, c := range snap {
			closes[i] = pyFloatRepr(c.Float64())
		}
		history[sym] = closes
	}
	lastClose := make(map[string]string, len(sg.lastClose))
	for sym, v := range sg.lastClose {
		lastClose[sym] = pyFloatRepr(v.Float64())
	}
	var lastDate *string
	if sg.lastUniverseDate != nil {
		s := sg.lastUniverseDate.Format("2006-01-02")
		lastDate = &s
	}
	positions := make(map[string]int64, len(sg.currentPositions))
	for sym, qty := range sg.currentPositions {
		positions[sym] = qty
	}
	return StateDict{
		Config: StateDictConfig{
			Universe:         append([]string(nil), sg.cfg.Universe...),
			MomentumLookback: sg.cfg.MomentumLookback,
			TopK:             sg.cfg.TopK,
			EquityAtSnapshot: sg.cfg.EquityProvider(),
			Timezone:         sg.cfg.Timezone,
		},
		History:          history,
		LastClose:        lastClose,
		LastUniverseDate: lastDate,
		CurrentPositions: positions,
	}
}

// LoadState restores from a StateDict (signal.py:407-433 [MUST-MATCH]). It
// rebuilds bounded deques (maxlen lookback+1), ensures every universe symbol
// has a deque and a position entry (defaulting to 0), parses last_close and
// last_universe_date, and replaces current_positions. The equity_provider is
// not restored (config-supplied). History closes that exceed maxlen are
// truncated to the most-recent maxlen by the deque push semantics.
func (sg *SignalGenerator) LoadState(d StateDict) error {
	maxlen := sg.cfg.MomentumLookback + 1

	newHistory := make(map[string]*priceDeque, len(sg.cfg.Universe))
	for sym, closes := range d.History {
		dq := newPriceDeque(maxlen)
		for _, cs := range closes {
			p, err := domain.ParsePrice(cs)
			if err != nil {
				return fmt.Errorf("sector_rotation load_state: history %s close %q: %w", sym, cs, err)
			}
			dq.push(p)
		}
		newHistory[sym] = dq
	}
	for _, sym := range sg.cfg.Universe {
		if _, ok := newHistory[sym]; !ok {
			newHistory[sym] = newPriceDeque(maxlen)
		}
	}
	sg.history = newHistory

	newLastClose := make(map[string]domain.Price, len(d.LastClose))
	for sym, cs := range d.LastClose {
		p, err := domain.ParsePrice(cs)
		if err != nil {
			return fmt.Errorf("sector_rotation load_state: last_close %s %q: %w", sym, cs, err)
		}
		newLastClose[sym] = p
	}
	sg.lastClose = newLastClose

	if d.LastUniverseDate != nil && *d.LastUniverseDate != "" {
		t, err := time.Parse("2006-01-02", *d.LastUniverseDate)
		if err != nil {
			return fmt.Errorf("sector_rotation load_state: last_universe_date %q: %w", *d.LastUniverseDate, err)
		}
		td := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		sg.lastUniverseDate = &td
	} else {
		sg.lastUniverseDate = nil
	}

	newPositions := make(map[string]int64, len(d.CurrentPositions))
	for sym, qty := range d.CurrentPositions {
		newPositions[sym] = qty
	}
	for _, sym := range sg.cfg.Universe {
		if _, ok := newPositions[sym]; !ok {
			newPositions[sym] = 0
		}
	}
	sg.currentPositions = newPositions

	return nil
}
