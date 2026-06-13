package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/config"
)

// NewPool builds a pgx connection pool from config and verifies
// connectivity with a bounded ping, so a bad DSN fails at startup instead
// of on the first query mid-run.
func NewPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.PostgresDSN())
	if err != nil {
		return nil, fmt.Errorf("db: parsing postgres dsn: %w", err)
	}
	// Pool sizing comes from config (TMS_PG_MAX_CONNS / TMS_PG_MIN_CONNS,
	// validated >= 1 and 0 <= min <= max at Load time); the rest are
	// conservative production defaults — tune per-service when load
	// profiles are known.
	poolCfg.MaxConns = int32(cfg.PGMaxConns)
	poolCfg.MinConns = int32(cfg.PGMinConns)
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 15 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute
	poolCfg.ConnConfig.ConnectTimeout = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: creating pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: pinging %s:%d/%s: %w", cfg.PGHost, cfg.PGPort, cfg.PGDatabase, err)
	}
	return pool, nil
}
