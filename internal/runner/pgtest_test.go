//go:build integration

package runner_test

// pgtest_test.go is the runner integration-test harness (EOD idempotency,
// signal-session emission, command halt). The ephemeral-PostgreSQL container
// bootstrap lives in internal/testutil/pgtest (shared with jobs/runs/study/api):
// a skip-if-no-docker guarded throwaway timescaledb container per test binary on
// a random loopback port, so the real (strategy_id, symbol, as_of)
// partial-unique index and the UPSERT idempotency can be verified against a real
// database. Only the package-local TRUNCATE list stays here.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/testutil/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, "runner")) }

// requirePG skips the test when the ephemeral DB is unavailable and truncates
// the live/signal tables for isolation (these tests share one DB; none run in
// parallel).
func requirePG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.RequirePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`TRUNCATE tms.signals, tms.commands, tms.audit_log, tms.sessions,
		          tms.halts, tms.bars_daily RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return pool
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}
