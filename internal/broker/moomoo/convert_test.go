package moomoo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func TestSymbolMapping(t *testing.T) {
	require.Equal(t, "US.AAPL", MoomooCode("AAPL"))
	require.Equal(t, "AAPL", SymbolFromMoomooCode("US.AAPL"))
	require.Equal(t, "AAPL", SymbolFromMoomooCode("AAPL")) // no prefix tolerated

	sec := SecurityForSymbol("MSFT")
	require.Equal(t, "MSFT", sec.GetCode())
	require.Equal(t, USMarket, sec.GetMarket())
	require.Equal(t, "MSFT", SymbolForSecurity(sec))
	require.Equal(t, "", SymbolForSecurity(nil))
}

func TestKLTypeForSeconds(t *testing.T) {
	cases := map[int]qotcommon.KLType{
		86400: qotcommon.KLType_KLType_Day,
		0:     qotcommon.KLType_KLType_Day,
		60:    qotcommon.KLType_KLType_1Min,
		300:   qotcommon.KLType_KLType_5Min,
		3600:  qotcommon.KLType_KLType_60Min,
	}
	for secs, want := range cases {
		got, err := KLTypeForSeconds(secs)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	_, err := KLTypeForSeconds(42)
	require.Error(t, err)
}

func TestSubTypeForKLType(t *testing.T) {
	st, err := SubTypeForKLType(qotcommon.KLType_KLType_Day)
	require.NoError(t, err)
	require.Equal(t, qotcommon.SubType_SubType_KL_Day, st)

	st, err = SubTypeForKLType(qotcommon.KLType_KLType_1Min)
	require.NoError(t, err)
	require.Equal(t, qotcommon.SubType_SubType_KL_1Min, st)

	_, err = SubTypeForKLType(qotcommon.KLType_KLType_Week)
	require.Error(t, err)
}

func TestKLTimeFormatParseDaily(t *testing.T) {
	// 2024-06-13 UTC midnight -> NY local is the prior evening, so the daily
	// date string is the NY calendar date. We assert round-trip stability of a
	// daily bar timestamp produced from a NY-midnight instant.
	nyMidnight := time.Date(2024, 6, 13, 4, 0, 0, 0, time.UTC) // 00:00 EDT == 04:00 UTC
	s := FormatKLTime(nyMidnight, qotcommon.KLType_KLType_Day)
	require.Equal(t, "2024-06-13", s)

	back, err := ParseKLTime(s)
	require.NoError(t, err)
	require.Equal(t, nyMidnight.UTC(), back)
}

func TestKLTimeFormatParseIntraday(t *testing.T) {
	// 14:30 UTC == 10:30 EDT on 2024-06-13.
	utc := time.Date(2024, 6, 13, 14, 30, 0, 0, time.UTC)
	s := FormatKLTime(utc, qotcommon.KLType_KLType_1Min)
	require.Equal(t, "2024-06-13 10:30:00", s)

	back, err := ParseKLTime(s)
	require.NoError(t, err)
	require.Equal(t, utc, back)
}

func TestBarKLineRoundTrip(t *testing.T) {
	b := domain.Bar{
		Symbol: "AAPL",
		TS:     time.Date(2024, 6, 13, 14, 0, 0, 0, time.UTC),
		Open:   domain.MustPrice("191.23"),
		High:   domain.MustPrice("193.10"),
		Low:    domain.MustPrice("190.50"),
		Close:  domain.MustPrice("192.84"),
		Volume: 123456,
	}
	k := KLineFromBar(b, qotcommon.KLType_KLType_1Min)
	require.False(t, k.GetIsBlank())

	got, err := BarFromKLine("AAPL", qotcommon.KLType_KLType_1Min, k)
	require.NoError(t, err)
	require.Equal(t, b.Symbol, got.Symbol)
	require.Equal(t, b.TS.Unix(), got.TS.Unix())
	require.Equal(t, b.Open, got.Open)
	require.Equal(t, b.High, got.High)
	require.Equal(t, b.Low, got.Low)
	require.Equal(t, b.Close, got.Close)
	require.Equal(t, b.Volume, got.Volume)
	require.NoError(t, got.Validate())
}

func TestBarFromKLineParsesTimeStringWhenNoTimestamp(t *testing.T) {
	k := &qotcommon.KLine{
		Time:       strptr("2024-06-13 10:30:00"),
		IsBlank:    boolptr(false),
		OpenPrice:  f64ptr(100),
		HighPrice:  f64ptr(101),
		LowPrice:   f64ptr(99),
		ClosePrice: f64ptr(100.5),
		Volume:     i64ptr(10),
	}
	got, err := BarFromKLine("X", qotcommon.KLType_KLType_1Min, k)
	require.NoError(t, err)
	require.Equal(t, time.Date(2024, 6, 13, 14, 30, 0, 0, time.UTC), got.TS)
}

func strptr(s string) *string   { return &s }
func boolptr(b bool) *bool      { return &b }
func f64ptr(f float64) *float64 { return &f }
func i64ptr(i int64) *int64     { return &i }
