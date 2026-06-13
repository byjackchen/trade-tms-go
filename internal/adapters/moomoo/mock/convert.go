package mock

// convert.go holds the mock server's domain<->wire helpers that complement the
// client-side converters in the parent package.

import (
	"time"

	"google.golang.org/protobuf/proto"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// klTypeForSubType inverts SubTypeForKLType for the K-line sub-types P5 cares
// about; ok is false for non-K-line sub-types (Basic/OrderBook/etc.).
func klTypeForSubType(st qotcommon.SubType) (qotcommon.KLType, bool) {
	switch st {
	case qotcommon.SubType_SubType_KL_Day:
		return qotcommon.KLType_KLType_Day, true
	case qotcommon.SubType_SubType_KL_1Min:
		return qotcommon.KLType_KLType_1Min, true
	case qotcommon.SubType_SubType_KL_5Min:
		return qotcommon.KLType_KLType_5Min, true
	case qotcommon.SubType_SubType_KL_15Min:
		return qotcommon.KLType_KLType_15Min, true
	case qotcommon.SubType_SubType_KL_30Min:
		return qotcommon.KLType_KLType_30Min, true
	case qotcommon.SubType_SubType_KL_60Min:
		return qotcommon.KLType_KLType_60Min, true
	default:
		return qotcommon.KLType_KLType_Unknown, false
	}
}

// klinesFromBars converts a slice of domain bars to wire KLines via the parent
// package's canonical converter (same code the real-vs-mock paths share).
func klinesFromBars(bars []domain.Bar, kl qotcommon.KLType) []*qotcommon.KLine {
	out := make([]*qotcommon.KLine, 0, len(bars))
	for _, b := range bars {
		out = append(out, mo.KLineFromBar(b, kl))
	}
	return out
}

// basicQotFromBars derives a BasicQot snapshot from a symbol's bar history: the
// latest bar supplies OHLC/volume/curPrice, the prior bar's close supplies
// lastClosePrice. An empty history yields a suspended, zero quote.
func basicQotFromBars(symbol string, bars []domain.Bar) *qotcommon.BasicQot {
	sec := mo.SecurityForSymbol(symbol)
	if len(bars) == 0 {
		return &qotcommon.BasicQot{
			Security:       sec,
			IsSuspended:    proto.Bool(true),
			ListTime:       proto.String(""),
			PriceSpread:    proto.Float64(0),
			UpdateTime:     proto.String(""),
			HighPrice:      proto.Float64(0),
			OpenPrice:      proto.Float64(0),
			LowPrice:       proto.Float64(0),
			CurPrice:       proto.Float64(0),
			LastClosePrice: proto.Float64(0),
			Volume:         proto.Int64(0),
			Turnover:       proto.Float64(0),
			TurnoverRate:   proto.Float64(0),
			Amplitude:      proto.Float64(0),
		}
	}
	last := bars[len(bars)-1]
	lastClose := 0.0
	if len(bars) >= 2 {
		lastClose = bars[len(bars)-2].Close.Float64()
	}
	return &qotcommon.BasicQot{
		Security:       sec,
		IsSuspended:    proto.Bool(false),
		ListTime:       proto.String(bars[0].TS.In(time.UTC).Format("2006-01-02")),
		PriceSpread:    proto.Float64(0.01),
		UpdateTime:     proto.String(mo.FormatKLTime(last.TS, qotcommon.KLType_KLType_Day)),
		HighPrice:      proto.Float64(last.High.Float64()),
		OpenPrice:      proto.Float64(last.Open.Float64()),
		LowPrice:       proto.Float64(last.Low.Float64()),
		CurPrice:       proto.Float64(last.Close.Float64()),
		LastClosePrice: proto.Float64(lastClose),
		Volume:         proto.Int64(last.Volume),
		Turnover:       proto.Float64(last.Close.Float64() * float64(last.Volume)),
		TurnoverRate:   proto.Float64(0),
		Amplitude:      proto.Float64(0),
	}
}
