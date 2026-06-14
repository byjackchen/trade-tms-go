//go:build integration

package api_test

// integration_test.go runs the API against a REAL migrated PostgreSQL: the
// PGStore SQL (AT TIME ZONE 'UTC' date math, ILIKE search, sync-run joins)
// and the wired HTTP handlers are exercised end-to-end. It follows the same
// ephemeral-docker harness as internal/jobs: one throwaway
// timescale/timescaledb container per test binary on a random loopback port,
// skipping cleanly when docker is unavailable (TMS_TEST_NO_DOCKER=1).
//
// Run: go test -tags integration ./internal/api/...

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/api"
	"github.com/byjackchen/trade-tms-go/internal/apistore"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/runs"
	"github.com/byjackchen/trade-tms-go/internal/testutil/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, "api")) }

// Local copies of the package-internal test constants/helpers (this file is an
// external test package — package api_test — so it cannot see stub_test.go's
// unexported identifiers; it must avoid the api -> apistore -> api import cycle
// that an in-package integration test would create).
const (
	testToken  = "secret-test-token"
	testOrigin = "http://localhost:13000"
)

var fixedNow = time.Date(2024, time.June, 12, 15, 30, 0, 0, time.UTC)

func pingOK(context.Context) error { return nil }

// itestServer truncates state, seeds fixtures and returns a live httptest
// server wired to the real PGStore + a real Queue. Skips without docker.
func itestServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	itestPool := pgtest.RequirePG(t)
	ctx := context.Background()
	_, err := itestPool.Exec(ctx, `TRUNCATE tms.bars_daily, tms.tickers, tms.fundamentals_sf1,
		tms.events, tms.dataset_sync, tms.dataset_sync_runs, tms.jobs, tms.audit_log RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	seedFixtures(t, ctx, itestPool)

	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	queue, err := jobs.NewQueue(itestPool, zerolog.Nop())
	require.NoError(t, err)

	srv, err := api.NewServer(api.Deps{
		Log:         zerolog.Nop(),
		Token:       testToken,
		CORSOrigins: []string{testOrigin},
		Jobs:        queue,
		Data:        apistore.NewPGStore(itestPool),
		Universe:    universe.NewStore(itestPool),
		Runs:        runs.NewStore(itestPool),
		Calendar:    cal,
		PingPG:      itestPool.Ping,
		PingRedis:   pingOK,
		Now:         func() time.Time { return fixedNow },
	})
	require.NoError(t, err)
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)
	return hs, itestPool
}

// seedFixtures inserts two tickers and a small bars_daily series with a known
// gap (BAC missing 2024-06-07) so coverage/gap detection has signal.
func seedFixtures(t *testing.T, ctx context.Context, itestPool *pgxpool.Pool) {
	t.Helper()
	_, err := itestPool.Exec(ctx, `
		INSERT INTO tms.tickers (ticker, table_name, name, exchange, is_delisted,
		    first_price_date, last_price_date)
		VALUES ('AAPL','SF1','Apple Inc','NASDAQ',false,'2024-06-06','2024-06-11'),
		       ('BAC','SF1','Bank of America','NYSE',false,'2024-06-06','2024-06-11')`)
	require.NoError(t, err)

	// AAPL: 4 sessions complete. BAC: missing 2024-06-07.
	dates := map[string][]string{
		"AAPL": {"2024-06-06", "2024-06-07", "2024-06-10", "2024-06-11"},
		"BAC":  {"2024-06-06", "2024-06-10", "2024-06-11"},
	}
	for tk, ds := range dates {
		for _, d := range ds {
			_, err := itestPool.Exec(ctx, `
				INSERT INTO tms.bars_daily (ticker, ts, source, open, high, low, close, volume)
				VALUES ($1, ($2 || ' 00:00:00+00')::timestamptz, 'SEP', 100000, 101000, 99000, 100500, 1000000)`,
				tk, d)
			require.NoError(t, err)
		}
	}

	_, err = itestPool.Exec(ctx, `
		INSERT INTO tms.dataset_sync (dataset, last_sync, row_count, schema_version)
		VALUES ('SEP', now(), 7, 1)`)
	require.NoError(t, err)
	_, err = itestPool.Exec(ctx, `
		INSERT INTO tms.dataset_sync_runs (dataset, kind, started_at, finished_at, rows_added, status)
		VALUES ('SEP', 'import', now(), now(), 7, 'ok')`)
	require.NoError(t, err)
}

func getJSON(t *testing.T, hs *httptest.Server, path string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, hs.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", path)
	var m map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&m))
	return m
}

func TestIntegration_Coverage(t *testing.T) {
	hs, _ := itestServer(t)
	body := getJSON(t, hs, "/api/v1/data/coverage")
	assert.Equal(t, "2024-06-12", body["latest_session"])

	tables := body["tables"].([]any)
	var bars map[string]any
	for _, raw := range tables {
		m := raw.(map[string]any)
		if m["table"] == "bars_daily" {
			bars = m
		}
	}
	require.NotNil(t, bars)
	assert.Equal(t, float64(7), bars["rows"])
	gaps := bars["gaps"].(map[string]any)
	assert.Equal(t, float64(1), gaps["tickers_with_gaps"])
	worst := gaps["worst"].([]any)
	require.Len(t, worst, 1)
	assert.Equal(t, "BAC", worst[0].(map[string]any)["ticker"])
}

func TestIntegration_CoverageTickerDrilldown(t *testing.T) {
	hs, _ := itestServer(t)
	body := getJSON(t, hs, "/api/v1/data/coverage?ticker=BAC")
	assert.Equal(t, "BAC", body["ticker"])
	assert.Equal(t, float64(1), body["missing_days"])
	missing := body["missing"].([]any)
	assert.Equal(t, []any{"2024-06-07"}, missing)
}

func TestIntegration_TickerSearch(t *testing.T) {
	hs, _ := itestServer(t)
	body := getJSON(t, hs, "/api/v1/data/tickers?q=app")
	results := body["results"].([]any)
	require.Len(t, results, 1)
	assert.Equal(t, "AAPL", results[0].(map[string]any)["ticker"])

	// Name substring search (ILIKE on name).
	body = getJSON(t, hs, "/api/v1/data/tickers?q=bank")
	results = body["results"].([]any)
	require.Len(t, results, 1)
	assert.Equal(t, "BAC", results[0].(map[string]any)["ticker"])
}

func TestIntegration_SyncRuns(t *testing.T) {
	hs, _ := itestServer(t)
	body := getJSON(t, hs, "/api/v1/data/sync-runs")
	assert.Len(t, body["datasets"], 1)
	assert.Len(t, body["runs"], 1)
}

func TestIntegration_RefreshEnqueuesRealJob(t *testing.T) {
	hs, pool := itestServer(t)
	req, err := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/data/refresh",
		strings.NewReader(`{"source":"api","tables":["sep"],"actor":"itest"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	job := out["job"].(map[string]any)
	jobID := int64(job["id"].(float64))

	// The job and its audit row are durable in PostgreSQL.
	var (
		kind  string
		actor string
	)
	ctx := context.Background()
	require.NoError(t, pool.QueryRow(ctx, `SELECT kind FROM tms.jobs WHERE id = $1`, jobID).Scan(&kind))
	assert.Equal(t, "data.refresh", kind)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT actor FROM tms.audit_log WHERE entity = 'job' AND entity_id = $1 ORDER BY id LIMIT 1`,
		fmt.Sprint(jobID)).Scan(&actor))
	assert.Equal(t, "api:itest", actor)

	// And it is visible through the jobs read API.
	listed := getJSON(t, hs, "/api/v1/jobs")
	assert.NotEmpty(t, listed["jobs"])
}
