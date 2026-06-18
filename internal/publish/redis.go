package publish

// redis.go is the live transport layer: it publishes Signal /
// StrategyState / PortfolioHealth / position / watchlist updates to Redis
// streams using the key shape defined in api-ws-redis.md §2.1/§2.2:
//
//	trader-{trader_id}:stream:{topic}
//
// Each stream entry is a field map {topic, payload}; only `payload` is consumed
// by readers (a JSON document string, §2.2). Redis is TRANSPORT ONLY (decision
// 5) — the durable truth is Postgres; a cockpit reconstructs from PG and tails
// these streams for continuity. Publish failures never abort the engine: the
// SignalSink wrapping this publisher logs and continues (the DB write is the
// gate that can stop the node, not Redis).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Stream topics (api-ws-redis.md §2.4). These are the exact topic
// strings the cockpit subscribes to; the per-trader key is
// trader-{id}:stream:{topic}.
const (
	TopicSignal          = "data.SignalUpdate"
	TopicStrategyState   = "data.StrategyStateUpdate"
	TopicPortfolioHealth = "data.PortfolioHealthUpdate"
	// TopicWatchlist is an additive topic for the live universe the
	// node is tracking (the cockpit watchlist panel reads it for continuity).
	// Kept under the same data.* namespace.
	TopicWatchlist = "data.WatchlistUpdate"
	// TopicBar is the live BAR tape: every closed K-line bar the streaming feed
	// emits (after intraday forming-coalescing), for an ephemeral cockpit ticker.
	// PURE TRANSPORT — never persisted (no PG row); the Redis stream is MAXLEN-
	// bounded and the UI only tails a small rolling window.
	TopicBar = "data.BarUpdate"
)

// MaxStreamLen caps each stream's length (XADD MAXLEN ~). Redis is transport;
// the durable history is in PG, so a bounded stream is sufficient for cockpit
// continuity and prevents unbounded memory growth on a long-running node.
const MaxStreamLen = 10000

// StreamKey renders the per-trader stream key
// (trader-{id}:stream:{topic}) for topic. Trader ids may contain arbitrary
// printable characters; they are used verbatim (the key shape does not escape them).
func StreamKey(traderID, topic string) string {
	return fmt.Sprintf("trader-%s:stream:%s", traderID, topic)
}

// Publisher writes live updates to Redis streams under one trader id. It is
// safe for concurrent use (go-redis client is). A nil *Publisher is a no-op
// publisher (Redis-less deployments / dry runs), so callers need not nil-check.
type Publisher struct {
	rdb      *redis.Client
	traderID string
	maxLen   int64
	log      zerolog.Logger
	now      func() time.Time
}

// Options configures a Publisher.
type Options struct {
	// TraderID is the Redis namespace (sessions.trader_id). Required.
	TraderID string
	// MaxStreamLen overrides MaxStreamLen (0 = default).
	MaxStreamLen int64
	// Logger is the structured logger.
	Logger zerolog.Logger
	// Now overrides the clock (tests); nil = time.Now.
	Now func() time.Time
}

// NewPublisher builds a Redis stream publisher. A nil client yields a no-op
// publisher (every Publish* call returns nil without touching Redis).
func NewPublisher(rdb *redis.Client, opts Options) *Publisher {
	if rdb == nil {
		return nil
	}
	maxLen := opts.MaxStreamLen
	if maxLen <= 0 {
		maxLen = MaxStreamLen
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Publisher{
		rdb:      rdb,
		traderID: opts.TraderID,
		maxLen:   maxLen,
		log:      opts.Logger.With().Str("component", "publish").Str("trader_id", opts.TraderID).Logger(),
		now:      now,
	}
}

// TraderID returns the trader namespace this publisher writes under ("" for a
// no-op publisher).
func (p *Publisher) TraderID() string {
	if p == nil {
		return ""
	}
	return p.traderID
}

// nowNS returns the current wall clock in int64 ns UTC (the ts_event /
// ts_init unit, api-ws-redis.md §2.4).
func (p *Publisher) nowNS() int64 { return p.now().UTC().UnixNano() }

// publish XADDs payload (a JSON object) onto topic's stream with the {topic,
// payload} field map and a bounded MAXLEN. A nil publisher is a no-op.
func (p *Publisher) publish(ctx context.Context, topic string, payload any) error {
	if p == nil {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("publish: marshal %s: %w", topic, err)
	}
	key := StreamKey(p.traderID, topic)
	if err := p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: key,
		MaxLen: p.maxLen,
		Approx: true,
		Values: map[string]any{"topic": topic, "payload": string(body)},
	}).Err(); err != nil {
		return fmt.Errorf("publish: XADD %s: %w", key, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wire envelopes (api-ws-redis.md §5). ts_event / ts_init are int64 ns UTC.
// ---------------------------------------------------------------------------

// SignalEnvelope is the SignalUpdate wire shape (§2.4 / §5.9):
// the outer {strategy_id, symbol, signal_json, ts_event, ts_init}. signal_json
// is the unwrapped SignalUnion variant payload (an object, NOT a string —
// the reader JSON-parses-if-string-else-as-is, §3.18; we emit the object form).
type SignalEnvelope struct {
	StrategyID string          `json:"strategy_id"`
	Symbol     string          `json:"symbol"`
	SignalJSON json.RawMessage `json:"signal_json"`
	TSEvent    int64           `json:"ts_event"`
	TSInit     int64           `json:"ts_init"`
}

// BarEnvelope is the BarUpdate wire shape: one closed K-line bar for the live
// tape. OHLC use domain.Price's decimal JSON; ts_event is the bar instant (ns).
type BarEnvelope struct {
	Symbol  string       `json:"symbol"`
	TSEvent int64        `json:"ts_event"`
	Open    domain.Price `json:"open"`
	High    domain.Price `json:"high"`
	Low     domain.Price `json:"low"`
	Close   domain.Price `json:"close"`
	Volume  int64        `json:"volume"`
	TSInit  int64        `json:"ts_init"`
}

// StrategyStateEnvelope is the StrategyStateUpdate wire shape (§5.8):
// {strategy_id, state_json, ts_event, ts_init}. state_json is an opaque JSON
// string (the strategy's state_summary serialized).
type StrategyStateEnvelope struct {
	StrategyID string `json:"strategy_id"`
	StateJSON  string `json:"state_json"`
	TSEvent    int64  `json:"ts_event"`
	TSInit     int64  `json:"ts_init"`
}

// PortfolioHealthEnvelope is the PortfolioHealthUpdate wire shape (§5.11). The
// wire carries floats (the REST endpoint stringifies them).
type PortfolioHealthEnvelope struct {
	DayPnL           float64 `json:"day_pnl"`
	DayPnLPct        float64 `json:"day_pnl_pct"`
	DailyLossHalt    bool    `json:"daily_loss_halt"`
	HaltHeadroomPct  float64 `json:"halt_headroom_pct"`
	ConcentrationPct float64 `json:"concentration_pct"`
	TSEvent          int64   `json:"ts_event"`
	TSInit           int64   `json:"ts_init"`
}

// WatchlistEnvelope is the additive WatchlistUpdate wire shape: the symbols the
// live node is currently tracking, with the as-of timestamp.
type WatchlistEnvelope struct {
	Symbols []string `json:"symbols"`
	TSEvent int64    `json:"ts_event"`
	TSInit  int64    `json:"ts_init"`
}

// PositionEnvelope is the empty-book signal-mode position snapshot (decision 6:
// no positions in signal mode). Published so the cockpit position panel has
// continuity (an explicit "flat" snapshot rather than a stale read).
type PositionEnvelope struct {
	Positions []any `json:"positions"`
	TSEvent   int64 `json:"ts_event"`
	TSInit    int64 `json:"ts_init"`
}

// ---------------------------------------------------------------------------
// Publish methods
// ---------------------------------------------------------------------------

// PublishSignal publishes one normalized signal as a SignalUpdate.
// tsEventNS is the engine ts_event (the bar as-of ns); ts_init is wall-clock.
func (p *Publisher) PublishSignal(ctx context.Context, n NormalizedSignal, tsEventNS int64) error {
	if p == nil {
		return nil
	}
	body, err := n.SignalJSON()
	if err != nil {
		return err
	}
	return p.publish(ctx, TopicSignal, SignalEnvelope{
		StrategyID: n.StrategyID,
		Symbol:     n.Symbol,
		SignalJSON: body,
		TSEvent:    tsEventNS,
		TSInit:     p.nowNS(),
	})
}

// PublishBar publishes one closed bar onto the live BAR tape (TopicBar). Pure
// transport: best-effort, never persisted. Safe on a nil publisher.
func (p *Publisher) PublishBar(ctx context.Context, b domain.Bar) error {
	if p == nil {
		return nil
	}
	return p.publish(ctx, TopicBar, BarEnvelope{
		Symbol:  b.Symbol,
		TSEvent: b.TS.UTC().UnixNano(),
		Open:    b.Open,
		High:    b.High,
		Low:     b.Low,
		Close:   b.Close,
		Volume:  b.Volume,
		TSInit:  p.nowNS(),
	})
}

// PublishStrategyState publishes a strategy's state_summary as a
// StrategyStateUpdate. summary is the JSON-serializable state object; it is
// serialized to the opaque state_json string.
func (p *Publisher) PublishStrategyState(ctx context.Context, strategyID string, summary any, tsEventNS int64) error {
	if p == nil {
		return nil
	}
	b, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("publish: marshal strategy state %s: %w", strategyID, err)
	}
	return p.publish(ctx, TopicStrategyState, StrategyStateEnvelope{
		StrategyID: strategyID,
		StateJSON:  string(b),
		TSEvent:    tsEventNS,
		TSInit:     p.nowNS(),
	})
}

// PublishPortfolioHealth publishes a PortfolioHealthUpdate from a snapshot.
func (p *Publisher) PublishPortfolioHealth(ctx context.Context, env PortfolioHealthEnvelope) error {
	if p == nil {
		return nil
	}
	if env.TSInit == 0 {
		env.TSInit = p.nowNS()
	}
	return p.publish(ctx, TopicPortfolioHealth, env)
}

// PublishWatchlist publishes the current tracked-symbol set.
func (p *Publisher) PublishWatchlist(ctx context.Context, symbols []string, tsEventNS int64) error {
	if p == nil {
		return nil
	}
	if symbols == nil {
		symbols = []string{}
	}
	return p.publish(ctx, TopicWatchlist, WatchlistEnvelope{
		Symbols: symbols,
		TSEvent: tsEventNS,
		TSInit:  p.nowNS(),
	})
}

// PublishEmptyPositions publishes the signal-mode flat position book (decision
// 6). Always an empty list — there are no positions in signal mode.
func (p *Publisher) PublishEmptyPositions(ctx context.Context, tsEventNS int64) error {
	if p == nil {
		return nil
	}
	return p.publish(ctx, TopicPosition(), PositionEnvelope{
		Positions: []any{},
		TSEvent:   tsEventNS,
		TSInit:    p.nowNS(),
	})
}

// TopicPosition is the signal-mode position topic. In signal mode there are no
// positions, so we publish a single aggregate empty-book snapshot under a
// data.* stream topic for cockpit continuity.
func TopicPosition() string { return "data.PositionUpdate" }
