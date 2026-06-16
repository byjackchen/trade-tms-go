package main

// trade.go implements `tms trade <command>`: a thin HTTP client for the operator-
// driven MANUAL trade desk served by the live node (`tms live --manual-mode
// paper|live`, the process holding the broker connection). It speaks the bearer-
// guarded /api/v1/trade/* surface:
//
//	tms trade place  --symbol AAPL --side buy --qty 10 [--type limit --limit 195] \
//	                 [--override] [--confirm <phrase|trade-pw>] [--key <idem>]
//	tms trade cancel <client_order_id>
//	tms trade close  --symbol AAPL [--qty N] [--confirm <phrase|trade-pw>]
//	tms trade sync   — DIRECTION 2: one-shot broker -> TMS sync (READ-ONLY pull +
//	                   reflect + reconcile; places NO orders; safe in ALL modes)
//
// SAFETY: this client carries NO activation material of its own — every safety gate
// (4-factor live activation, per-order confirm, risk gate, audit) lives in the desk
// behind the API. `tms trade sync` is the user's PRIMARY case (they trade in moomoo,
// then sync); it is read-only and cannot place an order.

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

// newTradeCmd builds the `tms trade` command group (manual desk HTTP client).
func newTradeCmd(env *runtimeEnv) *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "trade",
		Short: "Operator MANUAL trade desk: place/cancel/close orders + sync from broker",
		Long: "HTTP client for the MANUAL trade desk served by `tms live --manual-mode`.\n" +
			"Every safety gate (4-factor live activation, per-order confirm, risk gate,\n" +
			"audit) lives in the desk behind the API. `tms trade sync` pulls the broker's\n" +
			"ACTUAL state and reflects it into TMS (READ-ONLY; places no orders).",
		Args: cobra.NoArgs,
	}
	cmd.PersistentFlags().StringVar(&addr, "addr", "http://127.0.0.1:18091",
		"MANUAL trade desk base URL (the --manual-api-addr of the live node)")
	cmd.AddCommand(
		newTradePlaceCmd(env, &addr),
		newTradeCancelCmd(env, &addr),
		newTradeCloseCmd(env, &addr),
		newTradeSyncCmd(env, &addr),
	)
	return cmd
}

func newTradePlaceCmd(env *runtimeEnv, addr *string) *cobra.Command {
	var (
		symbol, side, otype, confirm, key, reason string
		qty                                       int64
		limit                                     float64
		override                                  bool
	)
	c := &cobra.Command{
		Use:   "place",
		Short: "Place a manual order (live needs --confirm with the per-order phrase)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if key == "" {
				// A stable default idempotency key per invocation; the operator SHOULD
				// pass --key for a retriable intent (so a retry dedupes at the desk).
				key = fmt.Sprintf("cli-%d", time.Now().UnixNano())
			}
			body := map[string]any{
				"idempotency_key": key,
				"symbol":          strings.ToUpper(strings.TrimSpace(symbol)),
				"side":            strings.ToUpper(strings.TrimSpace(side)),
				"qty":             qty,
				"type":            strings.ToUpper(strings.TrimSpace(otype)),
				"override":        override,
				"confirm_token":   confirm,
				"reason":          reason,
			}
			if strings.EqualFold(otype, "limit") {
				body["limit_price"] = limit
			}
			return tradePost(cmd.Context(), env, *addr, "/api/v1/trade/order", body)
		},
	}
	c.Flags().StringVar(&symbol, "symbol", "", "symbol (required)")
	c.Flags().StringVar(&side, "side", "", "buy | sell (required)")
	c.Flags().Int64Var(&qty, "qty", 0, "quantity (required, >0)")
	c.Flags().StringVar(&otype, "type", "market", "market | limit")
	c.Flags().Float64Var(&limit, "limit", 0, "limit price (required for --type limit)")
	c.Flags().BoolVar(&override, "override", false, "override a risk-gate violation (audited)")
	c.Flags().StringVar(&confirm, "confirm", "", "per-order confirm: live phrase, or the paper trade password")
	c.Flags().StringVar(&key, "key", "", "idempotency key (a retry with the same key never double-submits)")
	c.Flags().StringVar(&reason, "reason", "", "operator note (audit)")
	return c
}

func newTradeCancelCmd(env *runtimeEnv, addr *string) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <client_order_id>",
		Short: "Cancel a working manual order (idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return tradePost(cmd.Context(), env, *addr, "/api/v1/trade/order/"+args[0]+"/cancel", nil)
		},
	}
}

func newTradeCloseCmd(env *runtimeEnv, addr *string) *cobra.Command {
	var (
		symbol, confirm, key string
		qty                  int64
	)
	c := &cobra.Command{
		Use:   "close",
		Short: "Close a symbol's manual position (live is confirm-gated; qty<=0 = full)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body := map[string]any{"qty": qty, "confirm_token": confirm}
			// An idempotency key makes a retried close dedupe at the desk (no
			// real-money oversell on a double-click / client retry).
			if k := strings.TrimSpace(key); k != "" {
				body["idempotency_key"] = k
			}
			return tradePost(cmd.Context(), env, *addr,
				"/api/v1/trade/position/"+strings.ToUpper(strings.TrimSpace(symbol))+"/close", body)
		},
	}
	c.Flags().StringVar(&symbol, "symbol", "", "symbol (required)")
	c.Flags().Int64Var(&qty, "qty", 0, "shares to close (0 = full close)")
	c.Flags().StringVar(&confirm, "confirm", "", "per-order confirm: live phrase, or the paper trade password")
	c.Flags().StringVar(&key, "key", "", "idempotency key (a retried close with the same key never double-submits)")
	return c
}

func newTradeSyncCmd(env *runtimeEnv, addr *string) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "DIRECTION 2: pull the broker's ACTUAL state + reflect into TMS + reconcile (READ-ONLY)",
		Long: "One-shot broker -> TMS sync (the PRIMARY case): the operator trades directly\n" +
			"in moomoo, then runs `tms trade sync` to pull the account's actual positions/\n" +
			"orders/fills/funds and reflect them into TMS under the MANUAL book, then\n" +
			"reconcile vs the strategy books. READ-ONLY at the broker — places NO orders —\n" +
			"and safe in ALL modes including signal. Idempotent: re-syncing the same broker\n" +
			"state reflects nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return tradePost(cmd.Context(), env, *addr, "/api/v1/trade/sync", nil)
		},
	}
}

// tradePost POSTs body (nil => empty) to the manual desk endpoint, bearer-guarded
// by TMS_API_TOKEN, and prints the JSON response. A non-2xx is returned as an error
// carrying the server's error body (so 412/422 confirm/risk messages reach the
// operator verbatim).
func tradePost(parent context.Context, env *runtimeEnv, baseURL, path string, body map[string]any) error {
	ctx, stop := app.SignalContext(parent)
	defer stop()

	token := env.cfg.APIToken
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("TMS_API_TOKEN is required to authenticate against the manual trade desk")
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
		return fmt.Errorf("manual desk returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	fmt.Println(strings.TrimSpace(string(respBody)))
	return nil
}
