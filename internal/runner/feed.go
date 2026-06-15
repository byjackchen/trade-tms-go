package runner

// feed.go bridges the native moomoo OpenD client (or the protocol-faithful mock,
// switched by TMS_MOOMOO_ADDR — decision 1/2) into the livengine feed seams:
//
//   - MoomooFeed implements livengine.StreamFeed: it subscribes the universe's
//     K-line type, registers a push handler, and forwards each Qot_UpdateKL bar
//     as a core.BarEvent onto the stream the live loop drains. The producer
//     stops on ctx cancellation and closes the channel (no goroutine leak).
//   - MoomooWarmup implements livengine.WarmupProvider over Qot_RequestHistoryKL
//     (paged), supplying the out-of-band SEPA warmup tail.
//
// The client owns reconnect/backoff/keepalive (internal/adapters/moomoo); this
// feed is a thin, ctx-aware bridge.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// MoomooClient is the subset of *moomoo.Client the feed uses (so a test can
// drive a fake). The real client (internal/adapters/moomoo) satisfies it.
type MoomooClient interface {
	Start(ctx context.Context)
	Ready(ctx context.Context) error
	Subscribe(ctx context.Context, symbols []string, kl qotcommon.KLType) error
	RequestHistoryKL(ctx context.Context, symbol string, kl qotcommon.KLType, begin, end time.Time) ([]domain.Bar, error)
	// TradeClient returns the native Trd_* trading surface (paper/live execution).
	// The real *moomoo.Client returns itself; a market-data-only fake may return
	// nil (signal mode never calls it).
	TradeClient() moomoo.TradeClient
	Close() error
}

// MoomooFeed is a livengine.StreamFeed over a moomoo client's Qot_UpdateKL push.
type MoomooFeed struct {
	symbols []string
	kl      qotcommon.KLType
	buffer  int
	log     zerolog.Logger

	// pushCh receives bars from the client's OnKLine callback (set on the
	// client's Options before Start). The feed drains it into the BarEvent
	// stream the live loop consumes.
	pushCh chan domain.Bar

	// coalesce gates the per-symbol pending-bar close-detect path. For intraday
	// K-line types (e.g. KLType_1Min) moomoo re-pushes the FORMING (current) bar
	// many times per second; without coalescing those ~16x/sec/symbol forming
	// pushes saturate pushCh and the engine (which rejects same-TS events as time
	// reversals) only ever sees the FIRST, incomplete bar of each minute. With
	// coalescing the feed emits exactly ONE bar per (symbol, period) downstream —
	// the CLOSED (final OHLCV) one, detected when a strictly-newer barTS arrives.
	// The daily path (one push per day already) keeps the original direct forward,
	// preserving its prompt-forward + flush-on-close semantics.
	coalesce bool
	mu       sync.Mutex            // guards pending
	pending  map[string]domain.Bar // per-symbol forming bar awaiting close
}

// intraday reports whether kl is a sub-daily K-line type whose pushes carry a
// forming (not-yet-closed) current bar that must be coalesced + close-detected.
// Day/Week/Month/Quarter/Year push one closed bar per period and need no gating.
func intraday(kl qotcommon.KLType) bool {
	switch kl {
	case qotcommon.KLType_KLType_1Min,
		qotcommon.KLType_KLType_3Min,
		qotcommon.KLType_KLType_5Min,
		qotcommon.KLType_KLType_15Min,
		qotcommon.KLType_KLType_30Min,
		qotcommon.KLType_KLType_60Min:
		return true
	default:
		return false
	}
}

// NewMoomooFeed builds a feed for the given universe + K-line type. buffer sizes
// the internal push channel (0 -> a sensible default). The returned feed's
// PushHandler MUST be installed as the client's OnKLine before the client
// starts (so pushed bars reach the stream).
func NewMoomooFeed(symbols []string, kl qotcommon.KLType, buffer int, log zerolog.Logger) *MoomooFeed {
	if buffer <= 0 {
		buffer = 1024
	}
	return &MoomooFeed{
		symbols:  symbols,
		kl:       kl,
		buffer:   buffer,
		log:      log.With().Str("component", "moomoo-feed").Logger(),
		pushCh:   make(chan domain.Bar, buffer),
		coalesce: intraday(kl),
		pending:  make(map[string]domain.Bar),
	}
}

// PushHandler is the moomoo.KLineHandler to wire into the client's
// Options.OnKLine. Bars of the wrong K-line type are ignored.
//
// For the daily (and coarser) path each push is already a single CLOSED bar per
// period, so the bar is forwarded directly (preserving prompt-forward semantics).
//
// For the intraday path moomoo re-pushes the FORMING current bar many times per
// second; PushHandler coalesces per symbol and emits only on close-detect:
//   - barTS  > pending.TS: the pending bar just CLOSED -> emit pending downstream
//     (its final OHLCV), then track the new forming bar as pending.
//   - barTS == pending.TS: same minute still forming -> coalesce (keep the latest
//     OHLCV as pending; emit nothing).
//   - barTS  < pending.TS: stale/out-of-order -> ignore.
//
// This collapses the ~16x/sec/symbol forming flood to ONE emit per (symbol,
// period) IN TS ORDER, so pushCh never saturates and the engine sees the closed
// bar. The pending map holds at most #symbols entries (bounded memory).
func (f *MoomooFeed) PushHandler(symbol string, kl qotcommon.KLType, bars []domain.Bar) {
	if kl != f.kl {
		return
	}
	if !f.coalesce {
		for _, b := range bars {
			f.emit(symbol, b)
		}
		return
	}
	for _, b := range bars {
		f.mu.Lock()
		prev, ok := f.pending[symbol]
		switch {
		case !ok || b.TS.After(prev.TS):
			// New (later) period started: the previous forming bar (if any) just
			// closed. Capture it to emit AFTER releasing the lock; never hold the
			// mutex across a channel send.
			f.pending[symbol] = b
			f.mu.Unlock()
			if ok {
				f.emit(symbol, prev)
			}
		case b.TS.Equal(prev.TS):
			// Same period still forming: keep the latest OHLCV, emit nothing.
			f.pending[symbol] = b
			f.mu.Unlock()
		default:
			// Stale (older barTS): ignore.
			f.mu.Unlock()
		}
	}
}

// FlushPending emits every still-forming pending bar downstream and clears the
// pending map. It is the close-detect counterpart at stream shutdown: intraday
// close-detect normally emits a minute's bar when the NEXT minute's first push
// arrives, so the final (most recent) forming minute has no successor to close
// it. The runner calls this on ctx cancellation (Open's drain loop) so the last
// minute is not silently lost. No-op for the daily path (pending stays empty).
func (f *MoomooFeed) FlushPending() {
	f.mu.Lock()
	pend := f.pending
	f.pending = make(map[string]domain.Bar)
	f.mu.Unlock()
	// Emit in deterministic TS-then-symbol order for reproducible shutdown drains.
	rest := make([]domain.Bar, 0, len(pend))
	for _, b := range pend {
		rest = append(rest, b)
	}
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].TS.Equal(rest[j].TS) {
			return rest[i].Symbol < rest[j].Symbol
		}
		return rest[i].TS.Before(rest[j].TS)
	})
	for _, b := range rest {
		f.emit(b.Symbol, b)
	}
}

// emit forwards a (closed) bar onto the feed's internal channel, dropping (with a
// warning) only if the consumer has fallen catastrophically behind (back-pressure
// safety — a wedged consumer must not block the client's single reader goroutine).
func (f *MoomooFeed) emit(symbol string, b domain.Bar) {
	select {
	case f.pushCh <- b:
	default:
		f.log.Warn().Str("symbol", symbol).Time("ts", b.TS).
			Msg("moomoo feed push channel full; dropping bar (consumer fell behind)")
	}
}

// Open subscribes the universe (Qot_Sub) and starts the producer goroutine that
// forwards pushed bars as core.BarEvents until ctx is cancelled (clean drain).
// The client must already be Started + Ready (the runner does that). Open is
// called once by the session.
func (f *MoomooFeed) Open(ctx context.Context) (core.StreamSource, error) {
	ch := make(chan core.StreamEvent, f.buffer)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				// Stream shutdown: flush the final still-forming intraday bar(s)
				// (no successor push will close them), then drain whatever is
				// already buffered in pushCh into ch best-effort, and exit.
				f.FlushPending()
				for {
					select {
					case b := <-f.pushCh:
						select {
						case ch <- core.StreamEvent{Event: core.BarEvent{Bar: b}}:
						default:
							return
						}
					default:
						return
					}
				}
			case b := <-f.pushCh:
				select {
				case ch <- core.StreamEvent{Event: core.BarEvent{Bar: b}}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return core.NewChannelSource(ch), nil
}

// Subscribe issues the Qot_Sub for the feed's universe on the given client. The
// runner calls this after the client is Ready and before Open.
func (f *MoomooFeed) Subscribe(ctx context.Context, c MoomooClient) error {
	if len(f.symbols) == 0 {
		return fmt.Errorf("runner: moomoo feed has no symbols to subscribe")
	}
	if err := c.Subscribe(ctx, f.symbols, f.kl); err != nil {
		return fmt.Errorf("runner: moomoo subscribe: %w", err)
	}
	f.log.Info().Int("symbols", len(f.symbols)).Str("kl_type", f.kl.String()).Msg("subscribed live universe")
	return nil
}

// MoomooWarmup is a livengine.WarmupProvider over Qot_RequestHistoryKL.
type MoomooWarmup struct {
	client MoomooClient
	kl     qotcommon.KLType
	begin  time.Time
	end    time.Time
	mu     sync.Mutex
	cache  map[string][]domain.Bar
}

// NewMoomooWarmup builds a warmup provider that pulls [begin, end) history per
// symbol on demand (cached). kl is the warmup K-line type (daily for SEPA).
func NewMoomooWarmup(client MoomooClient, kl qotcommon.KLType, begin, end time.Time) *MoomooWarmup {
	return &MoomooWarmup{
		client: client,
		kl:     kl,
		begin:  begin,
		end:    end,
		cache:  make(map[string][]domain.Bar),
	}
}

// WarmupBars returns sym's pre-window history (ascending, strictly before the
// run window), pulled via Qot_RequestHistoryKL. Results are cached per symbol.
func (w *MoomooWarmup) WarmupBars(ctx context.Context, sym string) ([]domain.Bar, error) {
	w.mu.Lock()
	if cached, ok := w.cache[sym]; ok {
		w.mu.Unlock()
		return cached, nil
	}
	w.mu.Unlock()

	bars, err := w.client.RequestHistoryKL(ctx, sym, w.kl, w.begin, w.end)
	if err != nil {
		return nil, fmt.Errorf("runner: moomoo warmup history %s: %w", sym, err)
	}
	// Drop any bar dated at/after the run window end (the live feed owns those).
	out := make([]domain.Bar, 0, len(bars))
	for _, b := range bars {
		if !b.TS.UTC().Before(w.end) {
			continue
		}
		out = append(out, b)
	}
	w.mu.Lock()
	w.cache[sym] = out
	w.mu.Unlock()
	return out, nil
}

// compile-time checks.
var (
	_ MoomooClient = (*moomoo.Client)(nil)
)
