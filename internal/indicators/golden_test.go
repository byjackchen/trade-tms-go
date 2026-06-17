package indicators

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Golden tolerance. Every float assertion must match the pinned golden vectors
// in testdata/golden.json within this bound.
const tol = 1e-9

// golden mirrors testdata/golden.json. Floats decode as *float64 so JSON null
// (the NaN sentinel) becomes a nil pointer we map back to NaN.
type optFloat struct{ v float64 }

func (o *optFloat) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		o.v = math.NaN()
		return nil
	}
	return json.Unmarshal(b, &o.v)
}

func (o optFloat) f() float64 { return o.v }

type rollCase struct {
	Window int        `json:"window"`
	Ddof   int        `json:"ddof"`
	Out    []optFloat `json:"out"`
}
type rollBlock struct {
	X     []float64  `json:"x"`
	Cases []rollCase `json:"cases"`
}

// rollBlockOpt is a rolling block whose input vector may contain NaN (encoded
// as JSON null). Used for the NaN-propagation fixtures.
type rollBlockOpt struct {
	X     []optFloat `json:"x"`
	Cases []rollCase `json:"cases"`
}

func (b rollBlockOpt) xs() []float64 {
	out := make([]float64, len(b.X))
	for i := range b.X {
		out[i] = b.X[i].f()
	}
	return out
}

type goldenData struct {
	SMA           rollBlock    `json:"sma"`
	SMANaN        rollBlockOpt `json:"sma_nan"`
	SMABig        rollBlock    `json:"sma_big"`
	RollingSum    rollBlock    `json:"rolling_sum"`
	RollingStd    rollBlock    `json:"rolling_std"`
	RollingStdBig rollBlock    `json:"rolling_std_big"`
	RollingMax    rollBlock    `json:"rolling_max"`
	RollingMin    rollBlock    `json:"rolling_min"`
	RollingMaxBig rollBlock    `json:"rolling_max_big"`
	RollingMinBig rollBlock    `json:"rolling_min_big"`
	PctReturn     rollBlock    `json:"pct_return"`
	PctReturnBig  rollBlock    `json:"pct_return_big"`

	WindowReturn struct {
		Deque []float64 `json:"deque"`
		Out   optFloat  `json:"out"`
	} `json:"window_return"`

	ATR struct {
		High      []float64  `json:"high"`
		Low       []float64  `json:"low"`
		Close     []float64  `json:"close"`
		TrueRange []optFloat `json:"true_range"`
		Wilder14  []optFloat `json:"wilder_14"`
		Simple14  []optFloat `json:"simple_14"`
	} `json:"atr"`

	Stats struct {
		Spread []float64 `json:"spread"`
		FMean  float64   `json:"fmean"`
		PStdev float64   `json:"pstdev"`
		Stdev  float64   `json:"stdev"`
		ZScore float64   `json:"zscore"`
	} `json:"stats"`

	RollingZScore struct {
		X      []float64  `json:"x"`
		Window int        `json:"window"`
		Out    []optFloat `json:"out"`
	} `json:"rolling_zscore"`

	OLS struct {
		X                []float64 `json:"x"`
		Y                []float64 `json:"y"`
		Slope            float64   `json:"slope"`
		Intercept        float64   `json:"intercept"`
		PerfectLineSlope float64   `json:"perfect_line_slope"`
		DegenerateSlope  *float64  `json:"degenerate_slope"`
		Correlation      float64   `json:"correlation"`
	} `json:"ols"`

	MAHelpers struct {
		Close          []float64 `json:"close"`
		MASlopePct     float64   `json:"ma_slope_pct_200_20"`
		MAUptrendDays  int       `json:"ma_uptrend_days_200"`
		RollingHigh252 optFloat  `json:"rolling_high_252_last"`
		RollingLow252  optFloat  `json:"rolling_low_252_last"`
	} `json:"ma_helpers"`

	Swing struct {
		High     []float64 `json:"high"`
		Low      []float64 `json:"low"`
		Lookback int       `json:"lookback"`
		Swings   []struct {
			Idx   int     `json:"idx"`
			Price float64 `json:"price"`
			Kind  string  `json:"kind"`
		} `json:"swings"`
	} `json:"swing"`

	RoundHalfEven []struct {
		X  float64 `json:"x"`
		D2 float64 `json:"d2"`
		D3 float64 `json:"d3"`
		D0 float64 `json:"d0"`
	} `json:"round_half_even"`

	TrendTemplateLinear struct {
		Close           []float64 `json:"close"`
		High            []float64 `json:"high"`
		Low             []float64 `json:"low"`
		MarketCapUSD    float64   `json:"market_cap_usd"`
		MarketCapMinUSD float64   `json:"market_cap_min_usd"`
		Passed          bool      `json:"passed"`
		PassingRules    int       `json:"passing_rules"`
		CloseOut        float64   `json:"close_out"`
		MA50            float64   `json:"ma50"`
		MA150           float64   `json:"ma150"`
		MA200           float64   `json:"ma200"`
		High52w         float64   `json:"high_52w"`
		Low52w          float64   `json:"low_52w"`
		MA200Uptrend    int       `json:"ma200_uptrend_days"`
		Rules           []bool    `json:"rules"`
	} `json:"trend_template_linear"`

	TrendTemplateShort struct {
		Close        []float64 `json:"close"`
		Passed       bool      `json:"passed"`
		PassingRules int       `json:"passing_rules"`
		CloseOut     float64   `json:"close_out"`
		Rule8        bool      `json:"rule8"`
	} `json:"trend_template_short"`

	Stage map[string]struct {
		Close []float64 `json:"close"`
		Stage string    `json:"stage"`
	} `json:"stage"`

	VCP struct {
		High                         []float64 `json:"high"`
		Low                          []float64 `json:"low"`
		Volume                       []float64 `json:"volume"`
		Lookback                     int       `json:"lookback"`
		Detected                     bool      `json:"detected"`
		Contractions                 []float64 `json:"contractions"`
		LastContractionPct           float64   `json:"last_contraction_pct"`
		PivotPrice                   float64   `json:"pivot_price"`
		BaseLengthDays               int       `json:"base_length_days"`
		VolumeDryup                  bool      `json:"volume_dryup"`
		QualityScore                 float64   `json:"quality_score"`
		VolDryupRatio                float64   `json:"vol_dryup_ratio"`
		FinalContractionDurationDays int       `json:"final_contraction_duration_days"`
	} `json:"vcp"`

	VCPLinear struct {
		High     []float64 `json:"high"`
		Low      []float64 `json:"low"`
		Volume   []float64 `json:"volume"`
		Lookback int       `json:"lookback"`
		Detected bool      `json:"detected"`
	} `json:"vcp_linear"`

	BreakoutVolume struct {
		Volume              []float64 `json:"volume"`
		Base                int       `json:"base"`
		BaselineExclCurrent float64   `json:"baseline_excl_current"`
		Multiple            float64   `json:"multiple"`
		OK                  bool      `json:"ok"`
	} `json:"breakout_volume"`
}

func loadGolden(t *testing.T) goldenData {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatalf("read golden.json: %v", err)
	}
	var g goldenData
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("unmarshal golden.json: %v", err)
	}
	return g
}

// assertClose compares got vs want allowing NaN==NaN, else within tol.
func assertClose(t *testing.T, ctx string, got, want float64) {
	t.Helper()
	if math.IsNaN(want) {
		if !math.IsNaN(got) {
			t.Errorf("%s: want NaN, got %v", ctx, got)
		}
		return
	}
	if math.IsNaN(got) {
		t.Errorf("%s: got NaN, want %v", ctx, want)
		return
	}
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %.17g want %.17g (diff %.3g)", ctx, got, want, math.Abs(got-want))
	}
}

func assertSeries(t *testing.T, ctx string, got []float64, want []optFloat) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length got %d want %d", ctx, len(got), len(want))
	}
	for i := range got {
		assertClose(t, ctx+"["+itoa(i)+"]", got[i], want[i].f())
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
