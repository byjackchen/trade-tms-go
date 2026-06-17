package app

// Deployment-artifact regression guards. These pin two host-level safety
// properties of the repo's build/deploy files that nothing else compiles or
// executes in CI:
//
//  1. compose.yaml must never (re)introduce container_name values colliding
//     with a separate stack that may run containers literally named `tms-api`
//     and `tms-ui` on the shared dev host. Enabling the `app` profile with
//     those names would fail with a name conflict or tempt someone to stop the
//     other stack. App containers use the `tmsgo-` prefix.
//
//  2. .dockerignore must exist and exclude bin/ (stale binaries bust the
//     COPY . . layer cache), tmp/ (scratch dumps), docs/, .git, and — most
//     importantly — .env* so a developer's secrets are never copied into an
//     intermediate builder layer.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoRoot walks up from this source file to the directory containing
// go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found walking up from %s", file)
		dir = parent
	}
}

func TestComposeContainerNamesDoNotCollideWithReferenceStack(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "compose.yaml"))
	require.NoError(t, err)

	// Matches active AND commented-out placeholder services: the collision
	// would only bite when a later phase uncomments them, which is exactly
	// when nobody re-reviews the names.
	collide := regexp.MustCompile(`container_name:\s*tms-(api|ui|worker|live)\b`)
	assert.False(t, collide.Match(raw),
		"compose.yaml pins container_name %q, too close to a separate stack "+
			"that may run tms-api/tms-ui on this host; use the tmsgo- prefix",
		collide.FindString(string(raw)))
}

// TestAllContainerNamesUseTmsgoPrefix enforces the project convention that
// EVERY compose container is named `tmsgo-*` (uniform branding + no collision
// with a separate stack's `tms-*` containers on a shared host).
func TestAllContainerNamesUseTmsgoPrefix(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "compose.yaml"))
	require.NoError(t, err)

	names := regexp.MustCompile(`(?m)^\s*container_name:\s*(\S+)\s*$`).FindAllStringSubmatch(string(raw), -1)
	require.NotEmpty(t, names, "compose.yaml defines no container_name values")
	for _, m := range names {
		assert.Truef(t, strings.HasPrefix(m[1], "tmsgo-"),
			"container_name %q must use the tmsgo- prefix (all containers are tmsgo-*)", m[1])
	}
}

func TestDockerignoreCoversCacheBustersAndSecrets(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".dockerignore"))
	require.NoError(t, err, ".dockerignore must exist: COPY . . otherwise "+
		"ships bin/, tmp/, docs/ (cache busting) and a developer's .env "+
		"(secrets) into the builder layer")

	for _, entry := range []string{"bin/", "tmp/", ".env*", ".git", "docs/"} {
		re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(entry) + `\s*$`)
		assert.True(t, re.Match(raw), ".dockerignore missing required entry %q", entry)
	}

	// migrations/ is embedded into the binary and MUST stay in the context.
	banned := regexp.MustCompile(`(?m)^migrations/?\s*$`)
	assert.False(t, banned.Match(raw),
		".dockerignore must not exclude migrations/ (go:embed needs it)")
}
