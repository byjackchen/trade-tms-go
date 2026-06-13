package migrations

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fileNameRe is golang-migrate's expected naming convention.
var fileNameRe = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$`)

func sqlFiles(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := fs.ReadDir(Files, ".")
	require.NoError(t, err)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			b, err := fs.ReadFile(Files, e.Name())
			require.NoError(t, err)
			out[e.Name()] = string(b)
		}
	}
	return out
}

func TestNamingConventionAndPairs(t *testing.T) {
	files := sqlFiles(t)
	require.NotEmpty(t, files, "no migrations embedded")

	versions := map[int]map[string]bool{} // version -> {"up": true, "down": true}
	for name, body := range files {
		m := fileNameRe.FindStringSubmatch(name)
		require.NotNil(t, m, "file %q does not match NNNNNN_name.(up|down).sql", name)
		require.NotEmpty(t, strings.TrimSpace(body), "migration %q is empty", name)

		v, err := strconv.Atoi(m[1])
		require.NoError(t, err)
		if versions[v] == nil {
			versions[v] = map[string]bool{}
		}
		require.False(t, versions[v][m[3]], "duplicate %s migration for version %d", m[3], v)
		versions[v][m[3]] = true
	}

	// Every version has both directions and versions are contiguous from 1.
	var sorted []int
	for v := range versions {
		assert.True(t, versions[v]["up"], "version %d missing up migration", v)
		assert.True(t, versions[v]["down"], "version %d missing down migration", v)
		sorted = append(sorted, v)
	}
	sort.Ints(sorted)
	for i, v := range sorted {
		assert.Equal(t, i+1, v, "versions must be contiguous starting at 1, got %v", sorted)
	}
	assert.GreaterOrEqual(t, len(sorted), 6, "expected at least the six domain migrations")
}

// TestEveryCreatedTableIsDropped statically checks that each up migration's
// CREATE TABLE has a matching DROP TABLE in the same version's down
// migration, so `tms migrate down` actually reverses `up`.
func TestEveryCreatedTableIsDropped(t *testing.T) {
	files := sqlFiles(t)
	createRe := regexp.MustCompile(`(?i)CREATE TABLE\s+(tms\.[a-z0-9_]+)`)
	dropRe := regexp.MustCompile(`(?i)DROP TABLE IF EXISTS\s+(tms\.[a-z0-9_]+)`)

	for name, body := range files {
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		downName := strings.Replace(name, ".up.sql", ".down.sql", 1)
		down, ok := files[downName]
		require.True(t, ok, "missing down file for %s", name)

		created := map[string]bool{}
		for _, m := range createRe.FindAllStringSubmatch(body, -1) {
			created[strings.ToLower(m[1])] = true
		}
		dropped := map[string]bool{}
		for _, m := range dropRe.FindAllStringSubmatch(down, -1) {
			dropped[strings.ToLower(m[1])] = true
		}
		for tbl := range created {
			assert.True(t, dropped[tbl], "%s creates %s but %s does not drop it", name, tbl, downName)
		}
		for tbl := range dropped {
			assert.True(t, created[tbl], "%s drops %s which %s never creates", downName, tbl, name)
		}
	}
}

// TestIofsSourceWalksAllVersions proves the embedded FS is consumable by the
// exact source driver the migrator uses, and that First/Next enumerate every
// version (a malformed filename would silently vanish from the sequence).
func TestIofsSourceWalksAllVersions(t *testing.T) {
	src, err := iofs.New(Files, ".")
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })

	var got []uint
	v, err := src.First()
	require.NoError(t, err)
	for {
		got = append(got, v)

		up, _, err := src.ReadUp(v)
		require.NoError(t, err, "version %d has no readable up migration", v)
		_ = up.Close()
		down, _, err := src.ReadDown(v)
		require.NoError(t, err, "version %d has no readable down migration", v)
		_ = down.Close()

		next, err := src.Next(v)
		if err != nil {
			var pathErr *fs.PathError
			ok := errors.As(err, &pathErr) || errors.Is(err, os.ErrNotExist)
			require.True(t, ok, "unexpected error walking source: %v", err)
			break
		}
		v = next
	}

	require.GreaterOrEqual(t, len(got), 6)
	require.Equal(t, uint(1), got[0])
	for i := 1; i < len(got); i++ {
		require.Equal(t, got[i-1]+1, got[i], "gap in migration sequence: %v", got)
	}

	// The same property via the driver's open-by-URL path is not available
	// for embed.FS, so also sanity-check Prev symmetry from the last version.
	prev, err := src.Prev(got[len(got)-1])
	require.NoError(t, err)
	require.Equal(t, got[len(got)-2], prev)
}

// TestSchemaConventions enforces project-wide DDL rules that reviewers
// would otherwise have to eyeball: every table lives in the tms schema, and
// every CREATE TABLE in the up migrations is qualified.
func TestSchemaConventions(t *testing.T) {
	files := sqlFiles(t)
	createAnyRe := regexp.MustCompile(`(?i)CREATE TABLE\s+([a-z0-9_."]+)`)
	for name, body := range files {
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		for _, m := range createAnyRe.FindAllStringSubmatch(body, -1) {
			tbl := strings.ToLower(m[1])
			assert.True(t, strings.HasPrefix(tbl, "tms."),
				"%s: table %q must be schema-qualified under tms.", name, m[1])
		}
	}
}

// TestMoneyColumnsDocumented spot-checks that the fixed-point money
// convention is documented where it matters most: the daily-bars price
// columns must be BIGINT and carry the 1e-4 scale comment.
func TestMoneyColumnsDocumented(t *testing.T) {
	files := sqlFiles(t)
	md, ok := files["000002_marketdata.up.sql"]
	require.True(t, ok)

	for _, col := range []string{"open", "high", "low", "close", "close_adj", "close_unadj"} {
		re := regexp.MustCompile(fmt.Sprintf(`(?m)^\s+%s\s+BIGINT`, col))
		assert.True(t, re.MatchString(md), "bars_daily column %q must be BIGINT fixed-point", col)
	}
	assert.Contains(t, md, "1e-4", "marketdata migration must document the 1e-4 money scale")
	assert.Contains(t, md, "dollars * 10000", "marketdata migration must document the scale formula")
}
