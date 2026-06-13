package universe

// live.go ports the live universe assembly + top-N market-cap cap of
// runner/live_runner.py (docs/spec/calendar-universe.md §4) and persists
// the result as a universe snapshot.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// Live universe parameters (live_runner.py:43-60,256-298 [MUST-MATCH]).
const (
	// DefaultLiveUniverseLimit is the top-N cap default: moomoo OpenD caps
	// one account at 100 K-line subscriptions; SPY + sector ETFs + pair
	// legs take ~15.
	DefaultLiveUniverseLimit = 85
	// EnvLiveUniverseLimit overrides the cap.
	EnvLiveUniverseLimit = "TMS_LIVE_UNIVERSE_LIMIT"
	// WarmupCalendarDays is the warmup window (2*365 calendar days, no
	// leap handling — live_runner.py:258).
	WarmupCalendarDays = 730
)

// sectorETFs are the 11 Select Sector SPDR ETFs in
// sector_rotation.DEFAULT_UNIVERSE source order (signal.py:50-63
// [MUST-MATCH]).
var sectorETFs = [...]string{
	"XLK", "XLF", "XLE", "XLV", "XLY", "XLP", "XLU", "XLB", "XLI", "XLRE", "XLC",
}

// defaultPairs are strategies/pairs.DEFAULT_PAIRS in source order
// (docs/spec/strategy-pairs.md; signal.py:46-51 [MUST-MATCH]).
var defaultPairs = [...][2]string{
	{"KO", "PEP"},
	{"MA", "V"},
	{"XOM", "CVX"},
}

// SectorETFTickers returns a copy of the 11 sector ETFs in source order.
func SectorETFTickers() []string {
	out := make([]string, len(sectorETFs))
	copy(out, sectorETFs[:])
	return out
}

// DefaultPairs returns a copy of the configured pair tuples.
func DefaultPairs() [][2]string {
	out := make([][2]string, len(defaultPairs))
	copy(out, defaultPairs[:])
	return out
}

// PairLegTickers returns the deduped + sorted pair-leg subscription set:
// CVX, KO, MA, PEP, V, XOM (live_runner.py:260-261 [MUST-MATCH]). Pair
// legs are intentionally NOT excluded from the SEPA universe (spec §4.1).
func PairLegTickers() []string {
	set := make(map[string]struct{}, 2*len(defaultPairs))
	for _, p := range defaultPairs {
		set[p[0]] = struct{}{}
		set[p[1]] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ExcludedTickers returns the exact SEPA exclusion list: SPY plus the 11
// sector ETFs (live_runner.py:282-284 [MUST-MATCH]; pair legs are NOT in
// this set).
func ExcludedTickers() []string {
	return append([]string{"SPY"}, sectorETFs[:]...)
}

// excludedSet returns the exclusion list as a set.
func excludedSet() map[string]struct{} {
	set := make(map[string]struct{}, 1+len(sectorETFs))
	set["SPY"] = struct{}{}
	for _, t := range sectorETFs {
		set[t] = struct{}{}
	}
	return set
}

// ResolveUniverseLimit reads EnvLiveUniverseLimit via getenv (nil ->
// os.Getenv): unset/blank -> 85; integer after trimming -> that value
// (zero/negative allowed, handled downstream); anything else fails fast
// with the reference's message (live_runner.py:50-60 [MUST-MATCH]).
func ResolveUniverseLimit(getenv func(string) string) (int, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	raw := strings.TrimSpace(getenv(EnvLiveUniverseLimit))
	if raw == "" {
		return DefaultLiveUniverseLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", EnvLiveUniverseLimit, raw)
	}
	return n, nil
}

// ApplyUniverseLimit caps the SEPA universe to the top `limit` tickers by
// market cap descending (live_runner.py:63-87 [MUST-MATCH]):
//
//   - limit <= 0 or empty input -> empty;
//   - len(input) <= limit -> the input unchanged (original order);
//   - otherwise a STABLE sort by cap descending (ties — including all the
//     0.0 "unknown" caps — keep their input order) truncated to limit.
//
// Like Python's sorted(key=...), the lookup runs exactly once per ticker.
func ApplyUniverseLimit(tickers []string, lookup MarketCapLookup, limit int) []string {
	if limit <= 0 || len(tickers) == 0 {
		return []string{}
	}
	out := make([]string, len(tickers))
	copy(out, tickers)
	if len(out) <= limit {
		return out
	}
	caps := make([]float64, len(out))
	for i, t := range out {
		caps[i] = lookup(t)
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return caps[idx[a]] > caps[idx[b]] })
	ranked := make([]string, limit)
	for i := 0; i < limit; i++ {
		ranked[i] = out[idx[i]]
	}
	return ranked
}

// Builder computes, ranks and snapshots universes from the Store.
type Builder struct {
	store *Store
	cal   *calendar.Calendar
	log   zerolog.Logger
}

// NewBuilder wires a Builder. cal supplies the America/New_York zone used
// to normalize "today" (P1 locked decision; see package docs).
func NewBuilder(store *Store, cal *calendar.Calendar, log zerolog.Logger) *Builder {
	return &Builder{store: store, cal: cal, log: log.With().Str("component", "universe").Logger()}
}

// BuildParams parameterizes one universe build.
type BuildParams struct {
	// Now is the clock instant; zero means time.Now(). The as-of trading
	// date is its America/New_York calendar date.
	Now time.Time
	// Limit is the top-N market-cap cap (use ResolveUniverseLimit for the
	// env/default flow). Ignored when Uncapped. Per the reference,
	// limit <= 0 yields an EMPTY universe.
	Limit int
	// Uncapped skips the cap entirely — backtest parity (spec §4.4: the
	// backtest assembly applies no top-N cap).
	Uncapped bool
	// Kind labels the snapshot: live | eod | backtest | manual.
	Kind string
	// TopK bounds the ranked members; <= 0 ranks the full final universe.
	TopK int
}

// Result is one computed universe with full ranking diagnostics.
type Result struct {
	AsOf        calendar.Date
	WarmupStart calendar.Date
	Kind        string
	Limit       int // 0 when Uncapped
	// Raw is the post-exclusion, pre-cap SF1 universe (sorted ascending).
	Raw []string
	// Excluded lists the exclusion-set tickers actually removed, in raw
	// universe order.
	Excluded []string
	// Tickers is the final universe: Raw capped to top-N by market cap
	// (cap-descending order when the cap bites, raw order otherwise).
	Tickers []string
	// Caps maps every Raw ticker to its market cap (0.0 = unknown).
	Caps map[string]float64
	// Candidates is the screener ranking over Tickers (len <= TopK).
	Candidates []Candidate
	// Rules holds per-ticker trend-template diagnostics for Candidates.
	Rules map[string]TrendTemplateResult
	// Warmed counts tickers successfully warmed; WarmupErrors records the
	// per-ticker failures that were skipped (warn-and-continue, spec §4.1).
	Warmed       int
	WarmupErrors []string
}

// Build computes the SEPA universe exactly like the live assembly
// (spec §4.1-4.3): NY as-of date, 730-day warmup window, SF1 window
// universe, exclusions, market-cap cap; then warms a fresh screener from
// stored bars and ranks. Infrastructure errors abort; per-ticker warmup
// failures are logged and skipped.
func (b *Builder) Build(ctx context.Context, p BuildParams) (*Result, error) {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	asOf := calendar.DateOf(now, b.cal.Location())
	warmupStart := asOf.AddDays(-WarmupCalendarDays)

	raw, err := b.store.ListUniverseForWindow(ctx, warmupStart, asOf, TableSF1)
	if err != nil {
		return nil, err
	}

	excl := excludedSet()
	universe := make([]string, 0, len(raw))
	var removed []string
	for _, t := range raw {
		if _, drop := excl[t]; drop {
			removed = append(removed, t)
			continue
		}
		universe = append(universe, t)
	}

	caps, err := b.store.MarketCaps(ctx, universe)
	if err != nil {
		return nil, err
	}
	lookup := func(t string) float64 { return caps[t] }

	final := universe
	limit := p.Limit
	if p.Uncapped {
		limit = 0
	} else {
		final = ApplyUniverseLimit(universe, lookup, limit)
	}
	b.log.Info().
		Str("as_of", asOf.String()).
		Str("warmup_start", warmupStart.String()).
		Int("raw", len(universe)).
		Int("limit", limit).
		Bool("uncapped", p.Uncapped).
		Int("final", len(final)).
		Msg("sepa universe assembled")

	scr, err := NewScreener(ScreenerConfig{MarketCapLookup: lookup})
	if err != nil {
		return nil, err
	}
	res := &Result{
		AsOf:        asOf,
		WarmupStart: warmupStart,
		Kind:        p.Kind,
		Limit:       limit,
		Raw:         universe,
		Excluded:    removed,
		Tickers:     final,
		Caps:        caps,
		Rules:       make(map[string]TrendTemplateResult, len(final)),
	}
	for _, t := range final {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("universe: build canceled: %w", err)
		}
		bars, err := b.store.GetBars(ctx, t, warmupStart, asOf)
		if err == nil {
			err = scr.Warmup(t, bars)
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("universe: build canceled: %w", ctxErr)
			}
			// Warn-and-continue, the reference's warmup failure policy.
			b.log.Warn().Str("ticker", t).Err(err).Msg("warmup failed; ticker skipped")
			res.WarmupErrors = append(res.WarmupErrors, fmt.Sprintf("%s: %v", t, err))
			continue
		}
		res.Warmed++
	}

	k := p.TopK
	if k <= 0 {
		k = len(final)
	}
	res.Candidates = scr.TopK(k, asOf)
	for _, c := range res.Candidates {
		if tt, ok := scr.Evaluate(c.InstrumentID); ok {
			res.Rules[c.InstrumentID] = tt
		}
	}
	return res, nil
}
