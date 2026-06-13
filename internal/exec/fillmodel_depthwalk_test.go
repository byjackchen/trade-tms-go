package exec

// fillmodel_depthwalk_test.go locks the nautilus-compat depth-walk rule against
// the empirically observed Nautilus BacktestEngine matching, captured in
// tmp/parity/nautilus_out/depthwalk.json by tmp/parity/probe_depthwalk.py.
//
// The golden table covers volume/qty/side permutations that exercise both
// branches of compute_bar_quarter_sizes (large volumes where quarter = vol//4,
// and small volumes where the quarter floors to min_size=1 and the underflow
// guard fires). The fixture is checked into testdata/ so the unit test runs
// without the Python harness; regenerate it with `make parity-depthwalk`.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func depthwalkTS() time.Time { return time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC) }

// depthwalkCase mirrors one row of depthwalk.json.
type depthwalkCase struct {
	Volume       int64  `json:"volume"`
	CloseTickVol int64  `json:"close_tick_vol"`
	Side         string `json:"side"`
	Qty          int64  `json:"qty"`
	Legs         []struct {
		Qty string `json:"qty"`
		Px  string `json:"px"`
	} `json:"legs"`
}

func TestNautilusCompatDepthWalkGolden(t *testing.T) {
	path := filepath.Join("testdata", "depthwalk.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "golden depth-walk table must be present (make parity-depthwalk)")

	var cases []depthwalkCase
	require.NoError(t, json.Unmarshal(raw, &cases))
	require.NotEmpty(t, cases)

	const closePx = "105.00" // the probe's bar close
	model := NautilusCompatModel{}

	for _, c := range cases {
		name := fmt.Sprintf("vol%d_%s_qty%d", c.Volume, c.Side, c.Qty)
		t.Run(name, func(t *testing.T) {
			// Assert our close-tick formula matches Nautilus's reported ctv.
			assert.Equal(t, c.CloseTickVol, closeTickVolume(c.Volume),
				"close_tick_vol(vol=%d)", c.Volume)

			side := domain.OrderSideBuy
			if c.Side == "SELL" {
				side = domain.OrderSideSell
			}
			order := domain.NewMarketOrder("O-1", "S-000", "TST", side,
				domain.Qty(c.Qty), "probe", depthwalkTS())
			bar := domain.Bar{
				Symbol: "TST",
				TS:     depthwalkTS(),
				Open:   domain.MustPrice("100.00"),
				High:   domain.MustPrice("110.00"),
				Low:    domain.MustPrice("90.00"),
				Close:  domain.MustPrice(closePx),
				Volume: c.Volume,
			}
			legs, err := model.Fill(order, bar)
			require.NoError(t, err)

			require.Len(t, legs, len(c.Legs), "leg count for %s", name)
			for i, want := range c.Legs {
				wantQty, perr := strconv.ParseInt(want.Qty, 10, 64)
				require.NoError(t, perr)
				assert.Equal(t, wantQty, int64(legs[i].Qty), "%s leg %d qty", name, i)
				wantPx, perr := domain.ParsePrice(want.Px)
				require.NoError(t, perr)
				assert.Equal(t, wantPx, legs[i].Price, "%s leg %d px", name, i)
			}
		})
	}
}
