package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
)

// MissingConfig is returned when a required configuration value is unset.
// It mirrors the Python reference's MissingConfig(RuntimeError): callers get
// a consistent message with a setup pointer instead of a bare KeyError.
type MissingConfig struct {
	// Key is the environment variable name that is missing or empty.
	Key string
	// Hint optionally points the operator at how to obtain/set the value.
	Hint string
	// DotenvPath is the .env file that was loaded ("" if none was found).
	DotenvPath string
}

func (e *MissingConfig) Error() string {
	msg := fmt.Sprintf("required config %q is not set", e.Key)
	if e.Hint != "" {
		msg += ". " + e.Hint
	}
	if e.DotenvPath == "" {
		msg += " (no .env file found — copy .env.example to .env and edit)"
	} else {
		msg += fmt.Sprintf(" (loaded .env from %s)", e.DotenvPath)
	}
	return msg
}

// Config is a read-only snapshot of process configuration. Build one with
// Load; tests may construct it directly.
type Config struct {
	// --- PostgreSQL / TimescaleDB (P0) ---
	PGHost     string // TMS_PG_HOST
	PGPort     int    // TMS_PG_PORT
	PGUser     string // TMS_PG_USER
	PGPassword string // TMS_PG_PASSWORD
	PGDatabase string // TMS_PG_DATABASE
	PGSSLMode  string // TMS_PG_SSLMODE
	PGMaxConns int    // TMS_PG_MAX_CONNS (pool upper bound, must be >= 1)
	PGMinConns int    // TMS_PG_MIN_CONNS (pool warm floor, 0..PGMaxConns)

	// --- Redis (P0) ---
	RedisAddr     string // TMS_REDIS_ADDR (host:port)
	RedisDB       int    // TMS_REDIS_DB
	RedisPassword string // TMS_REDIS_PASSWORD (optional)

	// --- Runtime ---
	LogLevel  string // TMS_LOG_LEVEL (debug/info/warn/error; Python-style DEBUG/INFO/WARNING/ERROR accepted)
	LogFormat string // TMS_LOG_FORMAT ("auto"|"console"|"json")
	APIAddr   string // TMS_API_ADDR (listen address for `tms api`)

	// --- Data vendors ---
	// NasdaqDataLinkAPIKey has no safe default; use Require("NASDAQ_DATA_LINK_API_KEY").
	NasdaqDataLinkAPIKey string // NASDAQ_DATA_LINK_API_KEY
	// SharadarCacheDir: "" means "use repo-root default ./cache/sharadar"
	// (resolved by the data layer), matching the Python reference.
	SharadarCacheDir string // TMS_SHARADAR_CACHE_DIR

	// --- Strategy params resolution ---
	// StrategyParamsDir: "" means "use embedded baseline params"; set to e.g.
	// runs/active_params to run with tuned params (per-strategy fallback to
	// baseline), matching the Python reference.
	StrategyParamsDir string // TMS_STRATEGY_PARAMS_DIR

	// DotenvPath records where .env was loaded from ("" if none found),
	// so error messages can point at the file that was (not) used.
	DotenvPath string
}

// Load reads .env (non-overriding) and returns a fresh Config snapshot.
// It returns an error only for values that are present but malformed
// (e.g. a non-integer port); absence of optional values is not an error —
// required-at-use values are enforced via Require at the call site, exactly
// like the Python reference's Config.require.
func Load() (*Config, error) {
	dotenvPath, err := loadDotenv()
	if err != nil {
		return nil, fmt.Errorf("config: loading .env: %w", err)
	}

	pgPort, err := envInt("TMS_PG_PORT", 5432)
	if err != nil {
		return nil, err
	}
	pgMaxConns, err := envInt("TMS_PG_MAX_CONNS", 16)
	if err != nil {
		return nil, err
	}
	pgMinConns, err := envInt("TMS_PG_MIN_CONNS", 2)
	if err != nil {
		return nil, err
	}
	if pgMaxConns < 1 {
		return nil, fmt.Errorf("config: TMS_PG_MAX_CONNS must be >= 1, got %d", pgMaxConns)
	}
	if pgMinConns < 0 || pgMinConns > pgMaxConns {
		return nil, fmt.Errorf("config: TMS_PG_MIN_CONNS must be in [0, TMS_PG_MAX_CONNS=%d], got %d", pgMaxConns, pgMinConns)
	}
	redisDB, err := envInt("TMS_REDIS_DB", 0)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		PGHost:     envStr("TMS_PG_HOST", "127.0.0.1"),
		PGPort:     pgPort,
		PGUser:     envStr("TMS_PG_USER", "tms"),
		PGPassword: envStr("TMS_PG_PASSWORD", ""),
		PGDatabase: envStr("TMS_PG_DATABASE", "tms"),
		PGSSLMode:  envStr("TMS_PG_SSLMODE", "disable"),
		PGMaxConns: pgMaxConns,
		PGMinConns: pgMinConns,

		RedisAddr:     envStr("TMS_REDIS_ADDR", "127.0.0.1:6379"),
		RedisDB:       redisDB,
		RedisPassword: envStr("TMS_REDIS_PASSWORD", ""),

		LogLevel:  envStr("TMS_LOG_LEVEL", "info"),
		LogFormat: envStr("TMS_LOG_FORMAT", "auto"),
		APIAddr:   envStr("TMS_API_ADDR", ":8080"),

		NasdaqDataLinkAPIKey: envStr("NASDAQ_DATA_LINK_API_KEY", ""),
		SharadarCacheDir:     envStr("TMS_SHARADAR_CACHE_DIR", ""),
		StrategyParamsDir:    envStr("TMS_STRATEGY_PARAMS_DIR", ""),

		DotenvPath: dotenvPath,
	}
	return cfg, nil
}

// Require returns the value of a required env-backed key, or a
// *MissingConfig error when it is unset/empty. Use for values that have no
// safe default (API keys, passwords, account ids).
func (c *Config) Require(key string, hint string) (string, error) {
	var v string
	switch key {
	case "TMS_PG_HOST":
		v = c.PGHost
	case "TMS_PG_USER":
		v = c.PGUser
	case "TMS_PG_PASSWORD":
		v = c.PGPassword
	case "TMS_PG_DATABASE":
		v = c.PGDatabase
	case "TMS_REDIS_ADDR":
		v = c.RedisAddr
	case "NASDAQ_DATA_LINK_API_KEY":
		v = c.NasdaqDataLinkAPIKey
	default:
		// Unknown keys fall back to the live environment so newly added
		// adapters can adopt Require before a typed field exists.
		v = os.Getenv(key)
	}
	if v == "" {
		return "", &MissingConfig{Key: key, Hint: hint, DotenvPath: c.DotenvPath}
	}
	return v, nil
}

// PostgresURL renders a connection URL with the given scheme
// (e.g. "postgres" for pgxpool, "pgx5" for golang-migrate's pgx/v5 driver).
func (c *Config) PostgresURL(scheme string) string {
	u := url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(c.PGHost, strconv.Itoa(c.PGPort)),
		Path:   "/" + c.PGDatabase,
	}
	if c.PGPassword != "" {
		u.User = url.UserPassword(c.PGUser, c.PGPassword)
	} else {
		u.User = url.User(c.PGUser)
	}
	q := url.Values{}
	q.Set("sslmode", c.PGSSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// PostgresDSN is the pgxpool-ready connection string.
func (c *Config) PostgresDSN() string { return c.PostgresURL("postgres") }

// MigrateURL is the golang-migrate database URL (pgx/v5 driver).
func (c *Config) MigrateURL() string { return c.PostgresURL("pgx5") }

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer, got %q: %w", key, v, err)
	}
	return n, nil
}
