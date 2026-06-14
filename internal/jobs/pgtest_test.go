package jobs_test

// Integration-test harness: claim/heartbeat/cancel semantics are verified
// against a REAL PostgreSQL (FOR UPDATE SKIP LOCKED, partial unique
// indexes and make_interval cannot be faked meaningfully).
//
// Choice (documented per task): a skip-if-no-docker guarded harness that
// `docker run`s one ephemeral timescale/timescaledb container per test
// binary (the same image the compose stack uses, so migrations including
// the timescaledb extension apply identically), on a random loopback port
// so it never collides with the project's reserved ports. When docker is
// unavailable (CI without Dind, TMS_TEST_NO_DOCKER=1) every integration
// test skips with the reason; unit tests still run. No third-party
// testcontainers dependency — plain `docker run --rm` + readiness retry.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

const testPGImage = "timescale/timescaledb:latest-pg16"

var (
	testPool      *pgxpool.Pool
	pgUnavailable = "harness not initialized"
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	cleanup, err := startEphemeralPG()
	if err != nil {
		pgUnavailable = err.Error()
		fmt.Fprintf(os.Stderr, "jobs: integration tests will skip: %v\n", err)
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

	name := fmt.Sprintf("tmsgo-jobs-test-%d", time.Now().UnixNano())
	runCtx, cancelRun := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelRun()
	out, err := exec.CommandContext(runCtx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-e", "POSTGRES_USER=tms",
		"-e", "POSTGRES_PASSWORD=tms",
		"-e", "POSTGRES_DB=tms",
		"-p", "127.0.0.1:0:5432", // random free loopback port; never the reserved 55432
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

	// Postgres entrypoint starts, initializes, restarts: retry MigrateUp
	// until the server is genuinely ready (bounded).
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

// ---------------------------------------------------------------------------
// Per-test helpers
// ---------------------------------------------------------------------------

// recNotifier records published events for assertions.
type recNotifier struct {
	mu     sync.Mutex
	events []jobs.Event
}

func (r *recNotifier) Notify(_ context.Context, ev jobs.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recNotifier) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, ev := range r.events {
		out[i] = ev.Event
	}
	return out
}

// newTestQueue skips without docker, truncates job/audit state and builds
// a Queue. Integration tests share one database, so they must not run in
// parallel (none call t.Parallel()).
func newTestQueue(t *testing.T, opts ...jobs.Option) *jobs.Queue {
	t.Helper()
	if testPool == nil {
		t.Skipf("skipping: ephemeral postgres unavailable (%s)", pgUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := testPool.Exec(ctx, `TRUNCATE tms.jobs, tms.audit_log RESTART IDENTITY`)
	require.NoError(t, err)
	q, err := jobs.NewQueue(testPool, zerolog.Nop(), opts...)
	require.NoError(t, err)
	return q
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ageHeartbeat backdates a running job's heartbeat to simulate a dead
// worker.
func ageHeartbeat(t *testing.T, ctx context.Context, jobID int64, age time.Duration) {
	t.Helper()
	_, err := testPool.Exec(ctx,
		`UPDATE tms.jobs SET heartbeat_at = now() - make_interval(secs => $2) WHERE id = $1`,
		jobID, age.Seconds())
	require.NoError(t, err)
}

// auditActions returns the audit_log action sequence for one job.
func auditActions(t *testing.T, ctx context.Context, jobID int64) []string {
	t.Helper()
	rows, err := testPool.Query(ctx,
		`SELECT action FROM tms.audit_log WHERE entity = 'job' AND entity_id = $1 ORDER BY id`,
		fmt.Sprint(jobID))
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		out = append(out, a)
	}
	require.NoError(t, rows.Err())
	return out
}

// waitStatus polls until the job reaches want (worker tests).
func waitStatus(t *testing.T, ctx context.Context, q *jobs.Queue, jobID int64, want jobs.Status, within time.Duration) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		job, err := q.Get(ctx, jobID)
		require.NoError(t, err)
		if job.Status == want {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d never reached %s within %s (now %s, last_error=%v)",
				jobID, want, within, job.Status, job.LastError)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
