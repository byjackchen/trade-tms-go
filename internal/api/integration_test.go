//go:build integration

package api

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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/runs"
)

const testPGImage = "timescale/timescaledb:latest-pg16"

var (
	itestPool     *pgxpool.Pool
	pgUnavailable = "harness not initialized"
)

func TestMain(m *testing.M) {
	cleanup, err := startEphemeralPG()
	if err != nil {
		pgUnavailable = err.Error()
		fmt.Fprintf(os.Stderr, "api: integration tests will skip: %v\n", err)
		os.Exit(m.Run())
	}
	defer cleanup()
	os.Exit(m.Run())
}

func startEphemeralPG() (cleanup func(), err error) {
	if os.Getenv("TMS_TEST_NO_DOCKER") == "1" {
		return nil, fmt.Errorf("TMS_TEST_NO_DOCKER=1")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker not on PATH: %w", err)
	}
	infoCtx, cancelInfo := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelInfo()
	if out, err := exec.CommandContext(infoCtx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker daemon unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	name := fmt.Sprintf("tms-api-test-%d", time.Now().UnixNano())
	runCtx, cancelRun := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelRun()
	out, err := exec.CommandContext(runCtx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-e", "POSTGRES_USER=tms",
		"-e", "POSTGRES_PASSWORD=tms",
		"-e", "POSTGRES_DB=tms",
		"-p", "127.0.0.1:0:5432", // random loopback port; never the reserved 55432
		testPGImage).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run %s: %v (%s)", testPGImage, err, strings.TrimSpace(string(out)))
	}
	stop := func() {
		killCtx, cancelKill := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelKill()
		_ = exec.CommandContext(killCtx, "docker", "kill", name).Run()
	}

	port, err := mappedPort(name)
	if err != nil {
		stop()
		return nil, err
	}
	cfg := &config.Config{
		PGHost: "127.0.0.1", PGPort: port,
		PGUser: "tms", PGPassword: "tms", PGDatabase: "tms",
		PGSSLMode: "disable", PGMaxConns: 16, PGMinConns: 0,
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		if err = db.MigrateUp(cfg); err == nil {
			break
		}
		if time.Now().After(deadline) {
			stop()
			return nil, fmt.Errorf("postgres not ready after 90s: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	poolCtx, cancelPool := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelPool()
	pool, err := db.NewPool(poolCtx, cfg)
	if err != nil {
		stop()
		return nil, fmt.Errorf("connecting test pool: %w", err)
	}
	itestPool = pool
	return func() {
		pool.Close()
		stop()
	}, nil
}

func mappedPort(container string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "port", container, "5432/tcp").CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("docker port: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	_, portStr, err := net.SplitHostPort(line)
	if err != nil {
		return 0, fmt.Errorf("parsing docker port output %q: %w", line, err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return 0, fmt.Errorf("parsing port %q: %w", portStr, err)
	}
	return port, nil
}

// itestServer truncates state, seeds fixtures and returns a live httptest
// server wired to the real PGStore + a real Queue. Skips without docker.
func itestServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	if itestPool == nil {
		t.Skipf("skipping: ephemeral postgres unavailable (%s)", pgUnavailable)
	}
	ctx := context.Background()
	_, err := itestPool.Exec(ctx, `TRUNCATE tms.bars_daily, tms.tickers, tms.fundamentals_sf1,
		tms.events, tms.dataset_sync, tms.dataset_sync_runs, tms.jobs, tms.audit_log RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	seedFixtures(t, ctx)

	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	queue, err := jobs.NewQueue(itestPool, zerolog.Nop())
	require.NoError(t, err)

	srv, err := NewServer(Deps{
		Log:         zerolog.Nop(),
		Token:       testToken,
		CORSOrigins: []string{testOrigin},
		Jobs:        queue,
		Data:        NewPGStore(itestPool),
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
func seedFixtures(t *testing.T, ctx context.Context) {
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
