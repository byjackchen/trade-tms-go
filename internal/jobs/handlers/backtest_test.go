package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

func TestParseBacktestParams(t *testing.T) {
	t.Run("full payload", func(t *testing.T) {
		p, err := parseBacktestParams(json.RawMessage(
			`{"tickers":["AAPL"],"start":"2024-01-02","end":"2024-12-31","starting_balance":50000,"fill_profile":"realistic","strategy":"scripted","kind":"smoke","seed":7}`))
		require.NoError(t, err)
		assert.Equal(t, []string{"AAPL"}, p.Tickers)
		assert.Equal(t, "2024-01-02", p.Start)
		require.NotNil(t, p.StartingBalance)
		assert.Equal(t, 50000.0, *p.StartingBalance)
		assert.Equal(t, "realistic", p.FillProfile)
		assert.Equal(t, int64(7), p.Seed)
	})
	t.Run("missing start/end is error", func(t *testing.T) {
		_, err := parseBacktestParams(json.RawMessage(`{"tickers":["AAPL"]}`))
		require.Error(t, err)
	})
	t.Run("unknown field rejected", func(t *testing.T) {
		_, err := parseBacktestParams(json.RawMessage(`{"start":"a","end":"b","bogus":1}`))
		require.Error(t, err)
	})
}

func TestBuildIntents(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		intents, err := buildIntents([]intentJSON{
			{Date: "2024-01-03", Ticker: "AAPL", Side: "LONG", Qty: 100},
			{Date: "2024-01-10", Ticker: "AAPL", Side: "FLAT"},
		})
		require.NoError(t, err)
		require.Len(t, intents, 2)
		assert.Equal(t, domain.SideLong, intents[0].Side)
		assert.Equal(t, domain.Qty(100), intents[0].Qty)
		assert.Equal(t, domain.SideFlat, intents[1].Side)
	})
	t.Run("bad date", func(t *testing.T) {
		_, err := buildIntents([]intentJSON{{Date: "01/03/2024", Ticker: "X", Side: "LONG", Qty: 1}})
		require.Error(t, err)
	})
	t.Run("bad side", func(t *testing.T) {
		_, err := buildIntents([]intentJSON{{Date: "2024-01-03", Ticker: "X", Side: "buy", Qty: 1}})
		require.Error(t, err)
	})
}

// newTestBacktest builds a handler with a nil pool — buildConfig never touches
// the DB for an explicit ticker list, so config validation can be unit tested.
func newTestBacktest(t *testing.T) *Backtest {
	t.Helper()
	_, err := NewBacktest(nil, "runs", zerolog.Nop())
	require.Error(t, err) // nil pool rejected by the constructor
	// Build directly for unit testing the pure validation helpers (buildConfig
	// for an explicit ticker list never touches the DB).
	return &Backtest{runsDir: "runs", log: zerolog.Nop(), now: time.Now}
}

func TestBuildConfigValidation(t *testing.T) {
	h := newTestBacktest(t)
	ctx := context.Background()

	t.Run("explicit tickers", func(t *testing.T) {
		cfg, asm, runTS, err := h.buildConfig(ctx, backtestParams{
			Tickers: []string{"AAPL", "KO"},
			Start:   "2024-01-02", End: "2024-12-31",
			Strategy: "scripted",
		})
		require.NoError(t, err)
		assert.Nil(t, asm) // scripted path: no real-strategy assembly
		assert.Equal(t, []string{"AAPL", "KO"}, cfg.Tickers)
		assert.Equal(t, engine.ProfileRealistic, cfg.Profile) // default profile is now realistic
		assert.Equal(t, domain.MustMoney("100000.00"), cfg.StartingBalance) // default
		assert.NotEmpty(t, runTS)
	})
	t.Run("run_ts honored", func(t *testing.T) {
		_, _, runTS, err := h.buildConfig(ctx, backtestParams{
			Tickers: []string{"AAPL"}, Start: "2024-01-02", End: "2024-12-31",
			RunTS: "2026-01-01_00-00-00",
		})
		require.NoError(t, err)
		assert.Equal(t, "2026-01-01_00-00-00", runTS)
	})
	t.Run("unsupported strategy", func(t *testing.T) {
		_, _, _, err := h.buildConfig(ctx, backtestParams{
			Tickers: []string{"AAPL"}, Start: "2024-01-02", End: "2024-12-31", Strategy: "bogus",
		})
		require.Error(t, err)
	})
	t.Run("invalid start date", func(t *testing.T) {
		_, _, _, err := h.buildConfig(ctx, backtestParams{
			Tickers: []string{"AAPL"}, Start: "nope", End: "2024-12-31",
		})
		require.Error(t, err)
	})
	t.Run("no tickers, no universe", func(t *testing.T) {
		_, _, _, err := h.buildConfig(ctx, backtestParams{Start: "2024-01-02", End: "2024-12-31"})
		require.Error(t, err)
	})
	t.Run("unknown fill profile", func(t *testing.T) {
		_, _, _, err := h.buildConfig(ctx, backtestParams{
			Tickers: []string{"AAPL"}, Start: "2024-01-02", End: "2024-12-31", FillProfile: "magic",
		})
		require.Error(t, err)
	})
	t.Run("realistic profile params", func(t *testing.T) {
		cfg, _, _, err := h.buildConfig(ctx, backtestParams{
			Tickers: []string{"AAPL"}, Start: "2024-01-02", End: "2024-12-31",
			FillProfile: "realistic",
			Realistic:   &realisticJSON{SlippageBps: 2.0, CommissionBps: 1.0, CommissionPerShare: 0.01},
		})
		require.NoError(t, err)
		assert.Equal(t, engine.ProfileRealistic, cfg.Profile)
		assert.Equal(t, 2.0, cfg.Realistic.SlippageBps)
		assert.Equal(t, domain.MustMoney("0.01"), cfg.Realistic.CommissionPerShare)
	})
}
