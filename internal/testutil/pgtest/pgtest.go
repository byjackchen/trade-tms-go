// Package pgtest is the shared ephemeral-PostgreSQL harness for the
// integration test suites. It replaces the per-package copy-pasted "docker run
// one throwaway timescaledb container" bootstraps (runner, jobs, runs,
// hyperopt/study, api) with a single implementation.
//
// A real (migrated) timescale/timescaledb container is started once per test
// binary, on a random loopback port, so the SQL-level invariants
// (partial-unique indexes, FOR UPDATE SKIP LOCKED, hypertables, FK cascades,
// JSONB) that cannot be faked are exercised against a genuine database. When
// docker is unavailable (no daemon, not on PATH, or TMS_TEST_NO_DOCKER=1) every
// caller skips with a uniform message instead of failing.
//
// Usage from a package's TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, "jobs")) }
//
// and from each test:
//
//	pool := pgtest.RequirePG(t)
//
// The container is named "tmsgo-<pkg>-test-<nano>" where <pkg> is the label
// passed to Run, keeping the per-package naming scheme while sharing the
// bootstrap. Per-package TRUNCATE lists stay local to each test file — this
// harness only owns the container lifecycle and pool.
//
// This file is only compiled into integration-tagged test binaries; callers
// carry the //go:build integration constraint, so plain `go test ./...` never
// references it and never shells out to docker.
package pgtest

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

	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/internal/db"
)

const testPGImage = "timescale/timescaledb:latest-pg16"

var (
	testPool      *pgxpool.Pool
	pgUnavailable = "harness not initialized"
)

// Run is the TestMain entrypoint: it bootstraps one ephemeral migrated
// PostgreSQL container for the whole test binary (named "tmsgo-<pkg>-test-<nano>"),
// runs the suite, and tears the container down. When docker is unavailable the
// container is skipped and the suite still runs (DB tests will Skip via
// RequirePG). It returns the process exit code; callers do os.Exit(Run(m, pkg)).
func Run(m *testing.M, pkg string) int {
	cleanup, err := startEphemeralPG(pkg)
	if err != nil {
		pgUnavailable = err.Error()
		fmt.Fprintf(os.Stderr, "%s: integration tests will skip: %v\n", pkg, err)
		return m.Run()
	}
	defer cleanup()
	return m.Run()
}

// RequirePG returns the shared pool, skipping the test with a uniform message
// when the ephemeral database is unavailable. Callers own their own
// TRUNCATE/isolation; these tests share one database and must not run in
// parallel.
func RequirePG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testPool == nil {
		t.Skipf("skipping: ephemeral postgres unavailable (%s)", pgUnavailable)
	}
	return testPool
}

func startEphemeralPG(pkg string) (cleanup func(), err error) {
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

	name := fmt.Sprintf("tmsgo-%s-test-%d", pkg, time.Now().UnixNano())
	runCtx, cancelRun := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelRun()
	out, err := exec.CommandContext(runCtx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-e", "POSTGRES_USER=tms",
		"-e", "POSTGRES_PASSWORD=tms",
		"-e", "POSTGRES_DB=tms",
		"-p", "127.0.0.1:0:5432", // random free loopback port; never a reserved port
		testPGImage).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run %s: %v (%s)", testPGImage, err, strings.TrimSpace(string(out)))
	}
	stop := func() {
		killCtx, cancelKill := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelKill()
		_ = exec.CommandContext(killCtx, "docker", "kill", name).Run() // --rm removes it
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

	// The postgres entrypoint starts, initializes, then restarts: retry
	// MigrateUp until the server is genuinely ready (bounded).
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
	// First line like "127.0.0.1:54321".
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
