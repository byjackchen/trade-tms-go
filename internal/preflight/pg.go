package preflight

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/sharadar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/params/paramsdb"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sectorrotation"
)

// sepaWarmupBars is the SEPA lookback: the trend template needs a fully formed
// MA200, so 200 daily bars are the minimum warmup depth per SEPA stock.
const sepaWarmupBars = 200

// defaultWindowCalendarDays bounds the universe/warmup resolution window. It is
// the live Assembler's default warmup horizon (a year+ of calendar days), large
// enough that the SF1 window universe and the per-strategy lookbacks resolve
// against real coverage. The exact value only affects survivor-bias filtering of
// the universe, not the bar-depth count (which the probe measures directly).
const defaultWindowCalendarDays = 400

// PGProbes is the live Probes implementation: PostgreSQL (bars / caps / universe
// / fundamentals), the sharadar Store (the shared data frontier), the NYSE
// calendar, the params loader (promotion provenance + lookbacks), Redis, and a
// moomoo OpenD dial. It is the single wiring used by both the CLI subcommand and
// the API endpoint.
type PGProbes struct {
	pool      *pgxpool.Pool
	store     *sharadar.Store
	uni       *universe.Store
	loader    *params.Loader
	cal       *calendar.Calendar
	redis     *redis.Client
	moomooCfg moomoo.Options
	log       zerolog.Logger
}

// PGProbesConfig wires a PGProbes.
type PGProbesConfig struct {
	Pool      *pgxpool.Pool
	Calendar  *calendar.Calendar
	Redis     *redis.Client // may be nil (Redis-less); the Redis check then fails (blocker)
	ParamsDir string
	MoomooCfg moomoo.Options // Addr + MaxSubscriptions for the OpenD probe
	Log       zerolog.Logger
}

// NewPGProbes builds the live probes.
func NewPGProbes(c PGProbesConfig) *PGProbes {
	return &PGProbes{
		pool:      c.Pool,
		store:     sharadar.NewStore(c.Pool),
		uni:       universe.NewStore(c.Pool),
		loader:    params.NewLoader(paramsdb.NewReader(c.Pool), c.ParamsDir),
		cal:       c.Calendar,
		redis:     c.Redis,
		moomooCfg: c.MoomooCfg,
		log:       c.Log.With().Str("component", "preflight-probes").Logger(),
	}
}

var _ Probes = (*PGProbes)(nil)

func (p *PGProbes) PingPostgres(ctx context.Context) error {
	if p.pool == nil {
		return ErrNotConfigured
	}
	return p.pool.Ping(ctx)
}

func (p *PGProbes) PingRedis(ctx context.Context) error {
	if p.redis == nil {
		return ErrNotConfigured
	}
	return p.redis.Ping(ctx).Err()
}

func (p *PGProbes) DataFrontier(ctx context.Context, dataset string) (calendar.Date, bool, error) {
	return p.store.DataFrontier(ctx, dataset)
}

func (p *PGProbes) FundamentalsFrontier(ctx context.Context) (calendar.Date, bool, error) {
	return p.store.DataFrontier(ctx, sharadar.DatasetSF1)
}

// TradingTMinus1 resolves T-1 (the most recent NYSE session strictly before the
// exchange-local today) — the same target catchupWindow uses.
func (p *PGProbes) TradingTMinus1(now time.Time) (calendar.Date, error) {
	today := calendar.DateOf(now, p.cal.Location())
	sess, err := p.cal.PrevSession(today)
	if err != nil {
		return calendar.Date{}, fmt.Errorf("preflight: T-1 for %s: %w", today, err)
	}
	return sess.Date, nil
}

// TradingDaysBetween counts NYSE sessions in (from, to] — the staleness gap.
func (p *PGProbes) TradingDaysBetween(from, to calendar.Date) (int, error) {
	if !from.Before(to) {
		return 0, nil
	}
	// Sessions strictly after `from` up to and including `to`.
	sessions, err := p.cal.SessionsInRange(from.AddDays(1), to)
	if err != nil {
		return 0, fmt.Errorf("preflight: counting sessions (%s, %s]: %w", from, to, err)
	}
	return len(sessions), nil
}

func (p *PGProbes) ListUniverseForWindow(ctx context.Context, start, end calendar.Date, table string) ([]string, error) {
	return p.uni.ListUniverseForWindow(ctx, start, end, table)
}

func (p *PGProbes) MarketCaps(ctx context.Context, tickers []string) (map[string]float64, error) {
	return p.uni.MarketCaps(ctx, tickers)
}

// BarsAvailable counts daily bars on/before asOf per symbol in one query.
func (p *PGProbes) BarsAvailable(ctx context.Context, symbols []string, asOf calendar.Date) (map[string]int, error) {
	out := make(map[string]int, len(symbols))
	for _, s := range symbols {
		out[s] = 0
	}
	if len(symbols) == 0 {
		return out, nil
	}
	cutoff := time.Date(asOf.Year, asOf.Month, asOf.Day, 0, 0, 0, 0, time.UTC)
	rows, err := p.pool.Query(ctx, `
		SELECT ticker, count(*)
		FROM tms.bars_daily
		WHERE ticker = ANY($1) AND ts <= $2
		GROUP BY ticker`, symbols, cutoff)
	if err != nil {
		return nil, fmt.Errorf("preflight: counting bars: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			t string
			n int
		)
		if err := rows.Scan(&t, &n); err != nil {
			return nil, fmt.Errorf("preflight: scanning bar count: %w", err)
		}
		out[t] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("preflight: reading bar counts: %w", err)
	}
	return out, nil
}

// OpenDState dials OpenD, waits for the handshake, and reads GetGlobalState. A
// short bounded probe (it must not hang the preflight). No secrets are logged.
func (p *PGProbes) OpenDState(ctx context.Context) error {
	if strings.TrimSpace(p.moomooCfg.Addr) == "" {
		return errors.New("no moomoo address configured (TMS_MOOMOO_ADDR)")
	}
	client := moomoo.NewClient(p.moomooCfg)
	client.Start(ctx)
	defer func() { _ = client.Close() }()

	readyCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := client.Ready(readyCtx); err != nil {
		return fmt.Errorf("OpenD handshake: %w", err)
	}
	stCtx, cancelSt := context.WithTimeout(ctx, 5*time.Second)
	defer cancelSt()
	if _, err := client.GetGlobalState(stCtx); err != nil {
		return fmt.Errorf("GetGlobalState: %w", err)
	}
	return nil
}

// OpenDMaxSubscriptions returns the resolved OpenD per-connection subscription
// cap: the configured TMS_MOOMOO_MAX_SUB (moomooCfg.MaxSubscriptions), or
// moomoo.DefaultMaxSubscriptions when unset/non-positive — the SAME default the
// moomoo client applies (Options.withDefaults), so preflight sizes the live
// subscription set against the exact ceiling the client enforces.
func (p *PGProbes) OpenDMaxSubscriptions() int {
	if p.moomooCfg.MaxSubscriptions > 0 {
		return p.moomooCfg.MaxSubscriptions
	}
	return moomoo.DefaultMaxSubscriptions
}

// ResolveStrategy resolves the enabled strategies + their warmup exactly as the
// live Assembler would: same param resolution (so promotion provenance matches),
// same default-SEPA-universe fallback (ListUniverseForWindow over the window),
// and the same per-strategy lookback bars. The window mirrors the Assembler's
// live window (the warmup horizon up to T-1, so the bar-depth probe measures the
// bars the session will actually warm against).
func (p *PGProbes) ResolveStrategy(ctx context.Context, cfg Config) (*ResolvedSession, error) {
	strat := cfg.Strategy
	switch strat {
	case "sepa", "sector_rotation", "pairs", "orb", "multi":
	default:
		return nil, fmt.Errorf("unsupported strategy %q (want sepa|sector_rotation|pairs|orb|multi)", strat)
	}

	// Window: [T-1 - horizon, T-1]. T-1 is the live session's effective last
	// settled bar; warming against it is what the live path does.
	now := time.Now()
	if cfg.Now != nil {
		now = cfg.Now()
	}
	end, err := p.TradingTMinus1(now)
	if err != nil {
		return nil, err
	}
	start := end.AddDays(-defaultWindowCalendarDays)

	res := &ResolvedSession{WindowStart: start, WindowEnd: end}

	// SEPA stock universe (sepa/multi): explicit --tickers, else the default SF1
	// window universe (the SAME fallback live uses).
	needSEPA := strat == "sepa" || strat == "multi"
	if needSEPA {
		uni := normTickers(cfg.Tickers)
		if len(uni) == 0 {
			def, derr := p.uni.ListUniverseForWindow(ctx, start, end, universe.TableSF1)
			if derr != nil {
				return nil, fmt.Errorf("resolving default SEPA universe [%s, %s]: %w", start, end, derr)
			}
			uni = def
		}
		res.SEPAUniverse = uni
	}

	// Per-strategy warmup + promotion provenance.
	add := func(name string, syms []string, lookback int, promoted bool, source string) {
		res.Strategies = append(res.Strategies, EnabledStrategy{
			Name: name, WarmupSymbols: syms, LookbackBars: lookback,
			Promoted: promoted, ParamSource: source,
			Screened: name == "sepa", // SEPA screens a survivor-bias-free universe; others trade fixed baskets.
		})
	}

	resolveSEPA := func() error {
		_, doc, perr := p.loader.SEPA(ctx)
		if perr != nil {
			return fmt.Errorf("resolve sepa params: %w", perr)
		}
		add("sepa", res.SEPAUniverse, sepaWarmupBars, isPromoted(doc), source(doc))
		return nil
	}
	resolveSector := func() error {
		sp, doc, perr := p.loader.SectorRotation(ctx)
		if perr != nil {
			return fmt.Errorf("resolve sector params: %w", perr)
		}
		uni := sp.Universe
		if len(uni) == 0 {
			uni = sectorrotation.DefaultUniverse
		}
		syms := append(append([]string(nil), uni...), "SPY")
		add("sector_rotation", syms, int(sp.MomentumLookback)+1, isPromoted(doc), source(doc))
		return nil
	}
	resolvePairs := func() error {
		pp, doc, perr := p.loader.Pairs(ctx)
		if perr != nil {
			return fmt.Errorf("resolve pairs params: %w", perr)
		}
		legSet := map[string]struct{}{}
		for _, pair := range pp.Pairs {
			legSet[pair.LongLeg] = struct{}{}
			legSet[pair.ShortLeg] = struct{}{}
		}
		legs := make([]string, 0, len(legSet))
		for l := range legSet {
			if l != "" {
				legs = append(legs, l)
			}
		}
		add("pairs", legs, int(pp.Lookback), isPromoted(doc), source(doc))
		return nil
	}
	resolveORB := func() error {
		_, doc, perr := p.loader.IntradayBreakout(ctx)
		if perr != nil {
			return fmt.Errorf("resolve orb params: %w", perr)
		}
		sym := strings.TrimSpace(cfg.ORBSymbol)
		var syms []string
		if sym != "" {
			syms = []string{strings.ToUpper(sym)}
		} else if len(cfg.Tickers) == 1 {
			syms = normTickers(cfg.Tickers)
		}
		// ORB is an intraday opening-range strategy: its lookback is intraday, so
		// daily-bar warmup depth is not the gate. Lookback 0 means "no daily warmup
		// required" (the warmup check still flags a missing symbol).
		add("orb", syms, 0, isPromoted(doc), source(doc))
		return nil
	}

	switch strat {
	case "sepa":
		if err := resolveSEPA(); err != nil {
			return nil, err
		}
	case "sector_rotation":
		if err := resolveSector(); err != nil {
			return nil, err
		}
	case "pairs":
		if err := resolvePairs(); err != nil {
			return nil, err
		}
	case "orb":
		if err := resolveORB(); err != nil {
			return nil, err
		}
	case "multi":
		if err := resolveSEPA(); err != nil {
			return nil, err
		}
		if err := resolveSector(); err != nil {
			return nil, err
		}
		if err := resolvePairs(); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// isPromoted reports whether the document came from a promoted active_params row.
func isPromoted(doc *params.Document) bool {
	return doc != nil && doc.Source == params.OriginDB
}

// source renders the document provenance for the detail line.
func source(doc *params.Document) string {
	if doc == nil {
		return "baseline"
	}
	return string(doc.Source)
}

// normTickers upper-cases + trims + dedupes a ticker slice (order preserved).
func normTickers(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, t := range in {
		t = strings.ToUpper(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
