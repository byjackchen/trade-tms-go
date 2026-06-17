package main

import (
	"context"

	"github.com/byjackchen/trade-tms-go/internal/api"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/preflight"
)

// preflightAdapter adapts the internal/preflight engine to api.PreflightRunner,
// keeping the api package free of the heavy data deps (it depends only on the
// PreflightRunner interface). One adapter holds the wired PG/sharadar/moomoo
// probes and translates the api request shape into a preflight.Config + Report.
type preflightAdapter struct {
	probes *preflight.PGProbes
}

// newPreflightAdapter wires the adapter from the live probes.
func newPreflightAdapter(probes *preflight.PGProbes) *preflightAdapter {
	return &preflightAdapter{probes: probes}
}

var _ api.PreflightRunner = (*preflightAdapter)(nil)

// RunPreflight runs the go-live preflight for the requested session and maps the
// internal report onto the api wire shape.
func (a *preflightAdapter) RunPreflight(ctx context.Context, p api.PreflightParams) api.PreflightReport {
	rep := preflight.Run(ctx, preflight.Config{
		ExecPolicy:          domain.ExecutionPolicy(p.ExecPolicy),
		Env:                 domain.BrokerEnv(p.Env),
		Strategy:            p.Strategy,
		Tickers:             p.Tickers,
		ORBSymbol:           p.ORBSymbol,
		MaxStaleTradingDays: p.MaxStaleTradingDays,
		CheckOpenD:          p.CheckOpenD,
	}, a.probes)

	out := api.PreflightReport{
		ExecPolicy: string(rep.ExecPolicy),
		Env:        string(rep.Env),
		RunWord:    rep.RunWord,
		Strategy:   rep.Strategy,
		TS:         rep.TS.Format("2006-01-02T15:04:05Z07:00"),
		OK:         rep.OK,
		Checks:     make([]api.PreflightResult, 0, len(rep.Checks)),
	}
	for _, c := range rep.Checks {
		out.Checks = append(out.Checks, api.PreflightResult{
			Check:    c.Check,
			Status:   string(c.Status),
			Severity: string(c.Severity),
			Detail:   c.Detail,
		})
	}
	return out
}
