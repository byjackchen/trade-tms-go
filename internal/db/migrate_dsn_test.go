package db

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateDSNPinsSearchPath(t *testing.T) {
	got, err := migrateDSN("pgx5://tms:secret@127.0.0.1:55432/tms?sslmode=disable")
	require.NoError(t, err)

	u, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, "pgx5", u.Scheme)
	assert.Equal(t, "public", u.Query().Get("search_path"),
		"version-table resolution must not depend on the role-named schema")
	assert.Equal(t, "disable", u.Query().Get("sslmode"), "existing params preserved")
	pw, _ := u.User.Password()
	assert.Equal(t, "secret", pw, "credentials preserved")
}

func TestMigrateDSNOverridesCallerSearchPath(t *testing.T) {
	got, err := migrateDSN("pgx5://tms@localhost:5432/tms?search_path=tms")
	require.NoError(t, err)
	u, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, "public", u.Query().Get("search_path"))
}

func TestMigrateDSNRejectsGarbage(t *testing.T) {
	_, err := migrateDSN("pgx5://bad url\x00")
	require.Error(t, err)
}
