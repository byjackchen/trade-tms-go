package db

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// database driver
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/migrations"
)

// NewMigrator returns a golang-migrate instance backed by the migrations
// embedded in the binary (source: iofs over the top-level migrations
// package) and the configured Postgres (database driver: pgx/v5).
// Callers own Close().
func NewMigrator(cfg *config.Config) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.Files, ".")
	if err != nil {
		return nil, fmt.Errorf("db: loading embedded migrations: %w", err)
	}
	dsn, err := migrateDSN(cfg.MigrateURL())
	if err != nil {
		return nil, fmt.Errorf("db: building migrate dsn: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: connecting migrator to %s:%d/%s: %w", cfg.PGHost, cfg.PGPort, cfg.PGDatabase, err)
	}
	return m, nil
}

// MigrateUp applies all pending migrations. A no-op (already up to date)
// is success, not an error.
func MigrateUp(cfg *config.Config) error {
	m, err := NewMigrator(cfg)
	if err != nil {
		return err
	}
	defer closeMigrator(m)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate up: %w", err)
	}
	return nil
}

// MigrateDown rolls back n migrations (n > 0).
func MigrateDown(cfg *config.Config, n int) error {
	if n <= 0 {
		return fmt.Errorf("db: migrate down: steps must be > 0, got %d", n)
	}
	m, err := NewMigrator(cfg)
	if err != nil {
		return err
	}
	defer closeMigrator(m)
	if err := m.Steps(-n); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("db: migrate down %d: %w", n, err)
	}
	return nil
}

// MigrateStatus reports the current schema version and dirty flag.
// When no migration has been applied yet it returns (0, false, nil).
func MigrateStatus(cfg *config.Config) (version uint, dirty bool, err error) {
	m, err := NewMigrator(cfg)
	if err != nil {
		return 0, false, err
	}
	defer closeMigrator(m)
	version, dirty, err = m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("db: migrate status: %w", err)
	}
	return version, dirty, nil
}

// migrateDSN pins search_path=public on the migrator connection.
//
// Why: the application role is named "tms" and Postgres's default
// search_path is `"$user", public`. On a brand-new database the version
// table (unqualified "schema_migrations") therefore lands in public — but
// once migration 000001 has created the "tms" schema, "$user" starts
// resolving, and a later connection would silently look for (and create) an
// EMPTY tms.schema_migrations, re-running everything from version 0 into a
// dirty state. Pinning search_path makes version-table resolution stable
// across the schema's own lifecycle (all migration DDL is explicitly
// tms.-qualified, so it is unaffected). pgx forwards unrecognized URL
// parameters as server runtime settings, which is exactly what we want.
func migrateDSN(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing %q: %w", raw, err)
	}
	q := u.Query()
	q.Set("search_path", "public")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func closeMigrator(m *migrate.Migrate) {
	// Both errors are best-effort cleanup; surfacing them would mask the
	// primary operation's result, so they are intentionally dropped after
	// the migrator's own Close logging.
	_, _ = m.Close()
}
