package runner_test

// pgtest_test.go is the ephemeral-PostgreSQL harness for the runner integration
// tests (EOD idempotency, signal-session emission, command halt). It mirrors the
// jobs package harness: a skip-if-no-docker guarded `docker run` of one
// throwaway timescaledb container per test binary on a random loopback port, so
// the real (strategy_id, symbol, as_of) partial-unique index and the UPSERT
// idempotency can be verified against a real database (they cannot be faked).

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/internal/db"
)

const testPGImage = "timescale/timescaledb:latest-pg16"

var (
	testPool      *pgxpool.Pool
	pgUnavailable = "harness not initialized"
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	cleanup, err := startEphemeralPG()
	if err != nil {
		pgUnavailable = err.Error()
		fmt.Fprintf(os.Stderr, "runner: integration tests will skip: %v\n", err)
		return m.Run()
	}
	defer cleanup()
	return m.Run()
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

	name := fmt.Sprintf("tms-runner-test-%d", time.Now().UnixNano())
	runCtx, cancelRun := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelRun()
	out, err := exec.CommandContext(runCtx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-e", "POSTGRES_USER=tms",
		"-e", "POSTGRES_PASSWORD=tms",
		"-e", "POSTGRES_DB=tms",
		"-p", "127.0.0.1:0:5432",
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
	testPool = pool
	return func() { pool.Close(); stop() }, nil
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

// requirePG skips the test when the ephemeral DB is unavailable and truncates
// the live/signal tables for isolation (these tests share one DB; none run in
// parallel).
func requirePG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skipf("skipping: ephemeral postgres unavailable (%s)", pgUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := testPool.Exec(ctx,
		`TRUNCATE tms.signal_intents, tms.commands, tms.audit_log, tms.sessions,
		          tms.halts, tms.bars_daily RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return testPool
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}
