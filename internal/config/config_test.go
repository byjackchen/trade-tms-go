package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	// Run from an empty temp dir so no stray .env on the machine interferes.
	chdir(t, t.TempDir())

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", cfg.PGHost)
	assert.Equal(t, 5432, cfg.PGPort)
	assert.Equal(t, "tms", cfg.PGUser)
	assert.Equal(t, "tms", cfg.PGDatabase)
	assert.Equal(t, "disable", cfg.PGSSLMode)
	assert.Equal(t, 16, cfg.PGMaxConns)
	assert.Equal(t, 2, cfg.PGMinConns)
	assert.Equal(t, "127.0.0.1:6379", cfg.RedisAddr)
	assert.Equal(t, 0, cfg.RedisDB)
	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, ":8080", cfg.APIAddr)
}

func TestLoadEnvOverridesAndDSN(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("TMS_PG_HOST", "dbhost")
	t.Setenv("TMS_PG_PORT", "55432")
	t.Setenv("TMS_PG_USER", "alice")
	t.Setenv("TMS_PG_PASSWORD", "s3cr@t/x")
	t.Setenv("TMS_PG_DATABASE", "tmsdb")
	t.Setenv("TMS_PG_SSLMODE", "require")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://alice:s3cr%40t%2Fx@dbhost:55432/tmsdb?sslmode=require", cfg.PostgresDSN())
	assert.Equal(t, "pgx5://alice:s3cr%40t%2Fx@dbhost:55432/tmsdb?sslmode=require", cfg.MigrateURL())
}

func TestLoadRejectsMalformedInt(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("TMS_PG_PORT", "not-a-port")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TMS_PG_PORT")
}

func TestLoadValidatesPoolBounds(t *testing.T) {
	chdir(t, t.TempDir())

	t.Setenv("TMS_PG_MAX_CONNS", "0")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TMS_PG_MAX_CONNS")

	t.Setenv("TMS_PG_MAX_CONNS", "4")
	t.Setenv("TMS_PG_MIN_CONNS", "9")
	_, err = Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TMS_PG_MIN_CONNS")

	t.Setenv("TMS_PG_MIN_CONNS", "4")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 4, cfg.PGMaxConns)
	assert.Equal(t, 4, cfg.PGMinConns)
}

func TestRequireMissingConfig(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("NASDAQ_DATA_LINK_API_KEY", "")

	cfg, err := Load()
	require.NoError(t, err)

	_, err = cfg.Require("NASDAQ_DATA_LINK_API_KEY", "sign up at https://data.nasdaq.com")
	require.Error(t, err)
	var missing *MissingConfig
	require.True(t, errors.As(err, &missing), "error must be *MissingConfig, got %T", err)
	assert.Equal(t, "NASDAQ_DATA_LINK_API_KEY", missing.Key)
	assert.Contains(t, err.Error(), "data.nasdaq.com")
	assert.Contains(t, err.Error(), ".env.example")
}

func TestRequirePresent(t *testing.T) {
	chdir(t, t.TempDir())
	t.Setenv("NASDAQ_DATA_LINK_API_KEY", "k-123")

	cfg, err := Load()
	require.NoError(t, err)
	v, err := cfg.Require("NASDAQ_DATA_LINK_API_KEY", "")
	require.NoError(t, err)
	assert.Equal(t, "k-123", v)
}

func TestDotenvLoadedWithoutOverride(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	content := "" +
		"# comment line\n" +
		"TMS_PG_HOST=envfile-host\n" +
		"export TMS_PG_USER=envfile-user\n" +
		"TMS_REDIS_ADDR=\"redis-from-file:6379\"\n" +
		"TMS_LOG_LEVEL=debug # inline comment\n" +
		"\n"
	require.NoError(t, os.WriteFile(envFile, []byte(content), 0o600))
	chdir(t, dir)

	// Real environment must win over .env (override=False semantics).
	t.Setenv("TMS_PG_HOST", "real-env-host")
	// Make sure the rest are unset so .env supplies them.
	for _, k := range []string{"TMS_PG_USER", "TMS_REDIS_ADDR", "TMS_LOG_LEVEL"} {
		t.Setenv(k, "")
		require.NoError(t, os.Unsetenv(k))
	}

	cfg, err := Load()
	require.NoError(t, err)
	// macOS: /var is a symlink to /private/var and findDotenv walks from
	// the resolved Getwd path, so compare symlink-resolved paths.
	wantPath, err := filepath.EvalSymlinks(envFile)
	require.NoError(t, err)
	gotPath, err := filepath.EvalSymlinks(cfg.DotenvPath)
	require.NoError(t, err)
	assert.Equal(t, wantPath, gotPath)
	assert.Equal(t, "real-env-host", cfg.PGHost, ".env must not override real env")
	assert.Equal(t, "envfile-user", cfg.PGUser)
	assert.Equal(t, "redis-from-file:6379", cfg.RedisAddr)
	assert.Equal(t, "debug", cfg.LogLevel)
}

func TestDotenvQuotedHashPreserved(t *testing.T) {
	dir := t.TempDir()
	content := "TMS_PG_PASSWORD=\"p#ss w0rd\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600))
	chdir(t, dir)
	t.Setenv("TMS_PG_PASSWORD", "") // registers cleanup restore
	require.NoError(t, os.Unsetenv("TMS_PG_PASSWORD"))

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "p#ss w0rd", cfg.PGPassword, "a # inside quotes is part of the value, not a comment")
}

func TestDotenvMalformedLine(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("THIS IS NOT KEY VALUE\n"), 0o600))
	chdir(t, dir)

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed")
}

// chdir switches the working directory for the duration of the test.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(old) })
}
