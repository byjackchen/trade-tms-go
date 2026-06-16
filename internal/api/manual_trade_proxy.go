package api

// manual_trade_proxy.go fronts the MANUAL (operator-driven) trade-mutation surface
// on the main API host. The manual desk physically lives in the LIVE-NODE process —
// the only process that holds the broker (Trd_*) connection — and serves its
// bearer-guarded /api/v1/trade/* endpoints on its own listener (cmd/tms,
// --manual-api-addr, default :18091). The main API process cannot itself hold a
// broker connection, so it REVERSE-PROXIES /api/v1/trade/* onto the live node's
// manual listener when configured (TMS_MANUAL_TRADE_URL).
//
// This is the documented compose topology ("the compose stack reverse-proxies
// /api/v1/trade/* onto the live node's manual listener so the suite hits one host"):
// the UI's /api/proxy/* and the e2e suite both target the single API host (18080),
// and the trade calls land on the broker-connected live node. When no upstream is
// configured the surface returns 503 (no manual desk) — exactly as before, so a
// signal-only `app`-profile stack with no live node is unaffected.
//
// SAFETY: this layer adds NO trust. Every safety gate (4-factor live activation,
// per-order confirm, risk gate, idempotent client-order-ids, audit) runs inside the
// live node's ManualController; the proxy only forwards the bearer-authenticated
// request body + path verbatim and passes the upstream status/body through. It
// injects no credentials and grants no bypass.

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// ManualTradeProxy reverse-proxies the API's /api/v1/trade/* surface onto the live
// node's manual desk listener. Construct with NewManualTradeProxy; a nil proxy means
// "no manual desk reachable" and the /trade/* endpoints return 503.
type ManualTradeProxy struct {
	rp     *httputil.ReverseProxy
	target *url.URL
	log    zerolog.Logger
}

// NewManualTradeProxy builds a proxy to the live node's manual listener base URL
// (e.g. http://tmsgo-live:18091). An empty/invalid base returns (nil, err) and the
// caller leaves the proxy unset (so /trade/* stays 503). The proxy forwards the
// request path + method + body unchanged (the live listener serves the SAME
// /api/v1/trade/* paths) and preserves the Authorization header so the upstream
// re-authenticates the bearer token.
func NewManualTradeProxy(base string, log zerolog.Logger) (*ManualTradeProxy, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil, errors.New("api: empty manual-trade upstream url")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, errors.New("api: manual-trade upstream url must be absolute (scheme + host)")
	}
	plog := log.With().Str("component", "manual-trade-proxy").Str("upstream", u.String()).Logger()
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Route to the upstream host; keep the incoming path verbatim (the live
			// listener serves the identical /api/v1/trade/* routes). Strip the original
			// Host so the upstream sees its own.
			req.URL.Scheme = u.Scheme
			req.URL.Host = u.Host
			req.Host = u.Host
		},
		// A short read-header/response timeout: the manual desk is local in compose.
		Transport: &http.Transport{
			ResponseHeaderTimeout: 15 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, perr error) {
			// The upstream live node is unreachable (not started / starting / network).
			// 503 (not 502) so the e2e skip-guard + the UI treat it as "no manual desk
			// connected", consistent with the no-upstream case.
			plog.Warn().Err(perr).Msg("manual-trade upstream unreachable; returning 503")
			writeError(w, http.StatusServiceUnavailable, "unavailable",
				"manual trade desk not reachable (live node manual listener down)")
		},
	}
	return &ManualTradeProxy{rp: rp, target: u, log: plog}, nil
}

// handleTradeProxy forwards a /api/v1/trade/* request to the live node's manual
// listener. It is the single handler for every trade route when the proxy is wired.
func (s *Server) handleTradeProxy(w http.ResponseWriter, r *http.Request) {
	if s.manualProxy == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"manual trade desk not connected (no live-node manual listener configured)")
		return
	}
	s.manualProxy.rp.ServeHTTP(w, r)
}
