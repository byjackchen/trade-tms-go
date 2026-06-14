package runner_test

// sectorfixtures_test.go holds the small sector-rotation fixtures shared by the
// hermetic feed test (feed_test.go) and the integration-tagged live tests
// (live_integration_test.go). It carries no build tag so the plain `go test`
// build can see these helpers; the integration tests reuse them under the tag.

import (
	"github.com/byjackchen/trade-tms-go/internal/params"
)

// paramsSector is a wide 8-ETF sector universe with a short momentum lookback,
// so a 4-date series produces a real rebalance.
func paramsSector() params.SectorRotationParams {
	return params.SectorRotationParams{
		Universe:         []string{"E1", "E2", "E3", "E4", "E5", "E6", "E7", "E8"},
		MomentumLookback: 2,
		TopK:             8,
		Timezone:         "America/New_York",
	}
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
