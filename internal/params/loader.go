package params

// loader.go is the high-level entrypoint each strategy uses to obtain its
// resolved, typed, validated parameter struct. A Loader wraps a Resolver
// (DB + file-dir precedence over embedded baseline) and exposes one typed
// accessor per strategy plus a generic Resolve/Defaults for tooling.
//
// Usage (engine assembly):
//
//	ld := params.NewLoader(paramsdb.NewReader(pool), cfg.StrategyParamsDir)
//	sepa, err := ld.SEPA(ctx)          // resolved + validated SEPAParams
//
// Strategy ids are the canonical Python stems.

import (
	"context"
	"fmt"
)

// Canonical strategy ids (Python package stems; baseline file names).
const (
	StrategySEPA             = "sepa"
	StrategyPairs            = "pairs"
	StrategySectorRotation   = "sector_rotation"
	StrategyIntradayBreakout = "intraday_breakout"
)

// Loader resolves and types strategy parameters.
type Loader struct {
	resolver *Resolver
}

// NewLoader builds a Loader. db may be nil (embedded/file-only mode); dir may be
// "" (no file override — typically cfg.StrategyParamsDir).
func NewLoader(db PayloadReader, dir string) *Loader {
	return &Loader{resolver: &Resolver{DB: db, Dir: dir}}
}

// NewLoaderFromResolver wraps an already-built Resolver (for tests / custom
// PayloadReaders).
func NewLoaderFromResolver(r *Resolver) *Loader {
	return &Loader{resolver: r}
}

// Resolve returns the raw resolved Document for any strategy id (no typing).
func (l *Loader) Resolve(ctx context.Context, strategy string) (*Document, error) {
	return l.resolver.Resolve(ctx, strategy)
}

// Defaults resolves strategy and returns its parameter map (name -> Go value),
// for tooling that wants the untyped, post-resolution defaults.
func (l *Loader) Defaults(ctx context.Context, strategy string) (map[string]any, error) {
	doc, err := l.resolver.Resolve(ctx, strategy)
	if err != nil {
		return nil, err
	}
	return doc.Defaults()
}

// resolveMap resolves a strategy and returns its decoded param map.
func (l *Loader) resolveMap(ctx context.Context, strategy string) (pmap, *Document, error) {
	doc, err := l.resolver.Resolve(ctx, strategy)
	if err != nil {
		return nil, nil, err
	}
	m, err := doc.Defaults()
	if err != nil {
		return nil, nil, fmt.Errorf("params: %s: %w", strategy, err)
	}
	return pmap(m), doc, nil
}

// SEPA resolves + types + validates the SEPA parameters.
func (l *Loader) SEPA(ctx context.Context) (SEPAParams, *Document, error) {
	m, doc, err := l.resolveMap(ctx, StrategySEPA)
	if err != nil {
		return SEPAParams{}, nil, err
	}
	p, err := sepaFromMap(m)
	if err != nil {
		return SEPAParams{}, doc, fmt.Errorf("params: sepa: %w", err)
	}
	return p, doc, nil
}

// Pairs resolves + types + validates the Pairs parameters.
func (l *Loader) Pairs(ctx context.Context) (PairsParams, *Document, error) {
	m, doc, err := l.resolveMap(ctx, StrategyPairs)
	if err != nil {
		return PairsParams{}, nil, err
	}
	p, err := pairsFromMap(m)
	if err != nil {
		return PairsParams{}, doc, fmt.Errorf("params: pairs: %w", err)
	}
	return p, doc, nil
}

// SectorRotation resolves + types + validates the Sector Rotation parameters.
func (l *Loader) SectorRotation(ctx context.Context) (SectorRotationParams, *Document, error) {
	m, doc, err := l.resolveMap(ctx, StrategySectorRotation)
	if err != nil {
		return SectorRotationParams{}, nil, err
	}
	p, err := sectorFromMap(m)
	if err != nil {
		return SectorRotationParams{}, doc, fmt.Errorf("params: sector_rotation: %w", err)
	}
	return p, doc, nil
}

// IntradayBreakout resolves + types + validates the Intraday ORB parameters.
func (l *Loader) IntradayBreakout(ctx context.Context) (IntradayBreakoutParams, *Document, error) {
	m, doc, err := l.resolveMap(ctx, StrategyIntradayBreakout)
	if err != nil {
		return IntradayBreakoutParams{}, nil, err
	}
	p, err := intradayFromMap(m)
	if err != nil {
		return IntradayBreakoutParams{}, doc, fmt.Errorf("params: intraday_breakout: %w", err)
	}
	return p, doc, nil
}
