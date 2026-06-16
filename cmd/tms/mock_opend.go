package main

// mock_opend.go implements `tms mock-opend`: a STANDALONE protocol-faithful mock
// OpenD trading venue, bound to a TCP port, that the live node connects to via
// TMS_MOOMOO_ADDR. It is the runnable venue behind the DIRECTION-1 manual order
// lifecycle (and the paper auto-trading path) in the real compose env: the live
// node dials it, places orders over the genuine moomoo wire protocol, and the mock
// simulates accept->fill + maintains positions/funds + serves Trd_Get* reads
// (so DIRECTION-2 sync works too). Bars are driven from the project's Postgres
// (tms.bars_daily / bars_intraday) so quotes/fills are priced off real data.
//
// This is the same mock used in the Go unit tests (internal/adapters/moomoo/mock),
// promoted to a first-class binary + compose service so the manual-desk e2e specs
// can run the lifecycle end-to-end against a real listener — not only in-process.
// It is NEVER a real broker: it places no real orders and holds no real money.

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	moomock "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/mock"
	"github.com/byjackchen/trade-tms-go/internal/app"
	"github.com/byjackchen/trade-tms-go/internal/db"
)

func newMockOpenDCmd(env *runtimeEnv) *cobra.Command {
	var (
		listen        string
		paperAccID    uint64
		liveAccID     uint64
		startingPower float64
		serverVer     int
	)

	cmd := &cobra.Command{
		Use:   "mock-opend",
		Short: "Run the standalone protocol-faithful mock OpenD trading venue (paper; PG-driven bars)",
		Long: "Binds a TCP listener and serves the moomoo wire protocol (market-data +\n" +
			"the Trd_* trading venue) so a `tms live --mode paper` node — and its MANUAL\n" +
			"trade desk — can place/cancel/close orders + sync, end to end, with NO real\n" +
			"OpenD and NO real money. Bars are sourced from the project's Postgres. Point\n" +
			"the live node at it via TMS_MOOMOO_ADDR=<this-host>:<port>.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMockOpenD(cmd.Context(), env, mockOpenDArgs{
				listen:        strings.TrimSpace(listen),
				paperAccID:    paperAccID,
				liveAccID:     liveAccID,
				startingPower: startingPower,
				serverVer:     serverVer,
			})
		},
	}

	cmd.Flags().StringVar(&listen, "listen", "0.0.0.0:11111", "TCP listen address for the mock OpenD venue")
	cmd.Flags().Uint64Var(&paperAccID, "paper-acc-id", 0, "SIMULATE (paper) account id to enable on the venue (default TMS_MOOMOO_PAPER_ACC_ID)")
	cmd.Flags().Uint64Var(&liveAccID, "live-acc-id", 0, "fake-'live' (still simulated) account id to enable (default TMS_MOOMOO_LIVE_ACC_ID; testing only)")
	cmd.Flags().Float64Var(&startingPower, "starting-power", 1_000_000, "each account's initial buying power / cash")
	cmd.Flags().IntVar(&serverVer, "server-ver", 900, "advertised OpenD server version")
	return cmd
}

type mockOpenDArgs struct {
	listen        string
	paperAccID    uint64
	liveAccID     uint64
	startingPower float64
	serverVer     int
}

func runMockOpenD(parent context.Context, env *runtimeEnv, a mockOpenDArgs) error {
	log := env.log.With().Str("cmd", "mock-opend").Logger()

	// Account ids default from the SAME secret env the live node reads, so a compose
	// stack only needs to seed TMS_MOOMOO_PAPER_ACC_ID once for both services.
	if a.paperAccID == 0 {
		a.paperAccID = parseUintEnv("TMS_MOOMOO_PAPER_ACC_ID")
	}
	if a.liveAccID == 0 {
		a.liveAccID = parseUintEnv("TMS_MOOMOO_LIVE_ACC_ID")
	}
	if a.paperAccID == 0 && a.liveAccID == 0 {
		return fmt.Errorf("mock-opend: at least a paper acc id is required (--paper-acc-id or TMS_MOOMOO_PAPER_ACC_ID) so the trading venue has an account to trade")
	}

	ctx, stop := app.SignalContext(parent)
	defer stop()

	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	srv, err := moomock.New(moomock.Options{
		Listen:    a.listen,
		Source:    moomock.NewPGBarSource(pool),
		ServerVer: int32(a.serverVer),
		Logger:    log,
	})
	if err != nil {
		return fmt.Errorf("mock-opend: starting venue: %w", err)
	}
	// Attach the trading venue (paper + optional fake-live account) BEFORE Serve.
	srv.EnableTrading(moomock.VenueConfig{
		PaperAccID:    a.paperAccID,
		LiveAccID:     a.liveAccID,
		StartingPower: a.startingPower,
	})

	// AUTONOMOUS FILL DRIVER: the standalone venue has no in-process PushKLine
	// caller, so without this every working order would sit ACCEPTED forever. This
	// goroutine fills working orders on a wall-clock tick against the bar source's
	// latest close — making the manual-desk place->fill->position lifecycle, and the
	// DIRECTION-2 sync of a broker-side position (which requires a fill), work end to
	// end in the deployed stack. It stops on ctx cancellation.
	srv.StartAutoFill(ctx, 0)

	log.Warn().
		Str("addr", srv.Addr()).
		Uint64("paper_acc_id", a.paperAccID).
		Uint64("live_acc_id", a.liveAccID).
		Float64("starting_power", a.startingPower).
		Msg("MOCK OpenD trading venue listening (NOT a real broker — no real orders, no real money)")

	// Serve blocks until ctx is cancelled (SIGINT/SIGTERM) or the listener errors.
	if serr := srv.Serve(ctx); serr != nil && ctx.Err() == nil {
		return fmt.Errorf("mock-opend: serve: %w", serr)
	}
	log.Info().Msg("mock OpenD venue stopped")
	return nil
}
