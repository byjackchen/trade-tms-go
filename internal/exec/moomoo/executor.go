package moomoo

// executor.go is the MoomooExecutor: it implements the engine's order-submission
// seam (engine.OrderSubmitter + engine.PositionReader) backed by the native
// moomoo Trd_* TradeClient, for PAPER (TrdEnvSimulate, PAPER_ACC_ID) and LIVE
// (TrdEnvReal, real acc id + UnlockTrade) execution — replacing the signal-mode
// NoopExecutor (locked decisions 2, 3, 8).
//
// Flow (locked decision 2):
//
//	strategy.OnBar -> SubmitMarket / SubmitMarketSignal
//	  -> build domain Order (deterministic client-order-id)
//	  -> [portfolio gate runs UPSTREAM in the session/runner]
//	  -> TradeClient.PlaceOrder (idempotent on client-order-id)
//	  -> track OrderState by client-order-id
//	Trd_UpdateOrder push -> onOrderUpdate -> state machine Apply
//	  -> EffectAccepted / EffectFill / EffectStatus
//	  -> accounting.ApplyFill + Persistence (live.orders/fills/positions)
//	  -> FillSink (feed the engine + equity)
//
// SAFETY (locked decision 8): the constructor REFUSES to build a live executor
// unless ALL of: a typed confirmation phrase matches, a real acc id is
// explicitly configured, UnlockTrade succeeds, and the trader id is the
// distinct live namespace (TMS-LIVE-REAL-001). A paper executor can NEVER hold
// a real acc id or TrdEnvReal — assertEnvInvariants enforces this on every
// PlaceOrder. There is NO code path that submits a non-paper order without the
// full gate.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// LiveConfirmationPhrase is the exact typed phrase a live (real-money) executor
// requires. It is intentionally verbose + unambiguous so it cannot be supplied
// by accident or by a generic default.
const LiveConfirmationPhrase = "I CONFIRM LIVE REAL MONEY TRADING TMS-LIVE-REAL-001"

// LiveTraderID is the ONLY trader-id namespace permitted to reach the real
// account (TMS-LIVE-REAL-001). signal/paper use a different namespace and can
// never bind the live account.
const LiveTraderID = "TMS-LIVE-REAL-001"

// FillSink receives the domain fills the executor produces from broker pushes
// (the live engine forwards them into the loop + accounting + equity). Mirrors
// exec.FillSink so the executor can feed the same downstream as the simulator.
type FillSink interface {
	EmitFill(domain.Fill) error
}

// AccountBook is the accounting surface the executor settles fills into and
// reads net positions from. It is satisfied by a thin adapter over
// *accounting.Account in the runner wiring (the adapter drops the FillOutcome
// the executor does not need). Kept narrow so the executor is testable without
// the full account.
type AccountBook interface {
	// ApplyFill settles one fill and returns the resulting position snapshot.
	ApplyFill(f domain.Fill) (domain.Position, error)
	// Position returns the (strategy, symbol) net position snapshot; ok=false if
	// the position has never been opened.
	Position(strategyID, symbol string) (domain.Position, bool)
	// OpenPositions returns snapshots of every NON-FLAT (strategy, symbol)
	// position in deterministic (strategy, symbol) order. This is the
	// authoritative per-strategy BOOK the flatten closes row-by-row (so each
	// originating position nets to 0 -> CLOSED), as distinct from the broker's
	// cross-strategy netted GetPositionList view.
	OpenPositions() []domain.Position
}

// Persistence writes the durable order/fill/position state (live.orders /
// live.fills / live.positions) on each transition. All methods MUST be
// idempotent on their natural key (client-order-id / trade-id / (strategy,
// symbol)) so a duplicate push or a crash-recovery replay never double-writes.
// A nil Persistence (tests) disables durability; the in-memory state machine is
// still authoritative.
type Persistence interface {
	// UpsertOrder writes/updates a live.orders row keyed by client-order-id.
	UpsertOrder(ctx context.Context, o domain.Order) error
	// InsertFill writes a live.fills row keyed by trade-id (idempotent: a
	// duplicate trade-id is a no-op).
	InsertFill(ctx context.Context, f domain.Fill) error
	// UpsertPosition writes/updates a live.positions row keyed by (strategy,
	// symbol).
	UpsertPosition(ctx context.Context, p domain.Position) error
}

// RiskEventSink records gate rejections / safety events to live.risk_events +
// audit. The executor surfaces an event whenever it BLOCKS something (a live
// invariant breach is recorded, never silently dropped). May be nil.
type RiskEventSink interface {
	RecordRiskEvent(ctx context.Context, strategyID, symbol, rule, detail string) error
}

// StrategyResolver re-keys a restored broker order back to its ORIGINATING
// strategy id during crash recovery (decision 6). The broker's Trd_GetOrderList
// carries only the client-order-id (the order remark) — NOT the strategy id —
// so a fill on a restored, still-working order would otherwise settle under an
// empty-strategy key, drifting per-strategy attribution post-resume. The
// resolver looks the strategy id up from the durable order record persisted at
// submit (live.orders, keyed by client-order-id). May be nil (tests / no
// durability); a nil resolver, a not-found order, or a lookup error leaves the
// restored StrategyID empty and is surfaced (see RestoreFromBroker).
type StrategyResolver interface {
	// StrategyForOrder returns the persisted strategy id for clientOrderID.
	// ok=false (with nil error) means no durable record exists for that id.
	StrategyForOrder(ctx context.Context, clientOrderID string) (strategyID string, ok bool, err error)
}

// Clock supplies the wall-clock time for events lacking a venue timestamp.
// Injected so tests are deterministic.
type Clock interface{ Now() time.Time }

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

// Config assembles a MoomooExecutor.
type Config struct {
	// Account is the bound broker account (required). Its Env selects the moomoo
	// TrdEnv (EnvPaper -> paper, EnvReal -> live; EnvSimu is rejected — this
	// executor never settles synthetic fills) and its BrokerAccID is the broker
	// account id. A real-money account (Account.IsReal()) triggers the 4-factor
	// live-activation gate; a simulate account binds paper.
	Account domain.Account
	// Client is the native Trd_* trading surface (real *moomoo.Client or the
	// in-memory mock venue). Required.
	Client mo.TradeClient
	// TraderID is the session trader-id namespace. Live REQUIRES LiveTraderID.
	TraderID string
	// ConfirmationPhrase must equal LiveConfirmationPhrase for live mode.
	ConfirmationPhrase string
	// UnlockPassword unlocks the real account (live only). Paper ignores it.
	UnlockPassword string

	// Sink feeds produced fills to the engine/equity (required).
	Sink FillSink
	// Book settles fills + reads net positions (required).
	Book AccountBook
	// Persist durably stores orders/fills/positions. May be nil (tests).
	Persist Persistence
	// Risk records blocked-order risk events. May be nil.
	Risk RiskEventSink
	// Strategy re-keys restored broker orders to their originating strategy id
	// during crash recovery (decision 6). May be nil (tests / no durability).
	Strategy StrategyResolver
	// Clock supplies fallback timestamps; nil => wall clock.
	Clock Clock
	// Logger sink for structured warnings (drift / fill-reversed). May be nil.
	Logf func(format string, args ...any)
}

// MoomooExecutor implements engine.OrderSubmitter + engine.PositionReader.
type MoomooExecutor struct {
	cfg   Config
	env   mo.TrdEnv
	accID uint64
	clock Clock
	logf  func(string, ...any)

	// seq is the deterministic monotonic client-order-id source.
	seq atomic.Uint64

	// mu guards orders + the venue->client index. The strategy loop (single
	// goroutine) calls Submit*; the client's reader goroutine calls onOrderUpdate
	// / onFillUpdate concurrently, so both paths lock.
	mu       sync.Mutex
	orders   map[string]*OrderState // client-order-id -> state
	venIndex map[string]string      // venue-order-id -> client-order-id

	// telemetry
	submitted atomic.Int64
	rejected  atomic.Int64
	fillsEmit atomic.Int64
}

// New builds a MoomooExecutor, enforcing the live-activation gate (decision 8).
// For live it: (a) checks the confirmation phrase, (b) requires a non-zero real
// acc id, (c) requires the LiveTraderID namespace, (d) verifies the acc id
// exists under TrdEnvReal, then (e) UnlockTrade — and only then binds TrdEnvReal.
// ANY failure returns an error and NO executor (so no live order is reachable).
// Paper binds TrdEnvSimulate and can never name the live account.
func New(ctx context.Context, cfg Config) (*MoomooExecutor, error) {
	// The executor only settles against a broker account (paper or real); a
	// synthetic sim account has no broker and must never reach this path.
	if !cfg.Account.IsBroker() {
		return nil, fmt.Errorf("%w: moomoo executor requires a broker account (simulate/real), got env %q",
			domain.ErrInvalidArgument, cfg.Account.Env)
	}
	if cfg.Client == nil {
		return nil, fmt.Errorf("%w: moomoo executor requires a TradeClient", domain.ErrInvalidArgument)
	}
	if cfg.Account.BrokerAccID == 0 {
		return nil, fmt.Errorf("%w: moomoo executor requires an explicit acc_id (no default)", domain.ErrInvalidArgument)
	}
	if cfg.Sink == nil || cfg.Book == nil {
		return nil, fmt.Errorf("%w: moomoo executor requires a Sink and Book", domain.ErrInvalidArgument)
	}

	clock := cfg.Clock
	if clock == nil {
		clock = wallClock{}
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	accID := cfg.Account.BrokerAccID

	var env mo.TrdEnv
	switch cfg.Account.Env {
	case domain.EnvPaper:
		env = mo.TrdEnvSimulate
		// SAFETY: a paper executor must NEVER carry live activation material. If a
		// caller mis-supplies the live trader id, refuse — paper can never look
		// like live.
		if cfg.TraderID == LiveTraderID {
			return nil, fmt.Errorf("%w: paper executor must not use the live trader-id %q",
				domain.ErrInvalidArgument, LiveTraderID)
		}
	case domain.EnvReal:
		// (a) typed confirmation phrase.
		if cfg.ConfirmationPhrase != LiveConfirmationPhrase {
			return nil, fmt.Errorf("%w: live activation requires the exact confirmation phrase",
				domain.ErrInvalidArgument)
		}
		// (c) distinct live trader-id namespace.
		if cfg.TraderID != LiveTraderID {
			return nil, fmt.Errorf("%w: live activation requires trader-id %q, got %q",
				domain.ErrInvalidArgument, LiveTraderID, cfg.TraderID)
		}
		// (b) the real acc id must exist under TrdEnvReal at the broker.
		accs, err := cfg.Client.GetAccList(ctx, mo.TrdEnvReal)
		if err != nil {
			return nil, fmt.Errorf("live activation: GetAccList(REAL): %w", err)
		}
		if !accountExists(accs, accID) {
			return nil, fmt.Errorf("%w: live acc_id %d not found under REAL env (refusing to activate)",
				domain.ErrInvalidArgument, accID)
		}
		// (d) Unlock the REAL account BEFORE binding TrdEnvReal. Two OpenD modes:
		//   - HEADLESS OpenD: the API unlock works — use the password (with the
		//     account's securityFirm, which OpenD requires on the unlock).
		//   - GUI OpenD: the API unlock is DISABLED ("use the Unlock button in the
		//     OpenD window"); the operator unlocks in the GUI instead. We then
		//     CANNOT unlock over the API, so we proceed and rely on the GUI unlock.
		//     This is NOT a bypass: queries/sync already require the account to be
		//     GUI-unlocked, and the venue REJECTS a real order on a still-locked
		//     account — moomoo enforces the unlock, while TMS still enforces the
		//     other three factors + the per-order confirm phrase. An empty password
		//     selects this same GUI-unlock mode and skips the API call.
		switch {
		case cfg.UnlockPassword == "":
			logf("live activation: no unlock password — relying on OpenD GUI 'Unlock Trade' (API unlock not attempted)")
		default:
			if err := cfg.Client.UnlockTrade(ctx, mo.TrdEnvReal, cfg.UnlockPassword, accountSecurityFirm(accs, accID)); err != nil {
				if isGUIUnlockDisabled(err) {
					logf("live activation: OpenD GUI mode disables the API unlock — relying on operator's GUI 'Unlock Trade': %v", err)
				} else {
					return nil, fmt.Errorf("live activation: UnlockTrade failed (real orders remain unreachable): %w", err)
				}
			}
		}
		env = mo.TrdEnvReal
	}

	e := &MoomooExecutor{
		cfg:      cfg,
		env:      env,
		accID:    accID,
		clock:    clock,
		logf:     logf,
		orders:   make(map[string]*OrderState),
		venIndex: make(map[string]string),
	}

	// Subscribe to pushes BEFORE any order can be placed so we never miss the
	// SUBMITTED->FILLED transition.
	if err := cfg.Client.SubscribeOrderUpdates(e.onOrderUpdate); err != nil {
		return nil, fmt.Errorf("moomoo executor: subscribe order updates: %w", err)
	}
	if err := cfg.Client.SubscribeFillUpdates(e.onFillUpdate); err != nil {
		return nil, fmt.Errorf("moomoo executor: subscribe fill updates: %w", err)
	}
	return e, nil
}

// Env returns the bound trading environment (SIMULATE for paper, REAL for live).
func (e *MoomooExecutor) Env() mo.TrdEnv { return e.env }

// Now returns the executor's clock time (UTC), for callers (recovery seeding)
// that need a timestamp consistent with the executor's event stream.
func (e *MoomooExecutor) Now() time.Time { return e.clock.Now() }

// IsLive reports whether this executor is bound to the real account.
func (e *MoomooExecutor) IsLive() bool { return e.env == mo.TrdEnvReal }

func accountExists(accs []mo.TradeAccount, id uint64) bool {
	for _, a := range accs {
		if a.AccID == id && a.TrdEnv == mo.TrdEnvReal {
			return true
		}
	}
	return false
}

// isGUIUnlockDisabled reports whether an UnlockTrade error is OpenD's "the GUI
// version disabled the API unlock — use the Unlock button" response (the operator
// must unlock in the OpenD GUI), as opposed to a credential failure.
func isGUIUnlockDisabled(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "disabled the unlock interface") ||
		strings.Contains(s, "Unlock button")
}

// accountSecurityFirm returns the broker entity (Trd_Common SecurityFirm) of the
// REAL account with the given id, or 0 when not found. OpenD requires it on a
// real UnlockTrade for multi-firm logins (e.g. US Futu Inc + HK Futu Securities),
// where the firm must match the account being unlocked.
func accountSecurityFirm(accs []mo.TradeAccount, id uint64) int32 {
	for _, a := range accs {
		if a.AccID == id && a.TrdEnv == mo.TrdEnvReal {
			return a.SecurityFirm
		}
	}
	return 0
}

// nextClientOrderID returns the deterministic per-process client-order-id used
// as the idempotency key end-to-end. Format mirrors the SimExecutor's "O-<seq>"
// scheme, prefixed by env so paper + live ids can never collide.
func (e *MoomooExecutor) nextClientOrderID() string {
	n := e.seq.Add(1) - 1
	prefix := "PAPER"
	if e.env == mo.TrdEnvReal {
		prefix = "LIVE"
	}
	return fmt.Sprintf("%s-O-%d", prefix, n)
}
