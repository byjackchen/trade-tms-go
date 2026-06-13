package moomoo

// convert.go bridges between the project's domain types (plain ticker symbols,
// UTC time.Time, fixed-point Money) and moomoo's wire representation
// (market-qualified codes like "US.AAPL", New-York-local "YYYY-MM-DD HH:MM:SS"
// K-line time strings, and float64 prices).
//
// Conventions mirror the Python reference src/adapters/moomoo/data_client.py:
//   - instrument_id_to_moomoo_code: "US.<symbol>" (data_client.py:81)
//   - intraday K-line time strings are New-York-local naive
//     (data_client.py:112 tz_localize("America/New_York"))
//   - daily K-line time strings are "YYYY-MM-DD" at NY midnight.

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// nyLoc is the exchange timezone for US securities. moomoo emits K-line time
// strings in this zone; we attach/strip it on the boundary so the engine sees
// UTC (domain.Bar invariant: TS is UTC).
var nyLoc = mustLoadNY()

func mustLoadNY() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		// The tz database is required for correct DST handling of intraday
		// bars; failing fast is preferable to silently mis-stamping bars.
		panic(fmt.Sprintf("moomoo: load America/New_York: %v", err))
	}
	return loc
}

// USMarket is the QotMarket value for US-listed securities (the only market in
// the project universe). Exposed so callers and the mock server agree.
const USMarket = int32(qotcommon.QotMarket_QotMarket_US_Security)

// SecurityForSymbol builds a moomoo Security for a plain US ticker
// (e.g. "AAPL" -> {market: US, code: "AAPL"}). The wire code form is
// market-qualified at the string level ("US.AAPL") only in user-facing APIs;
// the protobuf Security carries market and bare code separately.
func SecurityForSymbol(symbol string) *qotcommon.Security {
	return &qotcommon.Security{
		Market: proto.Int32(USMarket),
		Code:   proto.String(symbol),
	}
}

// SymbolForSecurity extracts the plain ticker from a moomoo Security.
func SymbolForSecurity(s *qotcommon.Security) string {
	if s == nil {
		return ""
	}
	return s.GetCode()
}

// MoomooCode renders the user-facing "US.AAPL" form (data_client.py:81).
func MoomooCode(symbol string) string { return "US." + symbol }

// SymbolFromMoomooCode parses "US.AAPL" -> "AAPL" (data_client.py:87). A code
// with no market prefix is returned unchanged.
func SymbolFromMoomooCode(code string) string {
	if i := strings.IndexByte(code, '.'); i >= 0 {
		return code[i+1:]
	}
	return code
}

// KLType maps a bar width in seconds to the moomoo KLType. P5 supports the two
// widths the project uses: daily (the project's primary cadence) and 1-minute
// (the live intraday heartbeat). Unsupported widths return an error rather than
// silently picking a wrong type.
func KLTypeForSeconds(barSeconds int) (qotcommon.KLType, error) {
	switch barSeconds {
	case 86400, 0: // 0 is treated as daily (the daily bars table has no bar_seconds)
		return qotcommon.KLType_KLType_Day, nil
	case 60:
		return qotcommon.KLType_KLType_1Min, nil
	case 300:
		return qotcommon.KLType_KLType_5Min, nil
	case 900:
		return qotcommon.KLType_KLType_15Min, nil
	case 1800:
		return qotcommon.KLType_KLType_30Min, nil
	case 3600:
		return qotcommon.KLType_KLType_60Min, nil
	default:
		return qotcommon.KLType_KLType_Unknown, fmt.Errorf("moomoo: unsupported bar width %ds (no KLType)", barSeconds)
	}
}

// SubTypeForKLType maps a KLType to the matching push SubType (Qot_Sub
// subTypeList element). Only the K-line sub-types relevant to P5 are mapped.
func SubTypeForKLType(kl qotcommon.KLType) (qotcommon.SubType, error) {
	switch kl {
	case qotcommon.KLType_KLType_Day:
		return qotcommon.SubType_SubType_KL_Day, nil
	case qotcommon.KLType_KLType_1Min:
		return qotcommon.SubType_SubType_KL_1Min, nil
	case qotcommon.KLType_KLType_5Min:
		return qotcommon.SubType_SubType_KL_5Min, nil
	case qotcommon.KLType_KLType_15Min:
		return qotcommon.SubType_SubType_KL_15Min, nil
	case qotcommon.KLType_KLType_30Min:
		return qotcommon.SubType_SubType_KL_30Min, nil
	case qotcommon.KLType_KLType_60Min:
		return qotcommon.SubType_SubType_KL_60Min, nil
	default:
		return qotcommon.SubType_SubType_None, fmt.Errorf("moomoo: no SubType for KLType %v", kl)
	}
}

// klTimeLayoutIntraday / klTimeLayoutDaily are the moomoo K-line "time" string
// formats (NY-local).
const (
	klTimeLayoutIntraday = "2006-01-02 15:04:05"
	klTimeLayoutDaily    = "2006-01-02"
)

// FormatKLTime renders a UTC instant to moomoo's NY-local K-line time string.
// Daily bars use the date-only layout; intraday bars use the full layout.
func FormatKLTime(tsUTC time.Time, kl qotcommon.KLType) string {
	local := tsUTC.In(nyLoc)
	if kl == qotcommon.KLType_KLType_Day {
		return local.Format(klTimeLayoutDaily)
	}
	return local.Format(klTimeLayoutIntraday)
}

// ParseKLTime parses a moomoo K-line "time" string (NY-local) back to a UTC
// instant. It accepts both the date-only and full layouts. A KLine carrying a
// numeric "timestamp" field should prefer that (see BarFromKLine); this is the
// fallback / mock path.
func ParseKLTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layout := klTimeLayoutIntraday
	if len(s) == len("2006-01-02") {
		layout = klTimeLayoutDaily
	}
	t, err := time.ParseInLocation(layout, s, nyLoc)
	if err != nil {
		return time.Time{}, fmt.Errorf("moomoo: parse K-line time %q: %w", s, err)
	}
	return t.UTC(), nil
}

// BarFromKLine converts a moomoo KLine to a domain.Bar for the given symbol and
// K-line type. Prices go through the project's float64->fixed bridge
// (PriceFromFloat64, Decimal(str(x)) semantics). The bar timestamp prefers the
// numeric epoch-seconds "timestamp" field (moomoo populates it; it is
// unambiguous), falling back to parsing the NY-local "time" string.
func BarFromKLine(symbol string, kl qotcommon.KLType, k *qotcommon.KLine) (domain.Bar, error) {
	var b domain.Bar
	b.Symbol = symbol

	switch {
	case k.Timestamp != nil && k.GetTimestamp() != 0:
		// moomoo timestamp is epoch seconds as a float (data_client uses it).
		sec := k.GetTimestamp()
		whole := int64(sec)
		nanos := int64((sec - float64(whole)) * 1e9)
		b.TS = time.Unix(whole, nanos).UTC()
	default:
		ts, err := ParseKLTime(k.GetTime())
		if err != nil {
			return domain.Bar{}, err
		}
		b.TS = ts
	}

	o, err := domain.PriceFromFloat64(k.GetOpenPrice())
	if err != nil {
		return domain.Bar{}, fmt.Errorf("moomoo: %s open: %w", symbol, err)
	}
	h, err := domain.PriceFromFloat64(k.GetHighPrice())
	if err != nil {
		return domain.Bar{}, fmt.Errorf("moomoo: %s high: %w", symbol, err)
	}
	l, err := domain.PriceFromFloat64(k.GetLowPrice())
	if err != nil {
		return domain.Bar{}, fmt.Errorf("moomoo: %s low: %w", symbol, err)
	}
	c, err := domain.PriceFromFloat64(k.GetClosePrice())
	if err != nil {
		return domain.Bar{}, fmt.Errorf("moomoo: %s close: %w", symbol, err)
	}
	b.Open, b.High, b.Low, b.Close = o, h, l, c
	b.Volume = k.GetVolume()
	return b, nil
}

// KLineFromBar converts a domain.Bar to a moomoo KLine for the given K-line
// type. Used by the mock OpenD server to serve our stored bars over the wire.
// It populates both the NY-local "time" string and the numeric epoch-seconds
// "timestamp" field, matching real OpenD output.
func KLineFromBar(b domain.Bar, kl qotcommon.KLType) *qotcommon.KLine {
	ts := float64(b.TS.Unix())
	return &qotcommon.KLine{
		Time:       proto.String(FormatKLTime(b.TS, kl)),
		IsBlank:    proto.Bool(false),
		HighPrice:  proto.Float64(b.High.Float64()),
		OpenPrice:  proto.Float64(b.Open.Float64()),
		LowPrice:   proto.Float64(b.Low.Float64()),
		ClosePrice: proto.Float64(b.Close.Float64()),
		Volume:     proto.Int64(b.Volume),
		Timestamp:  proto.Float64(ts),
	}
}
