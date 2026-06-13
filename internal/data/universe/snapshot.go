package universe

// snapshot.go persists computed universes to tms.universe_snapshots
// (migration 000002 + the members column from 000007) and reads them back.
// Snapshots are append-only audit records: inputs (window, table filter,
// limit, exclusions, params) + the ordered ticker list + ranked members
// with reasons.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// Snapshot kinds (CHECK constraint on tms.universe_snapshots.kind).
const (
	KindLive     = "live"
	KindEOD      = "eod"
	KindBacktest = "backtest"
	KindManual   = "manual"
)

// ErrNoSnapshot is returned by readers when nothing matches.
var ErrNoSnapshot = errors.New("universe: no snapshot found")

// Member is one ranked universe member as stored in the members JSONB
// array. Rank is 1-based in ranking order (the screener sort key).
type Member struct {
	Ticker             string   `json:"ticker"`
	Rank               int      `json:"rank"`
	Score              float64  `json:"score"`
	TrendTemplateCount int      `json:"trend_template_count"`
	BreakoutProximity  float64  `json:"breakout_proximity"`
	MarketCapUSD       float64  `json:"market_cap_usd"`
	Reasons            []string `json:"reasons"`
}

// Snapshot mirrors one tms.universe_snapshots row.
type Snapshot struct {
	ID          int64
	AsOf        calendar.Date
	Kind        string
	TableFilter string        // "" -> NULL
	WindowStart calendar.Date // zero -> NULL
	WindowEnd   calendar.Date // zero -> NULL
	LimitN      int           // <= 0 -> NULL (uncapped)
	Tickers     []string
	Excluded    []string
	Params      map[string]any
	Members     []Member
	CreatedAt   time.Time
}

// jsonSafe replaces non-finite floats (JSON cannot represent them) with 0.
// The screener only produces non-finite scores from NaN source bars, which
// the reference universe never feeds it; documented in migration 000007.
func jsonSafe(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// SnapshotFromResult converts a Build result into a persistable snapshot.
// Member reasons are the passing trend-template rule names.
func SnapshotFromResult(res *Result) *Snapshot {
	members := make([]Member, 0, len(res.Candidates))
	for i, c := range res.Candidates {
		m := Member{
			Ticker:             c.InstrumentID,
			Rank:               i + 1,
			Score:              jsonSafe(c.Score),
			TrendTemplateCount: c.Metadata["trend_template_count"].(int),
			BreakoutProximity:  jsonSafe(c.Metadata["breakout_proximity"].(float64)),
			MarketCapUSD:       jsonSafe(c.Metadata["market_cap_usd"].(float64)),
			Reasons:            []string{},
		}
		if tt, ok := res.Rules[c.InstrumentID]; ok {
			m.Reasons = tt.PassingRuleNames()
		}
		members = append(members, m)
	}
	kind := res.Kind
	if kind == "" {
		kind = KindManual
	}
	excluded := res.Excluded
	if excluded == nil {
		excluded = []string{}
	}
	return &Snapshot{
		AsOf:        res.AsOf,
		Kind:        kind,
		TableFilter: TableSF1,
		WindowStart: res.WarmupStart,
		WindowEnd:   res.AsOf,
		LimitN:      res.Limit,
		Tickers:     res.Tickers,
		Excluded:    excluded,
		Params: map[string]any{
			"warmup_calendar_days": WarmupCalendarDays,
			"history_max_bars":     DefaultHistoryMaxBars,
			"breakout_lookback":    BreakoutBaseLookback,
			"market_cap_min_usd":   DefaultMarketCapMinUSD,
			"score_weights": map[string]any{
				"trend_template":     scoreWeightTrendTemplate,
				"breakout_proximity": scoreWeightProximity,
			},
			"exclusion_set": ExcludedTickers(),
			"timezone":      "America/New_York",
			"raw_count":     len(res.Raw),
			"warmed":        res.Warmed,
			"warmup_errors": res.WarmupErrors,
		},
		Members: members,
	}
}

// InsertSnapshot appends a snapshot and fills snap.ID/CreatedAt.
func (s *Store) InsertSnapshot(ctx context.Context, snap *Snapshot) error {
	membersJSON, err := json.Marshal(snap.Members)
	if err != nil {
		return fmt.Errorf("universe: marshaling snapshot members: %w", err)
	}
	if snap.Params == nil {
		snap.Params = map[string]any{}
	}
	paramsJSON, err := json.Marshal(snap.Params)
	if err != nil {
		return fmt.Errorf("universe: marshaling snapshot params: %w", err)
	}

	var (
		tableFilter            *string
		windowStart, windowEnd *string
		limitN                 *int
	)
	if snap.TableFilter != "" {
		tableFilter = &snap.TableFilter
	}
	if !snap.WindowStart.IsZero() {
		v := snap.WindowStart.String()
		windowStart = &v
	}
	if !snap.WindowEnd.IsZero() {
		v := snap.WindowEnd.String()
		windowEnd = &v
	}
	if snap.LimitN > 0 {
		limitN = &snap.LimitN
	}
	tickers := snap.Tickers
	if tickers == nil {
		tickers = []string{}
	}
	excluded := snap.Excluded
	if excluded == nil {
		excluded = []string{}
	}

	err = s.pool.QueryRow(ctx, `
		INSERT INTO tms.universe_snapshots
			(as_of, kind, table_filter, window_start, window_end,
			 limit_n, tickers, excluded, params, members)
		VALUES ($1::date, $2, $3, $4::date, $5::date, $6, $7, $8, $9, $10)
		RETURNING id, created_at`,
		snap.AsOf.String(), snap.Kind, tableFilter, windowStart, windowEnd,
		limitN, tickers, excluded, paramsJSON, membersJSON,
	).Scan(&snap.ID, &snap.CreatedAt)
	if err != nil {
		return fmt.Errorf("universe: inserting snapshot (as_of=%s kind=%s): %w", snap.AsOf, snap.Kind, err)
	}
	return nil
}

const snapshotColumns = `
	id, as_of::text, kind, COALESCE(table_filter, ''),
	COALESCE(window_start::text, ''), COALESCE(window_end::text, ''),
	COALESCE(limit_n, 0), tickers, excluded, params, members, created_at`

// scanSnapshot scans one row in snapshotColumns order.
func scanSnapshot(row pgx.Row) (*Snapshot, error) {
	var (
		snap                    Snapshot
		asOf, wStart, wEnd      string
		paramsJSON, membersJSON []byte
	)
	err := row.Scan(&snap.ID, &asOf, &snap.Kind, &snap.TableFilter,
		&wStart, &wEnd, &snap.LimitN, &snap.Tickers, &snap.Excluded,
		&paramsJSON, &membersJSON, &snap.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, fmt.Errorf("universe: scanning snapshot: %w", err)
	}
	if snap.AsOf, err = calendar.ParseDate(asOf); err != nil {
		return nil, err
	}
	if wStart != "" {
		if snap.WindowStart, err = calendar.ParseDate(wStart); err != nil {
			return nil, err
		}
	}
	if wEnd != "" {
		if snap.WindowEnd, err = calendar.ParseDate(wEnd); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(paramsJSON, &snap.Params); err != nil {
		return nil, fmt.Errorf("universe: decoding snapshot %d params: %w", snap.ID, err)
	}
	if err := json.Unmarshal(membersJSON, &snap.Members); err != nil {
		return nil, fmt.Errorf("universe: decoding snapshot %d members: %w", snap.ID, err)
	}
	return &snap, nil
}

// SnapshotByID loads one snapshot; ErrNoSnapshot when absent.
func (s *Store) SnapshotByID(ctx context.Context, id int64) (*Snapshot, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+snapshotColumns+` FROM tms.universe_snapshots WHERE id = $1`, id)
	return scanSnapshot(row)
}

// LatestSnapshot returns the newest snapshot of a kind (as_of DESC, then
// id DESC for same-day rebuilds); ErrNoSnapshot when none exist. An empty
// kind matches every kind (the API's "latest, whatever produced it" read).
func (s *Store) LatestSnapshot(ctx context.Context, kind string) (*Snapshot, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+snapshotColumns+`
		 FROM tms.universe_snapshots
		 WHERE ($1 = '' OR kind = $1)
		 ORDER BY as_of DESC, id DESC
		 LIMIT 1`, kind)
	return scanSnapshot(row)
}

// SnapshotAsOf returns the newest snapshot of a kind with as_of <= asOf
// (point-in-time read); ErrNoSnapshot when none qualify.
func (s *Store) SnapshotAsOf(ctx context.Context, kind string, asOf calendar.Date) (*Snapshot, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+snapshotColumns+`
		 FROM tms.universe_snapshots
		 WHERE kind = $1 AND as_of <= $2::date
		 ORDER BY as_of DESC, id DESC
		 LIMIT 1`, kind, asOf.String())
	return scanSnapshot(row)
}
