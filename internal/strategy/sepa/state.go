package sepa

// state.go ports state_dict / load_state (signal.py:545-601) — crash-recovery
// serialization. state_dict snapshots config (with a live equity reading, no
// account_size key), context, position (Decimals as str), and the full kline
// history as a column-oriented dict keyed "index" for the DatetimeIndex.
// load_state restores context/position/klines only (the caller supplies fresh
// config + equity provider), with the reference's defaulting on missing keys.

import "time"

// StateDict is the crash-recovery snapshot (signal.py:545-578). Field shapes
// mirror the reference dict exactly so JSON round-trips byte-compatibly.
type StateDict struct {
	Config   StateConfig   `json:"config"`
	Context  StateContext  `json:"context"`
	Position StatePosition `json:"position"`
	Klines   StateKlines   `json:"klines"`
}

// StateConfig is the config snapshot. EquityAtSnapshot is a live equity read at
// save time (audit-only); there is intentionally NO account_size key
// (test_signal.py:329-334).
type StateConfig struct {
	Symbol                 string  `json:"symbol"`
	EquityAtSnapshot       float64 `json:"equity_at_snapshot"`
	RiskPct                float64 `json:"risk_pct"`
	MarketCapMinUSD        float64 `json:"market_cap_min_usd"`
	HardStopPct            float64 `json:"hard_stop_pct"`
	PivotBufferPct         float64 `json:"pivot_buffer_pct"`
	BreakoutVolumeMultiple float64 `json:"breakout_volume_multiple"`
	VCPLookback            int     `json:"vcp_lookback"`
	HistoryMaxBars         int     `json:"history_max_bars"`
	Timezone               string  `json:"timezone"`
}

// StateContext is the external-context snapshot.
type StateContext struct {
	Regime           string  `json:"regime"`
	MarketCapUSD     float64 `json:"market_cap_usd"`
	EarningsBlackout bool    `json:"earnings_blackout"`
	Catalyst         bool    `json:"catalyst"`
}

// StatePosition is the position snapshot. Prices are str(Decimal). Grade is a
// pointer so it serializes null when flat (the reference grade is None).
type StatePosition struct {
	Shares     int     `json:"shares"`
	EntryPrice string  `json:"entry_price"`
	StopPrice  string  `json:"stop_price"`
	PivotPrice string  `json:"pivot_price"`
	Grade      *string `json:"grade"`
}

// StateKlines is the column-oriented kline history (reset_index().to_dict(
// orient="list")). The index column is "index" (datetimes as RFC3339-like).
type StateKlines struct {
	Index  []time.Time `json:"index"`
	Open   []float64   `json:"open"`
	High   []float64   `json:"high"`
	Low    []float64   `json:"low"`
	Close  []float64   `json:"close"`
	Volume []float64   `json:"volume"`
}

// StateDict builds the crash-recovery snapshot (signal.py:545-578). It reads
// live equity once for the audit field, matching float(equity_provider()).
func (g *Generator) StateDict() StateDict {
	var gradePtr *string
	if g.grade != "" {
		s := string(g.grade)
		gradePtr = &s
	}
	return StateDict{
		Config: StateConfig{
			Symbol:                 g.cfg.Symbol,
			EquityAtSnapshot:       g.cfg.EquityProvider(),
			RiskPct:                g.cfg.RiskPct,
			MarketCapMinUSD:        g.cfg.MarketCapMinUSD,
			HardStopPct:            g.cfg.HardStopPct,
			PivotBufferPct:         g.cfg.PivotBufferPct,
			BreakoutVolumeMultiple: g.cfg.BreakoutVolumeMultiple,
			VCPLookback:            g.cfg.VCPLookback,
			HistoryMaxBars:         g.cfg.HistoryMaxBars,
			Timezone:               g.cfg.Timezone,
		},
		Context: StateContext{
			Regime:           g.regime,
			MarketCapUSD:     g.marketCapUSD,
			EarningsBlackout: g.earningsBlackout,
			Catalyst:         g.catalyst,
		},
		Position: StatePosition{
			Shares:     g.position,
			EntryPrice: g.entryPrice.str,
			StopPrice:  g.stopPrice.str,
			PivotPrice: g.pivotPrice.str,
			Grade:      gradePtr,
		},
		Klines: StateKlines{
			Index:  append([]time.Time(nil), g.ts...),
			Open:   append([]float64(nil), g.open...),
			High:   append([]float64(nil), g.high...),
			Low:    append([]float64(nil), g.low...),
			Close:  append([]float64(nil), g.close...),
			Volume: append([]float64(nil), g.volume...),
		},
	}
}

// LoadState restores context/position/klines from a snapshot (signal.py:580-601).
// Config is NOT restored (the caller constructs with fresh config first). Missing
// keys default per the reference: regime->"unknown", cap->0, flags->false,
// shares->0, prices->Decimal("0"), grade->nil.
func (g *Generator) LoadState(d StateDict) {
	g.regime = orDefault(d.Context.Regime, "unknown")
	g.marketCapUSD = d.Context.MarketCapUSD
	g.earningsBlackout = d.Context.EarningsBlackout
	g.catalyst = d.Context.Catalyst

	g.position = d.Position.Shares
	g.entryPrice = decFromStr(d.Position.EntryPrice)
	g.stopPrice = decFromStr(d.Position.StopPrice)
	g.pivotPrice = decFromStr(d.Position.PivotPrice)
	if d.Position.Grade != nil {
		g.grade = Grade(*d.Position.Grade)
	} else {
		g.grade = ""
	}

	k := d.Klines
	g.ts = append([]time.Time(nil), k.Index...)
	g.open = append([]float64(nil), k.Open...)
	g.high = append([]float64(nil), k.High...)
	g.low = append([]float64(nil), k.Low...)
	g.close = append([]float64(nil), k.Close...)
	g.volume = append([]float64(nil), k.Volume...)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// decFromStr parses a stored str(Decimal) back into a decState. The reference
// load_state does Decimal(entry_price) on the stored string; an empty/"0"
// string yields decZero. We parse the float for arithmetic and keep the
// original string for re-serialization fidelity.
func decFromStr(s string) decState {
	if s == "" || s == "0" {
		return decZero
	}
	f := parsePyFloat(s)
	// Re-derive the canonical string from the float so a round-trip emits the
	// same shape the reference would (Decimal(str(f)) on save). For the values
	// SEPA stores this is a no-op; defensively re-canonicalize.
	return decState{val: f, str: pyFloatRepr(f)}
}
