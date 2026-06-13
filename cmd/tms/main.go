// Command tms is the single binary for the Trade Management System: data
// import, backtests, hyperopt, live trading, EOD workflow and the HTTP API
// are all subcommands, so every deployment artifact is one static binary.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/byjackchen/trade-tms-go/internal/app"
)

func main() {
	ctx, stop := app.SignalContext(context.Background())
	defer stop()

	root := newRootCmd()
	if err := root.ExecuteContext(ctx); err != nil {
		// Cobra already printed usage when appropriate (SilenceErrors is
		// off); ensure a single terse error line and a non-zero exit.
		fmt.Fprintln(os.Stderr, "tms:", err)
		os.Exit(1)
	}
}
