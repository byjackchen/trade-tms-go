package main

// trade.go implements `tms trade <command>`: the `tms trade` group. `run` is the
// live (real-time) trading NODE, `preflight` is the go-live precondition gate (both
// formerly `tms live`), and `sync` is a thin HTTP client for the READ-ONLY broker
// SYNC surface (DIRECTION 2) served by the trade node itself. TMS no longer offers
// an order ticket — the operator places orders at the broker directly; `tms trade
// sync` only pulls that externally-placed state back into TMS:
//
//	tms trade sync   — DIRECTION 2: one-shot broker -> TMS sync (READ-ONLY pull +
//	                   reflect + reconcile; places NO orders; safe in ALL modes)
//
// SAFETY: `tms trade sync` is the user's PRIMARY case (they trade in moomoo, then
// sync). It is READ-ONLY and cannot place an order — it carries no activation
// material; the broker binding lives in the node behind the bearer-guarded API.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/app"
)

// newTradeCmd builds the `tms trade` command group.
func newTradeCmd(env *runtimeEnv) *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "trade",
		Short: "Live trading node + go-live preflight + READ-ONLY broker sync",
		Long: "The `tms trade` group: `run` is the live trading node, `preflight` the\n" +
			"go-live gate, and `sync` an HTTP client for the READ-ONLY broker SYNC\n" +
			"surface the node serves. TMS offers no order ticket — the operator places\n" +
			"orders at the broker directly; `tms trade sync` pulls that ACTUAL state and\n" +
			"reflects it into TMS (READ-ONLY; places no orders) then reconciles.",
		Args: cobra.NoArgs,
	}
	cmd.PersistentFlags().StringVar(&addr, "addr", "http://127.0.0.1:18090",
		"trade node base URL (its health/sync listener, TMS_WORKER_HEALTH_ADDR / host 18090)")
	cmd.AddCommand(
		// `tms trade run` is the live (real-time) trading NODE; `tms trade
		// preflight` is the go-live precondition gate (both formerly `tms live`).
		newTradeRunCmd(env),
		newTradePreflightCmd(env),
		// READ-ONLY broker-sync HTTP client (DIRECTION 2).
		newTradeSyncCmd(env, &addr),
	)
	return cmd
}

func newTradeSyncCmd(env *runtimeEnv, addr *string) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "DIRECTION 2: pull the broker's ACTUAL state + reflect into TMS + reconcile (READ-ONLY)",
		Long: "One-shot broker -> TMS sync (the PRIMARY case): the operator trades directly\n" +
			"in moomoo, then runs `tms trade sync` to pull the account's actual positions/\n" +
			"orders/fills/funds and reflect them into TMS under the EXTERNAL book, then\n" +
			"reconcile vs the strategy books. READ-ONLY at the broker — places NO orders —\n" +
			"and safe in ALL modes including signal. Idempotent: re-syncing the same broker\n" +
			"state reflects nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return tradePost(cmd.Context(), env, *addr, "/api/v1/trade/sync", nil)
		},
	}
}

// tradePost POSTs body (nil => empty) to the trade node's bearer-guarded sync
// endpoint (TMS_API_TOKEN) and prints the JSON response. A non-2xx is returned as
// an error carrying the server's error body.
func tradePost(parent context.Context, env *runtimeEnv, baseURL, path string, body map[string]any) error {
	ctx, stop := app.SignalContext(parent)
	defer stop()

	token := env.cfg.APIToken
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("TMS_API_TOKEN is required to authenticate against the trade node sync surface")
	}

	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		buf = bytes.NewReader(b)
	} else {
		buf = bytes.NewReader([]byte("{}"))
	}

	url := strings.TrimRight(baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, buf)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("trade node returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	fmt.Println(strings.TrimSpace(string(respBody)))
	return nil
}
