package runner

// eod.go is the idempotent engine-REPLAY EOD refresh (P5 locked decision 4),
// replacing Python's non-idempotent pure-Python EOD side path.
//
// For an as_of date it:
//  1. assembles the SAME strategy / portfolio / context / warmup code as
//     backtest (runner.Assembler — decision 3);
//  2. replays [as_of-window, as_of] bars through a livengine.Session in Replay
//     mode (the deterministic batch path, identical to the streaming live path
//     by the consistency proof);
//  3. each strategy's evaluate_intent is UPSERTed into tms.signal_intents
//     idempotently on (strategy_id, symbol, as_of) — a re-run OVERWRITES rather
//     than duplicates — and published to Redis (SignalIntentUpdate);
//  4. each strategy's state_summary is published (StrategyStateUpdate);
//  5. a RefreshReport summarizes the run.
//
// Idempotency is the contract: RunRefresh twice for the same as_of yields the
// SAME tms.signal_intents rows (no dupes) — guaranteed by the UPSERT on the
// partial-unique (strategy_id, symbol, as_of) index (migration 000010).

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/publish"
)

// DefaultEODWarmupCalendarDays is the lookback the EOD replay reconstructs
// strategy state from: 400 calendar days before as_of (the SEPA warmup horizon;
// also more than enough for the sector momentum lookback and pairs z-score
// window). The warmup tail primes WarmupConsumers out of band; the run window
// [warmupStart, as_of] is replayed through the engine.
const DefaultEODWarmupCalendarDays = 400

// EODConfig configures one EOD refresh.
type EODConfig struct {
	// AsOf is the trading date to refresh (the idempotency key date).
	AsOf calendar.Date
	// Strategy selects the strategy set ("multi" is the canonical EOD set).
	Strategy string
	// Tickers is the SEPA stock universe (SEPA / multi).
	Tickers []string
	// ORBSymbol is the ORB instrument (orb path).
	ORBSymbol string
	// StartingBalance seeds the informational health NAV (USD; default 100000).
	StartingBalance float64
	// WindowCalendarDays overrides DefaultEODWarmupCalendarDays.
	WindowCalendarDays int
	// TraderID is the Redis namespace for published updates ("" -> no publish).
	TraderID string
}

// RefreshReport summarizes an EOD refresh (decision 4 "RefreshReport summary").
type RefreshReport struct {
	// AsOf is the refreshed date (YYYY-MM-DD).
	AsOf string `json:"as_of"`
	// Strategy is the strategy set refreshed.
	Strategy string `json:"strategy"`
	// WindowStart is the replay window start (YYYY-MM-DD).
	WindowStart string `json:"window_start"`
	// Tickers is the resolved instrument universe count.
	InstrumentCount int `json:"instrument_count"`
	// BarsReplayed is how many bars were replayed through the engine.
	BarsReplayed int `json:"bars_replayed"`
	// IntentRows is how many signal-intent rows were upserted.
	IntentRows int `json:"intent_rows"`
	// IntentsEmitted is how many evaluate_intent calls fired (per strategy per ts).
	IntentsEmitted int `json:"intents_emitted"`
	// WouldSubmitOrders is the count of would-be orders (telemetry; signal mode
	// places none).
	WouldSubmitOrders int64 `json:"would_submit_orders"`
	// PublishErrors is how many Redis publishes failed (best-effort transport).
	PublishErrors int `json:"publish_errors"`
	// RSRankStamped is how many SEPA intents were stamped with a cross-sectional
	// RS rank (TMS enhancement; not in the Python SEPA reference).
	RSRankStamped int `json:"rs_rank_stamped"`
}

// EOD runs idempotent EOD refreshes.
type EOD struct {
	pool      *pgxpool.Pool
	assembler *Assembler
	store     *publish.Store
	log       zerolog.Logger
}

// NewEOD builds an EOD runner. paramsDir is config.StrategyParamsDir.
func NewEOD(pool *pgxpool.Pool, paramsDir string, log zerolog.Logger) *EOD {
	return &EOD{
		pool:      pool,
		assembler: NewAssembler(pool, paramsDir),
		store:     publish.NewStore(pool),
		log:       log.With().Str("component", "eod-refresh").Logger(),
	}
}

// RunRefresh executes one idempotent EOD refresh and returns a RefreshReport.
// publisher may be nil (a no-op publisher) for a PG-only refresh.
func (e *EOD) RunRefresh(ctx context.Context, cfg EODConfig, publisher *publish.Publisher) (*RefreshReport, error) {
	if cfg.AsOf.IsZero() {
		return nil, fmt.Errorf("eod: as_of date is required")
	}
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = "multi"
	}
	startBal := cfg.StartingBalance
	if startBal <= 0 {
		startBal = 100000
	}
	windowDays := cfg.WindowCalendarDays
	if windowDays <= 0 {
		windowDays = DefaultEODWarmupCalendarDays
	}
	windowStart := cfg.AsOf.AddDays(-windowDays)

	log := e.log.With().Str("as_of", cfg.AsOf.String()).Str("strategy", strategy).Logger()
	log.Info().Str("window_start", windowStart.String()).Msg("eod refresh starting")

	// (1) Assemble the strategy set from DB params (same as backtest). EOD is a
	// batch replay (not a live moomoo subscription), so it keeps the FULL
	// survivor-bias-free universe — UniverseLimit is deliberately left 0 (no cap),
	// exactly as backtest/hyperopt do, so the EOD signal set matches a backtest's.
	as, err := e.assembler.Assemble(ctx, AssemblyInput{
		Strategy:        strategy,
		Tickers:         cfg.Tickers,
		ORBSymbol:       cfg.ORBSymbol,
		StartingBalance: startBal,
	}, windowStart, cfg.AsOf)
	if err != nil {
		return nil, err
	}

	// (2) Load the dispatch-ordered replay window bars.
	bars, err := e.assembler.LoadWindowBars(ctx, as, windowStart, cfg.AsOf)
	if err != nil {
		return nil, err
	}

	startMoney, err := domain.MoneyFromFloat64(startBal)
	if err != nil {
		return nil, fmt.Errorf("eod: invalid starting balance %.2f: %w", startBal, err)
	}

	// (3) Build the EOD sink (UPSERT mode keyed on as_of) + live session.
	asOfTime := time.Date(cfg.AsOf.Year, cfg.AsOf.Month, cfg.AsOf.Day, 0, 0, 0, 0, time.UTC)
	sink := NewSink(SinkOptions{
		Store:     e.store,
		Publisher: publisher,
		Mode:      SinkUpsert,
		AsOf:      &asOfTime,
		Logger:    log,
	})

	sess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecSignal,
		Strategies:      as.Assembly.Strategies,
		Gate:       as.Assembly.Gate,
		Context:         as.Assembly.Context,
		SPYSymbol:       as.SPYSymbol,
		Warmup:          as.Warmup,
		WarmupSymbols:   as.WarmupSymbols,
		StartingBalance: startMoney,
		Sink:            sink,
	})
	if err != nil {
		return nil, fmt.Errorf("eod: building live session: %w", err)
	}

	// (4) Prime warmup, then Replay the window through the SAME bar-handling
	// path the streaming live engine uses (the consistency proof guarantees the
	// emitted intents match the streaming path).
	if err := sess.Prime(ctx); err != nil {
		return nil, fmt.Errorf("eod: priming warmup: %w", err)
	}
	if err := sess.Replay(ctx, bars); err != nil {
		return nil, fmt.Errorf("eod: replay: %w", err)
	}

	// (4.5) TMS ENHANCEMENT (not in the Python SEPA reference): cross-sectional
	// RS rank. After the as-of intents are persisted, compute the universe RS rank
	// from tms.bars_daily as-of cfg.AsOf in ONE set-based query and stamp it (plus
	// the RS-dependent buy_readiness) onto each SEPA intent's JSONB. This makes
	// every forming signal rankable on the watchlist. Best-effort within the run:
	// an RS-stamp failure is logged but does not fail the whole refresh (the
	// intents are already durably persisted; RS is an enrichment).
	rsStamped := 0
	if e.pool != nil {
		var rserr error
		rsStamped, rserr = stampRSRank(ctx, e.pool, asOfTime, as.Tickers)
		if rserr != nil {
			log.Warn().Err(rserr).Msg("rs-rank stamping failed; intents persisted without rs_rank")
		}
	}

	// (5) Publish the watchlist (the universe the refresh tracked) for cockpit
	// continuity (best-effort).
	if publisher != nil {
		if perr := publisher.PublishWatchlist(ctx, as.Tickers, asOfTime.UnixNano()); perr != nil {
			log.Warn().Err(perr).Msg("redis publish watchlist failed")
		}
	}

	report := &RefreshReport{
		AsOf:              cfg.AsOf.String(),
		Strategy:          strategy,
		WindowStart:       windowStart.String(),
		InstrumentCount:   len(as.Tickers),
		BarsReplayed:      sess.BarsSeen(),
		IntentRows:        sink.IntentRows(),
		IntentsEmitted:    sess.EmittedIntents(),
		WouldSubmitOrders: sess.Executor().WouldSubmitCount(),
		PublishErrors:     sink.PublishErrors(),
		RSRankStamped:     rsStamped,
	}
	log.Info().
		Int("bars", report.BarsReplayed).
		Int("intent_rows", report.IntentRows).
		Int("intents", report.IntentsEmitted).
		Msg("eod refresh complete (idempotent upsert)")
	return report, nil
}
