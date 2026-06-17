package mock

// source.go provides the two BarSource implementations: an in-memory fixture
// source (MemBarSource) for unit tests, and a Postgres-backed source
// (PGBarSource) that drives klines from tms.bars_daily / tms.bars_intraday —
// the latter is what makes the mock a faithful gate driver over OUR data.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// MemBarSource is an in-memory BarSource keyed by (symbol, klType). Bars per
// key must be ascending by ts (Add keeps them sorted). Safe for concurrent
// reads after construction; Add is not concurrent-safe with reads.
type MemBarSource struct {
	// data[klType][symbol] = ascending bars
	data map[qotcommon.KLType]map[string][]domain.Bar
}

// NewMemBarSource returns an empty in-memory source.
func NewMemBarSource() *MemBarSource {
	return &MemBarSource{data: make(map[qotcommon.KLType]map[string][]domain.Bar)}
}

// Add registers bars for (symbol, kl), merging and re-sorting by ts ascending.
func (m *MemBarSource) Add(symbol string, kl qotcommon.KLType, bars []domain.Bar) {
	bySym := m.data[kl]
	if bySym == nil {
		bySym = make(map[string][]domain.Bar)
		m.data[kl] = bySym
	}
	merged := append(append([]domain.Bar(nil), bySym[symbol]...), bars...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].TS.Before(merged[j].TS) })
	bySym[symbol] = merged
}

// Bars returns symbol's bars in [begin, end] inclusive (ascending).
func (m *MemBarSource) Bars(_ context.Context, symbol string, kl qotcommon.KLType, begin, end time.Time) ([]domain.Bar, error) {
	bySym := m.data[kl]
	if bySym == nil {
		return nil, nil
	}
	all := bySym[symbol]
	var out []domain.Bar
	for _, b := range all {
		if b.TS.Before(begin) || b.TS.After(end) {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

// PGBarSource serves bars from the project's Postgres tables. Daily K-lines
// come from tms.bars_daily (SEP/SFP merged); intraday widths come from
// tms.bars_intraday filtered by bar_seconds. Prices are stored as 1e-4
// fixed-point BIGINT (domain.Price.Raw), so decoding is exact.
type PGBarSource struct {
	pool *pgxpool.Pool
}

// NewPGBarSource wraps a pgx pool.
func NewPGBarSource(pool *pgxpool.Pool) *PGBarSource { return &PGBarSource{pool: pool} }

// barSecondsForKLType maps a KLType to the bars_intraday bar_seconds width;
// daily returns 0 (served from bars_daily instead).
func barSecondsForKLType(kl qotcommon.KLType) (int, bool) {
	switch kl {
	case qotcommon.KLType_KLType_1Min:
		return 60, true
	case qotcommon.KLType_KLType_5Min:
		return 300, true
	case qotcommon.KLType_KLType_15Min:
		return 900, true
	case qotcommon.KLType_KLType_30Min:
		return 1800, true
	case qotcommon.KLType_KLType_60Min:
		return 3600, true
	default:
		return 0, false
	}
}

// Bars implements BarSource against Postgres.
func (s *PGBarSource) Bars(ctx context.Context, symbol string, kl qotcommon.KLType, begin, end time.Time) ([]domain.Bar, error) {
	if kl == qotcommon.KLType_KLType_Day {
		return s.dailyBars(ctx, symbol, begin, end)
	}
	secs, ok := barSecondsForKLType(kl)
	if !ok {
		return nil, fmt.Errorf("mock: PGBarSource: unsupported KLType %v", kl)
	}
	return s.intradayBars(ctx, symbol, secs, begin, end)
}

func (s *PGBarSource) dailyBars(ctx context.Context, symbol string, begin, end time.Time) ([]domain.Bar, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ts, open, high, low, close, volume
		FROM tms.bars_daily
		WHERE ticker = $1 AND ts >= $2 AND ts <= $3
		  AND open IS NOT NULL AND high IS NOT NULL
		  AND low IS NOT NULL AND close IS NOT NULL
		ORDER BY ts ASC, source ASC`,
		symbol, begin.UTC(), end.UTC())
	if err != nil {
		return nil, fmt.Errorf("mock: query bars_daily %s: %w", symbol, err)
	}
	return scanBars(rows, symbol)
}

func (s *PGBarSource) intradayBars(ctx context.Context, symbol string, barSeconds int, begin, end time.Time) ([]domain.Bar, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ts, open, high, low, close, volume
		FROM tms.bars_intraday
		WHERE ticker = $1 AND bar_seconds = $2 AND ts >= $3 AND ts <= $4
		ORDER BY ts ASC`,
		symbol, barSeconds, begin.UTC(), end.UTC())
	if err != nil {
		return nil, fmt.Errorf("mock: query bars_intraday %s: %w", symbol, err)
	}
	return scanBars(rows, symbol)
}

// scanBars decodes (ts, open, high, low, close, volume) rows into domain bars.
func scanBars(rows pgx.Rows, symbol string) ([]domain.Bar, error) {
	defer rows.Close()
	var out []domain.Bar
	for rows.Next() {
		var (
			ts              time.Time
			o, h, l, c, vol int64
		)
		if err := rows.Scan(&ts, &o, &h, &l, &c, &vol); err != nil {
			return nil, fmt.Errorf("mock: scan bar %s: %w", symbol, err)
		}
		out = append(out, domain.Bar{
			Symbol: symbol,
			TS:     ts.UTC(),
			Open:   domain.Price(o),
			High:   domain.Price(h),
			Low:    domain.Price(l),
			Close:  domain.Price(c),
			Volume: vol,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mock: read bars %s: %w", symbol, err)
	}
	return out, nil
}
