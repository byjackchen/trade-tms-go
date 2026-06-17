package sharadar

// client_test.go exercises the Nasdaq Data Link client against httptest
// servers speaking the datatables wire format (fixture shape per spec §3.1
// and the production API: datatable.data precedes datatable.columns,
// meta.next_cursor_id drives pagination).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAPIKey = "sk-test-secret-key"

// sepPage1/sepPage2 are SEP-shaped fixtures: data before columns, cursor
// chaining on page 1.
const sepColumnsJSON = `[
  {"name":"ticker","type":"String"},{"name":"date","type":"Date"},
  {"name":"open","type":"double"},{"name":"high","type":"double"},
  {"name":"low","type":"double"},{"name":"close","type":"double"},
  {"name":"volume","type":"double"},{"name":"closeadj","type":"double"},
  {"name":"closeunadj","type":"double"},{"name":"lastupdated","type":"Date"}]`

func sepPage(rows string, cursor string) string {
	cur := "null"
	if cursor != "" {
		cur = fmt.Sprintf("%q", cursor)
	}
	return fmt.Sprintf(`{"datatable":{"data":[%s],"columns":%s},"meta":{"next_cursor_id":%s}}`,
		rows, sepColumnsJSON, cur)
}

const sepRowAAPL = `["AAPL","2024-01-02",185.0,186.5,184.25,185.75,1000000.0,185.75,185.75,"2024-01-03"]`
const sepRowMSFT = `["MSFT","2024-01-02",370.0,372.0,369.0,371.5,2000000.0,371.5,371.5,"2024-01-03"]`
const sepRowNaN = `["NANY","2024-01-02",null,1.0,0.5,0.75,null,0.75,0.75,"2024-01-03"]`

func newTestClient(t *testing.T, srv *httptest.Server, opts ...ClientOption) *Client {
	t.Helper()
	all := append([]ClientOption{
		WithBaseURL(srv.URL),
		WithBackoffUnit(time.Millisecond),
		WithExportPollInterval(time.Millisecond),
	}, opts...)
	c, err := NewClient(testAPIKey, all...)
	require.NoError(t, err)
	return c
}

func collectRows(t *testing.T, c *Client, dataset string, filters []Filter) ([]Row, int64) {
	t.Helper()
	var rows []Row
	n, err := c.GetTable(context.Background(), dataset, filters, func(r Row) error {
		rows = append(rows, r)
		return nil
	})
	require.NoError(t, err)
	return rows, n
}

func TestNewClientRequiresAPIKey(t *testing.T) {
	_, err := NewClient("")
	require.ErrorIs(t, err, ErrMissingAPIKey)
	assert.Contains(t, err.Error(), "TMS_NASDAQ_DATA_LINK_API_KEY")
	assert.Contains(t, err.Error(), "https://data.nasdaq.com/account/profile")

	_, err = NewClient("   ")
	require.ErrorIs(t, err, ErrMissingAPIKey)

	c, err := NewClient("explicit-key")
	require.NoError(t, err)
	assert.Equal(t, "explicit-key", c.apiKey) // explicit key wins (spec §1)
}

func TestGetTablePaginationAndDecoding(t *testing.T) {
	var (
		mu   sync.Mutex
		reqs []http.Request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, *r.Clone(context.Background()))
		mu.Unlock()
		switch r.URL.Query().Get("qopts.cursor_id") {
		case "":
			_, _ = w.Write([]byte(sepPage(sepRowAAPL+","+sepRowNaN, "cursor-2")))
		case "cursor-2":
			_, _ = w.Write([]byte(sepPage(sepRowMSFT, "")))
		default:
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	rows, n := collectRows(t, c, "SHARADAR/SEP", DateRangeFilters("2024-01-02", "2024-01-02"))

	require.Len(t, reqs, 2, "cursor pagination must follow next_cursor_id")
	assert.Equal(t, int64(3), n)
	require.Len(t, rows, 3)

	// Request shape: filters + api_key on both pages, cursor only on page 2.
	q1 := reqs[0].URL.Query()
	assert.Equal(t, "/datatables/SHARADAR/SEP.json", reqs[0].URL.Path)
	assert.Equal(t, "2024-01-02", q1.Get("date.gte"))
	assert.Equal(t, "2024-01-02", q1.Get("date.lte"))
	assert.Equal(t, testAPIKey, q1.Get("api_key"))
	assert.Empty(t, q1.Get("qopts.cursor_id"))
	assert.Equal(t, "cursor-2", reqs[1].URL.Query().Get("qopts.cursor_id"))

	// Row decoding: by-name access across pages, including JSON nulls.
	tk, ok := rows[0].Str("ticker")
	require.True(t, ok)
	assert.Equal(t, "AAPL", tk)
	open, ok := rows[0].Float("open")
	require.True(t, ok)
	assert.Equal(t, 185.0, open)
	d, ok := rows[0].Date("date")
	require.True(t, ok)
	assert.Equal(t, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), d)

	_, ok = rows[1].Float("open") // null cell
	assert.False(t, ok)
	_, ok = rows[1].Float("volume")
	assert.False(t, ok)

	tk3, _ := rows[2].Str("ticker")
	assert.Equal(t, "MSFT", tk3)

	// Counters: one logical call = one fetch + one cache miss.
	sum := c.StateSummary()
	assert.Equal(t, int64(1), sum["fetch_count"])
	assert.Equal(t, int64(1), sum["cache_miss_count"])
	assert.Equal(t, int64(0), sum["cache_hit_count"])
	assert.NotNil(t, sum["last_fetch_ts"])
}

func TestGetTableRetriesOn429And5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"quandl_error":{"code":"QELx04","message":"rate limit"}}`, http.StatusTooManyRequests)
		case 2:
			http.Error(w, "bad gateway", http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(sepPage(sepRowAAPL, "")))
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, n := collectRows(t, c, "SHARADAR/SEP", nil)
	assert.Equal(t, int64(1), n)
	assert.Equal(t, int64(3), calls.Load(), "429 then 502 then success = 3 attempts")
}

func TestGetTableGivesUpAfterFourAttempts(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.GetTable(context.Background(), "SHARADAR/SEP", nil, func(Row) error { return nil })
	require.Error(t, err)
	// Contract: exactly 4 underlying calls and the "failed after" message
	// shape (spec §3.1).
	assert.Equal(t, int64(4), calls.Load())
	assert.Contains(t, err.Error(), "failed after 4 retries")
	assert.Contains(t, err.Error(), "HTTP 500")

	// Failed calls increment nothing (spec §3.1).
	sum := c.StateSummary()
	assert.Equal(t, int64(0), sum["fetch_count"])
	assert.Equal(t, int64(0), sum["cache_miss_count"])
	assert.Nil(t, sum["last_fetch_ts"])
	assert.Equal(t, int64(0), sum["last_fetch_ts_ns"])
}

func TestGetTableNonRetryableStatusFailsImmediately(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, "denied", status)
			}))
			defer srv.Close()

			c := newTestClient(t, srv)
			_, err := c.GetTable(context.Background(), "SHARADAR/SEP", nil, func(Row) error { return nil })
			require.Error(t, err)
			assert.Equal(t, int64(1), calls.Load(), "401/403/404 must not retry (spec §3.1)")
			assert.NotContains(t, err.Error(), "failed after")
			// No secrets in errors: the api_key query param never leaks.
			assert.NotContains(t, err.Error(), testAPIKey)
		})
	}
}

func TestGetTableContextCancellationDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	// Long backoff unit so cancellation, not the timer, ends the wait.
	c := newTestClient(t, srv, WithBackoffUnit(10*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := c.GetTable(ctx, "SHARADAR/SEP", nil, func(Row) error { return nil })
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second, "backoff sleep must be interruptible")
}

func TestGetTableRowCallbackErrorAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sepPage(sepRowAAPL+","+sepRowMSFT, "")))
	}))
	defer srv.Close()

	sentinel := errors.New("stop")
	c := newTestClient(t, srv)
	_, err := c.GetTable(context.Background(), "SHARADAR/SEP", nil, func(Row) error { return sentinel })
	assert.ErrorIs(t, err, sentinel)
}

func TestGetTableColumnsBeforeDataOrder(t *testing.T) {
	// Tolerate the columns-first key order too.
	body := fmt.Sprintf(`{"datatable":{"columns":%s,"data":[%s]},"meta":{"next_cursor_id":null}}`,
		sepColumnsJSON, sepRowAAPL)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	rows, n := collectRows(t, c, "SHARADAR/SEP", nil)
	assert.Equal(t, int64(1), n)
	tk, _ := rows[0].Str("ticker")
	assert.Equal(t, "AAPL", tk)
}

func TestStateSummaryShapeAndCacheHits(t *testing.T) {
	c, err := NewClient(testAPIKey)
	require.NoError(t, err)

	sum := c.StateSummary()
	// Exact key set (spec §3.1).
	for _, k := range []string{"source", "fetch_count", "cache_hit_count", "cache_miss_count",
		"last_fetch_ts", "last_fetch_ts_ns", "quota_used_today"} {
		_, ok := sum[k]
		assert.True(t, ok, "missing key %s", k)
	}
	assert.Len(t, sum, 7)
	assert.Equal(t, "sharadar", sum["source"])
	assert.Nil(t, sum["quota_used_today"], "quota_used_today is always null")
	assert.Nil(t, sum["last_fetch_ts"])

	c.RecordCacheHit()
	c.RecordCacheHit()
	sum = c.StateSummary()
	assert.Equal(t, int64(2), sum["cache_hit_count"])
	assert.Equal(t, int64(0), sum["fetch_count"], "record_cache_hit only bumps hits")
}

func TestTickersFilterCommaJoined(t *testing.T) {
	f := TickersFilter([]string{"AAPL", "MSFT", "BRK.A"})
	assert.Equal(t, "ticker", f.Key)
	assert.Equal(t, "AAPL,MSFT,BRK.A", f.Value)
}

func TestExportTablePollsThenDownloads(t *testing.T) {
	var polls atomic.Int64
	zipBytes := []byte("PK\x03\x04 fake zip payload")

	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/datatables/SHARADAR/SEP.json", func(w http.ResponseWriter, r *http.Request) {
		// assert (not require): FailNow must not run off the test goroutine.
		assert.Equal(t, "true", r.URL.Query().Get("qopts.export"))
		assert.Equal(t, "2024-01-02", r.URL.Query().Get("date.gte"))
		status := "regenerating"
		link := ""
		if polls.Add(1) >= 3 {
			status = "fresh"
			link = srv.URL + "/bulk/file.zip"
		}
		fmt.Fprintf(w, `{"datatable_bulk_download":{"file":{"link":%q,"status":%q}}}`, link, status)
	})
	mux.HandleFunc("/bulk/file.zip", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	dir := filepath.Join(t.TempDir(), "exports")
	c := newTestClient(t, srv)
	path, err := c.ExportTable(context.Background(), "SHARADAR/SEP",
		dir, []Filter{{Key: "date.gte", Value: "2024-01-02"}})
	require.NoError(t, err)

	// Filename contract: dataset with "/" -> "_" (spec §3.2).
	assert.Equal(t, filepath.Join(dir, "SHARADAR_SEP.zip"), path)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, zipBytes, got)
	assert.GreaterOrEqual(t, polls.Load(), int64(3), "must poll until status=fresh")
	// No stray temp file.
	_, err = os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(err))
}

func TestExportTableContextCancellationWhilePolling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"datatable_bulk_download":{"file":{"link":"","status":"creating"}}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, WithExportPollInterval(10*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := c.ExportTable(ctx, "SHARADAR/SEP", t.TempDir(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRowAccessors(t *testing.T) {
	idx := map[string]int{"s": 0, "f": 1, "intish": 2, "d": 3, "dt": 4, "null": 5, "strnum": 6}
	r := Row{cols: idx, vals: []any{"hello", 1.5, float64(22), "2024-03-05", "2024-03-05T00:00:00.000", nil, "3.25"}}

	s, ok := r.Str("s")
	require.True(t, ok)
	assert.Equal(t, "hello", s)

	f, ok := r.Float("f")
	require.True(t, ok)
	assert.Equal(t, 1.5, f)

	// Numeric eventcodes-style cell renders as integer string.
	is, ok := r.Str("intish")
	require.True(t, ok)
	assert.Equal(t, "22", is)

	d, ok := r.Date("d")
	require.True(t, ok)
	assert.Equal(t, time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC), d)

	dt, ok := r.Date("dt")
	require.True(t, ok)
	assert.Equal(t, time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC), dt)

	_, ok = r.Str("null")
	assert.False(t, ok)
	_, ok = r.Float("null")
	assert.False(t, ok)
	_, ok = r.Date("null")
	assert.False(t, ok)
	_, ok = r.Str("missing-col")
	assert.False(t, ok)
	assert.False(t, r.Has("missing-col"))
	assert.True(t, r.Has("null"), "Has reports column presence, not cell nullness")

	sn, ok := r.Float("strnum")
	require.True(t, ok)
	assert.Equal(t, 3.25, sn)

	// Empty string coerces to no-date (coerce-on-error).
	r2 := Row{cols: map[string]int{"d": 0}, vals: []any{""}}
	_, ok = r2.Date("d")
	assert.False(t, ok)
}

func TestDecodePageMalformedAndRowWidth(t *testing.T) {
	_, err := decodePage(strings.NewReader(`{"datatable":{"data":[[1,2]]}}`))
	require.Error(t, err, "missing columns must fail")

	_, err = decodePage(strings.NewReader(`not json`))
	require.Error(t, err)

	// Row width mismatch surfaces at GetTable level.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"datatable":{"data":[["AAPL"]],"columns":[{"name":"ticker","type":"String"},{"name":"date","type":"Date"}]},"meta":{"next_cursor_id":null}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv)
	_, err = c.GetTable(context.Background(), "SHARADAR/SEP", nil, func(Row) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cells")
}

func TestParseRetryAfter(t *testing.T) {
	assert.Equal(t, 3*time.Second, parseRetryAfter("3"))
	assert.Equal(t, time.Duration(0), parseRetryAfter(""))
	assert.Equal(t, time.Duration(0), parseRetryAfter("garbage"))
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	d := parseRetryAfter(future)
	assert.Greater(t, d, 20*time.Second)
}
