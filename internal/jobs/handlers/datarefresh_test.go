package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

func TestParseParams(t *testing.T) {
	t.Run("full payload", func(t *testing.T) {
		p, since, err := parseParams(json.RawMessage(
			`{"source":"parquet","tables":["sep","sf1"],"tickers":["AAPL"],"since":"2024-01-02"}`))
		require.NoError(t, err)
		assert.Equal(t, "parquet", p.Source)
		assert.Equal(t, []string{"sep", "sf1"}, p.Tables)
		assert.Equal(t, []string{"AAPL"}, p.Tickers)
		assert.Equal(t, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), since)
	})

	t.Run("minimal api payload", func(t *testing.T) {
		p, since, err := parseParams(json.RawMessage(`{"source":"api"}`))
		require.NoError(t, err)
		assert.Equal(t, "api", p.Source)
		assert.True(t, since.IsZero())
	})

	t.Run("missing source", func(t *testing.T) {
		_, _, err := parseParams(json.RawMessage(`{}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"source" is required`)
	})

	t.Run("unknown source", func(t *testing.T) {
		_, _, err := parseParams(json.RawMessage(`{"source":"csv"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `unknown source "csv"`)
	})

	t.Run("unknown field rejected", func(t *testing.T) {
		_, _, err := parseParams(json.RawMessage(`{"source":"parquet","tabels":["sep"]}`))
		require.Error(t, err) // typo'd key must fail loudly, not half-run
	})

	t.Run("bad since", func(t *testing.T) {
		_, _, err := parseParams(json.RawMessage(`{"source":"parquet","since":"01/02/2024"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid since")
	})
}

// noProgress is a ProgressFn stub.
func noProgress(context.Context, any) error { return nil }

func TestRunAPISourceUnavailable(t *testing.T) {
	h := &DataRefresh{log: zerolog.Nop()} // api == nil: data-sync phase not wired yet
	job := &jobs.Job{ID: 1, Kind: KindDataRefresh, Payload: json.RawMessage(`{"source":"api"}`)}
	_, err := h.Run(context.Background(), job, noProgress)
	require.ErrorIs(t, err, ErrAPISourceUnavailable)
}

func TestRunParquetRequiresExplicitCacheDir(t *testing.T) {
	// P1 locked decision (1): no repo-root discovery — unset env is a hard
	// error naming TMS_SHARADAR_CACHE_DIR.
	h := &DataRefresh{log: zerolog.Nop(), cacheDir: ""}
	job := &jobs.Job{ID: 1, Kind: KindDataRefresh, Payload: json.RawMessage(`{"source":"parquet"}`)}
	_, err := h.Run(context.Background(), job, noProgress)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TMS_SHARADAR_CACHE_DIR")
}

func TestKindConstant(t *testing.T) {
	h := &DataRefresh{log: zerolog.Nop()}
	assert.Equal(t, "data.refresh", h.Kind())
}
