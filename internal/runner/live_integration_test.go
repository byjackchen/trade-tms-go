package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/commands"
	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/publish"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// buildSectorSession assembles a signal-mode sector_rotation session writing to
// sink, gated by emitGate (nil = always emit).
func buildSectorSession(t *testing.T, sink livengine.IntentSink, emitGate func() bool) *livengine.Session {
	t.Helper()
	asm, err := strategyassembly.Assemble(strategyassembly.Input{
		Strategy:        "sector_rotation",
		StartingBalance: 100000,
		Params:          strategyassembly.Params{Sector: paramsSector()},
	})
	require.NoError(t, err)
	sess, err := livengine.NewSession(livengine.Config{
		Mode:            livengine.ModeSignal,
		Strategies:      asm.Strategies,
		Portfolio:       asm.Portfolio,
		StartingBalance: domain.MustMoney("100000"),
		Sink:            sink,
		EmitGate:        emitGate,
	})
	require.NoError(t, err)
	return sess
}

// sectorBars builds a rising 4-date daily series for the 8-ETF wide universe
// (so a rebalance fires and intents are emitted).
func sectorBars() []domain.Bar {
	syms := []string{"E1", "E2", "E3", "E4", "E5", "E6", "E7", "E8"}
	dates := []time.Time{
		time.Date(2024, time.January, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2024, time.January, 16, 0, 0, 0, 0, time.UTC),
		time.Date(2024, time.January, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC),
	}
	var bars []domain.Bar
	for i, d := range dates {
		px := domain.MustPrice(intToString(100+i) + ".00")
		for _, s := range syms {
			bars = append(bars, domain.Bar{Symbol: s, TS: d, Open: px, High: px, Low: px, Close: px, Volume: 1000})
		}
	}
	return bars
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// paramsSector is a wide 8-ETF sector universe with a short momentum lookback,
// so the 4-date series produces a real rebalance.
func paramsSector() params.SectorRotationParams {
	return params.SectorRotationParams{
		Universe:         []string{"E1", "E2", "E3", "E4", "E5", "E6", "E7", "E8"},
		MomentumLookback: 2,
		TopK:             8,
		Timezone:         "America/New_York",
	}
}

// countAppendRows counts streaming (as_of NULL) intent rows.
func countAppendRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM tms.signal_intents WHERE as_of IS NULL`).Scan(&n))
	return n
}

// TestSignalSessionAppendsToDB drives a streaming signal session over a
// SliceStreamFeed (virtual clock) through the runner.Sink (DB append) and proves
// intents land in tms.signal_intents (as_of NULL, append-only).
func TestSignalSessionAppendsToDB(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)

	sessionID := openTestSession(t, pool, "SIGNAL-TEST-1")
	sink := runner.NewSink(runner.SinkOptions{
		Store:     publish.NewStore(pool),
		Publisher: nil, // no Redis in the ephemeral harness
		Mode:      runner.SinkAppend,
		SessionID: &sessionID,
		Logger:    zerolog.Nop(),
	})

	sess := buildSectorSession(t, sink, nil)
	require.NoError(t, sess.Prime(ctx))
	vc := core.NewVirtualClock(time.Time{})
	require.NoError(t, sess.RunStream(ctx,
		livengine.SliceStreamFeed{Bars: sectorBars()}, core.StreamVirtual, vc))

	rows := countAppendRows(t, pool)
	assert.Positive(t, rows, "streaming session should append intent rows to PG")
	assert.Equal(t, sink.IntentRows(), rows, "sink row counter matches DB")
}

// TestHaltStopsNewIntents proves the EmitGate (halt) suppresses NEW-intent
// emission: a session that halts partway through stops appending rows, while
// bars keep flowing (state stays warm).
func TestHaltStopsNewIntents(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)

	halt := commands.NewHaltState(nil)
	sessionID := openTestSession(t, pool, "SIGNAL-TEST-2")
	sink := runner.NewSink(runner.SinkOptions{
		Store:     publish.NewStore(pool),
		Mode:      runner.SinkAppend,
		SessionID: &sessionID,
		Logger:    zerolog.Nop(),
	})

	// Halt BEFORE the run: the gate is closed for every timestamp.
	halt.Halt(commands.HaltManual, "test halt")
	sess := buildSectorSession(t, sink, halt.Emitting)
	require.NoError(t, sess.Prime(ctx))
	vc := core.NewVirtualClock(time.Time{})
	require.NoError(t, sess.RunStream(ctx,
		livengine.SliceStreamFeed{Bars: sectorBars()}, core.StreamVirtual, vc))

	assert.Zero(t, countAppendRows(t, pool), "halted session must append NO intent rows")
	assert.Positive(t, sess.BarsSeen(), "bars still flowed through the strategies while halted")
	assert.Positive(t, sess.HaltedFlushes(), "emission was suppressed for each timestamp")

	// Resume and run a fresh session: intents now flow.
	halt.Resume()
	sink2 := runner.NewSink(runner.SinkOptions{
		Store: publish.NewStore(pool), Mode: runner.SinkAppend, SessionID: &sessionID, Logger: zerolog.Nop(),
	})
	sess2 := buildSectorSession(t, sink2, halt.Emitting)
	require.NoError(t, sess2.Prime(ctx))
	require.NoError(t, sess2.RunStream(ctx,
		livengine.SliceStreamFeed{Bars: sectorBars()}, core.StreamVirtual, core.NewVirtualClock(time.Time{})))
	assert.Positive(t, countAppendRows(t, pool), "resumed session appends intents")
}

// fakeController records the commands applied to it (for the consumer test).
type fakeController struct {
	mode    string
	halted  bool
	stopped bool
	calls   []string
}

func (c *fakeController) Mode() string { return c.mode }
func (c *fakeController) SetMode(_ context.Context, m string) error {
	if m != "signal" {
		return assertErr("paper/live deferred to P6")
	}
	c.mode = m
	c.calls = append(c.calls, "set_mode:"+m)
	return nil
}
func (c *fakeController) Halt(_ context.Context, _ commands.HaltKind, _ string) error {
	c.halted = true
	c.calls = append(c.calls, "halt")
	return nil
}
func (c *fakeController) Resume(_ context.Context) error {
	c.halted = false
	c.calls = append(c.calls, "resume")
	return nil
}
func (c *fakeController) Stop(_ context.Context, _ string) error {
	c.stopped = true
	c.calls = append(c.calls, "stop")
	return nil
}
func (c *fakeController) Kill(_ context.Context, _ string) error {
	c.halted, c.stopped = true, true
	c.calls = append(c.calls, "kill")
	return nil
}
func (c *fakeController) Flatten(_ context.Context, _ string) (int, error) {
	c.calls = append(c.calls, "flatten")
	return 0, nil
}
func (c *fakeController) EmergencyKill(_ context.Context, _ string) (int, error) {
	c.halted, c.stopped = true, true
	c.calls = append(c.calls, "emergency_kill")
	return 0, nil
}
func (c *fakeController) Reconcile(_ context.Context) (bool, error) {
	c.calls = append(c.calls, "reconcile")
	return false, nil
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

// TestCommandConsumerHaltAudited enqueues a halt command, drains it via the
// consumer, and proves the Controller was called, the row transitioned to
// completed, and an audit row was written.
func TestCommandConsumerHaltAudited(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)

	enq := commands.NewEnqueuer(pool, nil, "")
	id, err := enq.Enqueue(ctx, commands.EnqueueParams{
		Source: "api", Name: commands.NameHalt,
		Args: commands.CommandArgs{Reason: "operator stop"}, RequestedBy: "tester",
	})
	require.NoError(t, err)
	require.Positive(t, id)

	ctrl := &fakeController{mode: "signal"}
	consumer, err := commands.NewConsumer(commands.ConsumerOptions{
		Pool: pool, Controller: ctrl, Actor: "tms-live:test", Logger: zerolog.Nop(),
	})
	require.NoError(t, err)

	// Drain once (cancel a short-lived ctx so Run returns after the drain).
	drainCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	require.NoError(t, consumer.Run(drainCtx))

	assert.True(t, ctrl.halted, "halt command applied to the controller")
	assert.Equal(t, []string{"halt"}, ctrl.calls)

	// Command row completed.
	var status string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM tms.commands WHERE id=$1`, id).Scan(&status))
	assert.Equal(t, "completed", status)

	// Audit row written.
	var auditCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM tms.audit_log WHERE action='live.command.halt' AND entity_id=$1`,
		intToString(int(id))).Scan(&auditCount))
	assert.Equal(t, 1, auditCount, "halt command audited")
}

// TestEnqueueConfirmationGate proves a paper/live mode switch requires a
// confirmation token while halt/kill never do.
func TestEnqueueConfirmationGate(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)
	enq := commands.NewEnqueuer(pool, nil, "")

	// halt is always allowed.
	_, err := enq.Enqueue(ctx, commands.EnqueueParams{
		Name: commands.NameHalt, RequestedBy: "t"})
	require.NoError(t, err)

	// set_mode -> live without a token is rejected.
	_, err = enq.Enqueue(ctx, commands.EnqueueParams{
		Name: commands.NameSetMode, Args: commands.CommandArgs{Mode: "live"}, RequestedBy: "t"})
	require.ErrorIs(t, err, commands.ErrConfirmationRequired)

	// set_mode -> live WITH a token is accepted.
	_, err = enq.Enqueue(ctx, commands.EnqueueParams{
		Name: commands.NameSetMode,
		Args: commands.CommandArgs{Mode: "live", ConfirmToken: "yes"}, RequestedBy: "t"})
	require.NoError(t, err)

	// set_mode -> signal needs no token.
	_, err = enq.Enqueue(ctx, commands.EnqueueParams{
		Name: commands.NameSetMode, Args: commands.CommandArgs{Mode: "signal"}, RequestedBy: "t"})
	require.NoError(t, err)
}

// TestHaltRehydratedOnRestart is the regression for the halt-durability defect:
// an active (uncleared) tms.halts row scoped to the resumed session must be
// re-applied to the in-memory HaltState on (re)start, so a crash/restart does NOT
// silently clear an operator/operational halt and resume emitting/trading.
// DATABASE-ORIENTED thesis: durable PG state is authoritative on restart.
func TestHaltRehydratedOnRestart(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)
	sessionID := openTestSession(t, pool, "PAPER-HALT-001")

	// Simulate a node that latched a MANUAL halt, persisted it, then crashed.
	node, err := runner.NewLive(pool, nil, runner.LiveConfig{TraderID: "PAPER-HALT-001"}, zerolog.Nop())
	require.NoError(t, err)
	node.SetSessionIDForTest(sessionID)
	node.RecordHaltForTest(ctx, string(commands.HaltManual), "operator halt before crash")

	// A fresh node (the restart) starts with a clean HaltState (NewHaltState).
	restarted, err := runner.NewLive(pool, nil, runner.LiveConfig{TraderID: "PAPER-HALT-001"}, zerolog.Nop())
	require.NoError(t, err)
	require.False(t, restarted.HaltState().IsHalted(), "fresh HaltState is not halted before rehydration")

	// Rehydration (run before the supervisor loop) must restore the halt.
	restarted.SetSessionIDForTest(sessionID)
	restarted.RehydrateHaltForTest(ctx)

	snap := restarted.HaltState().Snapshot()
	require.True(t, snap.Halted, "latched halt rehydrated from PG on restart")
	assert.False(t, restarted.HaltState().Emitting(), "a rehydrated halt suppresses NEW emission/trading")
	assert.Equal(t, commands.HaltManual, snap.Kind)
	assert.Equal(t, "operator halt before crash", snap.Reason)

	// After an operator Resume (clears the durable row), a subsequent restart does
	// NOT re-apply the halt.
	restarted.ClearHaltForTest(ctx, "operator")
	afterResume, err := runner.NewLive(pool, nil, runner.LiveConfig{TraderID: "PAPER-HALT-001"}, zerolog.Nop())
	require.NoError(t, err)
	afterResume.SetSessionIDForTest(sessionID)
	afterResume.RehydrateHaltForTest(ctx)
	assert.False(t, afterResume.HaltState().IsHalted(), "a cleared halt is not rehydrated")
	assert.True(t, afterResume.HaltState().Emitting())

	// A halt scoped to a DIFFERENT session is not rehydrated into this trader.
	otherSession := openTestSession(t, pool, "PAPER-HALT-OTHER")
	other, err := runner.NewLive(pool, nil, runner.LiveConfig{TraderID: "PAPER-HALT-OTHER"}, zerolog.Nop())
	require.NoError(t, err)
	other.SetSessionIDForTest(otherSession)
	other.RecordHaltForTest(ctx, string(commands.HaltBroker), "other-session halt")
	afterResume.RehydrateHaltForTest(ctx) // still scoped to sessionID
	assert.False(t, afterResume.HaltState().IsHalted(), "halts from other sessions do not leak in")
}

// openTestSession inserts a RUNNING session row and returns its id.
func openTestSession(t *testing.T, pool *pgxpool.Pool, traderID string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var id int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tms.sessions (trader_id, mode, status) VALUES ($1, 'signal', 'RUNNING') RETURNING id`,
		traderID).Scan(&id))
	return id
}
