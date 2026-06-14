package parity

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

// Script is the canonical deterministic order script: a fixed sequence of
// (date, ticker, side, qty) intents driving both engines. It mirrors the JSON
// at testdata/parity/script_*.json.
type Script struct {
	SchemaVersion   int               `json:"schema_version"`
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	Venue           string            `json:"venue"`
	StrategyID      string            `json:"strategy_id"`
	StartingBalance float64           `json:"starting_balance_usd"`
	Tickers         []string          `json:"tickers"`
	StartDate       string            `json:"start_date"`
	EndDate         string            `json:"end_date"`
	Scenarios       map[string]string `json:"scenarios"`
	Intents         []ScriptIntent    `json:"intents"`
}

// ScriptIntent is one scripted instruction in JSON form.
type ScriptIntent struct {
	Date   string `json:"date"`
	Ticker string `json:"ticker"`
	Side   string `json:"side"`
	Qty    int64  `json:"qty"`
}

// LoadScript reads and validates a parity script JSON file.
func LoadScript(path string) (*Script, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("parity: reading script %s: %w", path, err)
	}
	var s Script
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parity: parsing script %s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Script) validate() error {
	if s.StrategyID == "" {
		return fmt.Errorf("%w: parity script has empty strategy_id", domain.ErrInvalidArgument)
	}
	if len(s.Tickers) == 0 {
		return fmt.Errorf("%w: parity script has no tickers", domain.ErrInvalidArgument)
	}
	if s.StartingBalance <= 0 {
		return fmt.Errorf("%w: parity script starting balance must be positive", domain.ErrInvalidArgument)
	}
	if len(s.Intents) == 0 {
		return fmt.Errorf("%w: parity script has no intents", domain.ErrInvalidArgument)
	}
	known := make(map[string]struct{}, len(s.Tickers))
	for _, t := range s.Tickers {
		known[t] = struct{}{}
	}
	for i, in := range s.Intents {
		if _, ok := known[in.Ticker]; !ok {
			return fmt.Errorf("%w: parity intent %d references unknown ticker %q",
				domain.ErrInvalidArgument, i, in.Ticker)
		}
		side, err := domain.ParseSignalSide(in.Side)
		if err != nil {
			return fmt.Errorf("parity intent %d: %w", i, err)
		}
		if side != domain.SideFlat && in.Qty <= 0 {
			return fmt.Errorf("%w: parity intent %d (%s %s) needs positive qty",
				domain.ErrInvalidArgument, i, in.Side, in.Ticker)
		}
		if _, err := time.Parse("2006-01-02", in.Date); err != nil {
			return fmt.Errorf("%w: parity intent %d has bad date %q: %v",
				domain.ErrInvalidArgument, i, in.Date, err)
		}
	}
	return nil
}

// EngineConfig builds the engine Config that runs this script over feed, using
// the zero-cost nautilus-compat profile (the parity GATE). Intents are grouped
// into a single scripted strategy (the script's StrategyID), preserving order.
func (s *Script) EngineConfig() (engine.Config, error) {
	startDate, err := calendar.ParseDate(s.StartDate)
	if err != nil {
		return engine.Config{}, fmt.Errorf("parity: start_date: %w", err)
	}
	endDate, err := calendar.ParseDate(s.EndDate)
	if err != nil {
		return engine.Config{}, fmt.Errorf("parity: end_date: %w", err)
	}
	balance, err := domain.MoneyFromFloat64(s.StartingBalance)
	if err != nil {
		return engine.Config{}, fmt.Errorf("parity: starting balance: %w", err)
	}
	intents := make([]engine.Intent, 0, len(s.Intents))
	for i, in := range s.Intents {
		side, err := domain.ParseSignalSide(in.Side)
		if err != nil {
			return engine.Config{}, fmt.Errorf("parity intent %d: %w", i, err)
		}
		d, err := time.Parse("2006-01-02", in.Date)
		if err != nil {
			return engine.Config{}, fmt.Errorf("parity intent %d date: %w", i, err)
		}
		intents = append(intents, engine.Intent{
			Date:   time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC),
			Ticker: in.Ticker,
			Side:   side,
			Qty:    domain.Qty(in.Qty),
		})
	}
	return engine.Config{
		Tickers:         s.Tickers,
		Start:           startDate,
		End:             endDate,
		StartingBalance: balance,
		Profile:         engine.ProfileNautilusCompat,
		Strategies: []engine.StrategySpec{
			{ID: s.StrategyID, Intents: intents},
		},
	}, nil
}
